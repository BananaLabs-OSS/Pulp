package run

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

func TestApplicationRuntimeSiblingRegistryIsApplicationLocal(t *testing.T) {
	provider := &cellRuntime{spec: &manifest.CellSpec{Name: "provider", Provides: []string{"math.sum"}}}
	consumer := &cellRuntime{spec: &manifest.CellSpec{Name: "consumer", Consumes: []string{"math.sum"}, DependsOn: []string{"provider"}}}
	local := newSiblingRegistry(map[string]*cellRuntime{"provider": provider, "consumer": consumer})
	if !allowedToCall(local, "consumer", "provider", "math.sum") {
		t.Fatal("local application consumer could not call its declared provider")
	}
	if allowedToCall(local, "consumer", "provider", "math.subtract") {
		t.Fatal("consumer could call a provider not explicitly consumed")
	}

	otherProvider := &cellRuntime{spec: &manifest.CellSpec{Name: "provider", Provides: []string{"math.sum"}}}
	other := newSiblingRegistry(map[string]*cellRuntime{"provider": otherProvider})
	if allowedToCall(other, "consumer", "provider", "math.sum") {
		t.Fatal("foreign application accepted a caller absent from its local graph")
	}
	if _, err := other.callDirect(context.Background(), "consumer", "consumer", "math.sum", nil); err == nil {
		t.Fatal("foreign application unexpectedly resolved a missing caller target")
	}
}

func TestRepeatedPlacementSiblingCallsRequireExactInstanceAddress(t *testing.T) {
	providerSpec := &manifest.CellSpec{Name: "player-manager", Provides: []string{"player.tick.v1"}}
	consumerSpec := &manifest.CellSpec{Name: "game", Consumes: []string{"player.tick.v1"}, DependsOn: []string{"player-manager"}}
	registry := newSiblingRegistry(map[string]*cellRuntime{
		"player-manager@b1": {spec: providerSpec, address: "player-manager@b1"},
		"player-manager@b2": {spec: providerSpec, address: "player-manager@b2"},
		"game":              {spec: consumerSpec, address: "game"},
	})
	if !allowedToCall(registry, "game", "player-manager@b1", "player.tick.v1") {
		t.Fatal("exact repeated placement address was denied")
	}
	if allowedToCall(registry, "game", "player-manager", "player.tick.v1") {
		t.Fatal("ambiguous bare repeated placement address was accepted")
	}
	if missing := validateSiblingLinks(registry.runtimes); len(missing) != 0 {
		t.Fatalf("same-template repeated providers should remain placement-safe: %v", missing)
	}
}

func TestApplicationRuntimeRepeatedPlacementsIsolateScopeEventsStorageAndShutdown(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	builtWASM := buildLuaHarnessCell(t, filepath.Join("..", "testdata", "lua-math-engine"), "placement-shared", t.TempDir())
	wasmPath := filepath.Join(root, "shared.wasm")
	wasmBytes, err := os.ReadFile(builtWASM)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wasmPath, wasmBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	writePlacementAppFile(t, root, "worker.cell.toml", fmt.Sprintf("name = \"player-manager\"\nversion = \"1\"\nwasm = %q\n", filepath.Base(wasmPath)))
	writePlacementAppFile(t, root, "lua.cell.toml", fmt.Sprintf("name = \"lua-orchestrator\"\nversion = \"1\"\nwasm = %q\n", filepath.Base(wasmPath)))
	script := "return true"
	writePlacementAppFile(t, root, "app.lua", script)
	digest := sha256.Sum256([]byte(script))
	appPath := writePlacementAppFile(t, root, "pulp.app.toml", fmt.Sprintf(`
name = "placement-test"
version = "1"
cells = ["worker.cell.toml", "lua.cell.toml"]
[[cell_placements]]
cell = "player-manager"
instances = ["b1", "b2"]
[orchestrator]
manifest = "lua.cell.toml"
script = "app.lua"
sha256 = "%x"
`, digest))
	loadedApp, err := manifest.LoadApp(appPath)
	if err != nil {
		t.Fatalf("load repeated placement app: %v", err)
	}
	observer := &selectiveApplicationLifecycleObserver{listedApplication: loadedApp.Name}
	restoreApplicationLifecycleObserver(t, observer)
	direct, err := startDirectApplication(ctx, loadedApp, HostRuntimeOptions{})
	if err != nil {
		t.Fatalf("start direct repeated placements: %v", err)
	}
	runtime := direct.runtime
	first := runtime.runtimes["player-manager@b1"]
	second := runtime.runtimes["player-manager@b2"]
	if first == nil || second == nil || first.cell == second.cell {
		t.Fatalf("placement cells = b1:%#v b2:%#v", first, second)
	}
	if first.scope.CellInstanceID() != "b1" || second.scope.CellInstanceID() != "b2" {
		t.Fatalf("placement scopes = %q, %q", first.scope.CellInstanceID(), second.scope.CellInstanceID())
	}
	if first.eventTarget == second.eventTarget || first.eventTarget == "" || second.eventTarget == "" {
		t.Fatalf("placement event targets = %q, %q", first.eventTarget, second.eventTarget)
	}
	firstStorage, err := first.scope.ResourceKey("storage.sqlite", "player")
	if err != nil {
		t.Fatal(err)
	}
	secondStorage, err := second.scope.ResourceKey("storage.sqlite", "player")
	if err != nil {
		t.Fatal(err)
	}
	if firstStorage == secondStorage {
		t.Fatalf("placement storage keys collided: %q", firstStorage)
	}
	if _, bareExists := runtime.runtimes["player-manager"]; bareExists {
		t.Fatal("repeated package retained ambiguous bare sibling address")
	}
	if err := direct.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown repeated placements: %v", err)
	}
	wantLifecycle := []string{"start:placement-test/default:placement-test/default", "stop:placement-test/default"}
	if fmt.Sprint(observer.events) != fmt.Sprint(wantLifecycle) {
		t.Fatalf("direct application lifecycle = %v, want %v", observer.events, wantLifecycle)
	}

	// The deployment observer is trusted to select the applications whose
	// owner effects it implements. Pulp supplies the exact identity; an
	// unlisted composition remains otherwise unaffected.
	unlisted := *loadedApp
	unlisted.Name = "unlisted"
	unlistedDirect, err := startDirectApplication(ctx, &unlisted, HostRuntimeOptions{})
	if err != nil {
		t.Fatalf("start unlisted direct application: %v", err)
	}
	if err := unlistedDirect.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown unlisted direct application: %v", err)
	}
	if fmt.Sprint(observer.events) != fmt.Sprint(wantLifecycle) {
		t.Fatalf("unlisted app changed effect lifecycle: %v", observer.events)
	}

	explicit := &selectiveApplicationLifecycleObserver{listedApplication: loadedApp.Name}
	publicRuntime, err := NewDirectApplicationRuntime(appPath, DirectApplicationOptions{Lifecycle: explicit})
	if err != nil {
		t.Fatalf("new public direct runtime: %v", err)
	}
	if got := publicRuntime.Identity(); got != (ApplicationIdentity{ApplicationID: "placement-test", InstanceID: "default"}) {
		t.Fatalf("public direct identity = %s", got)
	}
	if err := publicRuntime.Start(ctx); err != nil {
		t.Fatalf("start public direct runtime: %v", err)
	}
	if err := publicRuntime.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown public direct runtime: %v", err)
	}
	if fmt.Sprint(explicit.events) != fmt.Sprint(wantLifecycle) {
		t.Fatalf("explicit public lifecycle = %v, want %v", explicit.events, wantLifecycle)
	}
	if fmt.Sprint(observer.events) != fmt.Sprint(wantLifecycle) {
		t.Fatalf("explicit public runtime mutated/invoked global lifecycle: %v", observer.events)
	}
}

type selectiveApplicationLifecycleObserver struct {
	listedApplication string
	events            []string
}

func (o *selectiveApplicationLifecycleObserver) AfterApplicationStart(context.Context, ApplicationIdentity) error {
	return fmt.Errorf("legacy lifecycle callback used instead of provider-aware callback")
}

func (o *selectiveApplicationLifecycleObserver) AfterApplicationStartWithProvider(_ context.Context, identity ApplicationIdentity, access ApplicationProviderAccess) error {
	if identity.ApplicationID == o.listedApplication {
		o.events = append(o.events, "start:"+identity.String()+":"+access.Identity().String())
	}
	return nil
}

func (o *selectiveApplicationLifecycleObserver) BeforeApplicationShutdown(_ context.Context, identity ApplicationIdentity) error {
	if identity.ApplicationID == o.listedApplication {
		o.events = append(o.events, "stop:"+identity.String())
	}
	return nil
}

func restoreApplicationLifecycleObserver(t *testing.T, observer ApplicationLifecycleObserver) {
	t.Helper()
	applicationLifecycleObservers.Lock()
	previous := applicationLifecycleObservers.observer
	applicationLifecycleObservers.observer = nil
	applicationLifecycleObservers.Unlock()
	if err := RegisterApplicationLifecycleObserver(observer); err != nil {
		t.Fatalf("register lifecycle observer: %v", err)
	}
	t.Cleanup(func() {
		applicationLifecycleObservers.Lock()
		applicationLifecycleObservers.observer = previous
		applicationLifecycleObservers.Unlock()
	})
}

func writePlacementAppFile(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestApplicationRuntimeFailureAndShutdownRemoveOnlyOwnEndpoints(t *testing.T) {
	registry := NewEndpointRegistry()
	blue := HostedApplication{Identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "blue"}}
	green := HostedApplication{Identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "green"}}
	blueScope, err := blue.NewCellScope("http", "primary")
	if err != nil {
		t.Fatal(err)
	}
	greenScope, err := green.NewCellScope("http", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Ready(ext.Endpoint{Scope: blueScope, Capability: "transport.http.inbound", Name: "public", Address: "127.0.0.1:41001"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Ready(ext.Endpoint{Scope: greenScope, Capability: "transport.http.inbound", Name: "public", Address: "127.0.0.1:41002"}); err != nil {
		t.Fatal(err)
	}

	runtime := &applicationRuntime{application: blue, config: ScopedApplicationRuntimeFactoryConfig{Endpoints: registry}, declaredUnion: map[string]bool{}, allCaps: nil}
	_ = runtime.startFailure(context.Canceled)
	if _, ok := registry.ApplicationAddress("sessions", "blue", "transport.http.inbound", "public"); ok {
		t.Fatal("failed blue application endpoint remained registered")
	}
	if address, ok := registry.ApplicationAddress("sessions", "green", "transport.http.inbound", "public"); !ok || address != "127.0.0.1:41002" {
		t.Fatalf("green endpoint changed during blue rollback: %q %t", address, ok)
	}
}

func TestApplicationRuntimeInstancesHaveDistinctStorageAndEventScopes(t *testing.T) {
	first := HostedApplication{Identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "blue"}, StorageNamespace: "sessions-storage", EventNamespace: "sessions-events"}
	second := HostedApplication{Identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "green"}, StorageNamespace: "sessions-storage", EventNamespace: "sessions-events"}
	firstScope, err := first.NewCellScope("cart", "primary")
	if err != nil {
		t.Fatal(err)
	}
	secondScope, err := second.NewCellScope("cart", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if firstScope.RoutingID() == secondScope.RoutingID() {
		t.Fatal("event scopes collide across repeated app instances")
	}
	firstKey, err := firstScope.ResourceKey("storage.sqlite", "cart")
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := secondScope.ResourceKey("storage.sqlite", "cart")
	if err != nil {
		t.Fatal(err)
	}
	if firstKey == secondKey {
		t.Fatal("storage keys collide across repeated app instances")
	}
}
