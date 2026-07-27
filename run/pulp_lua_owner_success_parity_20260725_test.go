package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	capabilities["effect.stripe.runtime"] = pulpLuaOwnerStripeRuntimeCapability(recorder)
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

// pulpLuaOwnerStripeRuntimeCapability mirrors the production narrow Stripe
// runtime ABI: it receives one canonical effect intent and returns its exact
// completed receipt. Keep it distinct from payment.stripe so this fixture does
// not accidentally conceal a capability-boundary regression.
func pulpLuaOwnerStripeRuntimeCapability(recorder *pulpLuaOwnerStripeRecorder) ext.Capability {
	bind := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		execute := func(ctx context.Context, module api.Module, requestPtr, requestLen, responsePtr, responseLen uint32) uint32 {
			if requestLen == 0 {
				return 1
			}
			wire, ok := module.Memory().Read(requestPtr, requestLen)
			if !ok {
				return 2
			}
			var intent pulpLuaOwnerStripeEffectIntent
			decoder := msgpack.NewDecoder(bytes.NewReader(wire))
			decoder.DisallowUnknownFields(true)
			if err := decoder.Decode(&intent); err != nil || !intent.valid() {
				return 3
			}

			var result any
			switch intent.Kind {
			case pulpLuaOwnerStripePaymentIntentCreate:
				recorder.record("stripe_payment_intent_create")
				result = map[string]any{"id": "pi_owner_parity", "payment_intent": "pi_owner_parity", "client_secret": "pi_owner_parity_secret", "status": "requires_payment_method"}
			case pulpLuaOwnerStripeCustomerCreate:
				recorder.record("stripe_customer_create")
				var payload struct {
					Email string `msgpack:"email"`
				}
				if err := msgpack.Unmarshal(intent.Payload, &payload); err != nil {
					return 3
				}
				result = map[string]any{"customer_id": "cus_owner_parity", "email": payload.Email}
			case pulpLuaOwnerStripeSetupIntentCreate:
				recorder.record("stripe_setup_intent_create")
				result = map[string]any{"id": "seti_owner_parity", "setup_intent": "seti_owner_parity", "client_secret": "seti_owner_parity_secret", "status": "requires_payment_method"}
			case pulpLuaOwnerStripeInvoiceItemCreate:
				recorder.record("stripe_invoice_item_create")
				result = map[string]any{"invoice_item_id": "ii_owner_parity"}
			case pulpLuaOwnerStripeInvoiceCreate:
				recorder.record("stripe_invoice_create")
				result = map[string]any{"invoice_id": "in_owner_parity", "status": "draft", "amount_due": int64(0), "amount_paid": int64(0)}
			case pulpLuaOwnerStripeInvoiceFinalize, pulpLuaOwnerStripeInvoiceMarkPaid:
				var payload struct {
					InvoiceID string `msgpack:"invoice_id"`
				}
				if err := msgpack.Unmarshal(intent.Payload, &payload); err != nil || payload.InvoiceID == "" {
					return 3
				}
				if intent.Kind == pulpLuaOwnerStripeInvoiceFinalize {
					recorder.record("stripe_invoice_finalize")
					result = map[string]any{"invoice_id": payload.InvoiceID, "status": "open", "amount_due": int64(0), "amount_paid": int64(0)}
				} else {
					recorder.record("stripe_invoice_mark_paid_out_of_band")
					result = map[string]any{"invoice_id": payload.InvoiceID, "status": "paid", "amount_due": int64(0), "amount_paid": int64(0)}
				}
			default:
				return 3
			}

			encodedResult, err := msgpack.Marshal(result)
			if err != nil {
				return 4
			}
			receipt := pulpLuaOwnerStripeEffectReceipt{
				Version:        intent.Version,
				IntentID:       intent.ID,
				Kind:           intent.Kind,
				IdempotencyKey: intent.IdempotencyKey,
				Status:         "completed",
				Result:         encodedResult,
			}
			return writePulpLuaOwnerStripeMsgpack(ctx, module, receipt, responsePtr, responseLen)
		}
		builder.NewFunctionBuilder().WithFunc(execute).Export("stripe_effect_execute")
		return nil
	}
	return ext.Capability{Name: "effect.stripe.runtime", Register: bind, Stub: bind}
}

const (
	pulpLuaOwnerStripeEffectVersion       = "pulp.effect.v1"
	pulpLuaOwnerStripePaymentIntentCreate = "pulp.effect.stripe.payment-intent.create.v1"
	pulpLuaOwnerStripeCustomerCreate      = "pulp.effect.stripe.customer.create.v1"
	pulpLuaOwnerStripeSetupIntentCreate   = "pulp.effect.stripe.setup-intent.create.v1"
	pulpLuaOwnerStripeInvoiceItemCreate   = "pulp.effect.stripe.invoice-item.create.v1"
	pulpLuaOwnerStripeInvoiceCreate       = "pulp.effect.stripe.invoice.create.v1"
	pulpLuaOwnerStripeInvoiceFinalize     = "pulp.effect.stripe.invoice.finalize.v1"
	pulpLuaOwnerStripeInvoiceMarkPaid     = "pulp.effect.stripe.invoice.mark-paid.v1"
)

// These mirror Fiber's canonical effect intent/receipt wire fields without
// adding Fiber as a Pulp test-module dependency.
type pulpLuaOwnerStripeEffectIntent struct {
	Version        string             `msgpack:"version"`
	ID             string             `msgpack:"id"`
	Kind           string             `msgpack:"kind"`
	IdempotencyKey string             `msgpack:"idempotency_key"`
	Payload        msgpack.RawMessage `msgpack:"payload"`
}

func (intent pulpLuaOwnerStripeEffectIntent) valid() bool {
	return intent.Version == pulpLuaOwnerStripeEffectVersion && intent.ID != "" &&
		intent.IdempotencyKey != "" && len(intent.Payload) != 0
}

type pulpLuaOwnerStripeEffectReceipt struct {
	Version        string             `msgpack:"version"`
	IntentID       string             `msgpack:"intent_id"`
	Kind           string             `msgpack:"kind"`
	IdempotencyKey string             `msgpack:"idempotency_key"`
	Status         string             `msgpack:"status"`
	Result         msgpack.RawMessage `msgpack:"result"`
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
