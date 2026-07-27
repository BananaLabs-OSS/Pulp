package host

// PORT (second slice) of Evolution/internal/poller/transitions_test.go +
// poller_test.go, driven THROUGH the real Evolution cell (evolution.wasm)
// under the Pulp host — the same poller-DB-seed + driveTick vehicle proven in
// cellharness_evolution_transitions_test.go.
//
// The native tests call p.enqueueNewOrders / p.checkExpirations directly on a
// native *poller. The cell links only under GOOS=wasip1 + the Pulp host, so it
// can't be `go test`-ed that way. These ports seed order/server rows on the
// cell's own SQLite and DRIVE the poller's mainTick (an inbound request runs
// OnStep -> poll.tickIfDue -> mainTick -> enqueueNewOrders + promoteQueue +
// checkExpirations), then assert the poller's OWN writes.
//
// Granularity note (identical to the first slice): one driven mainTick runs
// enqueue + promote + provision in the same cycle, so a transient `queued`
// state is not separately observable. These ports therefore assert the
// OBSERVABLE outcome of each transition (a server row is / is not created for
// the order; a fresh active server stays active) — exactly the behaviour the
// native unit tests pin, reached through the cell's real step loop.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

type fleetQueueValue struct {
	OrderID  string `msgpack:"order_id"`
	ServerID string `msgpack:"server_id"`
	Sequence int64  `msgpack:"sequence"`
	Position int    `msgpack:"position"`
}

func fleetQueue(t *testing.T, h *CellHarness) []fleetQueueValue {
	t.Helper()
	request, err := msgpack.Marshal(map[string]any{})
	if err != nil {
		t.Fatalf("encode Fleet queue query: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := h.cellsByName["fleet"].Call(ctx, "fleet.v1.query.queue.list", request)
	if err != nil {
		t.Fatalf("query Fleet queue: %v", err)
	}
	var queue []fleetQueueValue
	if err := msgpack.Unmarshal(response, &queue); err != nil {
		t.Fatalf("decode Fleet queue: %v", err)
	}
	return queue
}

func upsertFleetServer(t *testing.T, h *CellHarness, serverID, status, expiresAt string) {
	t.Helper()
	upsertFleetServerValue(t, h, map[string]any{
		"id": serverID, "order_id": "order-" + serverID,
		"template": "minecraft", "status": status, "expires_at": expiresAt,
	})
}

func upsertFleetServerValue(t *testing.T, h *CellHarness, server map[string]any) {
	t.Helper()
	serverID, _ := server["id"].(string)
	if serverID == "" {
		t.Fatal("Fleet server fixture requires id")
	}
	request, err := msgpack.Marshal(map[string]any{
		"id":     "test-upsert:" + serverID,
		"server": server,
	})
	if err != nil {
		t.Fatalf("encode Fleet server upsert: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.cellsByName["fleet"].Call(ctx, "fleet.v1.command.server.upsert", request); err != nil {
		t.Fatalf("upsert Fleet server %s: %v", serverID, err)
	}
}

func dispatchFleetMaintenance(t *testing.T, h *CellHarness, requestID string, now time.Time) {
	t.Helper()
	dispatchFleetMaintenanceRequest(t, h, map[string]any{
		"request_id": requestID,
		"now":        now.UTC().Format(time.RFC3339Nano),
		"limit":      uint32(100),
	})
}

func dispatchFleetMaintenanceRequest(t *testing.T, h *CellHarness, maintenance map[string]any) {
	t.Helper()
	request := struct {
		Event   string `msgpack:"event"`
		Payload any    `msgpack:"payload"`
	}{
		Event:   "fleet.workflow.maintenance.sweep.v1",
		Payload: map[string]any{"request": maintenance},
	}
	wire, err := msgpack.Marshal(request)
	if err != nil {
		t.Fatalf("encode Fleet maintenance workflow: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.cellsByName["lua-orchestrator"].Call(ctx, "orchestrator.dispatch", wire); err != nil {
		t.Fatalf("dispatch Fleet maintenance through Lua: %v", err)
	}
}

type notificationIntentValue struct {
	ID      string             `msgpack:"id"`
	Kind    string             `msgpack:"kind"`
	Payload msgpack.RawMessage `msgpack:"payload"`
}

func claimNotifications(t *testing.T, h *CellHarness, consumer string) []notificationIntentValue {
	t.Helper()
	request, err := msgpack.Marshal(map[string]any{
		"version": "pulp.effect.outbox.v1", "owner": "sessions.notifications",
		"consumer_id": consumer, "limit": uint32(100), "lease_duration_millis": int64(60_000),
	})
	if err != nil {
		t.Fatalf("encode notification claim: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := h.cellsByName["effects"].Call(ctx, "effect.outbox.claim.v1", request)
	if err != nil {
		t.Fatalf("claim notification outbox: %v", err)
	}
	var result struct {
		Leases []struct {
			Intent notificationIntentValue `msgpack:"intent"`
		} `msgpack:"leases"`
	}
	if err := msgpack.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode notification claim: %v", err)
	}
	intents := make([]notificationIntentValue, len(result.Leases))
	for i, lease := range result.Leases {
		intents[i] = lease.Intent
	}
	return intents
}

// TestEvolution_Enqueue_SkipsOrdersWithExistingServers ports
// TestEnqueue_SkipsOrdersWithExistingServers: a paid order that already owns a
// server is NOT re-enqueued into a second server.
func TestEvolution_Enqueue_SkipsOrdersWithExistingServers(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	orderID, order := ownerCheckoutPending(t, h, "enqueue-existing-owner", "existing@example.test", nil)
	settleOwnerCheckout(t, h, "evt-enqueue-existing-owner", order)
	first := fleetServerForID(t, h, orderID+"-server")

	// Stripe retries the same durable event. Commerce and Fleet must replay the
	// owner commands without creating a second server or changing its identity.
	settleOwnerCheckout(t, h, "evt-enqueue-existing-owner", order)
	replayed := fleetServerForID(t, h, orderID+"-server")
	if replayed.ID != first.ID || replayed.OrderID != orderID || replayed.NodeID != first.NodeID {
		t.Fatalf("duplicate payment delivery changed Fleet ownership: first=%#v replay=%#v", first, replayed)
	}
}

// TestEvolution_Enqueue_AssignsServersFIFO ports TestEnqueue_AssignsFIFOPositions:
// multiple paid orders each get their OWN server (the observable parity for the
// native "each order assigned a queue position" — one driven mainTick enqueues
// + promotes in the same cycle, so the transient position is not separately
// observable, but the per-order server is).
//
// The orders are seeded one at a time: a single mainTick provisions every
// promotable order synchronously (a real ~2s container-startup poll each), so
// seeding all three at once would block one tick past the 5s harness HTTP
// client timeout. Seeding incrementally keeps each tick to a single new
// provision (already-active orders are skipped by enqueue) while still proving
// each paid order lands its own distinct server.
func TestEvolution_Enqueue_AssignsServersFIFO(t *testing.T) {
	setStripeStubSetupPM(t, "pm_stub_card")
	h, db := startEvolutionDowntimeExtra(t, "", capacityFullCfg)
	seedDowntimeCatalog(t, db)

	queuedOrders := make([]string, 0, 3)
	for index, key := range []string{"enqueue-fifo-1", "enqueue-fifo-2", "enqueue-fifo-3"} {
		queuedOrders = append(queuedOrders, reservedOwnerCheckout(
			t, h, db, key, key+"@example.test",
		))
		if index < 2 {
			// Commerce's portable FIFO fact is second-granularity. Distinct
			// creation instants make this compatibility proof deterministic.
			time.Sleep(1100 * time.Millisecond)
		}
	}

	var queue []fleetQueueValue
	driveUntil(t, h, db, "reserved orders to enter the Fleet queue", func() bool {
		queue = fleetQueue(t, h)
		return len(queue) == len(queuedOrders)
	})
	for i, orderID := range queuedOrders {
		if queue[i].OrderID != orderID || queue[i].ServerID != orderID+"-server" {
			t.Fatalf("Fleet queue lost checkout order at position %d: got %#v want order %s", i+1, queue[i], orderID)
		}
		if i > 0 && queue[i].Sequence <= queue[i-1].Sequence {
			t.Fatalf("Fleet queue sequence is not FIFO: %#v", queue)
		}
	}
}

// TestEvolution_Enqueue_PreservesExtendServerID ports
// TestEnqueue_PreservesExtendServerID: a paid order carrying extend_server_id
// produces a server whose extends_server_id is preserved from the order.
func TestEvolution_Enqueue_PreservesExtendServerID(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	const (
		originalOrderID = "extension-original-order"
		serverID        = "extension-original-server"
		ownerEmail      = "extension-original-order@e.com"
	)
	seedOrderRow(t, db, originalOrderID, "fulfilled", false, false)
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO servers (id, order_id, template, state, created_at, expires_at, cpu_weight, memory_weight, restart_count)
		 VALUES (?, ?, 'minecraft', 'expiring', ?, ?, 0.33, 3, 0)`,
		serverID, originalOrderID, now, now.Add(30*24*time.Hour),
	); err != nil {
		t.Fatalf("seed extension target server: %v", err)
	}
	checkpoint(db)

	const key = "enqueue-preserve-extension-owner"
	body, err := json.Marshal(map[string]any{
		"server_id": serverID,
		"email":     ownerEmail,
	})
	if err != nil {
		t.Fatal(err)
	}
	status, response := h.Do("POST", "/api/extend-checkout", map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": key,
	}, body)
	if status != 200 {
		t.Fatalf("extension checkout: want 200, got %d (%s)", status, response)
	}

	request, err := msgpack.Marshal(map[string]any{
		"request_id": deterministicHarnessIdentifier("request", key),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := h.cellsByName["commerce"].Call(ctx, "commerce.extension.get.v1", request)
	if err != nil {
		t.Fatalf("query Commerce extension saga: %v", err)
	}
	var result struct {
		OK    bool `msgpack:"ok"`
		Value struct {
			Order struct {
				ExtendServerID string `msgpack:"extend_server_id"`
			} `msgpack:"order"`
			Server struct {
				ID string `msgpack:"id"`
			} `msgpack:"server"`
		} `msgpack:"value"`
	}
	if err := msgpack.Unmarshal(raw, &result); err != nil || !result.OK {
		t.Fatalf("decode Commerce extension saga: result=%#v err=%v", result, err)
	}
	if result.Value.Order.ExtendServerID != serverID || result.Value.Server.ID != serverID {
		t.Fatalf("extension lost target server identity: saga=%#v want=%s", result.Value, serverID)
	}
}

// TestEvolution_CheckExpirations_ActiveNotYetExpiring ports
// TestCheckExpirations_ActiveNotYetExpiring: a fresh active server (plenty of
// term remaining) stays active across driven ticks — checkExpirations does NOT
// prematurely move it out of the active state.
func TestEvolution_CheckExpirations_ActiveNotYetExpiring(t *testing.T) {
	h, _ := startEvolutionDowntime(t)
	now := time.Now().UTC()
	const serverID = "fleet-fresh-server"
	upsertFleetServer(t, h, serverID, "active", now.Add(30*24*time.Hour).Format(time.RFC3339Nano))
	dispatchFleetMaintenance(t, h, "maintenance-fresh-server", now)
	if state := fleetServerForID(t, h, serverID).Status; state != "active" {
		t.Fatalf("fresh Fleet server should stay active, got %q", state)
	}
}

// TestEvolution_CheckExpirations_NoChange ports TestCheckExpirations_NoChange:
// with no servers present, driven ticks make no server-state changes and the
// cell stays healthy.
func TestEvolution_CheckExpirations_NoChange(t *testing.T) {
	h, _ := startEvolutionDowntime(t)
	dispatchFleetMaintenance(t, h, "maintenance-empty-fleet", time.Now().UTC())
	if queue := fleetQueue(t, h); len(queue) != 0 {
		t.Fatalf("empty Fleet maintenance created queue state: %#v", queue)
	}
	if s, _ := h.Do("GET", "/health", nil, nil); s != 200 {
		t.Fatalf("cell unhealthy after empty owner maintenance: /health = %d", s)
	}
}
