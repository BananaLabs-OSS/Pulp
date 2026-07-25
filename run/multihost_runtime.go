package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

// ScopedApplicationRuntimeFactoryConfig supplies the host-owned dependencies
// needed to construct application-local cell graphs. StorageRoot and HTTPPort
// are retained as explicit application-host inputs: the caller performs the
// one-time extension setup using them before any runtime is created.
type ScopedApplicationRuntimeFactoryConfig struct {
	Registry         *host.Registry
	Limits           *host.Limits
	Logger           *slog.Logger
	ModuleCacheScope *host.ModuleCacheScope
	StorageRoot      string
	HTTPPort         string
	Endpoints        *EndpointRegistry
	// RequireScopedCapabilityLifecycle rejects known process-global extension
	// lifecycles when a host has more than one application instance.
	RequireScopedCapabilityLifecycle bool
	Lifecycle                        ApplicationLifecycleObserver
	// CrossApplications is host-owned routing state for the explicit
	// cross-application provider import. It is nil outside `pulp -host`, so
	// ordinary single-application runs do not receive that import at all.
	CrossApplications *crossApplicationRegistry
}

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
	cells   []*host.Cell
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
	started := make([]*host.Cell, 0, len(loaded.Cells.Order))
	for _, spec := range loaded.Cells.Order {
		scope, err := r.application.NewCellScope(spec.Name, "primary")
		if err != nil {
			return r.startFailure(ctx, started, err)
		}
		cell, err := r.loadCell(ctx, spec, scope)
		if err != nil {
			return r.startFailure(ctx, started, err)
		}
		configBytes, err := manifest.EncodeConfig(spec.Config)
		if err != nil {
			_ = cell.Close(context.Background())
			return r.startFailure(ctx, started, fmt.Errorf("encode config for %s: %w", spec.Name, err))
		}
		if err := cell.Init(ctx, configBytes); err != nil {
			_ = cell.Shutdown(context.Background())
			_ = cell.Close(context.Background())
			return r.startFailure(ctx, started, fmt.Errorf("init cell %s: %w", spec.Name, err))
		}
		started = append(started, cell)
	}
	r.cells = started
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

func (r *scopedApplicationRuntime) startFailure(ctx context.Context, cells []*host.Cell, cause error) error {
	var errs []error
	for index := len(cells) - 1; index >= 0; index-- {
		if err := cells[index].Shutdown(context.Background()); err != nil {
			errs = append(errs, err)
		}
		if err := cells[index].Close(context.Background()); err != nil {
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
	for index := len(r.cells) - 1; index >= 0; index-- {
		if err := r.cells[index].Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
		if err := r.cells[index].Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	r.cells = nil
	r.started = false
	if r.config.Endpoints != nil {
		r.config.Endpoints.RemoveApplication(r.application.Identity.ApplicationID, r.application.Identity.InstanceID)
	}
	return errors.Join(errs...)
}
