package run

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/BananaLabs-OSS/Pulp/internal/safe"
)

// runtimeOps implements the controlOps interface for the control
// socket. It owns per-cell shutdown: cancels the cell's context,
// waits for its step loop to exit, calls cell.Shutdown, and invokes
// each extension's TeardownCell hook (if set) to drop cell-scoped
// state while leaving other cells' state intact.
type runtimeOps struct {
	runtimes      map[string]*cellRuntime
	allCaps       []ext.Capability
	declaredUnion map[string]bool
	logger        *slog.Logger

	// Reload needs these to re-Load + re-Init a cell from disk and relaunch
	// its step loop while the host keeps running.
	registry  *host.Registry
	capByName map[string]ext.Capability
	parentCtx context.Context
	stepWG    sync.WaitGroup // joins every step goroutine (initial + reloaded)
	reloadMu  sync.Mutex     // serializes reloads so two never race one cell

	mu      sync.Mutex
	stopped map[string]bool // cell names that have already been shut down

	// watchdogStop cancels the allShutdown poller goroutine on process
	// exit so it doesn't leak when the signal path is taken instead of
	// the control-socket path.
	watchdogStop     chan struct{}
	watchdogStopOnce sync.Once
}

func (o *runtimeOps) status() []ctlStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]ctlStatus, 0, len(o.runtimes))
	for name, rt := range o.runtimes {
		state := "ready"
		switch {
		case rt.failed.Load():
			state = "failed"
		case o.stopped[name]:
			state = "stopped"
		}
		out = append(out, ctlStatus{
			Name:  name,
			State: state,
			Steps: rt.callNumber.Load(),
		})
	}
	return out
}

func (o *runtimeOps) shutdownCell(name string) error {
	o.mu.Lock()
	rt, ok := o.runtimes[name]
	if !ok {
		o.mu.Unlock()
		return fmt.Errorf("unknown cell: %q", name)
	}
	if o.stopped == nil {
		o.stopped = map[string]bool{}
	}
	if o.stopped[name] {
		o.mu.Unlock()
		return fmt.Errorf("cell %q already stopped", name)
	}
	o.stopped[name] = true
	o.mu.Unlock()

	o.logger.Info("control: shutting down cell", "cell", name)

	// 1. Cancel the cell's context — its step goroutine exits.
	rt.cancel()

	// 2. Shutdown the WASM module. Uses a fresh context because rt.ctx
	// is already cancelled.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if rt.cell != nil {
		if err := rt.cell.Shutdown(shutdownCtx); err != nil {
			o.logger.Error("control: cell shutdown failed", "cell", name, "err", err)
		}
		rt.cell.Close(context.Background())
		rt.cell = nil
	}

	// 3. Call TeardownCell on each capability the cell declared —
	// extensions that maintain per-cell state (routes, sockets) drop
	// just this cell's entries.
	for _, c := range o.allCaps {
		if !rt.declared[c.Name] || c.TeardownCell == nil {
			continue
		}
		if err := c.TeardownCell(shutdownCtx, name); err != nil {
			o.logger.Warn("control: capability teardown_cell failed",
				"cell", name, "capability", c.Name, "err", err)
		}
	}

	// 4. Drain any pending events in the cell's channel under panic
	// recovery — Finalize is best-effort here.
	drain := func() {
		for {
			select {
			case re := <-rt.events:
				for _, c := range re.caps {
					safe.CallFinalize(c, re.ev.ID, o.logger)
				}
			default:
				return
			}
		}
	}
	drain()

	o.logger.Info("control: cell stopped", "cell", name)
	return nil
}

func (o *runtimeOps) shutdownAll() error {
	o.mu.Lock()
	names := make([]string, 0, len(o.runtimes))
	for n := range o.runtimes {
		names = append(names, n)
	}
	o.mu.Unlock()

	for _, n := range names {
		o.mu.Lock()
		already := o.stopped[n]
		o.mu.Unlock()
		if already {
			continue
		}
		_ = o.shutdownCell(n)
	}
	return nil
}

// allShutdown returns a channel that closes when every non-failed
// cell has been stopped (either via the control socket or naturally).
// run.Main selects on this channel alongside the signal channel so a
// control-socket shutdown_all can exit the host cleanly without a
// signal.
//
// The poller goroutine is cancelled via stopWatchdog when run.Main
// takes the signal path — without that, a signal-driven exit would
// leak the goroutine until process teardown.
func (o *runtimeOps) allShutdown() <-chan struct{} {
	ch := make(chan struct{})
	o.mu.Lock()
	if o.watchdogStop == nil {
		o.watchdogStop = make(chan struct{})
	}
	stop := o.watchdogStop
	o.mu.Unlock()

	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			o.mu.Lock()
			total := 0
			stoppedCount := 0
			for name, rt := range o.runtimes {
				if rt.failed.Load() {
					continue
				}
				total++
				if o.stopped[name] {
					stoppedCount++
				}
			}
			o.mu.Unlock()
			if total > 0 && stoppedCount == total {
				close(ch)
				return
			}
		}
	}()
	return ch
}

// isStopped reports whether a cell has been shut down via the control
// socket. Used by run.Main's final shutdown loop to skip cells whose
// Shutdown + Close + TeardownCell have already run, avoiding a race on
// rt.cell being cleared under the runtimeOps lock.
func (o *runtimeOps) isStopped(name string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stopped[name]
}

// launchStep starts a fresh step goroutine for rt. It allocates a new
// stepDone channel (closed when the loop exits) so a later reload can join
// the old loop before starting a new one, and registers the goroutine with
// stepWG so run.Main's shutdown waits for it. Used for both the initial
// start and every reload.
func (o *runtimeOps) launchStep(rt *cellRuntime) {
	rt.stepDone = make(chan struct{})
	o.stepWG.Add(1)
	go func() {
		defer o.stepWG.Done()
		defer close(rt.stepDone)
		stepLoop(rt, o.capByName, o.logger)
	}()
}

// reloadCell hot-swaps a cell IN PLACE: it stops the running cell, drops its
// per-cell host state, then re-reads the WASM from disk (picking up a fresh
// build), re-Inits, and relaunches its step loop — all while the host process
// and every other cell keep running. This is the live-reload primitive: build
// a new cell.wasm, then `pulp ctl reload <cell>` to swap it in with no
// downtime and no restart.
//
// Reloads are serialized by reloadMu so two concurrent control connections
// can't tear the same cell down twice.
func (o *runtimeOps) reloadCell(name string) error {
	o.reloadMu.Lock()
	defer o.reloadMu.Unlock()

	o.mu.Lock()
	rt, ok := o.runtimes[name]
	if !ok {
		o.mu.Unlock()
		return fmt.Errorf("unknown cell: %q", name)
	}
	if o.stopped[name] {
		o.mu.Unlock()
		return fmt.Errorf("cell %q is stopped; cannot reload", name)
	}
	o.mu.Unlock()

	o.logger.Info("control: reloading cell", "cell", name)

	// 1. Stop the running cell: cancel its context and JOIN its step loop so
	// no goroutine is driving the old module when we tear it down.
	rt.cancel()
	if rt.stepDone != nil {
		<-rt.stepDone
	}

	// 2. Shutdown + Close the old module (fresh ctx — rt.ctx is cancelled).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if rt.cell != nil {
		if err := rt.cell.Shutdown(shutdownCtx); err != nil {
			o.logger.Warn("control: reload shutdown failed", "cell", name, "err", err)
		}
		rt.cell.Close(context.Background())
		rt.cell = nil
	}

	// 3. Drop per-cell host state (routes, sockets, DB handles) so the fresh
	// Init re-registers cleanly instead of colliding with the old cell's.
	for _, c := range o.allCaps {
		if !rt.declared[c.Name] || c.TeardownCell == nil {
			continue
		}
		if err := c.TeardownCell(shutdownCtx, name); err != nil {
			o.logger.Warn("control: reload teardown_cell failed",
				"cell", name, "capability", c.Name, "err", err)
		}
	}

	// 4. Drain any events still queued for the old cell.
	for drained := true; drained; {
		drained = false
		select {
		case re := <-rt.events:
			for _, c := range re.caps {
				safe.CallFinalize(c, re.ev.ID, o.logger)
			}
			drained = true
		default:
		}
	}

	// 5. Fresh context + reload the WASM from disk.
	rt.ctx, rt.cancel = context.WithCancel(o.parentCtx)
	spec := rt.spec
	configBytes, err := manifest.EncodeConfig(spec.Config)
	if err != nil {
		rt.failed.Store(true)
		return fmt.Errorf("reload %q: config encode: %w", name, err)
	}
	limits := &host.Limits{
		MaxMemoryPages: spec.MaxMemoryPages,
		CallTimeout:    time.Duration(spec.CallTimeoutMS) * time.Millisecond,
	}
	cell, err := host.Load(rt.ctx, spec, o.registry, limits, o.logger)
	if err != nil {
		rt.failed.Store(true)
		return fmt.Errorf("reload %q: load: %w", name, err)
	}
	if err := cell.Init(rt.ctx, configBytes); err != nil {
		cell.Close(context.Background())
		rt.failed.Store(true)
		return fmt.Errorf("reload %q: init: %w", name, err)
	}
	rt.cell = cell
	rt.failed.Store(false)
	rt.callNumber.Store(0)

	// 6. Relaunch the step loop on the new module.
	o.launchStep(rt)
	o.logger.Info("control: cell reloaded", "cell", name, "version", spec.Version)
	return nil
}

// stopWatchdog cancels the allShutdown poller goroutine. Idempotent.
func (o *runtimeOps) stopWatchdog() {
	o.watchdogStopOnce.Do(func() {
		o.mu.Lock()
		ch := o.watchdogStop
		o.mu.Unlock()
		if ch != nil {
			close(ch)
		}
	})
}
