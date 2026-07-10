package host

// PORT of Evolution/internal/poller/transitions_test.go (+ the enqueue slice of
// poller_test.go) driven THROUGH the real Evolution cell (evolution.wasm) under
// the Pulp host, WITHOUT the native internal/poller mirror.
//
// The native transitions_test calls p.enqueueNewOrders / p.checkExpirations
// directly on a native *poller. The cell links only under GOOS=wasip1 + the Pulp
// host, so it cannot be `go test`-ed that way. This harness instead seeds the
// order/server rows on the cell's own SQLite (the same reference-data pattern the
// downtime harness uses) and DRIVES the poller's mainTick via driveTick (an
// inbound request runs OnStep -> poll.tickIfDue -> mainTick ->
// enqueueNewOrders + promoteQueue + checkExpirations), then asserts the poller's
// OWN writes.
//
// NOTE on granularity: one driven mainTick runs enqueue + promote + provision in
// the same cycle, so the transient `queued` state the native unit test catches by
// calling enqueueNewOrders in isolation is not separately observable here. These
// ports therefore assert the OBSERVABLE outcome of the transition (a server row
// is / is not created for the order; an active server past its expiry leaves the
// active state), which is exactly the behaviour the native tests pin, reached
// through the cell's real step loop rather than a direct method call.

import (
	"database/sql"
	"testing"
	"time"
)

// seedOrderRow inserts a paid-shaped order directly on the cell's connection
// (reference data the enqueue path reads), mirroring the smoke test's insert plus
// the gift flags the native TestEnqueue_* cases toggle.
func seedOrderRow(t *testing.T, db *sql.DB, id, status string, isGift, giftClaimed bool) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO orders (id, stripe_session_id, server_type, tier_id, email, status, auto_redeem, is_gift, gift_claimed, created_at)
		 VALUES (?, ?, 'minecraft', 'standard', ?, ?, 0, ?, ?, ?)`,
		id, "ss_"+id, id+"@e.com", status, boolInt(isGift), boolInt(giftClaimed), now,
	); err != nil {
		t.Fatalf("seed order %s: %v", id, err)
	}
	checkpoint(db)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func serverCountForOrder(t *testing.T, db *sql.DB, orderID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM servers WHERE order_id = ?`, orderID).Scan(&n); err != nil {
		t.Fatalf("count servers for %s: %v", orderID, err)
	}
	return n
}

// TestEvolution_Enqueue_CreatesServerForPaidOrder ports
// TestEnqueue_CreatesServerAndQueueEntry: a paid order the poller sees gets a
// server row (with the minecraft template + a generated share token) created by
// the cell's OWN enqueueNewOrders.
func TestEvolution_Enqueue_CreatesServerForPaidOrder(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	seedOrderRow(t, db, "ord-paid", "paid", false, false)

	var template, shareToken string
	driveUntil(t, h, db, "paid order enqueued to a server", func() bool {
		return db.QueryRow(
			`SELECT template, share_token FROM servers WHERE order_id = 'ord-paid'`,
		).Scan(&template, &shareToken) == nil
	})
	if template != "minecraft" {
		t.Fatalf("server template = %q, want minecraft", template)
	}
	if shareToken == "" {
		t.Fatal("expected a generated share_token on the enqueued server")
	}
}

// TestEvolution_Enqueue_SkipsNonPaidOrders ports TestEnqueue_SkipsNonPaidOrders:
// a pending order is never turned into a server.
func TestEvolution_Enqueue_SkipsNonPaidOrders(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	seedOrderRow(t, db, "ord-pending", "pending", false, false)

	// Pump several ticks; a pending order must NOT produce a server.
	for i := 0; i < 12; i++ {
		driveTick(h, db)
		time.Sleep(20 * time.Millisecond)
	}
	if n := serverCountForOrder(t, db, "ord-pending"); n != 0 {
		t.Fatalf("pending order must not be enqueued, got %d servers", n)
	}
}

// TestEvolution_Enqueue_SkipsUnclaimedGifts ports TestEnqueue_SkipsUnclaimedGifts:
// a paid but unclaimed gift is not provisioned.
func TestEvolution_Enqueue_SkipsUnclaimedGifts(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	seedOrderRow(t, db, "ord-gift-unclaimed", "paid", true, false)

	for i := 0; i < 12; i++ {
		driveTick(h, db)
		time.Sleep(20 * time.Millisecond)
	}
	if n := serverCountForOrder(t, db, "ord-gift-unclaimed"); n != 0 {
		t.Fatalf("unclaimed gift must not be enqueued, got %d servers", n)
	}
}

// TestEvolution_Enqueue_EnqueuesClaimedGifts ports TestEnqueue_EnqueuesClaimedGifts:
// a claimed gift IS provisioned.
func TestEvolution_Enqueue_EnqueuesClaimedGifts(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	seedOrderRow(t, db, "ord-gift-claimed", "paid", true, true)

	driveUntil(t, h, db, "claimed gift enqueued to a server", func() bool {
		return serverCountForOrder(t, db, "ord-gift-claimed") > 0
	})
}

// TestEvolution_CheckExpirations_ActiveServerExpires ports the
// TestCheckExpirations_ExpiringToExpired / _ActiveToExpiring behaviour: a REAL
// active server (provisioned through the cell) whose expires_at is backdated past
// now leaves the active state on a driven tick — the cell's own checkExpirations
// runs the active->expiring->expired transition.
func TestEvolution_CheckExpirations_ActiveServerExpires(t *testing.T) {
	h, db := startEvolutionDowntime(t)

	srvID, _, _, _ := provisionActiveServer(t, h, db, "expire@example.com")

	// Backdate promoted_at + expires_at so the server is well past its term.
	past := time.Now().UTC().Add(-24 * time.Hour)
	if _, err := db.Exec(
		`UPDATE servers SET promoted_at = ?, expires_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-30*24*time.Hour), past, srvID,
	); err != nil {
		t.Fatalf("backdate expiry for %s: %v", srvID, err)
	}
	checkpoint(db)

	driveUntil(t, h, db, "active server past expiry leaves active state", func() bool {
		var state string
		if db.QueryRow(`SELECT state FROM servers WHERE id = ?`, srvID).Scan(&state) != nil {
			return false
		}
		return state != "active"
	})

	var state string
	if err := db.QueryRow(`SELECT state FROM servers WHERE id = ?`, srvID).Scan(&state); err != nil {
		t.Fatalf("read final state: %v", err)
	}
	// The native fallback path fires both transitions in one tick -> expired.
	if state != "expired" && state != "expiring" {
		t.Fatalf("expected expiring/expired after backdated expiry, got %q", state)
	}
}
