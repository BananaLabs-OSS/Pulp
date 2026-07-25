package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/vmihailenco/msgpack/v5"
)

// TestPulpLuaOwnerSuccessfulParity20260725 drives the installed Evolution
// application composition instead of calling an owner package directly:
//
//	Evolution public HTTP -> gene workflow saga -> Sessions Lua/preflight ->
//	Control + Identity + Fleet + Commerce -> Effects -> host receipt -> Commerce
//	-> ordered post-action host resolver -> terminal legacy HTTP response.
//
// It complements the fail-closed/preflight coverage in
// evolution_app_integration_test.go with terminal success receipts for every
// normal-checkout payment mode, plus live Lua dispatches for an admin mutation
// and a composed reporting read. The Stripe capability is deterministic and
// in-process; no network, credentials, or privileged production effects exist.
func TestPulpLuaOwnerSuccessfulParity20260725(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real Pulp -> Lua -> owner application E2E in short mode")
	}

	workspace, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	manifestPath := filepath.Join(workspace, "Evolution", "pulp-cell", "pulp.app.toml")
	application := HostedApplication{
		Identity:         ApplicationIdentity{ApplicationID: "evolution", InstanceID: "owner-success-parity"},
		ManifestPath:     manifestPath,
		StorageNamespace: "evolution-owner-success-parity",
		EventNamespace:   "evolution-owner-success-parity",
	}
	recorder := &pulpLuaOwnerStripeRecorder{}
	capabilities := evolutionAppCapabilities()
	capabilities["payment.stripe"] = pulpLuaOwnerStripeCapability(recorder)
	storageRoot := t.TempDir()
	endpoints := NewEndpointRegistry()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	t.Cleanup(cancel)
	started := startEvolutionHostedApplication(
		t, ctx, workspace, t.TempDir(), storageRoot, endpoints, nil,
		capabilities, discardLogger(), application,
	)
	t.Cleanup(func() { started.close(context.Background()) })

	seedPulpLuaOwnerControl(t, ctx, started.cells["control"].cell)
	legacy := openPulpLuaOwnerSessionsDB(t, storageRoot, application)
	seedPulpLuaOwnerSessionsCatalog(t, legacy)
	seedPulpLuaOwnerFleetNode(t, ctx, started.cells["fleet"].cell, "initial", 8, 0)

	baseAddress, ok := endpoints.ApplicationAddress(
		application.Identity.ApplicationID,
		application.Identity.InstanceID,
		"transport.http.inbound",
		"public",
	)
	if !ok {
		t.Fatal("staged Evolution application did not publish its public endpoint")
	}
	startEvolutionAppHTTPPump(t, ctx, started.cells["evolution"].cell, capabilities["transport.http.inbound"])
	baseURL := "http://" + baseAddress

	t.Run("paid payment intent", func(t *testing.T) {
		before := recorder.snapshot()
		fields := map[string]any{
			"email":       "paid-owner-parity@example.test",
			"server_type": "minecraft",
			"tier_id":     "standard",
		}
		assertPulpLuaOwnerCheckoutPreflight(t, ctx, started.cells["sessions"].cell, fields)
		response := dispatchPulpLuaOwnerCheckout(t, baseURL, fields)
		if response.Status != http.StatusOK ||
			response.JSON["client_secret"] != "pi_owner_parity_secret" ||
			response.JSON["free"] != false ||
			response.JSON["reserved"] != false {
			t.Fatalf("paid checkout = status %d body %s", response.Status, response.Body)
		}
		assertPulpLuaOwnerOrderPersisted(t, ctx, started.cells["commerce"].cell, "paid-owner-parity@example.test")
		after := recorder.snapshot()
		if after["stripe_payment_intent_create"] != before["stripe_payment_intent_create"]+1 {
			t.Fatalf("paid checkout host calls = %v -> %v", before, after)
		}
	})

	t.Run("reserved setup intent", func(t *testing.T) {
		// Saturating the only Fleet node makes the owner return its queued
		// reservation decision. Lua must turn that exact fact into the
		// Customer -> SetupIntent chain instead of minting a PaymentIntent.
		saturatePulpLuaOwnerFleetNodes(t, ctx, started.cells["fleet"].cell)
		before := recorder.snapshot()
		response := dispatchPulpLuaOwnerCheckout(t, baseURL, map[string]any{
			"email":       "reserved-owner-parity@example.test",
			"server_type": "minecraft",
			"tier_id":     "standard",
		})
		if response.Status != http.StatusOK ||
			response.JSON["client_secret"] != "seti_owner_parity_secret" ||
			response.JSON["reserved"] != true {
			t.Fatalf("reserved checkout = status %d body %s", response.Status, response.Body)
		}
		assertPulpLuaOwnerOrderPersisted(t, ctx, started.cells["commerce"].cell, "reserved-owner-parity@example.test")
		after := recorder.snapshot()
		if after["stripe_customer_create"] != before["stripe_customer_create"]+1 ||
			after["stripe_setup_intent_create"] != before["stripe_setup_intent_create"]+1 ||
			after["stripe_payment_intent_create"] != before["stripe_payment_intent_create"] {
			t.Fatalf("reserved checkout host calls = %v -> %v", before, after)
		}
	})

	t.Run("future scheduled", func(t *testing.T) {
		seedPulpLuaOwnerFleetNode(t, ctx, started.cells["fleet"].cell, "scheduled-reset", 8, 0)
		before := recorder.snapshot()
		response := dispatchPulpLuaOwnerCheckout(t, baseURL, map[string]any{
			"email":         "scheduled-owner-parity@example.test",
			"server_type":   "minecraft",
			"tier_id":       "standard",
			"auto_redeem":   false,
			"scheduled_at":  "2099-08-01",
			"duration_days": 1,
		})
		if response.Status != http.StatusOK ||
			response.JSON["client_secret"] != "seti_owner_parity_secret" ||
			response.JSON["reserved"] != true {
			t.Fatalf("scheduled checkout = status %d body %s", response.Status, response.Body)
		}
		assertPulpLuaOwnerOrderPersisted(t, ctx, started.cells["commerce"].cell, "scheduled-owner-parity@example.test")
		after := recorder.snapshot()
		if after["stripe_customer_create"] != before["stripe_customer_create"]+1 ||
			after["stripe_setup_intent_create"] != before["stripe_setup_intent_create"]+1 ||
			after["stripe_payment_intent_create"] != before["stripe_payment_intent_create"] {
			t.Fatalf("scheduled checkout host calls = %v -> %v", before, after)
		}
	})

	t.Run("free auto redeem invoice", func(t *testing.T) {
		// Free checkout is not complete when Lua first exposes its durable
		// notification/deploy actions. The real Evolution HTTP adapter must
		// execute those ordered actions, reread terminal Commerce state, and
		// only then preserve the legacy 200 response.
		seedPulpLuaOwnerCoupon(t, legacy, "FREEPARITY", 1400)
		seedPulpLuaOwnerCommerceCoupon(t, ctx, started.cells["commerce"].cell, "FREEPARITY", 1400)
		before := recorder.snapshot()
		response := dispatchPulpLuaOwnerCheckout(t, baseURL, map[string]any{
			"email":       "free-owner-parity@example.test",
			"server_type": "minecraft",
			"tier_id":     "standard",
			"promo_code":  "FREEPARITY",
			"auto_redeem": true,
		})
		if response.Status != http.StatusOK ||
			response.JSON["free"] != true ||
			fmt.Sprint(response.JSON["order_id"]) == "" ||
			(response.JSON["client_secret"] != nil && fmt.Sprint(response.JSON["client_secret"]) != "") {
			t.Fatalf("free checkout = status %d body %s", response.Status, response.Body)
		}
		assertPulpLuaOwnerOrderPersisted(t, ctx, started.cells["commerce"].cell, "free-owner-parity@example.test")
		after := recorder.snapshot()
		for _, operation := range []string{
			"stripe_customer_create",
			"stripe_invoice_item_create",
			"stripe_invoice_create",
			"stripe_invoice_finalize",
			"stripe_invoice_mark_paid_out_of_band",
		} {
			if after[operation] != before[operation]+1 {
				t.Fatalf("free checkout %s calls = %v -> %v", operation, before, after)
			}
		}
		if after["stripe_payment_intent_create"] != before["stripe_payment_intent_create"] {
			t.Fatalf("free checkout minted a PaymentIntent: %v -> %v", before, after)
		}
	})

	t.Run("admin allowlist mutation through Lua", func(t *testing.T) {
		result := dispatchPulpLuaOwnerEvent(t, ctx, started.cells["lua-orchestrator"].cell,
			"evolution.sessions.commerce.admin.coupon-allowlist-create.completed.v1",
			map[string]any{
				"request_id": "pulp-owner-parity-admin-allowlist",
				"actor": map[string]any{
					"id": "admin-owner-parity", "is_admin": true,
				},
				"command": map[string]any{
					"entry": map[string]any{
						"id": "allow-owner-parity", "email": " Admin@Example.Test ",
						"name": " Admin ", "note": " staged parity ",
					},
				},
			})
		projected := pulpLuaOwnerMap(t, result.Value, "admin allowlist projection")
		if fmt.Sprint(projected["status"]) != "200" ||
			!strings.Contains(fmt.Sprint(projected["body"]), "admin@example.test") {
			t.Fatalf("admin allowlist projection = %#v", projected)
		}
	})

	t.Run("analytics reporting through Lua", func(t *testing.T) {
		result := dispatchPulpLuaOwnerEvent(t, ctx, started.cells["lua-orchestrator"].cell,
			"evolution.sessions.commerce.admin.reporting.analytics.v1",
			map[string]any{
				"request_id": "pulp-owner-parity-analytics",
				"actor": map[string]any{
					"id": "admin-owner-parity", "is_admin": true,
				},
				"facts": map[string]any{
					"control": map[string]any{
						"tier_labels": map[string]any{"standard": "Standard"},
					},
					"fleet": map[string]any{
						"server_state_counts": map[string]any{"active": int64(0)},
					},
				},
				"now_unix": int64(1_800_000_000),
			})
		projected := pulpLuaOwnerMap(t, result.Value, "analytics projection")
		if fmt.Sprint(projected["status"]) != "200" ||
			!strings.Contains(fmt.Sprint(projected["body"]), `"revenue"`) {
			t.Fatalf("analytics projection = %#v", projected)
		}
	})
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type pulpLuaOwnerHTTPResponse struct {
	Status int
	Body   []byte
	JSON   map[string]any
}

func dispatchPulpLuaOwnerCheckout(t *testing.T, baseURL string, fields map[string]any) pulpLuaOwnerHTTPResponse {
	t.Helper()
	body := map[string]any{
		"age_confirmed": true,
		"tos_accepted":  true,
		"eula_accepted": true,
	}
	for key, value := range fields {
		body[key] = value
	}
	bodyWire, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode checkout HTTP body: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/api/checkout", strings.NewReader(string(bodyWire)))
	if err != nil {
		t.Fatalf("build checkout HTTP request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("send checkout through staged Evolution HTTP route: %v", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read checkout HTTP response: %v", err)
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode staged checkout status %d body %s: %v", response.StatusCode, raw, err)
	}
	return pulpLuaOwnerHTTPResponse{Status: response.StatusCode, Body: raw, JSON: decoded}
}

func pulpLuaOwnerCheckoutRequestWire(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	body := map[string]any{
		"age_confirmed": true,
		"tos_accepted":  true,
		"eula_accepted": true,
	}
	for key, value := range fields {
		body[key] = value
	}
	bodyWire, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode checkout: %v", err)
	}
	requestWire, err := msgpack.Marshal(map[string]any{
		"method": "POST", "path": "/api/checkout", "client_ip": "198.51.100.25",
		"headers": map[string]string{"Content-Type": "application/json"},
		"body":    bodyWire,
	})
	if err != nil {
		t.Fatalf("encode checkout gene request: %v", err)
	}
	return requestWire
}

func assertPulpLuaOwnerCheckoutPreflight(t *testing.T, ctx context.Context, sessions *host.Cell, fields map[string]any) map[string]any {
	t.Helper()
	response, err := sessions.Call(ctx, "sessions.checkout.preflight.v1", pulpLuaOwnerCheckoutRequestWire(t, fields))
	if err != nil {
		t.Fatalf("Sessions checkout preflight: %v", err)
	}
	var plan map[string]any
	if err := msgpack.Unmarshal(response, &plan); err != nil {
		t.Fatalf("decode Sessions checkout preflight: %v", err)
	}
	if fmt.Sprint(plan["http_status"]) != "200" {
		t.Fatalf("Sessions checkout preflight = %#v", plan)
	}
	return plan
}

func dispatchPulpLuaOwnerEvent(t *testing.T, ctx context.Context, orchestrator *host.Cell, event string, payload any) luaDispatchResult {
	t.Helper()
	request, err := msgpack.Marshal(luaDispatchRequest{Event: event, Payload: payload})
	if err != nil {
		t.Fatalf("encode %s: %v", event, err)
	}
	response, err := orchestrator.Call(ctx, "orchestrator.dispatch", request)
	if err != nil {
		t.Fatalf("dispatch %s: %v", event, err)
	}
	var result luaDispatchResult
	if err := msgpack.Unmarshal(response, &result); err != nil {
		t.Fatalf("decode %s: %v", event, err)
	}
	return result
}

func assertPulpLuaOwnerOrderPersisted(t *testing.T, ctx context.Context, commerce *host.Cell, email string) {
	t.Helper()
	request, err := msgpack.Marshal(map[string]any{"limit": 500})
	if err != nil {
		t.Fatal(err)
	}
	response, err := commerce.Call(ctx, "commerce.admin.order-fact.list.v1", request)
	if err != nil {
		t.Fatalf("list Commerce orders after checkout for %s: %v", email, err)
	}
	var result map[string]any
	if err := msgpack.Unmarshal(response, &result); err != nil {
		t.Fatalf("decode Commerce orders after checkout for %s: %v", email, err)
	}
	if result["ok"] != true {
		t.Fatalf("Commerce order list after checkout for %s failed: %#v", email, result)
	}
	orders, ok := result["value"].([]any)
	if !ok {
		t.Fatalf("Commerce order list after checkout for %s = %T", email, result["value"])
	}
	for _, raw := range orders {
		order := pulpLuaOwnerMap(t, raw, "Commerce order fact")
		if strings.EqualFold(fmt.Sprint(order["email"]), email) && fmt.Sprint(order["order_id"]) != "" {
			return
		}
	}
	t.Fatalf("Commerce order for %s was not persisted: %#v", email, orders)
}

func pulpLuaOwnerMap(t *testing.T, value any, name string) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s = %T, want map", name, value)
	}
	return result
}

func seedPulpLuaOwnerControl(t *testing.T, ctx context.Context, control *host.Cell) {
	t.Helper()
	createdAt := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	request, err := msgpack.Marshal(map[string]any{
		"version":   "sessions.control/v1",
		"import_id": "pulp-owner-success-parity-control-v1",
		"source":    "pulp-run-owner-success-parity/v1",
		"legacy_projection": map[string]any{
			"version": "sessions.control/v1",
			"games": []any{map[string]any{
				"id": "minecraft", "slug": "minecraft", "name": "Minecraft",
				"primary_template": "minecraft", "enabled": true,
			}},
			"visibility": []any{map[string]any{
				"game_id": "minecraft", "template": "minecraft", "tier_id": "standard",
				"label": "Minecraft", "enabled": true, "public": true, "listed": true,
			}},
			"tiers": []any{map[string]any{
				"id": "standard", "game_id": "minecraft", "name": "session", "label": "Standard",
				"price_cents": int64(1400), "currency": "usd", "duration": "336h",
				"max_cpu": 2.0, "max_ram_mb": 4096, "enabled": true, "created_at": createdAt,
			}},
			"runtime_templates": []any{map[string]any{
				"version": "sessions.control/v1", "game_id": "minecraft", "template": "minecraft",
				"approved":    true,
				"image":       "paper-server:1.21@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"approval_id": "pulp-owner-parity", "policy_version": "sessions-runtime-v1",
			}},
		},
	})
	if err != nil {
		t.Fatalf("encode Control seed: %v", err)
	}
	if _, err := control.Call(ctx, "control.v1.import_legacy_projection", request); err != nil {
		t.Fatalf("seed Control owner: %v", err)
	}
}

func openPulpLuaOwnerSessionsDB(t *testing.T, storageRoot string, application HostedApplication) *sql.DB {
	t.Helper()
	root := evolutionHostedStorageRoot(storageRoot, application)
	var path string
	err := filepath.WalkDir(root, func(candidate string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "data.db" {
			return nil
		}
		normalized := filepath.ToSlash(candidate)
		if strings.Contains(normalized, "/cells/sessions/") {
			path = candidate
		}
		return nil
	})
	if err != nil {
		t.Fatalf("find scoped Sessions reference database: %v", err)
	}
	if path == "" {
		t.Fatalf("scoped Sessions reference database is absent under %s", root)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open Sessions reference database: %v", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			t.Fatalf("Sessions reference database %s: %v", pragma, err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedPulpLuaOwnerSessionsCatalog(t *testing.T, db *sql.DB) {
	t.Helper()
	const schema = `
CREATE TABLE IF NOT EXISTS user_bans (
	id TEXT PRIMARY KEY,
	email TEXT NOT NULL UNIQUE,
	expires_at TIMESTAMP
);
CREATE TABLE IF NOT EXISTS servers (
	id TEXT PRIMARY KEY,
	state TEXT NOT NULL,
	cpu_weight REAL NOT NULL DEFAULT 0,
	memory_weight REAL NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS game_visibility (
	template TEXT NOT NULL,
	tier_id TEXT NOT NULL DEFAULT '',
	game_id TEXT NOT NULL,
	label TEXT,
	enabled BOOLEAN NOT NULL DEFAULT FALSE,
	hidden BOOLEAN NOT NULL DEFAULT FALSE,
	engine TEXT NOT NULL DEFAULT '',
	price_override INTEGER,
	duration_override TEXT,
	max_players_override INTEGER,
	tagline_override TEXT,
	tags_override_json TEXT,
	description_override TEXT,
	max_instances INTEGER,
	extend_instant_pct INTEGER,
	extend_queued_pct INTEGER,
	config_json TEXT,
	PRIMARY KEY (template, tier_id)
);
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
CREATE TABLE IF NOT EXISTS coupons (
	id TEXT PRIMARY KEY,
	code TEXT NOT NULL UNIQUE,
	discount_cents INTEGER NOT NULL DEFAULT 0,
	max_uses INTEGER NOT NULL DEFAULT 0,
	uses INTEGER NOT NULL DEFAULT 0,
	note TEXT NOT NULL DEFAULT '',
	expires_at TIMESTAMP,
	created_at TIMESTAMP NOT NULL
);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create Sessions checkout reference schema: %v", err)
	}
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO tiers (id, name, label, price_cents, duration, enabled, sort_order, max_cpu, max_ram_mb, created_at)
		 VALUES ('standard','session','Standard',1400,'336h',1,0,2.0,4096,?)`, now,
	); err != nil {
		t.Fatalf("seed Sessions tier: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO game_visibility (template, tier_id, game_id, label, enabled)
		 VALUES ('minecraft','standard','minecraft','Minecraft',1)`,
	); err != nil {
		t.Fatalf("seed Sessions visibility: %v", err)
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
		t.Fatalf("checkpoint Sessions catalog: %v", err)
	}
}

func seedPulpLuaOwnerCoupon(t *testing.T, db *sql.DB, code string, discountCents int64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO coupons (id, code, discount_cents, max_uses, uses, created_at)
		 VALUES (?, ?, ?, 0, 0, ?)`,
		"coupon-"+strings.ToLower(code), code, discountCents, time.Now().UTC(),
	); err != nil {
		t.Fatalf("seed Sessions coupon %s: %v", code, err)
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
		t.Fatalf("checkpoint Sessions coupon: %v", err)
	}
}

func seedPulpLuaOwnerCommerceCoupon(t *testing.T, ctx context.Context, commerce *host.Cell, code string, discountCents int64) {
	t.Helper()
	request, err := msgpack.Marshal(map[string]any{
		"idempotency_key": "owner-parity:coupon:" + code,
		"actor_id":        "owner-parity",
		"coupon": map[string]any{
			"id":               "owner-parity-" + strings.ToLower(code),
			"amount_off_cents": discountCents,
			"currency":         "usd",
			"duration":         "once",
			"active":           true,
			"metadata": map[string]string{
				"legacy_code": code,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := commerce.Call(ctx, "commerce.admin.coupon.upsert.v1", request)
	if err != nil {
		t.Fatalf("seed Commerce coupon %s: %v", code, err)
	}
	var result map[string]any
	if err := msgpack.Unmarshal(response, &result); err != nil {
		t.Fatalf("decode Commerce coupon seed %s: %v", code, err)
	}
	if result["ok"] != true {
		t.Fatalf("seed Commerce coupon %s = %#v", code, result)
	}
}

func seedPulpLuaOwnerFleetNode(t *testing.T, ctx context.Context, fleet *host.Cell, command string, capacity, used int64) {
	t.Helper()
	request, err := msgpack.Marshal(map[string]any{
		"id": "pulp-owner-parity-node-" + command,
		"node": map[string]any{
			"id": "pulp-owner-parity-node", "name": "Pulp Owner Parity",
			"cpu_capacity": capacity, "memory_capacity": capacity * 2048,
			"cpu_used": used, "memory_used": used * 2048, "status": "active",
		},
	})
	if err != nil {
		t.Fatalf("encode Fleet node: %v", err)
	}
	if _, err := fleet.Call(ctx, "fleet.v1.command.node.upsert", request); err != nil {
		t.Fatalf("seed Fleet node: %v", err)
	}
}

func saturatePulpLuaOwnerFleetNodes(t *testing.T, ctx context.Context, fleet *host.Cell) {
	t.Helper()
	response, err := fleet.Call(ctx, "fleet.v1.query.node.list", nil)
	if err != nil {
		t.Fatalf("list Fleet nodes before saturation: %v", err)
	}
	var nodes []struct {
		ID             string `msgpack:"id"`
		Name           string `msgpack:"name"`
		CPUCapacity    int64  `msgpack:"cpu_capacity"`
		MemoryCapacity int64  `msgpack:"memory_capacity"`
		Status         string `msgpack:"status"`
	}
	if err := msgpack.Unmarshal(response, &nodes); err != nil {
		t.Fatalf("decode Fleet nodes before saturation: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatal("Fleet has no node to saturate")
	}
	for index, node := range nodes {
		status := node.Status
		if node.CPUCapacity == 0 && node.MemoryCapacity == 0 {
			status = "offline"
		}
		request, err := msgpack.Marshal(map[string]any{
			"id": fmt.Sprintf("pulp-owner-parity-saturate-%d", index),
			"node": map[string]any{
				"id": node.ID, "name": node.Name,
				"cpu_capacity": node.CPUCapacity, "memory_capacity": node.MemoryCapacity,
				"cpu_used": node.CPUCapacity, "memory_used": node.MemoryCapacity,
				"status": status,
			},
		})
		if err != nil {
			t.Fatalf("encode Fleet node %s saturation: %v", node.ID, err)
		}
		if _, err := fleet.Call(ctx, "fleet.v1.command.node.upsert", request); err != nil {
			t.Fatalf("saturate Fleet node %s: %v", node.ID, err)
		}
		capacityRequest, _ := msgpack.Marshal(map[string]any{"id": node.ID})
		capacityResponse, err := fleet.Call(ctx, "fleet.v1.query.capacity.get", capacityRequest)
		if err != nil {
			t.Fatalf("read saturated Fleet node %s: %v", node.ID, err)
		}
		var capacity struct {
			CPUCapacity    int64 `msgpack:"cpu_capacity"`
			CPUUsed        int64 `msgpack:"cpu_used"`
			MemoryCapacity int64 `msgpack:"memory_capacity"`
			MemoryUsed     int64 `msgpack:"memory_used"`
		}
		if err := msgpack.Unmarshal(capacityResponse, &capacity); err != nil {
			t.Fatalf("decode saturated Fleet node %s: %v", node.ID, err)
		}
		if status != "offline" &&
			((capacity.CPUCapacity > 0 && capacity.CPUUsed < capacity.CPUCapacity) ||
				(capacity.MemoryCapacity > 0 && capacity.MemoryUsed < capacity.MemoryCapacity)) {
			t.Fatalf("Fleet node %s was not saturated: %#v", node.ID, capacity)
		}
	}
}

type pulpLuaOwnerStripeRecorder struct {
	mu    sync.Mutex
	calls map[string]int
}

func (r *pulpLuaOwnerStripeRecorder) record(operation string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[operation]++
}

func (r *pulpLuaOwnerStripeRecorder) snapshot() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make(map[string]int, len(r.calls))
	for operation, count := range r.calls {
		result[operation] = count
	}
	return result
}

func pulpLuaOwnerStripeCapability(recorder *pulpLuaOwnerStripeRecorder) ext.Capability {
	bind := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		write := func(operation string, value func(api.Module, uint32, uint32) any) func(context.Context, api.Module, uint32, uint32, uint32, uint32) uint32 {
			return func(ctx context.Context, module api.Module, requestPtr, requestLen, responsePtr, responseLen uint32) uint32 {
				recorder.record(operation)
				return writePulpLuaOwnerStripeMsgpack(ctx, module, value(module, requestPtr, requestLen), responsePtr, responseLen)
			}
		}
		empty := func(api.Module, uint32, uint32) any { return map[string]any{} }
		builder.NewFunctionBuilder().WithFunc(write("stripe_payment_intent_create", func(module api.Module, ptr, size uint32) any {
			var request struct {
				AmountCents int64  `msgpack:"amount_cents"`
				Currency    string `msgpack:"currency"`
			}
			readPulpLuaOwnerStripeMsgpack(module, ptr, size, &request)
			return map[string]any{
				"id": "pi_owner_parity", "status": "requires_payment_method",
				"amount": request.AmountCents, "currency": request.Currency,
				"client_secret": "pi_owner_parity_secret", "metadata": map[string]string{},
			}
		})).Export("stripe_payment_intent_create")
		builder.NewFunctionBuilder().WithFunc(write("stripe_customer_create", func(module api.Module, ptr, size uint32) any {
			var request struct {
				Email string `msgpack:"email"`
			}
			readPulpLuaOwnerStripeMsgpack(module, ptr, size, &request)
			return map[string]any{"id": "cus_owner_parity", "email": request.Email}
		})).Export("stripe_customer_create")
		builder.NewFunctionBuilder().WithFunc(write("stripe_setup_intent_create", func(module api.Module, ptr, size uint32) any {
			var request struct {
				Customer string `msgpack:"customer"`
				Usage    string `msgpack:"usage"`
			}
			readPulpLuaOwnerStripeMsgpack(module, ptr, size, &request)
			return map[string]any{
				"id": "seti_owner_parity", "status": "requires_payment_method",
				"client_secret": "seti_owner_parity_secret", "customer": request.Customer,
				"usage": request.Usage, "metadata": map[string]string{},
			}
		})).Export("stripe_setup_intent_create")
		builder.NewFunctionBuilder().WithFunc(write("stripe_invoice_item_create", func(api.Module, uint32, uint32) any {
			return map[string]any{"id": "ii_owner_parity"}
		})).Export("stripe_invoice_item_create")
		builder.NewFunctionBuilder().WithFunc(write("stripe_invoice_create", func(api.Module, uint32, uint32) any {
			return map[string]any{"id": "in_owner_parity", "status": "draft", "amount_due": int64(0), "amount_paid": int64(0)}
		})).Export("stripe_invoice_create")
		builder.NewFunctionBuilder().WithFunc(write("stripe_invoice_finalize", func(api.Module, uint32, uint32) any {
			return map[string]any{"id": "in_owner_parity", "status": "open", "amount_due": int64(0), "amount_paid": int64(0)}
		})).Export("stripe_invoice_finalize")
		builder.NewFunctionBuilder().WithFunc(write("stripe_invoice_mark_paid_out_of_band", func(api.Module, uint32, uint32) any {
			return map[string]any{"id": "in_owner_parity", "status": "paid", "amount_due": int64(0), "amount_paid": int64(0)}
		})).Export("stripe_invoice_mark_paid_out_of_band")

		for _, operation := range []string{
			"stripe_checkout_session_create", "stripe_checkout_session_get",
			"stripe_payment_intent_get", "stripe_payment_intent_capture", "stripe_payment_intent_cancel",
			"stripe_refund_create", "stripe_setup_intent_get", "stripe_balance_get",
			"stripe_coupon_create", "stripe_promotion_code_create", "stripe_promotion_code_lookup",
			"stripe_promotion_code_update",
		} {
			builder.NewFunctionBuilder().WithFunc(write(operation, empty)).Export(operation)
		}
		builder.NewFunctionBuilder().WithFunc(func(context.Context, api.Module, uint32, uint32) uint32 {
			recorder.record("stripe_webhook_verify")
			return 0
		}).Export("stripe_webhook_verify")
		return nil
	}
	return ext.Capability{Name: "payment.stripe", Register: bind, Stub: bind}
}

func readPulpLuaOwnerStripeMsgpack(module api.Module, ptr, size uint32, out any) {
	if size == 0 {
		return
	}
	raw, ok := module.Memory().Read(ptr, size)
	if !ok {
		return
	}
	_ = msgpack.Unmarshal(raw, out)
}

func writePulpLuaOwnerStripeMsgpack(ctx context.Context, module api.Module, value any, ptrOut, sizeOut uint32) uint32 {
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
