package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/dependency"
	"github.com/BananaLabs-OSS/Pulp/internal/fusion"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

// ScopedApplicationRuntimeFactoryConfig supplies the host-owned dependencies
// needed to construct application-local cell graphs. StorageRoot and HTTPPort
// are retained as explicit application-host inputs: the caller performs the
// one-time extension setup using them before any runtime is created.
type ScopedApplicationRuntimeFactoryConfig struct {
	Registry          *host.Registry
	Limits            *host.Limits
	Logger            *slog.Logger
	ModuleCacheScope  *host.ModuleCacheScope
	StorageRoot       string
	StorageNamespaces map[string]string
	HTTPPort          string
	Endpoints         *EndpointRegistry
	PlacementGrants   ext.PlacementGrantResolver
	// RequireScopedCapabilityLifecycle rejects known process-global extension
	// lifecycles when a host has more than one application instance.
	RequireScopedCapabilityLifecycle bool
	Lifecycle                        ApplicationLifecycleObserver
	// CrossApplications is host-owned routing state for the explicit
	// cross-application provider import. It is nil outside `pulp -host`, so
	// ordinary single-application runs do not receive that import at all.
	CrossApplications *crossApplicationRegistry
	// StepActivation is closed once the complete host dependency graph is
	// initialized. Nil starts autonomous cell steps immediately (direct apps).
	StepActivation <-chan struct{}
	// Fusion is an optional deployment-owned preparation hook. When present,
	// application startup asks it to prepare every eligible source-level fusion
	// group before any capability is set up or guest is loaded. A nil hook keeps
	// the historical isolated/manual-execution-unit behavior.
	Fusion FusionRuntimePreparer
}

// FusionRuntimePreparer binds registry/lock/toolchain policy to the generic
// application runtime. Implementations normally delegate to fusion.Coordinator
// after constructing the exact immutable PrepareRequest for this application.
// Returning a non-fused activation is the only supported isolated fallback;
// an error aborts startup before capabilities or cells become visible.
type FusionRuntimePreparer interface {
	PrepareFusion(context.Context, ApplicationIdentity, FusionGroup) (*FusionActivation, error)
}

// FusionRuntimePreparerFunc adapts a function to FusionRuntimePreparer.
type FusionRuntimePreparerFunc func(context.Context, ApplicationIdentity, FusionGroup) (*FusionActivation, error)

func (f FusionRuntimePreparerFunc) PrepareFusion(ctx context.Context, identity ApplicationIdentity, group FusionGroup) (*FusionActivation, error) {
	return f(ctx, identity, group)
}

// CoordinatorFusionPreparer is the standard adapter from application startup
// to the registry-backed fusion coordinator. Request supplies deployment
// policy and the exact lock/source bindings for each application and group.
type CoordinatorFusionPreparer struct {
	Coordinator *FusionCoordinator
	Request     func(ApplicationIdentity, FusionGroup) (FusionPrepareRequest, error)
}

func (p CoordinatorFusionPreparer) PrepareFusion(ctx context.Context, identity ApplicationIdentity, group FusionGroup) (*FusionActivation, error) {
	if p.Coordinator == nil {
		return nil, errors.New("fusion coordinator is required")
	}
	if p.Request == nil {
		return nil, errors.New("fusion request factory is required")
	}
	request, err := p.Request(identity, group)
	if err != nil {
		return nil, err
	}
	// The runtime owns the plan group; a request factory cannot substitute a
	// less constrained group and thereby activate an artifact for other cells.
	request.Group = group
	return p.Coordinator.Prepare(ctx, request)
}

// Public aliases keep the deployment API constructible without requiring
// callers to import Pulp's internal implementation package directly.
type FusionGroup = fusion.Group
type FusionActivation = fusion.Activation
type FusionCoordinator = fusion.Coordinator
type FusionPrepareRequest = fusion.PrepareRequest

// NewScopedApplicationRuntimeFactory returns a factory that reloads an
// application's manifest for every app instance and assigns an explicit
// ext.Scope to every cell. Import-free cells use the in-memory module cache;
// capability-bearing cells retain LoadScoped's isolated wazero Runtime. A
// fixed wazero "pulp" host module contains Cell-bound extension closures, so
// sharing that Runtime across application instances would collapse scopes.
// Their package bytes still share wazero's disk compilation cache.
//
// This factory owns cell Init/Shutdown only. The host-level event router and
// capability pollsters remain responsible for driving each live cell's step
// loop; callers must not treat this constructor alone as a replacement for
// Pulp's existing run.Main event loop.
func NewScopedApplicationRuntimeFactory(config ScopedApplicationRuntimeFactoryConfig) (ApplicationRuntimeFactory, error) {
	if config.Registry == nil {
		return nil, errors.New("scoped application runtime factory: registry is required")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	return ApplicationRuntimeFactoryFunc(func(ctx context.Context, app HostedApplication) (ApplicationRuntime, error) {
		return newApplicationRuntime(app, config), nil
	}), nil
}

type scopedApplicationRuntime struct {
	application HostedApplication
	config      ScopedApplicationRuntimeFactoryConfig

	mu      sync.Mutex
	cells   map[string]*host.Cell
	plan    *dependency.Plan
	started bool
}

func (r *scopedApplicationRuntime) Identity() ApplicationIdentity { return r.application.Identity }

// HTTPAddress exposes this application's ready public HTTP endpoint to the
// host gateway. It is intentionally empty before Start or after teardown, and
// it only queries this exact application instance's registry namespace.
func (r *scopedApplicationRuntime) HTTPAddress() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started || r.config.Endpoints == nil {
		return ""
	}
	address, _ := r.config.Endpoints.ApplicationAddress(
		r.application.Identity.ApplicationID,
		r.application.Identity.InstanceID,
		"transport.http.inbound",
		"public",
	)
	return address
}

func (r *scopedApplicationRuntime) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return fmt.Errorf("application %s is already started", r.application.Identity)
	}
	loaded, err := manifest.LoadApp(r.application.ManifestPath)
	if err != nil {
		return fmt.Errorf("load application %s: %w", r.application.Identity, err)
	}
	byName := make(map[string]*manifest.CellSpec, len(loaded.Cells.Order))
	for _, spec := range loaded.Cells.Order {
		byName[spec.Name] = spec
	}
	started := make(map[string]*host.Cell, len(loaded.Cells.Order))
	var startedMu sync.Mutex
	_, startErr := dependency.Execute(ctx, loaded.Cells.Plan, 0, func(ctx context.Context, id string) error {
		spec := byName[id]
		scope, err := r.application.NewCellScope(spec.Name, "primary")
		if err != nil {
			return err
		}
		cell, err := r.loadCell(ctx, spec, scope)
		if err != nil {
			return err
		}
		configBytes, err := manifest.EncodeConfig(spec.Config)
		if err != nil {
			_ = cell.Close(context.Background())
			return fmt.Errorf("encode config for %s: %w", spec.Name, err)
		}
		if err := cell.Init(ctx, configBytes); err != nil {
			_ = cell.Shutdown(context.Background())
			_ = cell.Close(context.Background())
			return fmt.Errorf("init cell %s: %w", spec.Name, err)
		}
		startedMu.Lock()
		started[id] = cell
		startedMu.Unlock()
		return nil
	})
	if startErr != nil {
		return r.startFailure(loaded.Cells.Plan, started, startErr)
	}
	r.cells = started
	r.plan = loaded.Cells.Plan
	r.started = true
	return nil
}

func (r *scopedApplicationRuntime) loadCell(ctx context.Context, spec *manifest.CellSpec, scope ext.Scope) (*host.Cell, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if len(spec.Capabilities) == 0 && r.config.ModuleCacheScope != nil {
		// An import-free module has no Cell-bound extension registrations,
		// therefore it is safe to instantiate from a host-wide cache scope.
		return host.LoadScopedCached(ctx, spec, nil, r.config.Limits, r.config.Logger, scope, r.config.ModuleCacheScope)
	}
	return host.LoadScoped(ctx, spec, r.config.Registry, r.config.Limits, r.config.Logger, scope)
}

func (r *scopedApplicationRuntime) startFailure(plan *dependency.Plan, cells map[string]*host.Cell, cause error) error {
	var errs []error
	for _, id := range plan.StopOrder() {
		cell := cells[id]
		if cell == nil {
			continue
		}
		if err := cell.Shutdown(context.Background()); err != nil {
			errs = append(errs, err)
		}
		if err := cell.Close(context.Background()); err != nil {
			errs = append(errs, err)
		}
	}
	if r.config.Endpoints != nil {
		r.config.Endpoints.RemoveApplication(r.application.Identity.ApplicationID, r.application.Identity.InstanceID)
	}
	return errors.Join(cause, errors.Join(errs...))
}

func (r *scopedApplicationRuntime) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	if r.plan != nil {
		for _, id := range r.plan.StopOrder() {
			cell := r.cells[id]
			if cell == nil {
				continue
			}
			if err := cell.Shutdown(ctx); err != nil {
				errs = append(errs, err)
			}
			if err := cell.Close(ctx); err != nil {
				errs = append(errs, err)
			}
		}
	}
	r.cells = nil
	r.plan = nil
	r.started = false
	if r.config.Endpoints != nil {
		r.config.Endpoints.RemoveApplication(r.application.Identity.ApplicationID, r.application.Identity.InstanceID)
	}
	return errors.Join(errs...)
}
