package run

// This file is a deliberately narrow integration-test kit.  It is not used by
// Pulp's CLI/runtime path: production continues to start hosts through
// startHostedApplications.  It exists so a downstream deployment module can
// prove its app-local provider/effect boundary against Pulp's real split-host
// graph while supplying local-only capability implementations.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/BananaLabs-OSS/Pulp/abi"
	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

// HostedEffectTestHostOptions keeps test-host construction explicit.  The
// caller supplies every capability, so tests can use a local SQLite ABI while
// retaining a real privileged extension under test.  It does not consult
// process-global production configuration beyond what a supplied extension
// itself deliberately reads.
type HostedEffectTestHostOptions struct {
	StorageRoot  string
	Capabilities map[string]ext.Capability
	Logger       *slog.Logger
	// ResolveWASM optionally replaces a manifest's package path before it is
	// loaded.  Source-composition tests use it to build a fresh WASI artifact;
	// release tests leave it nil and exercise their pinned bundle unchanged.
	ResolveWASM func(HostedApplication, string, string) (string, error)
}

// HostedEffectTestHost is an isolated, test-only split-host fixture.  It
// exposes only the minimum operations a deployment E2E needs: an exact local
// provider caller, a published HTTP endpoint, and deterministic shutdown.
// It never exposes a sibling application's cell graph.
type HostedEffectTestHost struct {
	ctx    context.Context
	cancel context.CancelFunc

	endpoints *EndpointRegistry
	caps      map[string]ext.Capability
	apps      map[ApplicationIdentity]*hostedEffectTestApplication
	order     []ApplicationIdentity

	pumpMu sync.Mutex
	pumps  map[ApplicationIdentity]context.CancelFunc
	pumpWG sync.WaitGroup
}

type hostedEffectTestApplication struct {
	application     HostedApplication
	cells           map[string]*cellRuntime
	loaded          []*host.Cell
	capabilities    []ext.Capability
	capabilityScope ext.Scope
	cross           *crossApplicationRegistry
}

// StartHostedEffectTestHost loads the exact application graph described by a
// validated host manifest.  It is intentionally separate from
// startHostedApplications: no production code can accidentally acquire test
// capabilities or this reduced public surface.
func StartHostedEffectTestHost(ctx context.Context, hostPath string, options HostedEffectTestHostOptions) (*HostedEffectTestHost, error) {
	if ctx == nil {
		return nil, errors.New("hosted effect test kit: context is required")
	}
	if options.StorageRoot == "" {
		return nil, errors.New("hosted effect test kit: storage root is required")
	}
	if len(options.Capabilities) == 0 {
		return nil, errors.New("hosted effect test kit: capabilities are required")
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	applications, err := (ManifestHostLoader{}).LoadHostApplications(ctx, hostPath)
	if err != nil {
		return nil, fmt.Errorf("hosted effect test kit: load host: %w", err)
	}
	applications, err = canonicalHostedApplications(applications)
	if err != nil {
		return nil, fmt.Errorf("hosted effect test kit: validate host: %w", err)
	}
	hostCtx, cancel := context.WithCancel(ctx)
	hosted := &HostedEffectTestHost{
		ctx: hostCtx, cancel: cancel, endpoints: NewEndpointRegistry(), caps: options.Capabilities,
		apps:  make(map[ApplicationIdentity]*hostedEffectTestApplication, len(applications)),
		pumps: make(map[ApplicationIdentity]context.CancelFunc),
	}
	cross := newCrossApplicationRegistry()
	for _, application := range applications {
		started, startErr := startHostedEffectTestApplication(hostCtx, options.StorageRoot, hosted.endpoints, cross, options.Capabilities, options.Logger, options.ResolveWASM, application)
		if startErr != nil {
			_ = hosted.Shutdown(context.Background())
			return nil, startErr
		}
		hosted.apps[application.Identity] = started
		hosted.order = append(hosted.order, application.Identity)
	}
	return hosted, nil
}

func startHostedEffectTestApplication(
	ctx context.Context,
	storageRoot string,
	endpoints *EndpointRegistry,
	cross *crossApplicationRegistry,
	capabilities map[string]ext.Capability,
	logger *slog.Logger,
	resolveWASM func(HostedApplication, string, string) (string, error),
	application HostedApplication,
) (*hostedEffectTestApplication, error) {
	loaded, err := manifest.LoadApp(application.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("hosted effect test kit: load application %s: %w", application.Identity, err)
	}
	if loaded.Name != application.Identity.ApplicationID {
		return nil, fmt.Errorf("hosted effect test kit: application %s loads %q", application.Identity, loaded.Name)
	}
	if resolveWASM != nil {
		for _, spec := range loaded.Cells.Order {
			wasmPath, resolveErr := resolveWASM(application, spec.Name, spec.WASMPath)
			if resolveErr != nil {
				return nil, fmt.Errorf("hosted effect test kit: resolve %s/%s WASM: %w", application.Identity, spec.Name, resolveErr)
			}
			if wasmPath == "" {
				return nil, fmt.Errorf("hosted effect test kit: resolve %s/%s returned an empty WASM path", application.Identity, spec.Name)
			}
			spec.WASMPath = wasmPath
		}
	}
	declared := map[string]bool{}
	for _, spec := range loaded.Cells.Order {
		for _, name := range spec.Capabilities {
			if _, ok := capabilities[name]; !ok {
				return nil, fmt.Errorf("hosted effect test kit: %s declares unavailable capability %q", application.Identity, name)
			}
			declared[name] = true
		}
	}
	capabilityScope, err := ext.NewScope(application.Identity.ApplicationID, application.Identity.InstanceID, "host", "primary")
	if err != nil {
		return nil, err
	}
	active := make([]ext.Capability, 0, len(declared))
	for name := range declared {
		capability := capabilities[name]
		if capability.Setup != nil {
			if err := capability.Setup(ext.SetupEnv{Scope: capabilityScope, StorageRoot: hostedEffectTestStorageRoot(storageRoot, application), Endpoints: endpoints, Logger: logger}); err != nil {
				return nil, fmt.Errorf("hosted effect test kit: setup %s capability %q: %w", application.Identity, name, err)
			}
		}
		active = append(active, capability)
	}

	cells := make(map[string]*cellRuntime, len(loaded.Cells.Order))
	for _, spec := range loaded.Cells.Order {
		cells[spec.Name] = &cellRuntime{spec: spec}
	}
	registry := host.NewRegistry()
	for _, capability := range capabilities {
		registry.Gated(capability)
	}
	registry.Always(siblingCapabilityWithCrossApplication(newSiblingRegistry(cells), cross, application))
	if missing := validateSiblingLinks(cells); len(missing) != 0 {
		return nil, fmt.Errorf("hosted effect test kit: %s local composition links: %v", application.Identity, missing)
	}

	started := &hostedEffectTestApplication{application: application, cells: cells, capabilities: active, capabilityScope: capabilityScope, cross: cross}
	for _, spec := range loaded.Cells.Order {
		scope, scopeErr := application.NewCellScope(spec.Name, "primary")
		if scopeErr != nil {
			started.close(context.Background())
			return nil, scopeErr
		}
		cell, loadErr := host.LoadScoped(ctx, spec, registry, nil, logger, scope)
		if loadErr != nil {
			started.close(context.Background())
			return nil, fmt.Errorf("hosted effect test kit: load %s/%s: %w", application.Identity, spec.Name, loadErr)
		}
		cells[spec.Name].cell = cell
		started.loaded = append(started.loaded, cell)
		config, configErr := manifest.EncodeConfig(spec.Config)
		if configErr != nil {
			started.close(context.Background())
			return nil, configErr
		}
		if initErr := cell.Init(ctx, config); initErr != nil {
			started.close(context.Background())
			return nil, fmt.Errorf("hosted effect test kit: init %s/%s: %w", application.Identity, spec.Name, initErr)
		}
	}
	if err := cross.markReady(application, &applicationRuntime{application: application, runtimes: cells}); err != nil {
		started.close(context.Background())
		return nil, fmt.Errorf("hosted effect test kit: register %s providers: %w", application.Identity, err)
	}
	return started, nil
}

func hostedEffectTestStorageRoot(root string, application HostedApplication) string {
	return filepath.Join(root, application.StorageNamespace, application.Identity.InstanceID)
}

// CallProvider invokes only a listed provider in the named application.  It
// is the test-kit equivalent of Pulp's lifecycle-scoped provider lease and
// deliberately cannot route to another application.
func (h *HostedEffectTestHost) CallProvider(ctx context.Context, identity ApplicationIdentity, cellName, provider string, args []byte) ([]byte, error) {
	if h == nil || h.apps == nil {
		return nil, errors.New("hosted effect test kit: host is not running")
	}
	application := h.apps[identity]
	if application == nil {
		return nil, fmt.Errorf("hosted effect test kit: application %s is unavailable", identity)
	}
	runtime := application.cells[cellName]
	if runtime == nil || runtime.cell == nil {
		return nil, fmt.Errorf("hosted effect test kit: cell %q is unavailable", cellName)
	}
	allowed := false
	for _, name := range runtime.spec.Provides {
		if name == provider {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("hosted effect test kit: cell %q does not provide %q", cellName, provider)
	}
	return runtime.cell.Call(ctx, provider, args)
}

// HTTPAddress returns the private endpoint published by one exact hosted
// application.  Tests may place their own loopback front door in front of it;
// this kit never starts a public production gateway.
func (h *HostedEffectTestHost) HTTPAddress(identity ApplicationIdentity) (string, bool) {
	if h == nil || h.endpoints == nil {
		return "", false
	}
	return h.endpoints.ApplicationAddress(identity.ApplicationID, identity.InstanceID, "transport.http.inbound", "public")
}

// StartHTTPPump delivers one extension's events to one exact cell.  This is
// intentionally explicit so a test cannot accidentally consume a sibling
// application's unscoped event stream.
func (h *HostedEffectTestHost) StartHTTPPump(identity ApplicationIdentity, cellName string) error {
	if h == nil || h.apps == nil {
		return errors.New("hosted effect test kit: host is not running")
	}
	h.pumpMu.Lock()
	defer h.pumpMu.Unlock()
	if _, exists := h.pumps[identity]; exists {
		return fmt.Errorf("hosted effect test kit: HTTP pump already started for %s", identity)
	}
	application := h.apps[identity]
	if application == nil || application.cells[cellName] == nil || application.cells[cellName].cell == nil {
		return fmt.Errorf("hosted effect test kit: HTTP target %s/%s is unavailable", identity, cellName)
	}
	capability, ok := h.caps["transport.http.inbound"]
	if !ok || capability.Poll == nil {
		return errors.New("hosted effect test kit: HTTP transport is unavailable")
	}
	pumpCtx, cancel := context.WithCancel(h.ctx)
	h.pumps[identity] = cancel
	cell := application.cells[cellName].cell
	h.pumpWG.Add(1)
	go func() {
		defer h.pumpWG.Done()
		var callNumber uint64
		for {
			select {
			case <-pumpCtx.Done():
				return
			default:
			}
			event, ok := capability.Poll()
			if !ok {
				time.Sleep(time.Millisecond)
				continue
			}
			payload, err := abi.EncodeStepEvent(event.Kind, event.Payload)
			if err == nil {
				_, _ = cell.Step(pumpCtx, abi.StepEnvelope{CallNumber: callNumber, WallTime: uint64(time.Now().UnixNano()), Payload: payload})
			}
			callNumber++
			if capability.Finalize != nil {
				capability.Finalize(event.ID)
			}
		}
	}()
	return nil
}

// Shutdown stops all explicit pumps and closes applications in reverse host
// order.  It is safe to call after a partial startup failure.
func (h *HostedEffectTestHost) Shutdown(ctx context.Context) error {
	if h == nil {
		return nil
	}
	h.pumpMu.Lock()
	for _, cancel := range h.pumps {
		cancel()
	}
	h.pumps = map[ApplicationIdentity]context.CancelFunc{}
	h.pumpMu.Unlock()
	h.pumpWG.Wait()
	if h.cancel != nil {
		h.cancel()
	}
	var failures []error
	for index := len(h.order) - 1; index >= 0; index-- {
		identity := h.order[index]
		if application := h.apps[identity]; application != nil {
			if err := application.close(ctx); err != nil {
				failures = append(failures, err)
			}
		}
	}
	h.apps = nil
	return errors.Join(failures...)
}

func (h *hostedEffectTestApplication) close(ctx context.Context) error {
	if h == nil {
		return nil
	}
	var failures []error
	if h.cross != nil {
		h.cross.markUnavailable(h.application.Identity)
	}
	for index := len(h.loaded) - 1; index >= 0; index-- {
		if err := h.loaded[index].Shutdown(ctx); err != nil {
			failures = append(failures, err)
		}
		if err := h.loaded[index].Close(context.Background()); err != nil {
			failures = append(failures, err)
		}
	}
	for _, capability := range h.capabilities {
		if capability.TeardownScope != nil {
			if err := capability.TeardownScope(ctx, h.capabilityScope); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}
