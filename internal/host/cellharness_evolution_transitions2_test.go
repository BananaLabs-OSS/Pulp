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
	"database/sql"
	"testing"
	"time"
)

// seedServerRow inserts a server row directly on the cell's connection (the
// same reference-data pattern seedOrderRow uses), so the enqueue path can see
// an order that already owns a server.
func seedServerRow(t *testing.T, db *sql.DB, id, orderID, state string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO servers (id, order_id, template, state, created_at, cpu_weight, memory_weight, restart_count)
		 VALUES (?, ?, 'minecraft', ?, ?, 0.33, 3, 0)`,
		id, orderID, state, now,
	); err != nil {
		t.Fatalf("seed server %s: %v", id, err)
	}
	checkpoint(db)
}

// TestEvolution_Enqueue_SkipsOrdersWithExistingServers ports
// TestEnqueue_SkipsOrdersWithExistingServers: a paid order that already owns a
// server is NOT re-enqueued into a second server.
func TestEvolution_Enqueue_SkipsOrdersWithExistingServers(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	seedOrderRow(t, db, "ord-has-srv", "paid", false, false)
	seedServerRow(t, db, "srv-existing", "ord-has-srv", "active")

	// Pump several ticks; the order already has a server, so enqueue must
	// not create a second one.
	for i := 0; i < 12; i++ {
		driveTick(h, db)
		time.Sleep(20 * time.Millisecond)
	}
	if n := serverCountForOrder(t, db, "ord-has-srv"); n != 1 {
		t.Fatalf("order with existing server must not be re-enqueued, got %d servers", n)
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
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)

	for _, id := range []string{"ord-f1", "ord-f2", "ord-f3"} {
		orderID := id
		seedOrderRow(t, db, orderID, "paid", false, false)
		driveUntil(t, h, db, "paid order "+orderID+" enqueued to a server", func() bool {
			return serverCountForOrder(t, db, orderID) > 0
		})
	}

	// All three orders own exactly one server each.
	for _, id := range []string{"ord-f1", "ord-f2", "ord-f3"} {
		if n := serverCountForOrder(t, db, id); n != 1 {
			t.Fatalf("order %s should own exactly 1 server, got %d", id, n)
		}
	}
}

// TestEvolution_Enqueue_PreservesExtendServerID ports
// TestEnqueue_PreservesExtendServerID: a paid order carrying extend_server_id
// produces a server whose extends_server_id is preserved from the order.
func TestEvolution_Enqueue_PreservesExtendServerID(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)

	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO orders (id, stripe_session_id, server_type, tier_id, email, status, auto_redeem, extend_server_id, created_at)
		 VALUES ('ord-ext','ss_ord-ext','minecraft','standard','ext@e.com','paid',0,'orig-server-1',?)`, now,
	); err != nil {
		t.Fatalf("seed extend order: %v", err)
	}
	checkpoint(db)

	var extends string
	driveUntil(t, h, db, "extend order enqueued to a server", func() bool {
		return db.QueryRow(
			`SELECT extends_server_id FROM servers WHERE order_id = 'ord-ext'`,
		).Scan(&extends) == nil
	})
	if extends != "orig-server-1" {
		t.Fatalf("extends_server_id = %q, want orig-server-1", extends)
	}
}

// TestEvolution_CheckExpirations_ActiveNotYetExpiring ports
// TestCheckExpirations_ActiveNotYetExpiring: a fresh active server (plenty of
// term remaining) stays active across driven ticks — checkExpirations does NOT
// prematurely move it out of the active state.
func TestEvolution_CheckExpirations_ActiveNotYetExpiring(t *testing.T) {
	h, db := startEvolutionDowntime(t)

	srvID, _, _, _ := provisionActiveServer(t, h, db, "fresh@example.com")

	// The server was just provisioned with a full term; drive several ticks
	// and confirm it never leaves active.
	for i := 0; i < 12; i++ {
		driveTick(h, db)
		time.Sleep(20 * time.Millisecond)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM servers WHERE id = ?`, srvID).Scan(&state); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state != "active" {
		t.Fatalf("fresh server should stay active, got %q", state)
	}
}

// TestEvolution_CheckExpirations_NoChange ports TestCheckExpirations_NoChange:
// with no servers present, driven ticks make no server-state changes and the
// cell stays healthy.
func TestEvolution_CheckExpirations_NoChange(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)

	for i := 0; i < 10; i++ {
		driveTick(h, db)
		time.Sleep(15 * time.Millisecond)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM servers`).Scan(&n); err != nil {
		t.Fatalf("count servers: %v", err)
	}
	if n != 0 {
		t.Fatalf("no orders seeded, expected 0 servers, got %d", n)
	}
	// The cell still serves requests (healthy) after the empty ticks.
	if s, _ := h.Do("GET", "/health", nil, nil); s != 200 {
		t.Fatalf("cell unhealthy after empty ticks: /health = %d", s)
	}
}
