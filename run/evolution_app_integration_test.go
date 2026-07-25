package run

import (
	"bytes"
	"context"
	"database/sql"
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
	if applications[0].Identity != (ApplicationIdentity{ApplicationID: "minecraft-resolver", InstanceID: "primary"}) ||
		applications[1].Identity != (ApplicationIdentity{ApplicationID: "sessions", InstanceID: "primary"}) ||
		applications[2].Identity != (ApplicationIdentity{ApplicationID: "evolution", InstanceID: "primary"}) ||
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

	seedEvolutionControlProjection(t, ctx, started[1].cells["control"].cell)

	evolution := started[2]
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
id = "minecraft-resolver"
manifest = %q
aliases = ["primary"]
storage_namespace = "minecraft-resolver"
event_namespace = "minecraft-resolver"

[[applications]]
id = "sessions"
manifest = %q
aliases = ["primary"]
storage_namespace = "sessions"
event_namespace = "sessions"

[[applications]]
id = "evolution"
manifest = %q
aliases = ["primary"]
storage_namespace = "evolution"
event_namespace = "evolution"
depends_on = ["minecraft-resolver", "sessions"]
`, filepath.ToSlash(resolverRelative), filepath.ToSlash(sessionsRelative), filepath.ToSlash(evolutionRelative))
	if err := os.WriteFile(hostPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write temporary host manifest: %v", err)
	}
	return hostPath
}

func evolutionApplicationCellSources(workspace string) map[string]string {
	return map[string]string{
		"jvm-jre-detect":     filepath.Join(workspace, "minecraft-resolver", "jvm-jre-detect"),
		"minecraft-resolver": filepath.Join(workspace, "minecraft-resolver", "pulp-cell"),
		"sessions":           filepath.Join(workspace, "Sessions-Gene", "pulp-cell"),
		"commerce":           filepath.Join(workspace, "Evolution", "commerce"),
		"fleet":              filepath.Join(workspace, "Evolution", "fleet"),
		"funding":            filepath.Join(workspace, "Evolution", "funding"),
		"identity":           filepath.Join(workspace, "Evolution", "identity"),
		"control":            filepath.Join(workspace, "Evolution", "control"),
		"effects":            filepath.Join(workspace, "Evolution", "effects"),
		"lua-orchestrator":   filepath.Join(workspace, "Pulp-Lua", "pulp-cell"),
		"evolution":          filepath.Join(workspace, "Evolution", "pulp-cell"),
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
	return []ext.Capability{
		{Name: "payment.stripe", Register: stripe, Stub: stripe},
		{Name: "storage.s3", Register: s3, Stub: s3},
		{Name: "spawn.docker", Register: docker, Stub: docker},
		{Name: "workers", Register: workers, Stub: workers},
	}
}
