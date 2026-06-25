// Package run is the Pulp runtime entry point. Deployment binaries
// blank-import extensions and call run.Main():
//
//	package main
//
//	import (
//		_ "github.com/BananaLabs-OSS/Pulp-ext-http"
//		_ "github.com/BananaLabs-OSS/Pulp-ext-docker"
//
//		"github.com/BananaLabs-OSS/Pulp/run"
//	)
//
//	func main() { run.Main() }
//
// Extensions registered via ext.Register are automatically picked up.
// The runtime accepts one or more manifests via repeated -manifest
// flags, starts one step-loop goroutine per cell, and one pollster
// goroutine per extension-with-Poll. Events flow from pollsters into
// per-cell event channels tagged by StepEvent.CellID; empty
// CellID broadcasts to every cell that declares the producing
// extension's capability.
package run

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BananaLabs-OSS/Pulp/abi"
	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/BananaLabs-OSS/Pulp/internal/safe"
)

// eventChanSize is the per-cell event channel buffer. Pollsters drop
// events (with a warn log) when a cell's channel fills up.
const eventChanSize = 64

// cellRuntime is the per-cell runtime state: the loaded WASM
// cell, its event channel, its goroutine's context, and the set of
// capabilities it declared. The step loop consumes from events, calls
// cell.Step(), and calls Finalize on the producing extension.
type cellRuntime struct {
	spec       *manifest.CellSpec
	cell     *host.Cell
	events     chan routedEvent
	ctx        context.Context
	cancel     context.CancelFunc
	declared   map[string]bool // capabilities this cell declared
	readyCh    chan struct{}   // closed after Init returns 0
	callNumber atomic.Uint64   // atomic: written by the step loop, read by ctl status

	// stepDone is closed when this cell's step goroutine exits. Recreated
	// each time a step loop is launched (initial start + every reload) so a
	// reload can join the old loop before starting the new one.
	stepDone chan struct{}

	// failed is set when the cell's Setup, Load, or Init returned an
	// error. Failed cells do not run their step loop; dependents of a
	// failed cell inherit the failed state. Atomic: written during startup,
	// read by ctl status concurrently.
	failed atomic.Bool
}

// routedEvent wraps an ext.StepEvent with a back-reference to the
// capability that produced it, so the step goroutine can call the
// right Finalize after processing.
type routedEvent struct {
	ev   ext.StepEvent
	caps []ext.Capability // extensions to call Finalize on (usually one)
}

func Main() {
	// `<exe> ctl <op> [cell]` is the control-socket CLIENT, not the host.
	// Dispatched before flag parsing so a cell can run `<exe> ctl reload <name>`
	// (via spawn.process) to hot-swap itself. Works for any deployment binary
	// that calls run.Main (pulp, projx-host, …).
	if len(os.Args) > 1 && os.Args[1] == "ctl" {
		os.Exit(RunCtl(os.Args[2:]))
	}

	var manifestPaths sliceFlag
	var storageRoot string
	var httpPort string
	flag.Var(&manifestPaths, "manifest", "path to pulp.cell.toml (repeatable; also accepts comma-separated values)")
	flag.StringVar(&storageRoot, "storage-root", "./data", "root directory for cell-scoped storage")
	flag.StringVar(&httpPort, "http-port", "", "override for the HTTP_PORT env var consumed by ext-http")
	flag.Parse()

	// The -http-port flag is a convenience shim: it forwards to the
	// HTTP_PORT env var that ext-http reads during Setup. Explicit env
	// vars win over the flag.
	if httpPort != "" {
		if _, ok := os.LookupEnv("HTTP_PORT"); !ok {
			_ = os.Setenv("HTTP_PORT", httpPort)
		}
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if len(manifestPaths) == 0 {
		logger.Error("missing required flag -manifest <path-to-pulp.cell.toml>")
		os.Exit(2)
	}

	set, err := manifest.LoadAll([]string(manifestPaths))
	if err != nil {
		logger.Error("manifest load failed", "err", err)
		os.Exit(1)
	}

	for _, spec := range set.Cells {
		logger.Info("pulp boot",
			"cell", spec.Name,
			"version", spec.Version,
			"manifest", spec.ManifestPath,
			"wasm", spec.WASMPath,
			"capabilities", spec.Capabilities,
			"provides", spec.Provides,
			"consumes", spec.Consumes,
		)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ------------------------------------------------------------------
	// Capability Setup (once, not once-per-cell)
	// ------------------------------------------------------------------

	// Union of capabilities declared across all cells — each gets its
	// Setup called at most once regardless of how many cells declared
	// it. The registry is shared across cell Loads.
	allCaps := ext.All()
	capByName := map[string]ext.Capability{}
	for _, c := range allCaps {
		capByName[c.Name] = c
	}

	// capToCells: given a capability name, which cells declared it?
	// Used by the fanout router to broadcast events with empty CellID.
	capToCells := map[string][]string{}
	for _, spec := range set.Cells {
		for _, capName := range spec.Capabilities {
			capToCells[capName] = append(capToCells[capName], spec.Name)
		}
	}

	declaredUnion := map[string]bool{}
	for _, spec := range set.Cells {
		for _, capName := range spec.Capabilities {
			declaredUnion[capName] = true
		}
	}

	// Setup each declared capability once. A Setup failure aborts the
	// whole host — the capability is shared, so no cell can rely on
	// the extension working.
	setupEnv := ext.SetupEnv{
		// CellName is left empty at Setup time — Setup is now a
		// one-time shared initialization, not per-cell. Extensions
		// that need per-cell state should maintain it in a map keyed
		// by the cell identity they see at Register time.
		StorageRoot: storageRoot,
		Logger:      logger,
	}
	for _, c := range allCaps {
		if declaredUnion[c.Name] {
			if err := safe.CallSetup(c, setupEnv, logger); err != nil {
				logger.Error("capability setup failed", "capability", c.Name, "err", err)
				os.Exit(1)
			}
			if c.Setup != nil {
				logger.Info("capability ready", "name", c.Name)
			}
		}
	}

	// ------------------------------------------------------------------
	// Build per-cell runtimes in topological order
	// ------------------------------------------------------------------

	runtimes := map[string]*cellRuntime{}
	for _, spec := range set.Order {
		pctx, pcancel := context.WithCancel(ctx)
		declared := map[string]bool{}
		for _, c := range spec.Capabilities {
			declared[c] = true
		}
		runtimes[spec.Name] = &cellRuntime{
			spec:     spec,
			events:   make(chan routedEvent, eventChanSize),
			ctx:      pctx,
			cancel:   pcancel,
			declared: declared,
			readyCh:  make(chan struct{}),
			stepDone: make(chan struct{}),
		}
	}

	// ------------------------------------------------------------------
	// Per-cell Load + Init with dependency barriers
	// ------------------------------------------------------------------

	registry := host.NewRegistry()
	for _, c := range allCaps {
		registry.Gated(c)
	}

	// Sibling-call capability is always bound — every cell can call
	// providers in other cells as long as its manifest declares them
	// via consumes or depends_on. Runtime permission check happens in
	// the pulp_call host function body.
	siblingReg := newSiblingRegistry(runtimes)
	registry.Always(siblingCapability(siblingReg))

	// Validate sibling links up front so a missing provider fails boot
	// instead of producing runtime errors when the call happens.
	if missing := validateSiblingLinks(runtimes); len(missing) > 0 {
		for _, m := range missing {
			logger.Error("sibling link validation", "issue", m)
		}
		os.Exit(1)
	}

	// Kick off an init goroutine per cell; each waits on its deps'
	// readyCh before Loading. Cells whose deps fail inherit the failed
	// state.
	var initWG sync.WaitGroup
	for _, spec := range set.Order {
		initWG.Add(1)
		go func(spec *manifest.CellSpec) {
			defer initWG.Done()
			rt := runtimes[spec.Name]

			for _, dep := range spec.DependsOn {
				depRT := runtimes[dep]
				select {
				case <-depRT.readyCh:
					if depRT.failed.Load() {
						logger.Error("cell init aborted — dependency failed",
							"cell", spec.Name, "failed_dep", dep)
						rt.failed.Store(true)
						close(rt.readyCh)
						return
					}
				case <-rt.ctx.Done():
					rt.failed.Store(true)
					close(rt.readyCh)
					return
				}
			}

			configBytes, err := manifest.EncodeConfig(spec.Config)
			if err != nil {
				logger.Error("config encode failed", "cell", spec.Name, "err", err)
				rt.failed.Store(true)
				close(rt.readyCh)
				return
			}

			limits := &host.Limits{
				MaxMemoryPages: spec.MaxMemoryPages,
				CallTimeout:    time.Duration(spec.CallTimeoutMS) * time.Millisecond,
			}
			cell, err := host.Load(rt.ctx, spec, registry, limits, logger)
			if err != nil {
				logger.Error("load failed", "cell", spec.Name, "err", err)
				rt.failed.Store(true)
				close(rt.readyCh)
				return
			}
			rt.cell = cell

			if err := cell.Init(rt.ctx, configBytes); err != nil {
				logger.Error("init failed", "cell", spec.Name, "err", err)
				rt.failed.Store(true)
				close(rt.readyCh)
				return
			}
			logger.Info("cell ready",
				"cell", spec.Name,
				"version", spec.Version,
				"capabilities", spec.Capabilities,
				"depends_on", spec.DependsOn,
			)
			close(rt.readyCh)
		}(spec)
	}

	// Wait for all cells to finish initializing (successfully or not).
	initWG.Wait()

	// If EVERY cell failed, there's nothing to run — exit.
	anyReady := false
	for _, rt := range runtimes {
		if !rt.failed.Load() {
			anyReady = true
			break
		}
	}
	if !anyReady {
		logger.Error("all cells failed to start")
		os.Exit(1)
	}

	// ------------------------------------------------------------------
	// Start per-extension pollsters + per-cell step goroutines
	// ------------------------------------------------------------------

	// Pollsters run for the host lifetime; each one polls a single
	// extension that has a non-nil Poll function. Events are tagged
	// with CellID by the extension (or left empty for broadcast).
	stopPoll := make(chan struct{})
	var pollWG sync.WaitGroup
	for _, c := range allCaps {
		if c.Poll == nil || !declaredUnion[c.Name] {
			continue
		}
		pollWG.Add(1)
		go runPollster(c, stopPoll, &pollWG, runtimes, capToCells, logger)
	}

	// runtimeOps owns step-loop launching so the control socket's reload op
	// can relaunch a cell through the same path. Built before the step loops
	// start; reload needs the registry, the capability lookup, and the parent
	// context to re-Load + re-Init a cell from disk while the host stays up.
	ops := &runtimeOps{
		runtimes:      runtimes,
		allCaps:       allCaps,
		declaredUnion: declaredUnion,
		logger:        logger,
		registry:      registry,
		capByName:     capByName,
		parentCtx:     ctx,
	}

	// Step goroutines — one per cell that initialized successfully.
	for _, rt := range runtimes {
		if rt.failed.Load() {
			continue
		}
		ops.launchStep(rt)
	}

	// ------------------------------------------------------------------
	// Start the control socket — enables graceful per-cell shutdown,
	// live reload, and remote status. Optional; if the socket fails to
	// bind the host keeps running without it.
	// ------------------------------------------------------------------

	ctlServer := startControlServer(ops, logger)
	defer ctlServer.stop()

	// ------------------------------------------------------------------
	// Wait for signal, then shut everything down
	// ------------------------------------------------------------------

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, ShutdownSignals()...)

	select {
	case sig := <-sigCh:
		logger.Info("signal received", "signal", sig.String())
	case <-ops.allShutdown():
		logger.Info("all cells shut down via control socket; exiting")
	}

	// Cancel the allShutdown poller — regardless of which branch above
	// won, the watchdog goroutine is no longer needed and would otherwise
	// spin until process teardown.
	ops.stopWatchdog()

	// Stop pollsters first so no new events are queued.
	close(stopPoll)
	pollWG.Wait()

	// Cancel each cell's context so step goroutines exit.
	for _, rt := range runtimes {
		rt.cancel()
	}
	ops.stepWG.Wait()

	// Drain any events still queued in per-cell channels and Finalize
	// them so extensions don't leak per-event slot state. This mirrors
	// what runtimeOps.shutdownCell does on the control-socket path —
	// here we cover the signal / allShutdown path too.
	for _, rt := range runtimes {
		for drained := true; drained; {
			drained = false
			select {
			case re := <-rt.events:
				for _, c := range re.caps {
					safe.CallFinalize(c, re.ev.ID, logger)
				}
				drained = true
			default:
			}
		}
	}

	// Per-cell Shutdown + probe logging. Cells already stopped by the
	// control socket's shutdownCell path are skipped — they've already
	// gone through Shutdown + Close + TeardownCell. Querying their cell
	// here would race with runtimeOps.shutdownCell clearing rt.cell.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	for name, rt := range runtimes {
		if ops.isStopped(name) {
			continue
		}
		if rt.cell == nil {
			continue
		}
		if last, ok := rt.cell.ProbeLastCall(shutdownCtx); ok {
			logger.Info("probe last envelope", "last_call", last)
		}
		if marker, ok := rt.cell.ProbeConfigMarker(shutdownCtx); ok {
			logger.Info("probe config marker", "marker", marker)
		}
		if err := rt.cell.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown failed", "cell", rt.spec.Name, "err", err)
		}
		rt.cell.Close(context.Background())
	}

	// Capability Teardown — called once per capability, not per cell.
	for _, c := range allCaps {
		if declaredUnion[c.Name] {
			safe.CallTeardown(shutdownCtx, c, logger)
		}
	}

	logger.Info("pulp exit clean")
}

// runPollster polls one extension in a loop, publishing each returned
// event to the appropriate cell's event channel. Events with a
// non-empty CellID go to that cell; events with an empty CellID
// broadcast to every cell declaring the extension's capability.
func runPollster(
	c ext.Capability,
	stop <-chan struct{},
	wg *sync.WaitGroup,
	runtimes map[string]*cellRuntime,
	capToCells map[string][]string,
	logger *slog.Logger,
) {
	defer wg.Done()
	broadcastTargets := capToCells[c.Name]

	for {
		select {
		case <-stop:
			return
		default:
		}

		ev, ok := safe.CallPoll(c, logger)
		if !ok {
			// Nothing available; idle briefly to avoid pegging the CPU.
			time.Sleep(200 * time.Microsecond)
			continue
		}

		routed := routedEvent{ev: ev, caps: []ext.Capability{c}}

		if ev.CellID != "" {
			rt, found := runtimes[ev.CellID]
			if !found || rt.failed.Load() {
				// Cell is unknown or failed-to-start; drop the event
				// and Finalize so the extension doesn't leak the slot.
				safe.CallFinalize(c, ev.ID, logger)
				continue
			}
			deliver(rt, routed, c, logger)
			continue
		}

		// Broadcast: deliver to every cell declaring c.Name.
		// Each delivered copy carries the same caps slice, so each cell's
		// step loop calls Finalize independently when it dequeues the event.
		// Extensions whose Finalize is idempotent (e.g. ext-http: lookup by
		// ID, no-op if already removed) handle this safely. Extensions with
		// non-idempotent Finalize must not be used in broadcast scenarios.
		delivered := false
		for _, name := range broadcastTargets {
			rt := runtimes[name]
			if rt == nil || rt.failed.Load() {
				continue
			}
			deliver(rt, routed, c, logger)
			delivered = true
		}
		if !delivered {
			safe.CallFinalize(c, ev.ID, logger)
		}
	}
}

// deliver sends the routed event to rt.events. If the channel is full,
// drop with a warn log (drop-newest preserves FIFO ordering of older
// queued events).
func deliver(rt *cellRuntime, r routedEvent, c ext.Capability, logger *slog.Logger) {
	select {
	case rt.events <- r:
	default:
		logger.Warn("cell event channel full; dropping event",
			"cell", rt.spec.Name,
			"kind", r.ev.Kind,
		)
		safe.CallFinalize(c, r.ev.ID, logger)
	}
}

// runStepLoop is the per-cell step loop. It reads events from the
// cell's channel, encodes them, calls cell.Step, and calls Finalize.
// When the cell's context is cancelled, it exits immediately without
// draining; the caller (run.Main or runtimeOps.shutdownCell) is
// responsible for draining remaining events and Finalize-ing them so
// extensions don't leak per-event slot state.
//
// Idle pacing: when no event arrives we still call Step with a nil
// payload so the cell's own tickers / timeouts can advance. Between
// idle steps we back off using an adaptive timer rather than a fixed
// busy-wait. Starting at 200µs (same as the previous behavior, low
// latency once an event lands) we double the sleep up to 10ms when
// the cell has been quiet for over a second, so a fleet of idle cells
// no longer burns a measurable slice of one core each. As soon as
// real work arrives the timer is reset, restoring the original
// snappy pickup latency.
func stepLoop(rt *cellRuntime, capByName map[string]ext.Capability, logger *slog.Logger) {
	const (
		idleMin     = 200 * time.Microsecond
		idleMax     = 10 * time.Millisecond
		idleRampAge = time.Second
	)
	idleSleep := idleMin
	idleSince := time.Time{}
	idleTimer := time.NewTimer(idleMin)
	if !idleTimer.Stop() {
		<-idleTimer.C
	}
	defer idleTimer.Stop()

	for {
		select {
		case <-rt.ctx.Done():
			return
		case re := <-rt.events:
			// Real event — reset idle pacing so the next idle gap
			// starts back at the snappy 200µs floor.
			idleSleep = idleMin
			idleSince = time.Time{}
			stepEv, err := abi.EncodeStepEvent(re.ev.Kind, re.ev.Payload)
			if err != nil {
				logger.Error("encode step event",
					"cell", rt.spec.Name, "kind", re.ev.Kind, "err", err)
				for _, c := range re.caps {
					safe.CallFinalize(c, re.ev.ID, logger)
				}
				continue
			}
			n := rt.callNumber.Load()
			env := abi.StepEnvelope{
				CallNumber: n,
				WallTime:   uint64(time.Now().UnixNano()),
				Payload:    stepEv,
			}
			if _, err := rt.cell.Step(rt.ctx, env); err != nil {
				logger.Error("step failed",
					"cell", rt.spec.Name,
					"call_number", n,
					"err", err)
			}
			for _, c := range re.caps {
				safe.CallFinalize(c, re.ev.ID, logger)
			}
			if n%10000 == 0 {
				logger.Info("step heartbeat",
					"cell", rt.spec.Name, "call_number", n)
			}
			rt.callNumber.Add(1)
		default:
			// No event pending — submit an empty step envelope so the
			// cell still advances wall-time and can run its own idle
			// logic (ticks, timeouts). Matches pre-multi-cell
			// behavior where the step loop always called Step, even
			// with nil payload.
			n := rt.callNumber.Load()
			env := abi.StepEnvelope{
				CallNumber: n,
				WallTime:   uint64(time.Now().UnixNano()),
				Payload:    nil,
			}
			if _, err := rt.cell.Step(rt.ctx, env); err != nil {
				logger.Error("step failed (idle)",
					"cell", rt.spec.Name,
					"call_number", n,
					"err", err)
			}
			if n%10000 == 0 {
				logger.Info("step heartbeat",
					"cell", rt.spec.Name, "call_number", n)
			}
			rt.callNumber.Add(1)

			// Idle back-off — wake early on a real event OR on cancel.
			// After idleRampAge of pure-idle we double idleSleep each
			// iteration up to idleMax so a truly quiet cell costs
			// microseconds of CPU per second instead of milliseconds.
			now := time.Now()
			if idleSince.IsZero() {
				idleSince = now
			}
			if now.Sub(idleSince) > idleRampAge && idleSleep < idleMax {
				idleSleep *= 2
				if idleSleep > idleMax {
					idleSleep = idleMax
				}
			}
			idleTimer.Reset(idleSleep)
			select {
			case <-rt.ctx.Done():
				if !idleTimer.Stop() {
					<-idleTimer.C
				}
				return
			case re := <-rt.events:
				if !idleTimer.Stop() {
					<-idleTimer.C
				}
				// Push the event back so the outer loop picks it up
				// uniformly. The cell's events channel is buffered, so
				// this send will only block if the channel filled
				// between the recv and the resend — in which case the
				// outer broadcast already handled drop semantics, so
				// we drop here too and Finalize.
				select {
				case rt.events <- re:
				default:
					for _, c := range re.caps {
						safe.CallFinalize(c, re.ev.ID, logger)
					}
				}
				idleSleep = idleMin
				idleSince = time.Time{}
			case <-idleTimer.C:
				// Tick — keep idling.
			}
		}
	}
}
