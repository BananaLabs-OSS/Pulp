package run

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/BananaLabs-OSS/Pulp/abi"
	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/vmihailenco/msgpack/v5"

	_ "github.com/BananaLabs-OSS/Pulp-ext-entropy"
	_ "github.com/BananaLabs-OSS/Pulp-ext-fs"
	_ "github.com/BananaLabs-OSS/Pulp-ext-http"
	_ "github.com/BananaLabs-OSS/Pulp-ext-sqlite"
	_ "modernc.org/sqlite"
)

type evolutionAppGeneRequest struct {
	Method string `msgpack:"method"`
	Path   string `msgpack:"path"`
}

type evolutionAppGeneResponse struct {
	Status uint32 `msgpack:"status"`
	Body   []byte `msgpack:"body"`
}

// TestEvolutionApplicationDispatchesRealLuaRoute runs the installed source
// compositions as three distinct hosted applications. The request crosses the
// actual production Evolution/Sessions boundary:
//
//	HTTP -> Evolution cell -> Evolution Lua -> pulp_app_call_v1 ->
//	Sessions Lua -> Sessions gene.
//
// It intentionally does not reassemble Sessions cells inside Evolution. The
// temporary pulp.host.toml declares Evolution's exact Resolver and Sessions
// dependencies, matching production composition.
func TestEvolutionApplicationDispatchesRealLuaRoute(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real multi-application Evolution integration test in short mode")
	}

	workspace, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	hostPath := writeEvolutionSessionsHost(t, workspace)
	applications, err := (ManifestHostLoader{}).LoadHostApplications(context.Background(), hostPath)
	if err != nil {
		t.Fatalf("load real Evolution/Sessions host: %v", err)
	}
	if len(applications) != 3 {
		t.Fatalf("host applications = %d, want 3", len(applications))
	}
	if applications[0].Identity != (ApplicationIdentity{ApplicationID: "sessions", InstanceID: "primary"}) ||
		applications[1].Identity != (ApplicationIdentity{ApplicationID: "minecraft-resolver", InstanceID: "primary"}) ||
		applications[2].Identity != (ApplicationIdentity{ApplicationID: "evolution", InstanceID: "primary"}) ||
		len(applications[1].DependsOn) != 1 || applications[1].DependsOn[0] != "sessions" ||
		len(applications[2].DependsOn) != 2 ||
		applications[2].DependsOn[0] != "minecraft-resolver" ||
		applications[2].DependsOn[1] != "sessions" {
		t.Fatalf("host dependency composition = %#v", applications)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	capabilities := evolutionAppCapabilities()
	cache := t.TempDir()
	storageRoot := t.TempDir()
	endpoints := NewEndpointRegistry()
	crossApplications := newCrossApplicationRegistry()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	started := make([]*evolutionHostedApplication, 0, len(applications))
	t.Cleanup(func() {
		for index := len(started) - 1; index >= 0; index-- {
			started[index].close(context.Background())
		}
	})
	for _, application := range applications {
		started = append(started, startEvolutionHostedApplication(
			t, ctx, workspace, cache, storageRoot, endpoints, crossApplications,
			capabilities, logger, application,
		))
	}

	var sessions, evolution *evolutionHostedApplication
	for _, application := range started {
		switch application.application.Identity.ApplicationID {
		case "sessions":
			sessions = application
		case "evolution":
			evolution = application
		}
	}
	if sessions == nil || evolution == nil || sessions.cells["control"] == nil {
		t.Fatalf("started applications do not expose Sessions control and Evolution: %#v", started)
	}
	seedEvolutionControlProjection(t, ctx, sessions.cells["control"].cell)

	assertEvolutionLuaCrossApplicationRoute(t, ctx, evolution.cells["lua-orchestrator"].cell)
	baseAddress, ok := endpoints.ApplicationAddress("evolution", "primary", "transport.http.inbound", "public")
	if !ok {
		t.Fatal("Evolution did not publish its scoped public HTTP endpoint")
	}
	startEvolutionAppHTTPPump(t, ctx, evolution.cells["evolution"].cell, capabilities["transport.http.inbound"])
	assertEvolutionAppHTTPRoute(t, "http://"+baseAddress)
	assertEvolutionAppNormalCheckoutPreflight(t, "http://"+baseAddress)
	assertEvolutionAppFleetCompositionFailClosed(t, "http://"+baseAddress)
}

// TestEvolutionApplicationMonolithCompatibility proves that the deliberate
// all-in-one descriptor remains a supported deployment shape. It uses the
// same Sessions Lua and package artifacts as split-host mode, but every call
// is a local sibling call and the Evolution cell explicitly opts into local
// legacy-owner imports.
func TestEvolutionApplicationMonolithCompatibility(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real monolithic Evolution integration test in short mode")
	}
	workspace, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	manifestPath := filepath.Join(workspace, "Evolution", "pulp-cell", "pulp.app.toml")
	composition, err := manifest.LoadApp(manifestPath)
	if err != nil {
		t.Fatalf("load monolithic Evolution application: %v", err)
	}
	evolutionSpec := composition.Cells.Lookup("evolution")
	if evolutionSpec == nil {
		t.Fatal("monolithic Evolution application does not contain the Evolution cell")
	}
	compatibility, ok := evolutionSpec.Config["legacy_owner_imports_single_app"].(bool)
	if !ok || !compatibility {
		t.Fatalf("legacy_owner_imports_single_app = %#v, want explicit true", evolutionSpec.Config["legacy_owner_imports_single_app"])
	}

	application := HostedApplication{
		Identity:         ApplicationIdentity{ApplicationID: "evolution", InstanceID: "primary"},
		ManifestPath:     manifestPath,
		StorageNamespace: "evolution-monolith",
		EventNamespace:   "evolution-monolith",
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	capabilities := evolutionAppCapabilities()
	storageRoot := t.TempDir()
	endpoints := NewEndpointRegistry()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	started := startEvolutionHostedApplication(
		t, ctx, workspace, t.TempDir(), storageRoot, endpoints, nil,
		capabilities, logger, application,
	)
	t.Cleanup(func() { started.close(context.Background()) })

	seedEvolutionControlProjection(t, ctx, started.cells["control"].cell)
	baseAddress, ok := endpoints.ApplicationAddress("evolution", "primary", "transport.http.inbound", "public")
	if !ok {
		t.Fatal("monolithic Evolution did not publish its scoped public HTTP endpoint")
	}
	startEvolutionAppHTTPPump(t, ctx, started.cells["evolution"].cell, capabilities["transport.http.inbound"])
	assertEvolutionAppHTTPRoute(t, "http://"+baseAddress)
	assertEvolutionAppNormalCheckoutPreflight(t, "http://"+baseAddress)
	assertEvolutionAppFleetCompositionFailClosed(t, "http://"+baseAddress)
}

func assertEvolutionAppFleetCompositionFailClosed(t *testing.T, baseURL string) {
	t.Helper()
	for _, test := range []struct {
		name   string
		method string
		path   string
		body   string
		status int
	}{
		{
			name: "availability uses Commerce projection", method: http.MethodGet,
			path:   "/api/voucher/composition-missing-order/availability?template=paper",
			status: http.StatusNotFound,
		},
		{
			name: "sessions requires Identity proof", method: http.MethodGet,
			path: "/api/sessions", status: http.StatusUnauthorized,
		},
		{
			name: "config requires Identity before Fleet", method: http.MethodPost,
			path: "/api/voucher/composition-missing-order/config",
			body: `{"motd":"composition check"}`, status: http.StatusUnauthorized,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(test.method, baseURL+test.path, strings.NewReader(test.body))
			if err != nil {
				t.Fatalf("build Fleet composition request: %v", err)
			}
			if test.body != "" {
				request.Header.Set("Content-Type", "application/json")
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatalf("%s %s: %v", test.method, test.path, err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read Fleet composition response: %v", err)
			}
			if response.StatusCode != test.status || !json.Valid(body) {
				t.Fatalf("%s %s = %d %s, want %d JSON", test.method, test.path, response.StatusCode, body, test.status)
			}
		})
	}
}

func assertEvolutionLuaCrossApplicationRoute(t *testing.T, ctx context.Context, lua *host.Cell) {
	t.Helper()
	request, err := msgpack.Marshal(evolutionAppGeneRequest{Method: http.MethodGet, Path: "/api/tiers"})
	if err != nil {
		t.Fatalf("marshal Evolution route request: %v", err)
	}
	dispatch, err := msgpack.Marshal(luaDispatchRequest{
		Event:   "evolution.sessions.tiers.get.v1",
		Payload: map[string]any{"request_msgpack": string(request)},
	})
	if err != nil {
		t.Fatalf("marshal Evolution Lua dispatch: %v", err)
	}
	response, err := lua.Call(ctx, "orchestrator.dispatch", dispatch)
	if err != nil {
		t.Fatalf("Evolution Lua cross-app dispatch: %v", err)
	}
	var result luaDispatchResult
	if err := msgpack.Unmarshal(response, &result); err != nil {
		t.Fatalf("decode Evolution Lua result: %v", err)
	}
	var raw []byte
	switch value := result.Value.(type) {
	case string:
		raw = []byte(value)
	case []byte:
		raw = value
	default:
		t.Fatalf("Evolution Lua response = %T, want raw MessagePack", result.Value)
	}
	var geneResponse evolutionAppGeneResponse
	if err := msgpack.Unmarshal(raw, &geneResponse); err != nil {
		t.Fatalf("decode Sessions response forwarded by Evolution Lua: %v", err)
	}
	if geneResponse.Status != http.StatusOK || !json.Valid(geneResponse.Body) || !strings.Contains(string(geneResponse.Body), "tier-compose-smoke") {
		t.Fatalf("Evolution Lua forwarded route = status %d body %s", geneResponse.Status, geneResponse.Body)
	}
}

// TestEvolutionApplicationMonolithFixtureCoversDeclaredCellsAndCapabilities
// keeps the real-app harness aligned with the production all-in-one
// composition. Every declared cell must have a concrete source and every
// declared capability must be available through its explicit test-host ABI.
func TestEvolutionApplicationMonolithFixtureCoversDeclaredCellsAndCapabilities(t *testing.T) {
	workspace, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	composition, err := manifest.LoadApp(filepath.Join(workspace, "Evolution", "pulp-cell", "pulp.app.toml"))
	if err != nil {
		t.Fatalf("load monolithic Evolution application: %v", err)
	}
	sources := evolutionApplicationCellSources(workspace)
	capabilities := evolutionAppCapabilities()
	for _, spec := range composition.Cells.Order {
		if source := sources[spec.Name]; source == "" {
			t.Errorf("monolith cell %q has no fixture source", spec.Name)
		}
		for _, name := range spec.Capabilities {
			capability, ok := capabilities[name]
			if !ok || capability.Register == nil || capability.Stub == nil {
				t.Errorf("monolith cell %q capability %q = %#v", spec.Name, name, capability)
			}
		}
	}
}

type evolutionHostedApplication struct {
	application     HostedApplication
	cells           map[string]*cellRuntime
	loaded          []*host.Cell
	capabilities    []ext.Capability
	capabilityScope ext.Scope
	cross           *crossApplicationRegistry
}

func startEvolutionHostedApplication(
	t *testing.T,
	ctx context.Context,
	workspace, cache, storageRoot string,
	endpoints *EndpointRegistry,
	cross *crossApplicationRegistry,
	capabilities map[string]ext.Capability,
	logger *slog.Logger,
	application HostedApplication,
) *evolutionHostedApplication {
	t.Helper()
	loaded, err := manifest.LoadApp(application.ManifestPath)
	if err != nil {
		t.Fatalf("load %s application: %v", application.Identity, err)
	}
	if loaded.Name != application.Identity.ApplicationID {
		t.Fatalf("host application %s loads app named %q", application.Identity, loaded.Name)
	}
	for _, spec := range loaded.Cells.Order {
		source, ok := evolutionApplicationCellSources(workspace)[spec.Name]
		if !ok {
			t.Fatalf("%s contains unexpected cell %q", application.Identity, spec.Name)
		}
		spec.WASMPath = buildLuaHarnessCell(t, source, application.Identity.ApplicationID+"-"+spec.Name, cache)
	}

	declared := map[string]bool{}
	for _, spec := range loaded.Cells.Order {
		for _, name := range spec.Capabilities {
			if _, ok := capabilities[name]; !ok {
				t.Fatalf("%s declares unavailable capability %q", application.Identity, name)
			}
			declared[name] = true
		}
	}
	capabilityScope, err := ext.NewScope(application.Identity.ApplicationID, application.Identity.InstanceID, "host", "primary")
	if err != nil {
		t.Fatalf("create %s capability scope: %v", application.Identity, err)
	}
	root := evolutionHostedStorageRoot(storageRoot, application)
	activeCapabilities := make([]ext.Capability, 0, len(declared))
	for name := range declared {
		capability := capabilities[name]
		if capability.Setup != nil {
			if err := capability.Setup(ext.SetupEnv{Scope: capabilityScope, StorageRoot: root, Endpoints: endpoints, Logger: logger}); err != nil {
				t.Fatalf("setup %s capability %q: %v", application.Identity, name, err)
			}
		}
		activeCapabilities = append(activeCapabilities, capability)
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
		t.Fatalf("%s local composition links: %v", application.Identity, missing)
	}

	harness := &evolutionHostedApplication{
		application:     application,
		cells:           cells,
		capabilities:    activeCapabilities,
		capabilityScope: capabilityScope,
		cross:           cross,
	}
	for _, spec := range loaded.Cells.Order {
		scope, err := application.NewCellScope(spec.Name, "primary")
		if err != nil {
			harness.close(context.Background())
			t.Fatalf("create %s/%s scope: %v", application.Identity, spec.Name, err)
		}
		cell, err := host.LoadScoped(ctx, spec, registry, nil, logger, scope)
		if err != nil {
			harness.close(context.Background())
			t.Fatalf("load %s/%s: %v", application.Identity, spec.Name, err)
		}
		cells[spec.Name].cell = cell
		harness.loaded = append(harness.loaded, cell)
		config, err := manifest.EncodeConfig(spec.Config)
		if err != nil {
			harness.close(context.Background())
			t.Fatalf("encode %s/%s config: %v", application.Identity, spec.Name, err)
		}
		if err := cell.Init(ctx, config); err != nil {
			harness.close(context.Background())
			t.Fatalf("init %s/%s: %v", application.Identity, spec.Name, err)
		}
	}
	// crossApplicationRegistry intentionally exposes only declared provider
	// APIs. This uses the same runtime view as `pulp -host`, without giving
	// Evolution a pointer to Sessions' cell graph.
	if cross != nil {
		if err := cross.markReady(application, &applicationRuntime{application: application, runtimes: cells}); err != nil {
			harness.close(context.Background())
			t.Fatalf("register %s providers: %v", application.Identity, err)
		}
	}
	return harness
}

func (h *evolutionHostedApplication) close(ctx context.Context) {
	if h == nil {
		return
	}
	if h.cross != nil {
		h.cross.markUnavailable(h.application.Identity)
	}
	for index := len(h.loaded) - 1; index >= 0; index-- {
		_ = h.loaded[index].Shutdown(ctx)
		_ = h.loaded[index].Close(context.Background())
	}
	for _, capability := range h.capabilities {
		if capability.TeardownScope != nil {
			_ = capability.TeardownScope(ctx, h.capabilityScope)
		}
	}
}

func writeEvolutionSessionsHost(t *testing.T, workspace string) string {
	t.Helper()
	root := t.TempDir()
	// The ordinary pulp.app.toml remains the explicit all-in-one compatibility
	// composition. Host mode has its own two-cell descriptor so this test can
	// never accidentally prove a local sibling fallback.
	resolverManifest := filepath.Join(workspace, "minecraft-resolver", "application", "pulp.app.toml")
	evolutionManifest := filepath.Join(workspace, "Evolution", "pulp-cell", "pulp.host-app.toml")
	sessionsManifest := filepath.Join(workspace, "Sessions-Gene", "application", "pulp.app.toml")
	resolverRelative, err := filepath.Rel(root, resolverManifest)
	if err != nil {
		t.Fatalf("resolve Minecraft Resolver manifest relative to host: %v", err)
	}
	evolutionRelative, err := filepath.Rel(root, evolutionManifest)
	if err != nil {
		t.Fatalf("resolve Evolution manifest relative to host: %v", err)
	}
	sessionsRelative, err := filepath.Rel(root, sessionsManifest)
	if err != nil {
		t.Fatalf("resolve Sessions manifest relative to host: %v", err)
	}
	hostPath := filepath.Join(root, "pulp.host.toml")
	content := fmt.Sprintf(`schema_version = 1
name = "evolution-sessions-real-integration"

[[applications]]
id = "sessions"
manifest = %q
aliases = ["primary"]
storage_namespace = "sessions"
event_namespace = "sessions"

[[applications]]
id = "minecraft-resolver"
manifest = %q
aliases = ["primary"]
storage_namespace = "minecraft-resolver"
event_namespace = "minecraft-resolver"
depends_on = ["sessions"]

[[applications]]
id = "evolution"
manifest = %q
aliases = ["primary"]
storage_namespace = "evolution"
event_namespace = "evolution"
depends_on = ["minecraft-resolver", "sessions"]
`, filepath.ToSlash(sessionsRelative), filepath.ToSlash(resolverRelative), filepath.ToSlash(evolutionRelative))
	if err := os.WriteFile(hostPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write temporary host manifest: %v", err)
	}
	return hostPath
}

func evolutionApplicationCellSources(workspace string) map[string]string {
	return map[string]string{
		"jvm-jre-detect":                        filepath.Join(workspace, "minecraft-resolver", "jvm-jre-detect"),
		"minecraft-resolver":                    filepath.Join(workspace, "minecraft-resolver", "pulp-cell"),
		"sessions":                              filepath.Join(workspace, "Sessions-Gene", "composition-cell"),
		"sessions-identity-retention-binding":   filepath.Join(workspace, "Sessions-Gene", "identity-retention-binding"),
		"sessions-order-config":                 filepath.Join(workspace, "Sessions-Gene", "order-config"),
		"sessions-provisioning-failure-cleanup": filepath.Join(workspace, "Sessions-Gene", "provisioning-failure-cleanup"),
		"commerce":                              filepath.Join(workspace, "Evolution", "commerce"),
		"fleet":                                 filepath.Join(workspace, "Evolution", "fleet"),
		"funding":                               filepath.Join(workspace, "Evolution", "funding"),
		"identity":                              filepath.Join(workspace, "Evolution", "identity"),
		"control":                               filepath.Join(workspace, "Evolution", "control"),
		"effects":                               filepath.Join(workspace, "Evolution", "effects"),
		"public-upload":                         filepath.Join(workspace, "Evolution", "public-upload"),
		"exact-object-upload":                   filepath.Join(workspace, "Evolution", "exact-object-upload"),
		"artifact-validator":                    filepath.Join(workspace, "Evolution", "artifact-validator"),
		"configuration-registry":                filepath.Join(workspace, "Evolution", "configuration-registry"),
		"fixed-window-counter":                  filepath.Join(workspace, "Evolution", "fixed-window-counter"),
		"workload-inventory":                    filepath.Join(workspace, "Evolution", "workload-inventory"),
		"capacity-scheduler":                    filepath.Join(workspace, "Evolution", "capacity-scheduler"),
		"workload-provisioning":                 filepath.Join(workspace, "Evolution", "workload-provisioning"),
		"runtime-control":                       filepath.Join(workspace, "Evolution", "runtime-control"),
		"artifact-lifecycle":                    filepath.Join(workspace, "Evolution", "artifact-lifecycle"),
		"archive-lifecycle":                     filepath.Join(workspace, "Evolution", "archive-lifecycle"),
		"observation-registry":                  filepath.Join(workspace, "Evolution", "observation-registry"),
		"notification-outbox":                   filepath.Join(workspace, "Pulp-engines", "notification-outbox-sqlite-cell", "cmd", "notification-outbox"),
		"minecraft-profile-resolver":            filepath.Join(workspace, "Evolution", "minecraft-profile-resolver"),
		"lua-orchestrator":                      filepath.Join(workspace, "Pulp-Lua", "pulp-cell"),
		"evolution":                             filepath.Join(workspace, "Evolution", "pulp-cell"),
	}
}

func evolutionHostedStorageRoot(root string, application HostedApplication) string {
	return filepath.Join(root, application.StorageNamespace, application.Identity.InstanceID)
}

func startEvolutionAppHTTPPump(t *testing.T, ctx context.Context, cell *host.Cell, capability ext.Capability) {
	t.Helper()
	if cell == nil || capability.Poll == nil {
		t.Fatal("Evolution HTTP transport is unavailable")
	}
	pumpCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
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
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Evolution HTTP pump did not stop")
		}
	})
}

func assertEvolutionAppHTTPRoute(t *testing.T, baseURL string) {
	t.Helper()
	response, err := (&http.Client{Timeout: 10 * time.Second}).Get(baseURL + "/api/tiers")
	if err != nil {
		t.Fatalf("GET Evolution -> Sessions route: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read Evolution -> Sessions route: %v", err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "tier-compose-smoke") {
		t.Fatalf("GET /api/tiers through Evolution -> Sessions = %d %s", response.StatusCode, body)
	}
}

func assertEvolutionAppNormalCheckoutPreflight(t *testing.T, baseURL string) {
	t.Helper()
	request, err := http.NewRequest(
		http.MethodPost,
		baseURL+"/api/checkout",
		bytes.NewReader([]byte(`{"age_confirmed":false}`)),
	)
	if err != nil {
		t.Fatalf("build normal checkout request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("POST Evolution -> Sessions normal checkout: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read normal checkout response: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest ||
		!strings.Contains(string(body), "Age confirmation required") {
		t.Fatalf("POST /api/checkout through Evolution -> Lua saga -> Sessions = %d %s", response.StatusCode, body)
	}
}

func seedEvolutionControlProjection(t *testing.T, ctx context.Context, control *host.Cell) {
	t.Helper()
	if control == nil {
		t.Fatal("Sessions Control cell is unavailable")
	}
	createdAt := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	request, err := msgpack.Marshal(map[string]any{
		"version":   "sessions.control/v1",
		"import_id": "pulp-evolution-app-e2e-tier-v1",
		"source":    "pulp-run-integration/v1",
		"legacy_projection": map[string]any{
			"version": "sessions.control/v1",
			"games": []any{map[string]any{
				"id": "minecraft", "slug": "minecraft", "name": "Minecraft", "enabled": true,
			}},
			"tiers": []any{map[string]any{
				"id": "tier-compose-smoke", "game_id": "minecraft", "name": "compose-smoke",
				"label": "Composition Smoke", "price_cents": int64(1200), "currency": "usd",
				"duration": "24h", "extend_instant_pct": 75, "extend_queued_pct": 50,
				"max_cpu": 1.0, "max_ram_mb": 1024, "enabled": true, "sort_order": 1,
				"created_at": createdAt,
			}},
		},
	})
	if err != nil {
		t.Fatalf("encode Control projection import: %v", err)
	}
	if _, err := control.Call(ctx, "control.v1.import_legacy_projection", request); err != nil {
		t.Fatalf("import Control tier projection: %v", err)
	}
}

// seedEvolutionAppTier is retained for the independent Sessions bundle test,
// which owns its own staged-fixture migration. Evolution application E2Es use
// seedEvolutionControlProjection above because Control is now authoritative.
func seedEvolutionAppTier(t *testing.T, databasePath string) {
	t.Helper()
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open Sessions database: %v", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	const schema = `
CREATE TABLE IF NOT EXISTS tiers (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	label TEXT NOT NULL,
	price_cents INTEGER NOT NULL DEFAULT 0,
	duration TEXT NOT NULL,
	extend_instant_pct INTEGER NOT NULL DEFAULT 75,
	extend_queued_pct INTEGER NOT NULL DEFAULT 50,
	max_cpu REAL NOT NULL DEFAULT 0,
	max_ram_mb INTEGER NOT NULL DEFAULT 0,
	enabled BOOLEAN NOT NULL DEFAULT TRUE,
	sort_order INTEGER NOT NULL DEFAULT 0,
	created_at TIMESTAMP NOT NULL
);
INSERT INTO tiers (id, name, label, price_cents, duration, max_cpu, max_ram_mb, enabled, sort_order, created_at)
VALUES ('tier-compose-smoke', 'compose-smoke', 'Composition Smoke', 1200, '24h', 1.0, 1024, TRUE, 1, CURRENT_TIMESTAMP);`
	if _, err := database.Exec(schema); err != nil {
		t.Fatalf("seed Sessions tier: %v", err)
	}
}

func evolutionAppCapabilities() map[string]ext.Capability {
	capabilities := map[string]ext.Capability{}
	for _, capability := range ext.All() {
		capabilities[capability.Name] = capability
	}
	for _, capability := range evolutionAppBackendStubs() {
		capabilities[capability.Name] = capability
	}
	return capabilities
}

func evolutionAppBackendStubs() []ext.Capability {
	unavailable4 := func(_ context.Context, _ api.Module, _, _, _, _ uint32) uint32 { return 4 }
	unavailable2 := func(_ context.Context, _ api.Module, _, _ uint32) uint32 { return 4 }
	stripe := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		for _, name := range []string{"stripe_checkout_session_create", "stripe_checkout_session_get", "stripe_payment_intent_create", "stripe_payment_intent_get", "stripe_payment_intent_capture", "stripe_payment_intent_cancel", "stripe_refund_create", "stripe_customer_create", "stripe_setup_intent_create", "stripe_setup_intent_get", "stripe_invoice_create", "stripe_invoice_finalize", "stripe_invoice_mark_paid_out_of_band", "stripe_invoice_item_create", "stripe_balance_get", "stripe_coupon_create", "stripe_promotion_code_create", "stripe_promotion_code_lookup", "stripe_promotion_code_update"} {
			builder.NewFunctionBuilder().WithFunc(unavailable4).Export(name)
		}
		builder.NewFunctionBuilder().WithFunc(unavailable2).Export("stripe_webhook_verify")
		return nil
	}
	// Effects uses a separate, one-operation Stripe runtime ABI. Do not reuse
	// the broad payment.stripe fixture because that would conceal a capability
	// boundary drift in the production composition.
	stripeEffectRuntime := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		builder.NewFunctionBuilder().WithFunc(unavailable4).Export("stripe_effect_execute")
		return nil
	}
	s3 := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		for _, name := range []string{"s3_presign", "s3_presign_put", "s3_head", "s3_get", "s3_list", "s3_put_multipart_init", "s3_put_multipart_part"} {
			builder.NewFunctionBuilder().WithFunc(unavailable4).Export(name)
		}
		for _, name := range []string{"s3_put", "s3_copy", "s3_delete", "s3_put_multipart_complete", "s3_put_multipart_abort"} {
			builder.NewFunctionBuilder().WithFunc(unavailable2).Export(name)
		}
		return nil
	}
	docker := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		for _, name := range []string{"docker_exec", "docker_files_read"} {
			builder.NewFunctionBuilder().WithFunc(unavailable4).Export(name)
		}
		for _, name := range []string{"docker_files_write", "docker_restart"} {
			builder.NewFunctionBuilder().WithFunc(unavailable2).Export(name)
		}
		return nil
	}
	workers := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		submit := func(_ context.Context, _ api.Module, _, _ uint32) uint32 { return 0 }
		result := func(_ context.Context, _ api.Module, _, _, _ uint32) uint32 { return 0 }
		builder.NewFunctionBuilder().WithFunc(submit).Export("workers_submit")
		builder.NewFunctionBuilder().WithFunc(submit).Export("workers_submit_fire")
		builder.NewFunctionBuilder().WithFunc(result).Export("workers_result")
		return nil
	}
	// The public-upload owner itself is SQLite-only. Its companion adapters
	// declare three distinct exact-object capabilities; expose only their real
	// narrow import names here, never generic storage.s3. The unavailable result
	// is intentional in this route/composition harness.
	exactObjectPublicUpload := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		for _, name := range []string{"s3_exact_object_presign_put", "s3_exact_object_validate_put", "s3_exact_object_delete"} {
			builder.NewFunctionBuilder().WithFunc(unavailable4).Export(name)
		}
		return nil
	}
	exactObjectDownloadReference := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		builder.NewFunctionBuilder().WithFunc(unavailable4).Export("s3_exact_object_download_reference")
		return nil
	}
	artifactValidation := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		builder.NewFunctionBuilder().WithFunc(unavailable4).Export("s3_exact_object_validate_artifact_zip")
		return nil
	}
	// Template reload is an exact host import, not the former opaque HTTP
	// proxy. The composition harness exposes only the ABI-identical unavailable
	// stub; route-specific tests provide the fake host implementation.
	templateReload := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		builder.NewFunctionBuilder().WithFunc(unavailable4).Export("bananagine_template_reload_execute_v1")
		return nil
	}
	pfcPlanPrepare := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		builder.NewFunctionBuilder().WithFunc(unavailable4).Export("provisioning_failure_cleanup_plan_prepare_v1")
		return nil
	}
	pfcExecute := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		builder.NewFunctionBuilder().WithFunc(unavailable4).Export("provisioning_failure_cleanup_execute_v1")
		return nil
	}
	fleetRuntime := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		execute := func(ctx context.Context, module api.Module, requestPtr, requestLen, responsePtrOut, responseLenOut uint32) uint32 {
			if module == nil || module.Memory() == nil {
				return 2
			}
			request, ok := module.Memory().Read(requestPtr, requestLen)
			if !ok {
				return 2
			}
			var intent evolutionAppFleetRuntimeIntent
			if err := msgpack.Unmarshal(request, &intent); err != nil {
				return 3
			}
			result, ok := evolutionAppFleetRuntimeResult(intent)
			if !ok {
				return 4
			}
			receipt := evolutionAppFleetRuntimeReceipt{
				Version:        "pulp.effect.v1",
				IntentID:       intent.ID,
				Kind:           intent.Kind,
				IdempotencyKey: intent.IdempotencyKey,
				Status:         "completed",
				Result:         result,
			}
			return writeEvolutionAppMsgpack(ctx, module, receipt, responsePtrOut, responseLenOut)
		}
		builder.NewFunctionBuilder().WithFunc(execute).Export("fleet_effect_execute")
		return nil
	}
	// Fleet observations are a separate, deliberately narrower host surface
	// from lifecycle effects. Keep the test host ABI-identical to the declared
	// capability so application loading cannot silently fall back to a broader
	// runtime/Docker capability.
	fleetObservation := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		execute := func(ctx context.Context, module api.Module, requestPtr, requestLen, responsePtrOut, responseLenOut uint32) uint32 {
			if module == nil || module.Memory() == nil {
				return 2
			}
			request, ok := module.Memory().Read(requestPtr, requestLen)
			if !ok {
				return 2
			}
			envelope, observation, ok := evolutionAppFleetObservationRequest(request)
			if !ok {
				return 3
			}
			data, ok := evolutionAppFleetObservationData(observation.Field)
			if !ok {
				return 4
			}
			result, err := msgpack.Marshal(struct {
				Contract       string `msgpack:"contract"`
				ServerID       string `msgpack:"server_id"`
				NodeID         string `msgpack:"node_id"`
				ContainerID    string `msgpack:"container_id"`
				Field          string `msgpack:"field"`
				Generation     string `msgpack:"generation"`
				SourceRevision string `msgpack:"source_revision"`
				ObservedAt     string `msgpack:"observed_at"`
				Data           any    `msgpack:"data"`
			}{observation.Contract, observation.ServerID, observation.NodeID, observation.ContainerID, observation.Field, observation.Generation, observation.SourceRevision, "2026-07-26T12:00:00Z", data})
			if err != nil {
				return 5
			}
			receipt := evolutionAppFleetRuntimeReceipt{Version: envelope.Version, IntentID: envelope.ID, Kind: envelope.Kind, IdempotencyKey: envelope.IdempotencyKey, Status: "completed", Result: result}
			return writeEvolutionAppMsgpack(ctx, module, receipt, responsePtrOut, responseLenOut)
		}
		builder.NewFunctionBuilder().WithFunc(execute).Export("fleet_observation_execute")
		return nil
	}
	statusSignal := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		publish := func(ctx context.Context, module api.Module, requestPtr, requestLen, responsePtrOut, responseLenOut uint32) uint32 {
			if module == nil || module.Memory() == nil {
				return 2
			}
			request, ok := module.Memory().Read(requestPtr, requestLen)
			if !ok {
				return 2
			}
			receipt, ok := evolutionAppStatusSignalReceipt(request)
			if !ok {
				return 4
			}
			return writeEvolutionAppMsgpack(ctx, module, receipt, responsePtrOut, responseLenOut)
		}
		builder.NewFunctionBuilder().WithFunc(publish).Export("status_signal_publish")
		return nil
	}
	// Test-only host fixture for the exact authenticated service-observation
	// ABI. It accepts only Fiber's bounded opaque-definition command and
	// returns the canonical typed receipt; it owns no URL, credentials, or
	// generic transport surface.
	serviceObservation := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		execute := func(ctx context.Context, module api.Module, requestPtr, requestLen, responsePtrOut, responseLenOut uint32) uint32 {
			if module == nil || module.Memory() == nil {
				return 2
			}
			request, ok := module.Memory().Read(requestPtr, requestLen)
			if !ok {
				return 2
			}
			receipt, ok := evolutionAppServiceObservationReceipt(request)
			if !ok {
				return 4
			}
			return writeEvolutionAppMsgpack(ctx, module, receipt, responsePtrOut, responseLenOut)
		}
		builder.NewFunctionBuilder().WithFunc(execute).Export("service_observation_execute")
		return nil
	}
	// Test-only host fixture for Fiber's exact fenced HTTP-probe ABI. It
	// accepts no URL, method, headers, timeout, or body configuration from the
	// guest and returns deterministic bounded evidence. Production hosts use
	// the separately configured Pulp-ext-http capability.
	httpProbe := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		execute := func(ctx context.Context, module api.Module, requestPtr, requestLen, responsePtrOut, responseLenOut uint32) uint32 {
			if module == nil || module.Memory() == nil {
				return 2
			}
			request, ok := module.Memory().Read(requestPtr, requestLen)
			if !ok {
				return 2
			}
			receipt, ok := evolutionAppHTTPProbeReceiptFor(request)
			if !ok {
				return 4
			}
			return writeEvolutionAppMsgpack(ctx, module, receipt, responsePtrOut, responseLenOut)
		}
		builder.NewFunctionBuilder().WithFunc(execute).Export("http_probe_execute")
		return nil
	}
	// The server-mutation bridge is a closed deployment-owned host ABI. The
	// composition fixture exposes its exact import shape but deliberately
	// returns unavailable: route tests must not acquire mutation authority.
	serverMutation := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		builder.NewFunctionBuilder().WithFunc(
			func(context.Context, api.Module, uint32, uint32, uint32, uint32) uint32 { return 99 },
		).Export("server_mutation_execute_v4")
		return nil
	}
	capacityObservation := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		builder.NewFunctionBuilder().WithFunc(unavailable4).Export("capacity_observation_execute")
		return nil
	}
	return []ext.Capability{
		{Name: "payment.stripe", Register: stripe, Stub: stripe},
		{Name: "effect.stripe.runtime", Register: stripeEffectRuntime, Stub: stripeEffectRuntime},
		{Name: "storage.s3", Register: s3, Stub: s3},
		{Name: "spawn.docker", Register: docker, Stub: docker},
		{Name: "workers", Register: workers, Stub: workers},
		{Name: "storage.s3.public-upload.v1", Register: exactObjectPublicUpload, Stub: exactObjectPublicUpload},
		{Name: "storage.s3.exact-object-download-reference.v1", Register: exactObjectDownloadReference, Stub: exactObjectDownloadReference},
		{Name: "storage.s3.artifact-validation.v1", Register: artifactValidation, Stub: artifactValidation},
		{Name: "bananagine.template.reload.v1", Register: templateReload, Stub: templateReload},
		{Name: "sessions.provisioning-failure-cleanup.plan-prepare.v1", Register: pfcPlanPrepare, Stub: pfcPlanPrepare},
		{Name: "sessions.provisioning-failure-cleanup.execute.v1", Register: pfcExecute, Stub: pfcExecute},
		{Name: "effect.fleet.runtime", Register: fleetRuntime, Stub: fleetRuntime},
		{Name: "effect.fleet.observation", Register: fleetObservation, Stub: fleetObservation},
		{Name: "effect.capacity.observation", Register: capacityObservation, Stub: capacityObservation},
		{Name: "effect.status.signal", Register: statusSignal, Stub: statusSignal},
		{Name: "effect.service.observation", Register: serviceObservation, Stub: serviceObservation},
		{Name: "effect.http.probe.v1", Register: httpProbe, Stub: httpProbe},
		{Name: "effect.server-mutation.v4", Register: serverMutation, Stub: serverMutation},
	}
}

type evolutionAppFleetRuntimeIntent struct {
	Version        string             `msgpack:"version"`
	ID             string             `msgpack:"id"`
	Kind           string             `msgpack:"kind"`
	IdempotencyKey string             `msgpack:"idempotency_key"`
	Payload        msgpack.RawMessage `msgpack:"payload"`
}

type evolutionAppFleetRuntimeReceipt struct {
	Version        string             `msgpack:"version"`
	IntentID       string             `msgpack:"intent_id"`
	Kind           string             `msgpack:"kind"`
	IdempotencyKey string             `msgpack:"idempotency_key"`
	Status         string             `msgpack:"status"`
	Result         msgpack.RawMessage `msgpack:"result"`
}

type evolutionAppFleetObservationIntent struct {
	Contract       string `msgpack:"contract"`
	ServerID       string `msgpack:"server_id"`
	NodeID         string `msgpack:"node_id"`
	ContainerID    string `msgpack:"container_id"`
	Field          string `msgpack:"field"`
	Generation     string `msgpack:"generation"`
	SourceRevision string `msgpack:"source_revision"`
}

func evolutionAppFleetObservationRequest(raw []byte) (evolutionAppFleetRuntimeIntent, evolutionAppFleetObservationIntent, bool) {
	var envelopeFields map[string]msgpack.RawMessage
	if err := msgpack.Unmarshal(raw, &envelopeFields); err != nil || len(envelopeFields) != 5 {
		return evolutionAppFleetRuntimeIntent{}, evolutionAppFleetObservationIntent{}, false
	}
	for name := range envelopeFields {
		switch name {
		case "version", "id", "kind", "idempotency_key", "payload":
		default:
			return evolutionAppFleetRuntimeIntent{}, evolutionAppFleetObservationIntent{}, false
		}
	}
	var envelope evolutionAppFleetRuntimeIntent
	if err := msgpack.Unmarshal(raw, &envelope); err != nil || envelope.Version != "pulp.effect.v1" ||
		envelope.Kind != "pulp.effect.fleet.runtime-observation.execute.v1" ||
		!evolutionAppCanonicalEffectField(envelope.ID) || !evolutionAppCanonicalEffectField(envelope.IdempotencyKey) {
		return evolutionAppFleetRuntimeIntent{}, evolutionAppFleetObservationIntent{}, false
	}
	var payloadFields map[string]msgpack.RawMessage
	if err := msgpack.Unmarshal(envelope.Payload, &payloadFields); err != nil || len(payloadFields) != 7 {
		return evolutionAppFleetRuntimeIntent{}, evolutionAppFleetObservationIntent{}, false
	}
	for name := range payloadFields {
		switch name {
		case "contract", "server_id", "node_id", "container_id", "field", "generation", "source_revision":
		default:
			return evolutionAppFleetRuntimeIntent{}, evolutionAppFleetObservationIntent{}, false
		}
	}
	var observation evolutionAppFleetObservationIntent
	if err := msgpack.Unmarshal(envelope.Payload, &observation); err != nil || observation.Contract != "fleet.live-observation.v1" ||
		!evolutionAppCanonicalEffectField(observation.ServerID) || !evolutionAppCanonicalEffectField(observation.NodeID) ||
		!evolutionAppCanonicalEffectField(observation.ContainerID) || !evolutionAppCanonicalEffectField(observation.Generation) ||
		observation.SourceRevision != observation.Generation {
		return evolutionAppFleetRuntimeIntent{}, evolutionAppFleetObservationIntent{}, false
	}
	return envelope, observation, true
}

func evolutionAppFleetObservationData(field string) (map[string]any, bool) {
	switch field {
	case "settings":
		return map[string]any{"settings": map[string]string{}}, true
	case "gamerules":
		return map[string]any{"gamerules": map[string]string{}}, true
	case "player_history":
		return map[string]any{"player_history": []any{}}, true
	case "access_snapshot":
		return map[string]any{"access_snapshot": map[string]any{"whitelist": []any{}, "operators": []any{}, "bans": []any{}}}, true
	case "artifacts":
		return map[string]any{"artifacts": []any{}}, true
	default:
		return nil, false
	}
}

type evolutionAppStatusSignalPayload struct {
	Target        string `msgpack:"target"`
	Signal        string `msgpack:"signal"`
	Detail        string `msgpack:"detail"`
	ExpiresAtUnix int64  `msgpack:"expires_at_unix"`
}

type evolutionAppStatusSignalResult struct {
	Target        string `msgpack:"target"`
	Signal        string `msgpack:"signal"`
	ExpiresAtUnix int64  `msgpack:"expires_at_unix"`
}

type evolutionAppServiceObservationFence struct {
	LeaseID        string `msgpack:"lease_id"`
	Attempt        uint32 `msgpack:"attempt"`
	LeaseExpiresAt string `msgpack:"lease_expires_at"`
}

type evolutionAppServiceObservationCommand struct {
	Contract            string                              `msgpack:"contract"`
	ServiceDefinitionID string                              `msgpack:"service_definition_id"`
	CommandID           string                              `msgpack:"command_id"`
	IdempotencyKey      string                              `msgpack:"idempotency_key"`
	Fence               evolutionAppServiceObservationFence `msgpack:"fence"`
	ObservedAt          string                              `msgpack:"observed_at"`
}

type evolutionAppServiceObservationValue struct {
	Status     string `msgpack:"status"`
	Message    string `msgpack:"message,omitempty"`
	HTTPStatus uint16 `msgpack:"http_status,omitempty"`
	Evidence   string `msgpack:"evidence"`
	ObservedAt string `msgpack:"observed_at"`
}

type evolutionAppServiceObservationResult struct {
	Contract            string                              `msgpack:"contract"`
	ServiceDefinitionID string                              `msgpack:"service_definition_id"`
	CommandID           string                              `msgpack:"command_id"`
	IdempotencyKey      string                              `msgpack:"idempotency_key"`
	Fence               evolutionAppServiceObservationFence `msgpack:"fence"`
	Observation         evolutionAppServiceObservationValue `msgpack:"observation"`
}

type evolutionAppHTTPProbeIntent struct {
	Version        string `msgpack:"version"`
	IntentID       string `msgpack:"intent_id"`
	IdempotencyKey string `msgpack:"idempotency_key"`
	Fence          string `msgpack:"fence"`
	Destination    string `msgpack:"destination"`
}

type evolutionAppHTTPProbeReceipt struct {
	Version        string `msgpack:"version"`
	IntentID       string `msgpack:"intent_id"`
	IdempotencyKey string `msgpack:"idempotency_key"`
	Fence          string `msgpack:"fence"`
	Destination    string `msgpack:"destination"`
	Transport      string `msgpack:"transport"`
	HTTPStatus     uint16 `msgpack:"http_status"`
	BodyBytes      uint32 `msgpack:"body_bytes"`
	BodySHA256     string `msgpack:"body_sha256,omitempty"`
}

func evolutionAppFleetRuntimeResult(intent evolutionAppFleetRuntimeIntent) (msgpack.RawMessage, bool) {
	if intent.Version != "pulp.effect.v1" || strings.TrimSpace(intent.ID) == "" ||
		strings.TrimSpace(intent.IdempotencyKey) == "" {
		return nil, false
	}
	var fields map[string]msgpack.RawMessage
	if err := msgpack.Unmarshal(intent.Payload, &fields); err != nil {
		return nil, false
	}
	allowed := map[string]bool{"server_id": true, "node_id": true, "container_id": true, "reason": true}
	status := "deprovisioned"
	switch intent.Kind {
	case "pulp.effect.fleet.server.deprovision.v1":
	case "pulp.effect.fleet.extension.apply.v1":
		allowed["extension"] = true
		allowed["rcon_action"] = true
		status = "save_flushed"
	default:
		return nil, false
	}
	values := make(map[string]string, len(fields))
	for name, raw := range fields {
		if !allowed[name] {
			return nil, false
		}
		var value string
		if err := msgpack.Unmarshal(raw, &value); err != nil {
			return nil, false
		}
		values[name] = value
	}
	for _, name := range []string{"server_id", "node_id", "container_id"} {
		if value := values[name]; strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return nil, false
		}
	}
	if intent.Kind == "pulp.effect.fleet.extension.apply.v1" &&
		(values["extension"] != "rcon" || values["rcon_action"] != "save_flush") {
		return nil, false
	}
	result, err := msgpack.Marshal(map[string]string{
		"server_id": values["server_id"], "node_id": values["node_id"], "container_id": values["container_id"], "status": status,
	})
	if err != nil {
		return nil, false
	}
	return result, true
}

func evolutionAppStatusSignalReceipt(request []byte) (evolutionAppFleetRuntimeReceipt, bool) {
	var intent evolutionAppFleetRuntimeIntent
	decoder := msgpack.NewDecoder(bytes.NewReader(request))
	decoder.DisallowUnknownFields(true)
	if err := decoder.Decode(&intent); err != nil ||
		intent.Version != "pulp.effect.v1" ||
		intent.Kind != "pulp.effect.status.signal.publish.v1" ||
		!evolutionAppCanonicalEffectField(intent.ID) ||
		!evolutionAppCanonicalEffectField(intent.IdempotencyKey) {
		return evolutionAppFleetRuntimeReceipt{}, false
	}
	var payload evolutionAppStatusSignalPayload
	decoder = msgpack.NewDecoder(bytes.NewReader(intent.Payload))
	decoder.DisallowUnknownFields(true)
	if err := decoder.Decode(&payload); err != nil ||
		!evolutionAppStatusTarget(payload.Target) ||
		!evolutionAppStatusState(payload.Signal) ||
		strings.TrimSpace(payload.Detail) != payload.Detail ||
		payload.Detail == "" ||
		len(payload.Detail) > 512 ||
		!evolutionAppNoControlCharacters(payload.Detail) ||
		payload.ExpiresAtUnix <= 0 ||
		time.Unix(payload.ExpiresAtUnix, 0).UTC().Year() > 9999 {
		return evolutionAppFleetRuntimeReceipt{}, false
	}
	result, err := msgpack.Marshal(evolutionAppStatusSignalResult{
		Target: payload.Target, Signal: payload.Signal, ExpiresAtUnix: payload.ExpiresAtUnix,
	})
	if err != nil {
		return evolutionAppFleetRuntimeReceipt{}, false
	}
	return evolutionAppFleetRuntimeReceipt{
		Version:        intent.Version,
		IntentID:       intent.ID,
		Kind:           intent.Kind,
		IdempotencyKey: intent.IdempotencyKey,
		Status:         "completed",
		Result:         result,
	}, true
}

func evolutionAppServiceObservationReceipt(request []byte) (evolutionAppFleetRuntimeReceipt, bool) {
	var intent evolutionAppFleetRuntimeIntent
	reader := bytes.NewReader(request)
	decoder := msgpack.NewDecoder(reader)
	decoder.DisallowUnknownFields(true)
	if err := decoder.Decode(&intent); err != nil || reader.Len() != 0 ||
		intent.Version != "pulp.effect.v1" ||
		intent.Kind != "pulp.effect.service-observation.execute.v1" ||
		!evolutionAppCanonicalEffectField(intent.ID) ||
		!evolutionAppCanonicalEffectField(intent.IdempotencyKey) {
		return evolutionAppFleetRuntimeReceipt{}, false
	}
	var command evolutionAppServiceObservationCommand
	reader = bytes.NewReader(intent.Payload)
	decoder = msgpack.NewDecoder(reader)
	decoder.DisallowUnknownFields(true)
	if err := decoder.Decode(&command); err != nil || reader.Len() != 0 ||
		command.Contract != "service-observation.v1" ||
		!evolutionAppCanonicalEffectField(command.ServiceDefinitionID) ||
		!evolutionAppCanonicalEffectField(command.CommandID) ||
		!evolutionAppCanonicalEffectField(command.IdempotencyKey) ||
		!evolutionAppCanonicalEffectField(command.Fence.LeaseID) ||
		command.Fence.Attempt == 0 ||
		command.CommandID != intent.ID ||
		command.IdempotencyKey != intent.IdempotencyKey {
		return evolutionAppFleetRuntimeReceipt{}, false
	}
	observedAt, observedErr := time.Parse(time.RFC3339Nano, command.ObservedAt)
	expiresAt, expiresErr := time.Parse(time.RFC3339Nano, command.Fence.LeaseExpiresAt)
	if observedErr != nil || expiresErr != nil || !observedAt.Before(expiresAt) {
		return evolutionAppFleetRuntimeReceipt{}, false
	}
	result := evolutionAppServiceObservationResult{
		Contract:            command.Contract,
		ServiceDefinitionID: command.ServiceDefinitionID,
		CommandID:           command.CommandID,
		IdempotencyKey:      command.IdempotencyKey,
		Fence:               command.Fence,
		Observation: evolutionAppServiceObservationValue{
			Status:     "operational",
			HTTPStatus: http.StatusOK,
			Evidence:   "authenticated",
			ObservedAt: command.ObservedAt,
		},
	}
	resultWire, err := msgpack.Marshal(result)
	if err != nil {
		return evolutionAppFleetRuntimeReceipt{}, false
	}
	return evolutionAppFleetRuntimeReceipt{
		Version: intent.Version, IntentID: intent.ID, Kind: intent.Kind,
		IdempotencyKey: intent.IdempotencyKey, Status: "completed", Result: resultWire,
	}, true
}

func evolutionAppHTTPProbeReceiptFor(request []byte) (evolutionAppHTTPProbeReceipt, bool) {
	var intent evolutionAppHTTPProbeIntent
	reader := bytes.NewReader(request)
	decoder := msgpack.NewDecoder(reader)
	decoder.DisallowUnknownFields(true)
	if err := decoder.Decode(&intent); err != nil || reader.Len() != 0 ||
		intent.Version != "http-probe.v1" ||
		!evolutionAppCanonicalEffectField(intent.IntentID) ||
		!evolutionAppCanonicalEffectField(intent.IdempotencyKey) ||
		!evolutionAppCanonicalEffectField(intent.Fence) ||
		!evolutionAppHTTPProbeDestination(intent.Destination) {
		return evolutionAppHTTPProbeReceipt{}, false
	}
	bodyDigest := sha256.Sum256(nil)
	return evolutionAppHTTPProbeReceipt{
		Version: intent.Version, IntentID: intent.IntentID,
		IdempotencyKey: intent.IdempotencyKey, Fence: intent.Fence,
		Destination: intent.Destination, Transport: "observed",
		HTTPStatus: http.StatusNoContent, BodySHA256: hex.EncodeToString(bodyDigest[:]),
	}, true
}

func evolutionAppHTTPProbeDestination(value string) bool {
	return value == "status.website.6227748c2fbaff8f"
}

func evolutionAppCanonicalEffectField(value string) bool {
	return value != "" && len(value) <= 256 && strings.TrimSpace(value) == value && evolutionAppNoControlCharacters(value)
}

func evolutionAppNoControlCharacters(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func evolutionAppStatusTarget(target string) bool {
	switch target {
	case "payments", "provisioner", "email":
		return true
	default:
		return false
	}
}

func evolutionAppStatusState(state string) bool {
	switch state {
	case "ok", "degraded", "down":
		return true
	default:
		return false
	}
}

func writeEvolutionAppMsgpack(ctx context.Context, module api.Module, value any, ptrOut, sizeOut uint32) uint32 {
	encoded, err := msgpack.Marshal(value)
	if err != nil {
		return 5
	}
	allocator := module.ExportedFunction("pulp_alloc")
	if allocator == nil {
		return 7
	}
	result, err := allocator.Call(ctx, uint64(len(encoded)))
	if err != nil || len(result) == 0 || result[0] == 0 {
		return 7
	}
	ptr := uint32(result[0])
	if !module.Memory().Write(ptr, encoded) ||
		!module.Memory().WriteUint32Le(ptrOut, ptr) ||
		!module.Memory().WriteUint32Le(sizeOut, uint32(len(encoded))) {
		return 8
	}
	return 0
}

func TestEvolutionAppFleetRuntimeCapabilityIsNarrow(t *testing.T) {
	payload, err := msgpack.Marshal(map[string]string{
		"server_id": "server-1", "node_id": "node-1", "container_id": "container-1",
		"extension": "rcon", "rcon_action": "save_flush",
	})
	if err != nil {
		t.Fatal(err)
	}
	valid := evolutionAppFleetRuntimeIntent{
		Version: "pulp.effect.v1", ID: "intent-1", Kind: "pulp.effect.fleet.extension.apply.v1",
		IdempotencyKey: "intent-1:effect", Payload: payload,
	}
	if _, ok := evolutionAppFleetRuntimeResult(valid); !ok {
		t.Fatal("allowed Fleet save-flush intent was rejected")
	}
	valid.Kind = "pulp.effect.fleet.extension.apply.v2"
	if _, ok := evolutionAppFleetRuntimeResult(valid); ok {
		t.Fatal("unsupported Fleet effect kind was accepted")
	}
	valid.Kind = "pulp.effect.fleet.extension.apply.v1"
	invalidPayload, err := msgpack.Marshal(map[string]string{
		"server_id": "server-1", "node_id": "node-1", "container_id": "container-1",
		"extension": "rcon", "rcon_action": "restart",
	})
	if err != nil {
		t.Fatal(err)
	}
	valid.Payload = invalidPayload
	if _, ok := evolutionAppFleetRuntimeResult(valid); ok {
		t.Fatal("non-save-flush Fleet extension was accepted")
	}
}

func TestEvolutionAppFleetObservationCapabilityUsesExactABI(t *testing.T) {
	capability, ok := evolutionAppCapabilities()["effect.fleet.observation"]
	if !ok || capability.Register == nil || capability.Stub == nil {
		t.Fatalf("effect.fleet.observation capability = %#v", capability)
	}
	payload, err := msgpack.Marshal(evolutionAppFleetObservationIntent{
		Contract: "fleet.live-observation.v1", ServerID: "server-1", NodeID: "node-1", ContainerID: "container-1",
		Field: "settings", Generation: "revision-1", SourceRevision: "revision-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := msgpack.Marshal(evolutionAppFleetRuntimeIntent{
		Version: "pulp.effect.v1", ID: "observation-1", Kind: "pulp.effect.fleet.runtime-observation.execute.v1",
		IdempotencyKey: "observation-1:settings", Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, observation, ok := evolutionAppFleetObservationRequest(request); !ok || observation.Field != "settings" {
		t.Fatalf("exact Fleet observation ABI rejected: %#v, %t", observation, ok)
	}
	var widened map[string]any
	if err := msgpack.Unmarshal(payload, &widened); err != nil {
		t.Fatal(err)
	}
	widened["command"] = "attacker-controlled"
	payload, err = msgpack.Marshal(widened)
	if err != nil {
		t.Fatal(err)
	}
	request, err = msgpack.Marshal(evolutionAppFleetRuntimeIntent{
		Version: "pulp.effect.v1", ID: "observation-1", Kind: "pulp.effect.fleet.runtime-observation.execute.v1",
		IdempotencyKey: "observation-1:settings", Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := evolutionAppFleetObservationRequest(request); ok {
		t.Fatal("widened Fleet observation payload was accepted")
	}
}

func TestEvolutionAppStatusSignalCapabilityIsNarrowAndDeterministic(t *testing.T) {
	payload, err := msgpack.Marshal(evolutionAppStatusSignalPayload{
		Target: "payments", Signal: "degraded", Detail: "payment authority is slow", ExpiresAtUnix: 1_800_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	intent := evolutionAppFleetRuntimeIntent{
		Version: "pulp.effect.v1", ID: "status-1", Kind: "pulp.effect.status.signal.publish.v1",
		IdempotencyKey: "status-1:publish", Payload: payload,
	}
	request, err := msgpack.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := evolutionAppStatusSignalReceipt(request)
	if !ok {
		t.Fatal("canonical status signal was rejected")
	}
	second, ok := evolutionAppStatusSignalReceipt(request)
	if !ok ||
		first.Version != second.Version ||
		first.IntentID != second.IntentID ||
		first.Kind != second.Kind ||
		first.IdempotencyKey != second.IdempotencyKey ||
		first.Status != second.Status ||
		!bytes.Equal(first.Result, second.Result) {
		t.Fatalf("status receipt is not deterministic: first=%#v second=%#v", first, second)
	}
	var result evolutionAppStatusSignalResult
	if err := msgpack.Unmarshal(first.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.Target != "payments" || result.Signal != "degraded" || result.ExpiresAtUnix != 1_800_000_000 {
		t.Fatalf("status receipt result = %#v", result)
	}

	intent.Kind = "pulp.effect.notification.email.send.v1"
	otherEffect, err := msgpack.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := evolutionAppStatusSignalReceipt(otherEffect); ok {
		t.Fatal("non-status host effect was accepted")
	}
	unsafePayload, err := msgpack.Marshal(map[string]any{
		"target": "payments", "signal": "ok", "detail": "healthy", "expires_at_unix": int64(1_800_000_000),
		"url": "https://guest.example/escape",
	})
	if err != nil {
		t.Fatal(err)
	}
	intent.Kind, intent.Payload = "pulp.effect.status.signal.publish.v1", unsafePayload
	unsafeRequest, err := msgpack.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := evolutionAppStatusSignalReceipt(unsafeRequest); ok {
		t.Fatal("guest-controlled HTTP field was accepted")
	}
}

func TestEvolutionAppServiceObservationFixtureIsExactStrictAndDeterministic(t *testing.T) {
	command := evolutionAppServiceObservationCommand{
		Contract:            "service-observation.v1",
		ServiceDefinitionID: "sessions.stripe.primary",
		CommandID:           "service-observation-1",
		IdempotencyKey:      "service-observation-1",
		Fence: evolutionAppServiceObservationFence{
			LeaseID: "service-observation-lease-1", Attempt: 1,
			LeaseExpiresAt: "2026-07-26T12:01:00Z",
		},
		ObservedAt: "2026-07-26T12:00:00Z",
	}
	payload, err := msgpack.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	intent := evolutionAppFleetRuntimeIntent{
		Version: "pulp.effect.v1", ID: command.CommandID,
		Kind:           "pulp.effect.service-observation.execute.v1",
		IdempotencyKey: command.IdempotencyKey, Payload: payload,
	}
	request, err := msgpack.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := evolutionAppServiceObservationReceipt(request)
	if !ok {
		t.Fatal("canonical service observation was rejected")
	}
	second, ok := evolutionAppServiceObservationReceipt(request)
	if !ok {
		t.Fatal("canonical service observation replay was rejected")
	}
	firstWire, err := msgpack.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondWire, err := msgpack.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstWire, secondWire) {
		t.Fatal("service observation receipt is not deterministic")
	}
	var result evolutionAppServiceObservationResult
	if err := msgpack.Unmarshal(first.Result, &result); err != nil ||
		result.Contract != command.Contract ||
		result.ServiceDefinitionID != command.ServiceDefinitionID ||
		result.CommandID != command.CommandID ||
		result.IdempotencyKey != command.IdempotencyKey ||
		result.Fence != command.Fence ||
		result.Observation.Status != "operational" ||
		result.Observation.Evidence != "authenticated" ||
		result.Observation.HTTPStatus != http.StatusOK ||
		result.Observation.ObservedAt != command.ObservedAt {
		t.Fatalf("service observation result = %#v, %v", result, err)
	}

	widened := command
	widenedPayload, err := msgpack.Marshal(map[string]any{
		"contract": command.Contract, "service_definition_id": command.ServiceDefinitionID,
		"command_id": command.CommandID, "idempotency_key": command.IdempotencyKey,
		"fence": command.Fence, "observed_at": command.ObservedAt,
		"url": "https://guest.example/escape",
	})
	if err != nil {
		t.Fatal(err)
	}
	intent.Payload = widenedPayload
	widenedWire, err := msgpack.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := evolutionAppServiceObservationReceipt(widenedWire); ok {
		t.Fatal("guest-controlled service observation transport field was accepted")
	}
	widened.CommandID = "different-command"
	mismatchedPayload, err := msgpack.Marshal(widened)
	if err != nil {
		t.Fatal(err)
	}
	intent.Payload = mismatchedPayload
	mismatchedWire, err := msgpack.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := evolutionAppServiceObservationReceipt(mismatchedWire); ok {
		t.Fatal("service observation command/envelope identity mismatch was accepted")
	}
	if _, ok := evolutionAppServiceObservationReceipt(append(request, 0xc0)); ok {
		t.Fatal("service observation trailing wire data was accepted")
	}

	var fixture *ext.Capability
	for _, capability := range evolutionAppBackendStubs() {
		if capability.Name == "effect.service.observation" {
			copyOfCapability := capability
			fixture = &copyOfCapability
			break
		}
	}
	if fixture == nil {
		t.Fatal("service observation fixture capability is missing")
	}
	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	defer runtime.Close(ctx)
	builder := runtime.NewHostModuleBuilder("service_observation_fixture")
	if err := fixture.Register(builder, nil); err != nil {
		t.Fatal(err)
	}
	compiled, err := builder.Compile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer compiled.Close(ctx)
	exports := compiled.ExportedFunctions()
	execute := exports["service_observation_execute"]
	if len(exports) != 1 || execute == nil ||
		len(execute.ParamTypes()) != 4 || len(execute.ResultTypes()) != 1 {
		t.Fatal("service observation fixture ABI drifted or widened")
	}
	for _, valueType := range append(execute.ParamTypes(), execute.ResultTypes()...) {
		if valueType != api.ValueTypeI32 {
			t.Fatalf("service observation ABI contains non-i32 type %v", valueType)
		}
	}
}

func TestEvolutionAppHTTPProbeFixtureIsExactStrictAndDeterministic(t *testing.T) {
	intent := evolutionAppHTTPProbeIntent{
		Version: "http-probe.v1", IntentID: "probe-1",
		IdempotencyKey: "probe-1", Fence: "status-probe.v1.fixture",
		Destination: "status.website.6227748c2fbaff8f",
	}
	request, err := msgpack.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := evolutionAppHTTPProbeReceiptFor(request)
	if !ok {
		t.Fatal("canonical HTTP probe was rejected")
	}
	second, ok := evolutionAppHTTPProbeReceiptFor(request)
	if !ok || first != second || first.Transport != "observed" ||
		first.HTTPStatus != http.StatusNoContent || len(first.BodySHA256) != 64 {
		t.Fatalf("HTTP probe receipt is not exact and deterministic: %#v / %#v", first, second)
	}
	widened, err := msgpack.Marshal(map[string]any{
		"version": intent.Version, "intent_id": intent.IntentID,
		"idempotency_key": intent.IdempotencyKey, "fence": intent.Fence,
		"destination": intent.Destination, "url": "https://guest.example/escape",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := evolutionAppHTTPProbeReceiptFor(widened); ok {
		t.Fatal("guest-controlled HTTP probe URL was accepted")
	}
	invalidDestination := intent
	invalidDestination.Destination = "guest-selected"
	invalidWire, err := msgpack.Marshal(invalidDestination)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := evolutionAppHTTPProbeReceiptFor(invalidWire); ok {
		t.Fatal("unbounded HTTP probe destination was accepted")
	}
	if _, ok := evolutionAppHTTPProbeReceiptFor(append(request, 0xc0)); ok {
		t.Fatal("HTTP probe trailing wire data was accepted")
	}

	var fixture *ext.Capability
	for _, capability := range evolutionAppBackendStubs() {
		if capability.Name == "effect.http.probe.v1" {
			copyOfCapability := capability
			fixture = &copyOfCapability
			break
		}
	}
	if fixture == nil {
		t.Fatal("HTTP probe fixture capability is missing")
	}
	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	defer runtime.Close(ctx)
	builder := runtime.NewHostModuleBuilder("http_probe_fixture")
	if err := fixture.Register(builder, nil); err != nil {
		t.Fatal(err)
	}
	compiled, err := builder.Compile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer compiled.Close(ctx)
	exports := compiled.ExportedFunctions()
	execute := exports["http_probe_execute"]
	if len(exports) != 1 || execute == nil ||
		len(execute.ParamTypes()) != 4 || len(execute.ResultTypes()) != 1 {
		t.Fatal("HTTP probe fixture ABI drifted or widened")
	}
	for _, valueType := range append(execute.ParamTypes(), execute.ResultTypes()...) {
		if valueType != api.ValueTypeI32 {
			t.Fatalf("HTTP probe ABI contains non-i32 type %v", valueType)
		}
	}
}
