package run

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/tetratelabs/wazero"
)

// TestMultiHostSupervisorSharesCodeButNotApplicationState exercises the
// production host-manifest loader, the real ModuleCache, and a stateful WASM
// module under the supervisor. Both apps refer to one byte-for-byte package;
// sessions is intentionally instantiated twice. The single compilation and
// per-instance counter results prove the intended boundary: code is shared,
// mutable WASM state is not.
func TestMultiHostSupervisorSharesCodeButNotApplicationState(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sharedWASMPath := writeRealMultiHostBytes(t, root, "shared/counter.wasm", multiHostCounterWASM)
	_, evolutionAppPath := writeRealMultiHostApp(t, root, "evolution", sharedWASMPath, "return { app = 'evolution' }\n")
	_, sessionsAppPath := writeRealMultiHostApp(t, root, "sessions", sharedWASMPath, "return { app = 'sessions' }\n")
	hostPath := writeRealMultiHostFile(t, root, "pulp.host.toml", fmt.Sprintf(`
name = "multi-host-test"
[[applications]]
id = "evolution"
manifest = %q
aliases = ["primary"]
storage_namespace = "evolution-storage"
event_namespace = "evolution-events"

[[applications]]
id = "sessions"
manifest = %q
instances = 2
aliases = ["blue", "green"]
storage_namespace = "sessions-storage"
event_namespace = "sessions-events"
depends_on = ["evolution"]
`, filepath.ToSlash(filepath.Join("apps", "evolution", filepath.Base(evolutionAppPath))), filepath.ToSlash(filepath.Join("apps", "sessions", filepath.Base(sessionsAppPath)))))

	wasmBytes, err := os.ReadFile(sharedWASMPath)
	if err != nil {
		t.Fatalf("read shared WASM: %v", err)
	}
	wasmRuntime := wazero.NewRuntime(ctx)
	defer wasmRuntime.Close(ctx)
	moduleCache := host.NewModuleCache()
	defer moduleCache.Close(ctx)
	cacheScope, err := moduleCache.NewScope(wasmRuntime, host.ModuleRuntimeConfig{Fingerprint: "test-memory=64;interruptible=false"})
	if err != nil {
		t.Fatalf("new module-cache scope: %v", err)
	}

	var recorder realMultiHostRecorder
	runtimes := make(map[ApplicationIdentity]*realMultiHostRuntime)
	factory := ApplicationRuntimeFactoryFunc(func(ctx context.Context, app HostedApplication) (ApplicationRuntime, error) {
		// A runtime loads its own app object. The manifest package is used only
		// for immutable composition input; no CellSpec/Lua config is shared with
		// another instance.
		loaded, err := manifest.LoadApp(app.ManifestPath)
		if err != nil {
			return nil, err
		}
		script, err := os.ReadFile(loaded.OrchestrationScript)
		if err != nil {
			return nil, err
		}
		cellScope, err := app.NewCellScope("stateful-counter", "primary")
		if err != nil {
			return nil, err
		}
		runtime := &realMultiHostRuntime{
			identity:    app.Identity,
			cellScope:   cellScope,
			luaSource:   string(script),
			wasmBytes:   wasmBytes,
			cacheScope:  cacheScope,
			recorder:    &recorder,
			storageKey:  realMultiHostKey(t, cellScope, "storage", "counter"),
			eventTarget: cellScope.RoutingID(),
		}
		runtimes[app.Identity] = runtime
		return runtime, nil
	})
	supervisor := testMultiHostSupervisor(t, ManifestHostLoader{}, factory)

	if err := supervisor.Start(ctx, hostPath); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := moduleCache.Stats().Compilations; got != 1 {
		t.Fatalf("shared WASM compilations = %d, want 1", got)
	}
	if got := moduleCache.Stats().Entries; got != 1 {
		t.Fatalf("shared WASM cache entries = %d, want 1", got)
	}

	evolution := runtimes[ApplicationIdentity{ApplicationID: "evolution", InstanceID: "primary"}]
	blue := runtimes[ApplicationIdentity{ApplicationID: "sessions", InstanceID: "blue"}]
	green := runtimes[ApplicationIdentity{ApplicationID: "sessions", InstanceID: "green"}]
	if evolution == nil || blue == nil || green == nil {
		t.Fatalf("runtimes = %#v, want evolution + two sessions instances", runtimes)
	}
	assertRealMultiHostCounter(t, evolution, 1)
	assertRealMultiHostCounter(t, blue, 1)
	assertRealMultiHostCounter(t, green, 1)
	assertRealMultiHostCounter(t, blue, 2)
	assertRealMultiHostCounter(t, green, 2)
	if evolution.cellScope.RoutingID() == blue.eventTarget || blue.eventTarget == green.eventTarget {
		t.Fatalf("event routing scopes collide: evolution=%q blue=%q green=%q", evolution.eventTarget, blue.eventTarget, green.eventTarget)
	}
	if evolution.storageKey == blue.storageKey || blue.storageKey == green.storageKey {
		t.Fatalf("storage scopes collide: evolution=%q blue=%q green=%q", evolution.storageKey, blue.storageKey, green.storageKey)
	}
	if evolution.luaSource == blue.luaSource {
		t.Fatalf("application Lua sources unexpectedly match: %q", evolution.luaSource)
	}

	if err := supervisor.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	wantLifecycle := []string{
		"start:evolution/primary", "start:sessions/blue", "start:sessions/green",
		"shutdown:sessions/green", "shutdown:sessions/blue", "shutdown:evolution/primary",
	}
	if got := recorder.lifecycle(); lifecycleDiff(wantLifecycle, got) != "" {
		t.Fatal(lifecycleDiff(wantLifecycle, got))
	}
}

// TestMultiHostRealCellLoaderContract runs two real host.Cell values through
// the cache-aware scoped loader. It is intentionally import-free: a Cell-bound
// extension registry cannot be reused in one wazero Runtime without a dynamic
// dispatcher, whereas import-free packages may share the host-wide compiled
// module safely.
func TestMultiHostRealCellLoaderContract(t *testing.T) {
	ctx := context.Background()
	wasmPath := writeRealMultiHostBytes(t, t.TempDir(), "stateful-pulp.wasm", multiHostLifecycleWASM)
	wasmRuntime := wazero.NewRuntime(ctx)
	defer wasmRuntime.Close(ctx)
	moduleCache := host.NewModuleCache()
	defer moduleCache.Close(ctx)
	cacheScope, err := moduleCache.NewScope(wasmRuntime, host.ModuleRuntimeConfig{Fingerprint: "test-memory=64;interruptible=false"})
	if err != nil {
		t.Fatalf("new module-cache scope: %v", err)
	}
	spec := &manifest.CellSpec{Name: "stateful", WASMPath: wasmPath}
	blueApp := testHostedApplication("sessions", "blue")
	greenApp := testHostedApplication("sessions", "green")
	blueScope, err := blueApp.NewCellScope("stateful", "primary")
	if err != nil {
		t.Fatal(err)
	}
	greenScope, err := greenApp.NewCellScope("stateful", "primary")
	if err != nil {
		t.Fatal(err)
	}
	blue, err := host.LoadScopedCached(ctx, spec, nil, nil, nil, blueScope, cacheScope)
	if err != nil {
		t.Fatalf("load blue cell: %v", err)
	}
	green, err := host.LoadScopedCached(ctx, spec, nil, nil, nil, greenScope, cacheScope)
	if err != nil {
		_ = blue.Close(ctx)
		t.Fatalf("load green cell: %v", err)
	}
	if got := moduleCache.Stats().Compilations; got != 1 {
		t.Fatalf("real host-cell compilations = %d, want 1", got)
	}
	if blue.Scope().RoutingID() == green.Scope().RoutingID() {
		t.Fatalf("real host-cell scopes collide: %q", blue.Scope().RoutingID())
	}
	if err := green.Close(ctx); err != nil {
		t.Fatalf("close green: %v", err)
	}
	if err := blue.Close(ctx); err != nil {
		t.Fatalf("close blue: %v", err)
	}
}

type realMultiHostRuntime struct {
	identity    ApplicationIdentity
	cellScope   ext.Scope
	luaSource   string
	wasmBytes   []byte
	cacheScope  *host.ModuleCacheScope
	recorder    *realMultiHostRecorder
	storageKey  string
	eventTarget string

	instance *host.CachedModuleInstance
}

func (r *realMultiHostRuntime) Identity() ApplicationIdentity { return r.identity }

func (r *realMultiHostRuntime) Start(ctx context.Context) error {
	lease, err := r.cacheScope.Acquire(ctx, r.wasmBytes)
	if err != nil {
		return err
	}
	instance, err := lease.Instantiate(ctx, wazero.NewModuleConfig().WithName(r.cellScope.RoutingID()))
	if err != nil {
		return err
	}
	r.instance = instance
	r.recorder.record("start", r.identity)
	return nil
}

func (r *realMultiHostRuntime) Shutdown(ctx context.Context) error {
	r.recorder.record("shutdown", r.identity)
	if r.instance == nil {
		return nil
	}
	return r.instance.Close(ctx)
}

func assertRealMultiHostCounter(t *testing.T, runtime *realMultiHostRuntime, want uint64) {
	t.Helper()
	got, err := runtime.instance.Module().ExportedFunction("inc").Call(context.Background())
	if err != nil {
		t.Fatalf("%s increment: %v", runtime.identity, err)
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("%s counter = %v, want %d", runtime.identity, got, want)
	}
}

type realMultiHostRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *realMultiHostRecorder) record(operation string, identity ApplicationIdentity) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, operation+":"+identity.String())
}

func (r *realMultiHostRecorder) lifecycle() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func realMultiHostKey(t *testing.T, scope ext.Scope, resourceType, resourceID string) string {
	t.Helper()
	key, err := scope.ResourceKey(resourceType, resourceID)
	if err != nil {
		t.Fatalf("resource key: %v", err)
	}
	return key.String()
}

func writeRealMultiHostApp(t *testing.T, root, applicationID, wasmPath, lua string) (string, string) {
	t.Helper()
	dir := filepath.Join(root, "apps", applicationID)
	cellPath := writeRealMultiHostFile(t, dir, "stateful.cell.toml", fmt.Sprintf("name = \"stateful-counter\"\nversion = \"1\"\nwasm = %q\n", filepath.ToSlash(filepath.Join("..", "..", "shared", filepath.Base(wasmPath)))))
	scriptPath := writeRealMultiHostFile(t, dir, "logic.lua", lua)
	scriptBytes, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}
	digest := sha256.Sum256(scriptBytes)
	appPath := writeRealMultiHostFile(t, dir, "pulp.app.toml", fmt.Sprintf("name = %q\nversion = \"1\"\ncells = [%q]\n[orchestrator]\nmanifest = %q\nscript = %q\nsha256 = %q\n", applicationID, filepath.Base(cellPath), filepath.Base(cellPath), filepath.Base(scriptPath), fmt.Sprintf("%x", digest)))
	return cellPath, appPath
}

func writeRealMultiHostBytes(t *testing.T, root, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func writeRealMultiHostFile(t *testing.T, root, name, content string) string {
	return writeRealMultiHostBytes(t, root, name, []byte(content))
}

var multiHostCounterWASM = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f,
	0x03, 0x02, 0x01, 0x00,
	0x06, 0x06, 0x01, 0x7f, 0x01, 0x41, 0x00, 0x0b,
	0x07, 0x07, 0x01, 0x03, 0x69, 0x6e, 0x63, 0x00, 0x00,
	0x0a, 0x0d, 0x01, 0x0b, 0x00, 0x23, 0x00, 0x41, 0x01, 0x6a, 0x24, 0x00, 0x23, 0x00, 0x0b,
}

var multiHostLifecycleWASM = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f,
	0x03, 0x04, 0x03, 0x00, 0x00, 0x00,
	0x07, 0x29, 0x03,
	0x09, 0x70, 0x75, 0x6c, 0x70, 0x5f, 0x69, 0x6e, 0x69, 0x74, 0x00, 0x00,
	0x09, 0x70, 0x75, 0x6c, 0x70, 0x5f, 0x73, 0x74, 0x65, 0x70, 0x00, 0x01,
	0x0d, 0x70, 0x75, 0x6c, 0x70, 0x5f, 0x73, 0x68, 0x75, 0x74, 0x64, 0x6f, 0x77, 0x6e, 0x00, 0x02,
	0x0a, 0x10, 0x03,
	0x04, 0x00, 0x41, 0x00, 0x0b,
	0x04, 0x00, 0x41, 0x00, 0x0b,
	0x04, 0x00, 0x41, 0x00, 0x0b,
}
