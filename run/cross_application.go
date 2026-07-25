package run

// Cross-application provider calls are intentionally a host-only import.
// They are not a general service locator: a caller must name an exact
// application instance, cell, and provider, and the target application must
// be a direct `depends_on` entry in the caller's host manifest. The host
// manifest validator already rejects dependency cycles, so this preserves an
// acyclic application graph while leaving package bytes shareable and every
// application's mutable WASM state isolated.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

var (
	errCrossApplicationDenied       = errors.New("cross-application call is not authorized by host dependencies")
	errCrossApplicationUnavailable  = errors.New("cross-application target is not initialized")
	errCrossApplicationInvalidRoute = errors.New("cross-application target must include application, instance, cell, and provider")
)

// crossApplicationRegistry is allocated once for a `pulp -host` process. A
// completed application start publishes its providers; shutdown revokes and
// joins every in-flight inbound call before the application's cells close.
// It contains no package data or application state.
type crossApplicationRegistry struct {
	mu      sync.RWMutex
	entries map[ApplicationIdentity]*crossApplicationEntry
}

type crossApplicationEntry struct {
	runtime *applicationRuntime
	invoke  func(context.Context, string, string, []byte) ([]byte, error)

	mu     sync.Mutex
	active bool
	calls  sync.WaitGroup
}

// crossApplicationCaller is captured from the exact scoped cell while its
// host imports are registered. Host-level depends_on grants only the
// application edge; this cell's exact host_consumes list grants the provider
// call without polluting locally resolved sibling consumes.
type crossApplicationCaller struct {
	application  HostedApplication
	cellAddress  string
	hostConsumes []string
}

func newCrossApplicationRegistry() *crossApplicationRegistry {
	return &crossApplicationRegistry{entries: make(map[ApplicationIdentity]*crossApplicationEntry)}
}

func (r *crossApplicationRegistry) markReady(application HostedApplication, runtime *applicationRuntime) error {
	if r == nil || runtime == nil {
		return errors.New("cross-application registry and runtime are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[application.Identity]; exists {
		return fmt.Errorf("cross-application target %s is already registered", application.Identity)
	}
	r.entries[application.Identity] = &crossApplicationEntry{
		runtime: runtime,
		active:  true,
		invoke: func(ctx context.Context, cellName, provider string, args []byte) ([]byte, error) {
			return callDeclaredProvider(runtime, ctx, cellName, provider, args)
		},
	}
	return nil
}

// markUnavailable removes a target from future resolution and waits for calls
// that already acquired its lease. It is safe to call after a failed partial
// start and deliberately does not affect any other application instance.
func (r *crossApplicationRegistry) markUnavailable(identity ApplicationIdentity) {
	if r == nil {
		return
	}
	r.mu.Lock()
	entry := r.entries[identity]
	delete(r.entries, identity)
	r.mu.Unlock()
	if entry == nil {
		return
	}
	entry.mu.Lock()
	entry.active = false
	entry.mu.Unlock()
	entry.calls.Wait()
}

func (r *crossApplicationRegistry) acquire(target ApplicationIdentity) (*crossApplicationEntry, error) {
	if r == nil {
		return nil, errCrossApplicationUnavailable
	}
	r.mu.RLock()
	entry := r.entries[target]
	if entry == nil {
		r.mu.RUnlock()
		return nil, errCrossApplicationUnavailable
	}
	entry.mu.Lock()
	if !entry.active {
		entry.mu.Unlock()
		r.mu.RUnlock()
		return nil, errCrossApplicationUnavailable
	}
	entry.calls.Add(1)
	entry.mu.Unlock()
	r.mu.RUnlock()
	return entry, nil
}

func (r *crossApplicationRegistry) call(ctx context.Context, caller crossApplicationCaller, target ApplicationIdentity, cellName, provider string, args []byte) ([]byte, error) {
	if err := validateCrossApplicationRoute(target, cellName, provider); err != nil {
		return nil, err
	}
	if !allowsCrossApplicationCall(caller, target, provider) {
		return nil, errCrossApplicationDenied
	}
	entry, err := r.acquire(target)
	if err != nil {
		return nil, err
	}
	defer entry.calls.Done()
	if entry.invoke == nil {
		return nil, errCrossApplicationUnavailable
	}
	return entry.invoke(ctx, cellName, provider, args)
}

func validateCrossApplicationRoute(target ApplicationIdentity, cellName, provider string) error {
	if target.ApplicationID == "" || target.InstanceID == "" || cellName == "" || provider == "" {
		return errCrossApplicationInvalidRoute
	}
	return nil
}

// allowsCrossApplicationCall is intentionally application-ID based because
// pulp.host.toml declares dependencies at the application level. The target
// instance is still mandatory at call time, so there is no implicit default
// instance or global provider fallback.
func allowsCrossApplicationCall(caller crossApplicationCaller, target ApplicationIdentity, provider string) bool {
	if caller.application.Identity == target || !containsExact(caller.hostConsumes, provider) {
		return false
	}
	for _, dependency := range caller.application.DependsOn {
		if dependency == target.ApplicationID {
			return true
		}
	}
	return false
}

func callDeclaredProvider(runtime *applicationRuntime, ctx context.Context, cellName, provider string, args []byte) ([]byte, error) {
	if runtime == nil {
		return nil, errCrossApplicationUnavailable
	}
	target := runtime.runtimes[cellName]
	if target == nil || target.failed.Load() || target.cell == nil {
		return nil, fmt.Errorf("%w: provider cell %q", errCrossApplicationUnavailable, cellName)
	}
	for _, declared := range target.spec.Provides {
		if declared == provider {
			return target.cell.Call(ctx, provider, args)
		}
	}
	return nil, fmt.Errorf("%w: cell %q does not provide %q", errCrossApplicationUnavailable, cellName, provider)
}

// crossApplicationCapability binds the versioned guest wire used by
// Pulp-Lua's pulp.app_call_raw. It is registered only by the `-host` runtime
// factory. Response allocation follows the existing sibling ABI exactly.
//
// pulp_app_call_v1(app, instance, cell, provider, args, out_ptr, out_len)
// uses pointer/length pairs for each input, followed by response out-pointers.
func crossApplicationCapability(registry *crossApplicationRegistry, caller HostedApplication) ext.Capability {
	return ext.Capability{
		Name: "pulp.cross-application.v1",
		Register: func(builder wazero.HostModuleBuilder, cell ext.Cell) error {
			// This standalone capability has no application-local manifest registry
			// from which to obtain consumes, so it deliberately binds fail-closed.
			return registerCrossApplicationImport(builder, registry, crossApplicationCaller{application: caller, cellAddress: cell.Name()})
		},
	}
}

func registerCrossApplicationImport(builder wazero.HostModuleBuilder, registry *crossApplicationRegistry, caller crossApplicationCaller) error {
	builder.NewFunctionBuilder().WithFunc(func(ctx context.Context, module api.Module,
		appPtr, appLen,
		instancePtr, instanceLen,
		cellPtr, cellLen,
		providerPtr, providerLen,
		argsPtr, argsLen,
		responsePtrOut, responseLenOut uint32,
	) (rc uint32) {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Default().Error("pulp_app_call_v1 host panic", "caller_app", caller.application.Identity, "caller_cell", caller.cellAddress, "panic", recovered, "stack", string(debug.Stack()))
				rc = 99
			}
		}()
		if appLen == 0 || instanceLen == 0 || cellLen == 0 || providerLen == 0 {
			return 1
		}
		app, ok := module.Memory().Read(appPtr, appLen)
		if !ok {
			return 2
		}
		instance, ok := module.Memory().Read(instancePtr, instanceLen)
		if !ok {
			return 2
		}
		cell, ok := module.Memory().Read(cellPtr, cellLen)
		if !ok {
			return 2
		}
		provider, ok := module.Memory().Read(providerPtr, providerLen)
		if !ok {
			return 2
		}
		var args []byte
		if argsLen > 0 {
			args, ok = module.Memory().Read(argsPtr, argsLen)
			if !ok {
				return 2
			}
			args = append([]byte(nil), args...)
		}
		response, err := registry.call(ctx, caller, ApplicationIdentity{ApplicationID: string(app), InstanceID: string(instance)}, string(cell), string(provider), args)
		if err != nil {
			if errors.Is(err, errCrossApplicationDenied) {
				return 11
			}
			return 4
		}
		return writeSiblingResponse(ctx, module, response, responsePtrOut, responseLenOut)
	}).Export("pulp_app_call_v1")
	return nil
}

// crossApplicationUnavailableCapability keeps the versioned import linkable
// for a Pulp-Lua package that is also used by a single-application runtime.
// It deliberately exposes no routing data and always returns the normal
// unavailable code (4); only `pulp -host` installs crossApplicationCapability.
func crossApplicationUnavailableCapability() ext.Capability {
	return ext.Capability{
		Name: "pulp.cross-application.v1",
		Register: func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
			return registerCrossApplicationUnavailableImport(builder)
		},
	}
}

func registerCrossApplicationUnavailableImport(builder wazero.HostModuleBuilder) error {
	builder.NewFunctionBuilder().WithFunc(func(_ context.Context, _ api.Module,
		_, _, _, _, _, _, _, _, _, _, _, _ uint32,
	) uint32 {
		return 4
	}).Export("pulp_app_call_v1")
	return nil
}
