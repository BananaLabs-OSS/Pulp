package run

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

// ApplicationIdentity is the stable identity of one application instance in a
// Pulp host. ApplicationID names the composition; InstanceID distinguishes
// independently stateful copies of that composition. The pair is deliberately
// required everywhere a runtime is constructed, so an extension or cell can
// scope its state without falling back to a process-global application.
type ApplicationIdentity struct {
	ApplicationID string
	InstanceID    string
}

// String returns the canonical, human-readable identity used by diagnostics.
func (id ApplicationIdentity) String() string {
	return id.ApplicationID + "/" + id.InstanceID
}

func (id ApplicationIdentity) validate() error {
	if strings.TrimSpace(id.ApplicationID) == "" {
		return errors.New("application ID is required")
	}
	if strings.TrimSpace(id.InstanceID) == "" {
		return fmt.Errorf("application %q: instance ID is required", id.ApplicationID)
	}
	return nil
}

// HostedApplication is one entry in a host composition. ManifestPath is
// intentionally opaque to the supervisor: a loader can resolve a TOML entry,
// a test fixture, or a future registry reference before it reaches the
// runtime factory.
type HostedApplication struct {
	Identity         ApplicationIdentity
	ManifestPath     string
	StorageNamespace string
	EventNamespace   string
	DependsOn        []string
}

// ManifestHostLoader adapts the first-class pulp.host.toml API to the runtime
// supervisor. LoadHost validates the complete host composition once; each
// returned entry identifies one independently instantiated application.
//
// The application manifest itself is intentionally represented by its path.
// A runtime factory must load a fresh manifest/cell graph for each instance,
// rather than mutating the Host's validated in-memory Application. That is
// what makes two instances of the same package code actually independent.
type ManifestHostLoader struct{}

// ValidateHost reads and validates a pulp.host.toml without starting cells,
// binding extensions, or touching privileged host effects. Deployment bundle
// validators use it to verify the exact staged composition before launch.
func ValidateHost(path string) error {
	_, err := manifest.LoadHost(path)
	return err
}

// LoadHostApplications implements HostManifestLoader using manifest.LoadHost.
func (ManifestHostLoader) LoadHostApplications(ctx context.Context, path string) ([]HostedApplication, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	host, err := manifest.LoadHost(path)
	if err != nil {
		return nil, err
	}
	applications := make([]HostedApplication, 0)
	for _, application := range host.ApplicationOrder {
		for _, instance := range application.Instances {
			applications = append(applications, HostedApplication{
				Identity: ApplicationIdentity{
					ApplicationID: application.ID,
					InstanceID:    instance.Alias,
				},
				ManifestPath:     application.ManifestPath,
				StorageNamespace: application.StorageNamespace,
				EventNamespace:   application.EventNamespace,
				DependsOn:        append([]string(nil), application.DependsOn...),
			})
		}
	}
	return applications, nil
}

func (app HostedApplication) validate() error {
	if err := app.Identity.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(app.ManifestPath) == "" {
		return fmt.Errorf("application %s: manifest path is required", app.Identity)
	}
	return nil
}

// NewCellScope creates the mandatory extension ownership scope for one cell
// placement in this application instance. Runtime factories pass this exact
// scope to every scoped cell they create; using only a cell name is forbidden
// for a multi-application runtime because it would make equal cell names
// collide across applications or instances.
func (app HostedApplication) NewCellScope(cellID, cellInstanceID string) (ext.Scope, error) {
	if err := app.Identity.validate(); err != nil {
		return ext.Scope{}, err
	}
	return ext.NewScope(app.Identity.ApplicationID, app.Identity.InstanceID, cellID, cellInstanceID)
}

// HostManifestLoader is the narrow integration point for the host-manifest
// package. Its adapter owns parsing and validation of the host TOML format;
// the supervisor only owns lifecycle orchestration. This keeps the runtime
// testable and prevents the run package from coupling to a particular schema.
type HostManifestLoader interface {
	LoadHostApplications(ctx context.Context, path string) ([]HostedApplication, error)
}

// HostManifestLoaderFunc adapts a function to HostManifestLoader.
type HostManifestLoaderFunc func(context.Context, string) ([]HostedApplication, error)

// LoadHostApplications implements HostManifestLoader.
func (fn HostManifestLoaderFunc) LoadHostApplications(ctx context.Context, path string) ([]HostedApplication, error) {
	return fn(ctx, path)
}

// ApplicationRuntimeFactory creates an isolated runtime for one application
// instance. A factory must create fresh Lua state, cell graph, cancellation
// tree, and extension-instance scope for every call. The supervisor never
// passes another application's runtime to this method, so cross-application
// calls can only be introduced through an explicit future federation API.
type ApplicationRuntimeFactory interface {
	NewApplicationRuntime(ctx context.Context, app HostedApplication) (ApplicationRuntime, error)
}

// ApplicationRuntimeFactoryFunc adapts a function to ApplicationRuntimeFactory.
type ApplicationRuntimeFactoryFunc func(context.Context, HostedApplication) (ApplicationRuntime, error)

// NewApplicationRuntime implements ApplicationRuntimeFactory.
func (fn ApplicationRuntimeFactoryFunc) NewApplicationRuntime(ctx context.Context, app HostedApplication) (ApplicationRuntime, error) {
	return fn(ctx, app)
}

// ApplicationRuntime is a fully isolated application lifecycle. Start must
// not return until its Lua orchestrator and declared cell graph are ready;
// Shutdown must stop only that application. The host does not expose sibling
// runtimes here by design.
type ApplicationRuntime interface {
	Identity() ApplicationIdentity
	Start(context.Context) error
	Shutdown(context.Context) error
}

// MultiHostSupervisor loads applications and manages their lifecycles as one
// fail-fast host unit. It starts in canonical (application ID, instance ID)
// order and stops in reverse order. Methods are serialized, so callers may
// safely issue concurrent lifecycle requests without interleaving starts,
// rollbacks, or shutdowns.
type MultiHostSupervisor struct {
	mu      sync.Mutex
	loader  HostManifestLoader
	factory ApplicationRuntimeFactory

	state    multiHostState
	runtimes []ApplicationRuntime
}

type multiHostState uint8

const (
	multiHostNew multiHostState = iota
	multiHostRunning
	multiHostStopped
)

var (
	// ErrMultiHostRunning reports a duplicate Start while an application host is
	// already active.
	ErrMultiHostRunning = errors.New("multi-application host is already running")
	// ErrMultiHostStopped reports an attempt to restart a supervisor after a
	// completed shutdown. Creating a new supervisor gives the next host run a
	// clean lifecycle boundary.
	ErrMultiHostStopped = errors.New("multi-application host has been stopped")
)

// NewMultiHostSupervisor creates a lifecycle supervisor. The supplied loader
// is normally a small adapter around manifest.LoadHost once that API is wired;
// keeping it injectable lets the lifecycle be tested without WASM or TOML.
func NewMultiHostSupervisor(loader HostManifestLoader, factory ApplicationRuntimeFactory) (*MultiHostSupervisor, error) {
	if loader == nil {
		return nil, errors.New("host manifest loader is required")
	}
	if factory == nil {
		return nil, errors.New("application runtime factory is required")
	}
	return &MultiHostSupervisor{loader: loader, factory: factory}, nil
}

// Start loads, validates, and starts every application described by hostPath.
// Any failure stops the failing runtime (if one was created) and every already
// started application in reverse deterministic order before returning the
// original error joined with any rollback errors.
func (s *MultiHostSupervisor) Start(ctx context.Context, hostPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch s.state {
	case multiHostRunning:
		return ErrMultiHostRunning
	case multiHostStopped:
		return ErrMultiHostStopped
	}

	apps, err := s.loader.LoadHostApplications(ctx, hostPath)
	if err != nil {
		return fmt.Errorf("load host applications: %w", err)
	}
	apps, err = canonicalHostedApplications(apps)
	if err != nil {
		return err
	}

	started := make([]ApplicationRuntime, 0, len(apps))
	for _, app := range apps {
		runtime, err := s.factory.NewApplicationRuntime(ctx, app)
		if err != nil {
			return s.startFailure(err, started)
		}
		if runtime == nil {
			return s.startFailure(fmt.Errorf("create application runtime %s: factory returned nil", app.Identity), started)
		}
		if got := runtime.Identity(); got != app.Identity {
			shutdownErr := runtime.Shutdown(context.Background())
			return s.startFailure(errors.Join(
				fmt.Errorf("create application runtime %s: runtime identity is %s", app.Identity, got),
				shutdownErr,
			), started)
		}

		// Add before Start so a partially started runtime is included in the
		// fail-fast rollback contract.
		started = append(started, runtime)
		if err := runtime.Start(ctx); err != nil {
			return s.startFailure(fmt.Errorf("start application %s: %w", app.Identity, err), started)
		}
	}

	s.runtimes = started
	s.state = multiHostRunning
	return nil
}

func (s *MultiHostSupervisor) startFailure(cause error, started []ApplicationRuntime) error {
	rollbackErr := shutdownRuntimes(context.Background(), started)
	if rollbackErr != nil {
		return errors.Join(cause, fmt.Errorf("rollback application host: %w", rollbackErr))
	}
	return cause
}

// Shutdown stops all started applications in reverse startup order. It is
// idempotent after a successful shutdown; a shutdown before Start is a no-op.
// All runtimes get a shutdown opportunity even when an earlier one fails.
func (s *MultiHostSupervisor) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == multiHostStopped || s.state == multiHostNew {
		return nil
	}
	err := shutdownRuntimes(ctx, s.runtimes)
	s.runtimes = nil
	s.state = multiHostStopped
	return err
}

func shutdownRuntimes(ctx context.Context, runtimes []ApplicationRuntime) error {
	var errs []error
	for index := len(runtimes) - 1; index >= 0; index-- {
		runtime := runtimes[index]
		if err := runtime.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown application %s: %w", runtime.Identity(), err))
		}
	}
	return errors.Join(errs...)
}

func canonicalHostedApplications(apps []HostedApplication) ([]HostedApplication, error) {
	canonical := append([]HostedApplication(nil), apps...)
	seen := make(map[ApplicationIdentity]struct{}, len(canonical))
	byApplication := make(map[string][]HostedApplication, len(canonical))
	for _, app := range canonical {
		if err := app.validate(); err != nil {
			return nil, fmt.Errorf("invalid hosted application: %w", err)
		}
		if _, exists := seen[app.Identity]; exists {
			return nil, fmt.Errorf("duplicate hosted application %s", app.Identity)
		}
		seen[app.Identity] = struct{}{}
		byApplication[app.Identity.ApplicationID] = append(byApplication[app.Identity.ApplicationID], app)
	}
	for applicationID, instances := range byApplication {
		for _, dependency := range instances[0].DependsOn {
			if _, exists := byApplication[dependency]; !exists {
				return nil, fmt.Errorf("application %q depends on unknown application %q", applicationID, dependency)
			}
		}
		for _, instance := range instances[1:] {
			if !sameStringSet(instances[0].DependsOn, instance.DependsOn) {
				return nil, fmt.Errorf("application %q has inconsistent instance dependencies", applicationID)
			}
		}
		sort.Slice(instances, func(left, right int) bool {
			return instances[left].Identity.InstanceID < instances[right].Identity.InstanceID
		})
		byApplication[applicationID] = instances
	}

	applicationOrder, err := canonicalApplicationOrder(byApplication)
	if err != nil {
		return nil, err
	}
	ordered := make([]HostedApplication, 0, len(canonical))
	for _, applicationID := range applicationOrder {
		ordered = append(ordered, byApplication[applicationID]...)
	}
	return ordered, nil
}

func canonicalApplicationOrder(byApplication map[string][]HostedApplication) ([]string, error) {
	remaining := make(map[string]map[string]struct{}, len(byApplication))
	dependents := make(map[string][]string, len(byApplication))
	for applicationID, instances := range byApplication {
		dependencies := make(map[string]struct{}, len(instances[0].DependsOn))
		for _, dependency := range instances[0].DependsOn {
			if _, exists := dependencies[dependency]; exists {
				continue
			}
			dependencies[dependency] = struct{}{}
			dependents[dependency] = append(dependents[dependency], applicationID)
		}
		remaining[applicationID] = dependencies
	}

	ready := make([]string, 0, len(remaining))
	for applicationID, dependencies := range remaining {
		if len(dependencies) == 0 {
			ready = append(ready, applicationID)
		}
	}
	sort.Strings(ready)
	ordered := make([]string, 0, len(remaining))
	for len(ready) > 0 {
		applicationID := ready[0]
		ready = ready[1:]
		ordered = append(ordered, applicationID)
		for _, dependent := range dependents[applicationID] {
			delete(remaining[dependent], applicationID)
			if len(remaining[dependent]) == 0 {
				ready = append(ready, dependent)
			}
		}
		sort.Strings(ready)
	}
	if len(ordered) != len(byApplication) {
		return nil, errors.New("application dependency cycle")
	}
	return ordered, nil
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[string]struct{}, len(left))
	for _, value := range left {
		seen[value] = struct{}{}
	}
	for _, value := range right {
		if _, exists := seen[value]; !exists {
			return false
		}
	}
	return true
}
