package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/BananaLabs-OSS/Pulp/internal/safe"
)

// applicationRuntime reuses Pulp's existing cellRuntime, sibling registry,
// poller, and step-loop machinery for one isolated application instance.
type applicationRuntime struct {
	application HostedApplication
	config      ScopedApplicationRuntimeFactoryConfig

	mu                sync.Mutex
	ctx               context.Context
	cancel            context.CancelFunc
	runtimes          map[string]*cellRuntime
	eventTargets      map[string]*cellRuntime
	allCaps           []ext.Capability
	declaredUnion     map[string]bool
	capabilityConfigs map[string]map[string]any
	setupCaps         map[string]bool
	registry          *host.Registry
	ops               *runtimeOps
	started           bool
	providerAccess    *applicationProviderAccess
}

// ApplicationLifecycleObserver is the narrow host-effect seam around one
// fully isolated application. It never exposes another runtime or cell graph.
type ApplicationLifecycleObserver interface {
	AfterApplicationStart(context.Context, ApplicationIdentity) error
	BeforeApplicationShutdown(context.Context, ApplicationIdentity) error
}

// ApplicationProviderAccess is a callback-scoped view of initialized
// providers in one application instance. It deliberately has no lookup by
// application ID and no access to sibling application runtimes.
type ApplicationProviderAccess interface {
	Identity() ApplicationIdentity
	CallProvider(context.Context, string, string, []byte) ([]byte, error)
}

// ApplicationLifecycleObserverV2 is an optional extension of the original
// lifecycle observer. Hosts continue to support v1 observers unchanged.
type ApplicationLifecycleObserverV2 interface {
	ApplicationLifecycleObserver
	AfterApplicationStartWithProvider(context.Context, ApplicationIdentity, ApplicationProviderAccess) error
}

type applicationProviderAccess struct {
	identity ApplicationIdentity
	runtimes map[string]*cellRuntime
	mu       sync.Mutex
	active   bool
	calls    sync.WaitGroup
}

func (a *applicationProviderAccess) Identity() ApplicationIdentity { return a.identity }

func (a *applicationProviderAccess) CallProvider(ctx context.Context, cellName, provider string, args []byte) ([]byte, error) {
	a.mu.Lock()
	if !a.active {
		a.mu.Unlock()
		return nil, fmt.Errorf("application %s provider access is revoked", a.identity)
	}
	a.calls.Add(1)
	a.mu.Unlock()
	defer a.calls.Done()
	runtime := a.runtimes[cellName]
	if runtime == nil || runtime.failed.Load() || runtime.cell == nil {
		return nil, fmt.Errorf("application %s provider cell %q is unavailable", a.identity, cellName)
	}
	allowed := false
	for _, name := range runtime.spec.Provides {
		if name == provider {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("application %s cell %q does not provide %q", a.identity, cellName, provider)
	}
	return runtime.cell.Call(ctx, provider, args)
}

func (a *applicationProviderAccess) revoke() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.active = false
	a.mu.Unlock()
	a.calls.Wait()
}

var applicationLifecycleObservers struct {
	sync.Mutex
	observer ApplicationLifecycleObserver
}

// RegisterApplicationLifecycleObserver installs the one process-local host
// observer used for subsequently started `pulp -host` and `pulp -app`
// applications. Duplicate registration is rejected so two deployment packages
// cannot both execute effects for the same application lifecycle.
func RegisterApplicationLifecycleObserver(observer ApplicationLifecycleObserver) error {
	if observer == nil {
		return errors.New("application lifecycle observer is nil")
	}
	applicationLifecycleObservers.Lock()
	defer applicationLifecycleObservers.Unlock()
	if applicationLifecycleObservers.observer != nil {
		return errors.New("application lifecycle observer is already registered")
	}
	applicationLifecycleObservers.observer = observer
	return nil
}

func registeredApplicationLifecycleObserver() ApplicationLifecycleObserver {
	applicationLifecycleObservers.Lock()
	defer applicationLifecycleObservers.Unlock()
	return applicationLifecycleObservers.observer
}

func newApplicationRuntime(app HostedApplication, config ScopedApplicationRuntimeFactoryConfig) *applicationRuntime {
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	return &applicationRuntime{application: app, config: config}
}

func (r *applicationRuntime) Identity() ApplicationIdentity { return r.application.Identity }

func (r *applicationRuntime) HTTPAddress() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started || r.config.Endpoints == nil {
		return ""
	}
	address, _ := r.config.Endpoints.ApplicationAddress(r.application.Identity.ApplicationID, r.application.Identity.InstanceID, "transport.http.inbound", "public")
	return address
}

func (r *applicationRuntime) Start(parent context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return fmt.Errorf("application %s is already started", r.application.Identity)
	}
	loaded, err := manifest.LoadApp(r.application.ManifestPath)
	if err != nil {
		return fmt.Errorf("load application %s: %w", r.application.Identity, err)
	}
	r.ctx, r.cancel = context.WithCancel(parent)
	r.allCaps, err = selectedRuntimeCapabilities()
	if err != nil {
		r.reset()
		return err
	}
	r.declaredUnion, r.setupCaps = map[string]bool{}, map[string]bool{}
	for _, spec := range loaded.Cells.Order {
		for _, name := range spec.Capabilities {
			r.declaredUnion[name] = true
		}
	}
	r.resolveCapabilityConfigs(loaded.Placements)
	if err := r.setupCapabilities(); err != nil {
		r.reset()
		return err
	}
	r.registry = host.NewRegistry()
	for _, capability := range r.allCaps {
		r.registry.Gated(capability)
	}
	r.runtimes = make(map[string]*cellRuntime, len(loaded.Placements))
	for _, placement := range loaded.Placements {
		spec := placement.Spec
		scope, err := r.application.NewCellScope(spec.Name, placement.InstanceID)
		if err != nil {
			return r.startFailure(err)
		}
		cellCtx, moduleCancel := context.WithCancel(r.ctx)
		stepCtx, stepCancel := context.WithCancel(cellCtx)
		declared := map[string]bool{}
		for _, name := range spec.Capabilities {
			declared[name] = true
		}
		r.runtimes[placement.Address] = &cellRuntime{spec: spec, address: placement.Address, scope: scope, events: make(chan routedEvent, eventChanSize), ctx: cellCtx, moduleCancel: moduleCancel, stepCtx: stepCtx, cancel: stepCancel, declared: declared, readyCh: make(chan struct{}), stepDone: make(chan struct{})}
	}
	r.registry.Always(siblingCapabilityWithCrossApplication(newSiblingRegistry(r.runtimes), r.config.CrossApplications, r.application))
	if missing := validateSiblingLinks(r.runtimes); len(missing) != 0 {
		return r.startFailure(fmt.Errorf("application %s sibling links: %v", r.application.Identity, missing))
	}
	for _, placement := range loaded.Placements {
		spec := placement.Spec
		rt := r.runtimes[placement.Address]
		configBytes, err := manifest.EncodeConfig(spec.Config)
		if err != nil {
			rt.failed.Store(true)
			close(rt.readyCh)
			return r.startFailure(err)
		}
		limits := &host.Limits{MaxMemoryPages: spec.MaxMemoryPages, CallTimeout: time.Duration(spec.CallTimeoutMS) * time.Millisecond}
		cell, err := host.LoadScoped(rt.ctx, spec, r.registry, limits, r.config.Logger, rt.effectiveScope())
		if err != nil {
			rt.failed.Store(true)
			close(rt.readyCh)
			return r.startFailure(fmt.Errorf("load cell %s: %w", spec.Name, err))
		}
		rt.cell, rt.registry, rt.limits, rt.configBytes = cell, r.registry, limits, configBytes
		if err := cell.Init(rt.ctx, configBytes); err != nil {
			rt.failed.Store(true)
			close(rt.readyCh)
			_ = cell.Close(context.Background())
			return r.startFailure(fmt.Errorf("init cell %s: %w", spec.Name, err))
		}
		close(rt.readyCh)
	}
	r.eventTargets = make(map[string]*cellRuntime, len(r.runtimes)*2)
	for _, rt := range r.runtimes {
		rt.eventTarget = ext.CellIDOf(rt.cell)
		r.eventTargets[rt.eventTarget] = rt
		// A bare legacy cell name is accepted only when there is exactly one
		// placement of that reusable package. Repeated placements require the
		// exact `cell@instance` sibling target and always receive scoped events.
		if rt.address == rt.spec.Name {
			r.eventTargets[rt.spec.Name] = rt
		}
	}
	capByName := map[string]ext.Capability{}
	for _, c := range r.allCaps {
		capByName[c.Name] = c
	}
	r.ops = &runtimeOps{runtimes: r.runtimes, allCaps: r.allCaps, declaredUnion: r.declaredUnion, logger: r.config.Logger, registry: r.registry, capByName: capByName, parentCtx: r.ctx}
	for _, rt := range r.runtimes {
		r.ops.launchStep(rt)
	}
	r.started = true
	r.providerAccess = &applicationProviderAccess{identity: r.application.Identity, runtimes: r.runtimes, active: true}
	if err := deploymentOperatorCommands.bind(r.application.Identity, r.providerAccess); err != nil {
		return r.startFailure(fmt.Errorf("bind operator commands: %w", err))
	}
	if r.config.CrossApplications != nil {
		if err := r.config.CrossApplications.markReady(r.application, r); err != nil {
			return r.startFailure(fmt.Errorf("register cross-application providers: %w", err))
		}
	}
	if r.config.Lifecycle != nil {
		var err error
		if observer, ok := r.config.Lifecycle.(ApplicationLifecycleObserverV2); ok {
			err = observer.AfterApplicationStartWithProvider(r.ctx, r.application.Identity, r.providerAccess)
		} else {
			err = r.config.Lifecycle.AfterApplicationStart(r.ctx, r.application.Identity)
		}
		if err != nil {
			return r.startFailure(fmt.Errorf("application start observer: %w", err))
		}
	}
	return nil
}

func (r *applicationRuntime) setupCapabilities() error {
	if err := r.validateCapabilityLifecycle(); err != nil {
		return err
	}
	scope, err := ext.NewScope(r.application.Identity.ApplicationID, r.application.Identity.InstanceID, "host", "primary")
	if err != nil {
		return err
	}
	storageRoot := r.config.StorageRoot
	if r.application.StorageNamespace != "" {
		storageRoot = filepath.Join(storageRoot, r.application.StorageNamespace, r.application.Identity.InstanceID)
	}
	env := ext.SetupEnv{Scope: scope, Endpoints: r.config.Endpoints, StorageRoot: storageRoot, StorageNamespaces: r.config.StorageNamespaces, HTTPPort: r.config.HTTPPort, Logger: r.config.Logger}
	for _, c := range r.allCaps {
		if r.declaredUnion[c.Name] {
			env.Config = r.capabilityConfigs[c.Name]
			if err := safe.CallSetup(c, env, r.config.Logger); err != nil {
				return fmt.Errorf("setup capability %s: %w", c.Name, err)
			}
			r.setupCaps[c.Name] = true
		}
	}
	return nil
}

// resolveCapabilityConfigs captures the already-resolved config of the first
// placement that declares each application-scoped capability. Setup remains a
// once-per-application lifecycle: repeated declarations therefore reuse the
// first deterministic placement in manifest order instead of merging unrelated
// cell config tables.
//
// Each capability receives its own deep copy. An extension may normalize its
// setup input without changing the guest config later encoded for pulp_init,
// another capability's input, or another application instance.
func (r *applicationRuntime) resolveCapabilityConfigs(placements []manifest.CellPlacement) {
	r.capabilityConfigs = make(map[string]map[string]any)
	for _, placement := range placements {
		if placement.Spec == nil {
			continue
		}
		for _, capability := range placement.Spec.Capabilities {
			if _, resolved := r.capabilityConfigs[capability]; resolved {
				continue
			}
			r.capabilityConfigs[capability] = cloneCapabilitySetupConfig(placement.Spec.Config)
		}
	}
}

func cloneCapabilitySetupConfig(config map[string]any) map[string]any {
	if len(config) == 0 {
		return nil
	}
	cloned := make(map[string]any, len(config))
	for key, value := range config {
		cloned[key] = cloneCapabilitySetupValue(value)
	}
	return cloned
}

func cloneCapabilitySetupValue(value any) any {
	cloned := cloneCapabilitySetupReflect(reflect.ValueOf(value))
	if !cloned.IsValid() {
		return nil
	}
	return cloned.Interface()
}

func cloneCapabilitySetupReflect(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return reflect.Value{}
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneCapabilitySetupReflect(value.Elem())
		result := reflect.New(value.Type()).Elem()
		result.Set(cloned)
		return result
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			result.SetMapIndex(iterator.Key(), cloneCapabilitySetupReflect(iterator.Value()))
		}
		return result
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := 0; index < value.Len(); index++ {
			result.Index(index).Set(cloneCapabilitySetupReflect(value.Index(index)))
		}
		return result
	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for index := 0; index < value.Len(); index++ {
			result.Index(index).Set(cloneCapabilitySetupReflect(value.Index(index)))
		}
		return result
	default:
		return value
	}
}

func (r *applicationRuntime) validateCapabilityLifecycle() error {
	if !r.config.RequireScopedCapabilityLifecycle {
		return nil
	}
	for _, capability := range r.allCaps {
		if !r.declaredUnion[capability.Name] {
			continue
		}
		if capability.Setup != nil && capability.TeardownScope == nil {
			return fmt.Errorf("capability %s has setup but no scoped teardown and is not safe for multiple application instances", capability.Name)
		}
		if capability.Teardown != nil && capability.TeardownScope == nil {
			return fmt.Errorf("capability %s has only process-global teardown and is not safe for multiple application instances", capability.Name)
		}
	}
	return nil
}

func (r *applicationRuntime) teardownCapabilities() {
	if r.allCaps == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	scope, err := ext.NewScope(r.application.Identity.ApplicationID, r.application.Identity.InstanceID, "host", "primary")
	if err != nil {
		return
	}
	for _, c := range r.allCaps {
		if r.setupCaps[c.Name] {
			if c.TeardownScope != nil {
				if err := c.TeardownScope(ctx, scope); err != nil {
					r.config.Logger.Warn("capability scoped teardown failed", "capability", c.Name, "err", err)
				}
			} else if !r.config.RequireScopedCapabilityLifecycle {
				safe.CallTeardown(ctx, c, r.config.Logger)
			}
		}
	}
}

func (r *applicationRuntime) startFailure(cause error) error {
	r.stopLocked(context.Background())
	return cause
}

func (r *applicationRuntime) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return nil
	}
	return r.stopLocked(ctx)
}

func (r *applicationRuntime) stopLocked(ctx context.Context) error {
	var errs []error
	// Reject and join inbound calls before tearing down any provider cell. This
	// is deliberately earlier than lifecycle effects: an effect observer may
	// stop its own outbound work independently, but a foreign application must
	// never enter a cell that is beginning teardown.
	if r.config.CrossApplications != nil {
		r.config.CrossApplications.markUnavailable(r.application.Identity)
	}
	if r.config.Lifecycle != nil {
		if err := r.config.Lifecycle.BeforeApplicationShutdown(ctx, r.application.Identity); err != nil {
			errs = append(errs, err)
		}
	}
	if r.providerAccess != nil {
		deploymentOperatorCommands.unbind(r.application.Identity)
		r.providerAccess.revoke()
	}
	for _, rt := range r.runtimes {
		rt.cancel()
	}
	if r.ops != nil {
		r.ops.stepWG.Wait()
	}
	for _, rt := range r.runtimes {
		for {
			select {
			case event := <-rt.events:
				for _, c := range event.caps {
					safe.CallFinalize(c, event.ev.ID, r.config.Logger)
				}
			default:
				goto drained
			}
		}
	drained:
		if rt.cell != nil {
			if err := rt.cell.Shutdown(ctx); err != nil {
				errs = append(errs, err)
			}
			if err := rt.cell.Close(context.Background()); err != nil {
				errs = append(errs, err)
			}
		}
		if rt.moduleCancel != nil {
			rt.moduleCancel()
		}
	}
	r.teardownCapabilities()
	if r.config.Endpoints != nil {
		r.config.Endpoints.RemoveApplication(r.application.Identity.ApplicationID, r.application.Identity.InstanceID)
	}
	r.reset()
	return errors.Join(errs...)
}

func (r *applicationRuntime) reset() {
	if r.cancel != nil {
		r.cancel()
	}
	r.ctx = nil
	r.cancel = nil
	r.runtimes = nil
	r.eventTargets = nil
	r.ops = nil
	r.registry = nil
	r.providerAccess = nil
	r.capabilityConfigs = nil
	r.setupCaps = nil
	r.started = false
}
