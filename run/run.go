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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BananaLabs-OSS/Pulp/abi"
	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/BananaLabs-OSS/Pulp/internal/safe"
	"github.com/tetratelabs/wazero"
)

// eventChanSize is the per-cell event channel buffer. Pollsters drop
// events (with a warn log) when a cell's channel fills up.
const eventChanSize = 64

// HostRuntimeOptions are the host-level settings passed to every independent
// application runtime created for a pulp.host.toml composition. Application
// factories must apply the namespace-bearing scope themselves; these options
// intentionally carry no process-global mutable application state.
type HostRuntimeOptions struct {
	StorageRoot string
	HTTPPort    string
	Logger      *slog.Logger
}

// DirectApplicationOptions configures one monolithic `pulp -app` runtime.
// Lifecycle is explicit and instance-local: constructing a runtime through
// NewDirectApplicationRuntime never reads or mutates the process-global
// observer registration used by the CLI/deployment binary.
type DirectApplicationOptions struct {
	StorageRoot string
	HTTPPort    string
	Logger      *slog.Logger
	Lifecycle   ApplicationLifecycleObserver
}

func validateRuntimeInputs(hostPath, appPath string, manifestPaths []string) error {
	inputs := 0
	if hostPath != "" {
		inputs++
	}
	if appPath != "" {
		inputs++
	}
	if len(manifestPaths) != 0 {
		inputs++
	}
	if inputs == 0 {
		return errors.New("provide exactly one of -host <path-to-pulp.host.toml>, -app <path-to-pulp.app.toml>, or -manifest <path-to-pulp.cell.toml>")
	}
	if inputs != 1 {
		return errors.New("-host, -app, and -manifest are mutually exclusive")
	}
	return nil
}

// hostedApplicationHost owns the shared import-free module runtime used by a
// single `pulp -host` process. Capability-bearing cells deliberately receive
// isolated host.LoadScoped runtimes from the application factory.
type hostedApplicationHost struct {
	supervisor    *MultiHostSupervisor
	gateway       *HostGateway
	moduleCache   *host.ModuleCache
	moduleRuntime wazero.Runtime
	endpoints     *EndpointRegistry
	pollStop      chan struct{}
	pollWG        sync.WaitGroup
}

// directApplicationHost is the single-application CLI analogue of a hosted
// application. It intentionally reuses applicationRuntime so `pulp -app` and
// `pulp -host` instantiate the same placement graph and sibling semantics.
type directApplicationHost struct {
	runtime  *applicationRuntime
	pollStop chan struct{}
	pollWG   sync.WaitGroup
}

// directApplicationRuntime is the exported API's encapsulated lifecycle. It
// owns extension polling as well as applicationRuntime, so callers exercise
// the same production path as `pulp -app` without importing internal types.
type directApplicationRuntime struct {
	mu      sync.Mutex
	app     *manifest.Application
	options DirectApplicationOptions
	host    *directApplicationHost
	stopped bool
}

// NewDirectApplicationRuntime validates appPath and returns a startable
// monolithic runtime. The returned ApplicationRuntime exposes only Identity,
// Start, and Shutdown; cells, extension registries, and host internals remain
// encapsulated. An explicit Lifecycle observer is scoped to this runtime and
// does not alter RegisterApplicationLifecycleObserver state.
func NewDirectApplicationRuntime(appPath string, options DirectApplicationOptions) (ApplicationRuntime, error) {
	app, err := manifest.LoadApp(appPath)
	if err != nil {
		return nil, err
	}
	return newDirectApplicationRuntime(app, options), nil
}

func newDirectApplicationRuntime(app *manifest.Application, options DirectApplicationOptions) *directApplicationRuntime {
	return &directApplicationRuntime{app: app, options: options}
}

func (r *directApplicationRuntime) Identity() ApplicationIdentity {
	if r == nil || r.app == nil {
		return ApplicationIdentity{}
	}
	return ApplicationIdentity{ApplicationID: r.app.Name, InstanceID: "default"}
}

func (r *directApplicationRuntime) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.host != nil {
		return ErrMultiHostRunning
	}
	if r.stopped {
		return ErrMultiHostStopped
	}
	hosted, err := startDirectApplicationWithLifecycle(ctx, r.app, HostRuntimeOptions{
		StorageRoot: r.options.StorageRoot,
		HTTPPort:    r.options.HTTPPort,
		Logger:      r.options.Logger,
	}, r.options.Lifecycle)
	if err != nil {
		return err
	}
	r.host = hosted
	return nil
}

func (r *directApplicationRuntime) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.host == nil {
		return nil
	}
	err := r.host.Shutdown(ctx)
	r.host = nil
	r.stopped = true
	return err
}

func startDirectApplication(ctx context.Context, app *manifest.Application, options HostRuntimeOptions) (*directApplicationHost, error) {
	return startDirectApplicationWithLifecycle(ctx, app, options, registeredApplicationLifecycleObserver())
}

func startDirectApplicationWithLifecycle(ctx context.Context, app *manifest.Application, options HostRuntimeOptions, lifecycle ApplicationLifecycleObserver) (*directApplicationHost, error) {
	if app == nil {
		return nil, errors.New("application is required")
	}
	runtime := newApplicationRuntime(HostedApplication{
		Identity:     ApplicationIdentity{ApplicationID: app.Name, InstanceID: "default"},
		ManifestPath: app.ManifestPath,
	}, ScopedApplicationRuntimeFactoryConfig{
		Registry:    host.NewRegistry(),
		Limits:      &host.Limits{},
		Logger:      options.Logger,
		StorageRoot: options.StorageRoot,
		HTTPPort:    options.HTTPPort,
		// Deployment-owned template bootstrap and effect pollers register through
		// this same trusted observer in both monolithic and split modes.
		Lifecycle: lifecycle,
	})
	if err := runtime.Start(ctx); err != nil {
		return nil, err
	}
	direct := &directApplicationHost{runtime: runtime, pollStop: make(chan struct{})}
	direct.startPollsters(options.Logger)
	return direct, nil
}

func (h *directApplicationHost) startPollsters(logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	for _, capability := range h.runtime.allCaps {
		if capability.Poll == nil || !h.runtime.declaredUnion[capability.Name] {
			continue
		}
		h.pollWG.Add(1)
		go func(capability ext.Capability) {
			defer h.pollWG.Done()
			for {
				select {
				case <-h.pollStop:
					return
				default:
				}
				event, ok := safe.CallPoll(capability, logger)
				if !ok {
					time.Sleep(200 * time.Microsecond)
					continue
				}
				cell := h.runtime.eventTargets[event.CellID]
				if event.CellID == "" {
					// Preserve legacy untagged delivery only when this application
					// has one unambiguous declaring placement. Repeated engines
					// must receive their exact scoped event target.
					for _, candidate := range h.runtime.runtimes {
						if !candidate.declared[capability.Name] || candidate.failed.Load() {
							continue
						}
						if cell != nil {
							cell = nil
							break
						}
						cell = candidate
					}
				}
				if cell == nil || cell.failed.Load() {
					logger.Warn("application event rejected", "capability", capability.Name, "target", event.CellID)
					safe.CallFinalize(capability, event.ID, logger)
					continue
				}
				deliver(cell, routedEvent{ev: event, caps: []ext.Capability{capability}}, capability, logger)
			}
		}(capability)
	}
}

func (h *directApplicationHost) Shutdown(ctx context.Context) error {
	if h == nil {
		return nil
	}
	if h.pollStop != nil {
		close(h.pollStop)
		h.pollWG.Wait()
		h.pollStop = nil
	}
	if h.runtime != nil {
		return h.runtime.Shutdown(ctx)
	}
	return nil
}

func (h *hostedApplicationHost) Shutdown(ctx context.Context) error {
	if h == nil {
		return nil
	}
	var errs []error
	// Stop accepting new front-door requests before application listeners and
	// cells disappear underneath the gateway's reverse proxies.
	if h.gateway != nil {
		errs = append(errs, h.gateway.Shutdown(ctx))
	}
	if h.pollStop != nil {
		close(h.pollStop)
		h.pollWG.Wait()
		h.pollStop = nil
	}
	if h.supervisor != nil {
		errs = append(errs, h.supervisor.Shutdown(ctx))
	}
	if h.moduleCache != nil {
		errs = append(errs, h.moduleCache.Close(ctx))
	}
	if h.moduleRuntime != nil {
		errs = append(errs, h.moduleRuntime.Close(ctx))
	}
	return errors.Join(errs...)
}

func startHostedApplications(ctx context.Context, hostPath string, options HostRuntimeOptions, loader HostManifestLoader) (*hostedApplicationHost, error) {
	hostManifest, err := manifest.LoadHost(hostPath)
	if err != nil {
		return nil, err
	}
	moduleRuntime := wazero.NewRuntime(ctx)
	moduleCache := host.NewModuleCache()
	endpoints := NewEndpointRegistry()
	crossApplications := newCrossApplicationRegistry()
	cacheScope, err := moduleCache.NewScope(moduleRuntime, host.ModuleRuntimeConfig{
		Fingerprint: "pulp-host-cli/v1",
	})
	if err != nil {
		_ = moduleCache.Close(context.Background())
		_ = moduleRuntime.Close(context.Background())
		return nil, err
	}
	applicationInstances := 0
	for _, application := range hostManifest.Applications {
		applicationInstances += len(application.Instances)
	}
	factory, err := NewScopedApplicationRuntimeFactory(ScopedApplicationRuntimeFactoryConfig{
		Registry:                         host.NewRegistry(),
		Limits:                           &host.Limits{},
		Logger:                           options.Logger,
		ModuleCacheScope:                 cacheScope,
		StorageRoot:                      options.StorageRoot,
		HTTPPort:                         options.HTTPPort,
		Endpoints:                        endpoints,
		RequireScopedCapabilityLifecycle: applicationInstances > 1,
		Lifecycle:                        registeredApplicationLifecycleObserver(),
		CrossApplications:                crossApplications,
	})
	if err != nil {
		_ = moduleCache.Close(context.Background())
		_ = moduleRuntime.Close(context.Background())
		return nil, err
	}
	supervisor, err := startHostedApplicationsWithFactory(ctx, hostPath, loader, factory)
	if err != nil {
		_ = moduleCache.Close(context.Background())
		_ = moduleRuntime.Close(context.Background())
		return nil, err
	}
	hosted := &hostedApplicationHost{supervisor: supervisor, moduleCache: moduleCache, moduleRuntime: moduleRuntime, endpoints: endpoints}
	if err := hosted.startPollsters(options.Logger); err != nil {
		_ = hosted.Shutdown(context.Background())
		return nil, err
	}
	if len(hostManifest.Routes) == 0 {
		return hosted, nil
	}
	addr, err := hostGatewayAddress(options.HTTPPort)
	if err != nil {
		_ = hosted.Shutdown(context.Background())
		return nil, err
	}
	gateway, err := NewSupervisorHostGateway(addr, hostManifest, supervisor, options.Logger)
	if err != nil {
		_ = hosted.Shutdown(context.Background())
		return nil, err
	}
	if err := gateway.Start(ctx); err != nil {
		_ = hosted.Shutdown(context.Background())
		return nil, err
	}
	hosted.gateway = gateway
	return hosted, nil
}

// startPollsters owns one Poll consumer per shared extension capability. Events
// are routed only by full scoped routing IDs; bare/empty targets are rejected
// in a multi-application host rather than being consumed by the wrong app.
func (h *hostedApplicationHost) startPollsters(logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	h.supervisor.mu.Lock()
	instances := append([]ApplicationRuntime(nil), h.supervisor.runtimes...)
	h.supervisor.mu.Unlock()
	targets := map[string]*cellRuntime{}
	declared := map[string]bool{}
	for _, instance := range instances {
		runtime, ok := instance.(*applicationRuntime)
		if !ok {
			continue
		}
		for name, cell := range runtime.eventTargets {
			if name != cell.eventTarget {
				continue
			}
			if _, exists := targets[name]; exists {
				return fmt.Errorf("duplicate multi-host event target %q", name)
			}
			targets[name] = cell
		}
		for name := range runtime.declaredUnion {
			declared[name] = true
		}
	}
	h.pollStop = make(chan struct{})
	allCaps, err := selectedRuntimeCapabilities()
	if err != nil {
		return err
	}
	for _, capability := range allCaps {
		if capability.Poll == nil || !declared[capability.Name] {
			continue
		}
		h.pollWG.Add(1)
		go func(capability ext.Capability) {
			defer h.pollWG.Done()
			for {
				select {
				case <-h.pollStop:
					return
				default:
				}
				event, ok := safe.CallPoll(capability, logger)
				if !ok {
					time.Sleep(200 * time.Microsecond)
					continue
				}
				cell := targets[event.CellID]
				if event.CellID == "" || cell == nil || cell.failed.Load() {
					logger.Warn("multi-host event rejected", "capability", capability.Name, "target", event.CellID)
					safe.CallFinalize(capability, event.ID, logger)
					continue
				}
				deliver(cell, routedEvent{ev: event, caps: []ext.Capability{capability}}, capability, logger)
			}
		}(capability)
	}
	return nil
}

func hostGatewayAddress(requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", errors.New("-host with routes requires -http-port for the host gateway")
	}
	if strings.Contains(requested, ":") {
		return requested, nil
	}
	return ":" + requested, nil
}

func startHostedApplicationsWithFactory(ctx context.Context, hostPath string, loader HostManifestLoader, factory ApplicationRuntimeFactory) (*MultiHostSupervisor, error) {
	supervisor, err := NewMultiHostSupervisor(loader, factory)
	if err != nil {
		return nil, err
	}
	if err := supervisor.Start(ctx, hostPath); err != nil {
		return nil, err
	}
	return supervisor, nil
}

// cellRuntime is the per-cell runtime state: the loaded WASM
// cell, its event channel, its goroutine's context, and the set of
// capabilities it declared. The step loop consumes from events, calls
// cell.Step(), and calls Finalize on the producing extension.
type cellRuntime struct {
	spec *manifest.CellSpec
	// address is the application-local placement address. It equals spec.Name
	// for legacy singleton cells and is `name@instance` for repeated packages.
	address     string
	scope       ext.Scope
	cell        *host.Cell
	eventTarget string // ext.CellIDOf(cell); legacy cells retain spec.Name
	events      chan routedEvent
	ctx         context.Context
	cancel      context.CancelFunc
	declared    map[string]bool // capabilities this cell declared
	readyCh     chan struct{}   // closed after Init returns 0
	callNumber  atomic.Uint64   // atomic: written by the step loop, read by ctl status

	// stepDone is closed when this cell's step goroutine exits. Recreated
	// each time a step loop is launched (initial start + every reload) so a
	// reload can join the old loop before starting the new one.
	stepDone chan struct{}

	// failed is set when the cell's Setup, Load, or Init returned an
	// error. Failed cells do not run their step loop; dependents of a
	// failed cell inherit the failed state. Atomic: written during startup,
	// read by ctl status concurrently.
	failed atomic.Bool

	// registry/limits/configBytes are retained from startup so the step-loop
	// SUPERVISOR can re-instantiate the cell after a wasm trap (restart=on_crash)
	// without run.Main's startup context — the host stays up, the cell is reborn.
	registry    *host.Registry
	limits      *host.Limits
	configBytes []byte
}

func newRuntimeCellScope(app *manifest.Application, spec *manifest.CellSpec) (ext.Scope, error) {
	if app == nil {
		return ext.LegacyScope(spec.Name), nil
	}
	return ext.NewScope(app.Name, "default", spec.Name, "default")
}

func (rt *cellRuntime) effectiveScope() ext.Scope {
	if err := rt.scope.Validate(); err == nil {
		return rt.scope
	}
	if rt.spec == nil {
		return ext.LegacyScope("default")
	}
	return ext.LegacyScope(rt.spec.Name)
}

func (rt *cellRuntime) eventTargetID() string {
	if rt.eventTarget != "" {
		return rt.eventTarget
	}
	if rt.cell != nil {
		return ext.CellIDOf(rt.cell)
	}
	if scope := rt.effectiveScope(); !scope.IsLegacy() {
		return scope.RoutingID()
	}
	if rt.spec == nil {
		return ""
	}
	return rt.spec.Name
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
	var hostPath string
	var appPath string
	var validateHost bool
	var storageRoot string
	var httpPort string
	flag.StringVar(&hostPath, "host", "", "path to pulp.host.toml (cannot be combined with -app or -manifest)")
	flag.BoolVar(&validateHost, "validate-host", false, "validate -host manifest and exit without starting cells")
	flag.StringVar(&appPath, "app", "", "path to pulp.app.toml (cannot be combined with -manifest)")
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

	if err := validateRuntimeInputs(hostPath, appPath, []string(manifestPaths)); err != nil {
		logger.Error("invalid runtime input", "err", err)
		os.Exit(2)
	}
	if validateHost && hostPath == "" {
		logger.Error("-validate-host requires -host")
		os.Exit(2)
	}
	if hostPath != "" {
		if validateHost {
			if err := ValidateHost(hostPath); err != nil {
				logger.Error("host manifest validation failed", "host", hostPath, "err", err)
				os.Exit(1)
			}
			logger.Info("host manifest valid", "host", hostPath)
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		supervisor, err := startHostedApplications(ctx, hostPath, HostRuntimeOptions{
			StorageRoot: storageRoot,
			HTTPPort:    httpPort,
			Logger:      logger,
		}, ManifestHostLoader{})
		if err != nil {
			logger.Error("multi-application host failed to start", "host", hostPath, "err", err)
			os.Exit(1)
		}

		logger.Info("pulp multi-application host ready", "host", hostPath)
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, ShutdownSignals()...)
		defer signal.Stop(sigCh)
		sig := <-sigCh
		logger.Info("signal received", "signal", sig.String())
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := supervisor.Shutdown(shutdownCtx); err != nil {
			logger.Error("multi-application host shutdown failed", "err", err)
			os.Exit(1)
		}
		logger.Info("pulp multi-application host exited cleanly")
		return
	}

	set, app, err := loadManifestInputs(appPath, []string(manifestPaths))
	if err != nil {
		logger.Error("manifest load failed", "err", err)
		os.Exit(1)
	}
	if app != nil {
		logger.Info("pulp application",
			"name", app.Name,
			"version", app.Version,
			"manifest", app.ManifestPath,
			"orchestrator", app.OrchestratorCell,
			"script_sha256", app.OrchestrationSHA256,
		)
		// `-app` uses the same expanded placement runtime as `-host`. The
		// legacy loop below remains only for repeated `-manifest` operation.
		direct := newDirectApplicationRuntime(app, DirectApplicationOptions{
			StorageRoot: storageRoot,
			HTTPPort:    httpPort,
			Logger:      logger,
			Lifecycle:   registeredApplicationLifecycleObserver(),
		})
		if err := direct.Start(context.Background()); err != nil {
			logger.Error("application failed to start", "err", err)
			os.Exit(1)
		}
		logger.Info("pulp application ready", "name", app.Name, "placements", len(app.Placements))
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, ShutdownSignals()...)
		defer signal.Stop(sigCh)
		sig := <-sigCh
		logger.Info("signal received", "signal", sig.String())
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := direct.Shutdown(shutdownCtx); err != nil {
			logger.Error("application shutdown failed", "err", err)
			os.Exit(1)
		}
		return
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
		if len(spec.SharedMemoryGroups) > 0 {
			logger.Warn("shared_memory_groups not yet implemented — field parsed but no zero-copy linking exists; field is a no-op",
				"cell", spec.Name, "groups", spec.SharedMemoryGroups)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ------------------------------------------------------------------
	// Capability Setup (once, not once-per-cell)
	// ------------------------------------------------------------------

	// Union of capabilities declared across all cells — each gets its
	// Setup called at most once regardless of how many cells declared
	// it. The registry is shared across cell Loads.
	allCaps, err := selectedRuntimeCapabilities()
	if err != nil {
		logger.Error("capability provider selection failed", "err", err)
		os.Exit(1)
	}
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
	setupScope := ext.LegacyScope("host")
	if app != nil {
		setupScope, err = ext.NewScope(app.Name, "default", "host", "default")
		if err != nil {
			logger.Error("application setup scope", "err", err)
			os.Exit(1)
		}
	}
	setupEnv := ext.SetupEnv{
		// CellName is left empty at Setup time — Setup is now a
		// one-time shared initialization, not per-cell. Extensions
		// that need per-cell state should maintain it in a map keyed
		// by the cell identity they see at Register time.
		Scope:       setupScope,
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
		scope, err := newRuntimeCellScope(app, spec)
		if err != nil {
			logger.Error("cell scope", "cell", spec.Name, "err", err)
			os.Exit(1)
		}
		pctx, pcancel := context.WithCancel(ctx)
		declared := map[string]bool{}
		for _, c := range spec.Capabilities {
			declared[c] = true
		}
		runtimes[spec.Name] = &cellRuntime{
			spec:     spec,
			scope:    scope,
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
			cell, err := host.LoadScoped(rt.ctx, spec, registry, limits, logger, rt.effectiveScope())
			if err != nil {
				logger.Error("load failed", "cell", spec.Name, "err", err)
				rt.failed.Store(true)
				close(rt.readyCh)
				return
			}
			rt.cell = cell
			rt.registry, rt.limits, rt.configBytes = registry, limits, configBytes // retained for crash re-instantiate

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

	// Cell names remain the control/sibling-call keys inside this one runtime.
	// Event delivery has a separate index: explicit application scopes use their
	// full routing identity, while legacy cells keep their historical name key.
	eventTargets := make(map[string]*cellRuntime, len(runtimes))
	for _, rt := range runtimes {
		if rt.failed.Load() || rt.cell == nil {
			continue
		}
		rt.eventTarget = ext.CellIDOf(rt.cell)
		if previous, exists := eventTargets[rt.eventTarget]; exists && previous != rt {
			logger.Error("ambiguous scoped cell event target", "target", rt.eventTarget,
				"cell", rt.spec.Name, "other_cell", previous.spec.Name)
			os.Exit(1)
		}
		eventTargets[rt.eventTarget] = rt
		// Name targets are retained for extensions written before application
		// scoping. They are unambiguous within this runtime because manifest
		// validation already rejects duplicate cell names.
		if rt.spec.Name != rt.eventTarget {
			eventTargets[rt.spec.Name] = rt
		}
	}

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
		go runPollster(c, stopPoll, &pollWG, runtimes, eventTargets, capToCells, logger)
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
	eventTargets map[string]*cellRuntime,
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
			rt, found := eventTargets[ev.CellID]
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
// reinstantiateCell rebuilds a trapped cell module in place: re-Load from disk +
// re-Init, then swap it onto rt and close the dead one. Returns false if the
// rebuild itself fails (the supervisor then backs off). The host stays up
// throughout — only the one cell is reborn, so the cockpit self-heals.
func reinstantiateCell(rt *cellRuntime, logger *slog.Logger) bool {
	newCell, err := host.LoadScoped(rt.ctx, rt.spec, rt.registry, rt.limits, logger, rt.effectiveScope())
	if err != nil {
		logger.Error("re-instantiate: load failed", "cell", rt.spec.Name, "err", err)
		return false
	}
	if err := newCell.Init(rt.ctx, rt.configBytes); err != nil {
		logger.Error("re-instantiate: init failed", "cell", rt.spec.Name, "err", err)
		newCell.Close(context.Background())
		return false
	}
	old := rt.cell
	rt.cell = newCell
	rt.eventTarget = ext.CellIDOf(newCell)
	if old != nil {
		old.Close(context.Background())
	}
	return true
}

func stepLoop(rt *cellRuntime, capByName map[string]ext.Capability, logger *slog.Logger) {
	const (
		idleMin     = 200 * time.Microsecond
		// An idle step allocates a StepEnvelope inside each WASM cell.  WASM
		// linear memory can grow but does not shrink, so a 10ms ceiling turns an
		// otherwise idle cell into a permanent ~100 allocations/second memory
		// fuse.  Event delivery still wakes the loop immediately; this ceiling
		// governs only the no-work path and keeps autonomous cell ticks timely
		// without exhausting every long-lived cell in a few days.
		idleMax     = time.Second
		idleRampAge = time.Second
	)
	idleSleep := idleMin
	idleSince := time.Time{}
	idleTimer := time.NewTimer(idleMin)
	if !idleTimer.Stop() {
		<-idleTimer.C
	}
	defer idleTimer.Stop()

	// Crash supervisor: re-instantiate the cell after a wasm trap when
	// restart=on_crash/always (the previously-unimplemented manifest policy), with
	// a loop-breaker so a cell that traps on init can't spin forever. A bare panic
	// is already caught in-cell (pulp_step recover); this catches the rest — a true
	// module trap or proc_exit — so "cell did not respond" self-heals.
	restarts := 0
	restartWindow := time.Time{}
	superviseTrap := func(callN uint64) {
		if rt.spec.Restart != manifest.RestartOnCrash && rt.spec.Restart != manifest.RestartAlways {
			return
		}
		now := time.Now()
		if now.Sub(restartWindow) > 30*time.Second {
			restarts, restartWindow = 0, now
		}
		if restarts >= 5 {
			logger.Error("cell crash-restart limit hit (5 in 30s) — leaving it down", "cell", rt.spec.Name)
			return
		}
		restarts++
		if reinstantiateCell(rt, logger) {
			logger.Warn("cell RE-INSTANTIATED after trap", "cell", rt.spec.Name, "call_number", callN, "restart_count", restarts)
		}
	}

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
				superviseTrap(n)
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
				superviseTrap(n)
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
