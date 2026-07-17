package host

// "Reserve now, charge when live" — the CHARGE, driven through the real cell.
//
// These exist because the charge-success path had NO coverage: the stripe stub
// defaults its PaymentIntent status to "requires_capture", so every reserved
// order in every other test silently took the DECLINE branch. A green suite
// proved nothing about the money actually moving. Forcing "succeeded" is what
// makes the success path real here.
//
// The load-bearing assertion is the stripe_session_id SWAP. That column is
// misnamed — it holds the PaymentIntent id — and THREE separate failure paths
// gate on `strings.HasPrefix(stripe_session_id, "pi_")`: provision-failed,
// container-never-ready, and checkRefunds. When the prefix does not match they
// each still mark the order `refunded` WITHOUT moving any money (one even logs
// "was free, skipping refund"). So a charged reserved order still carrying its
// "seti_..." id would, if provisioning then failed, leave a real customer out of
// pocket while the DB claims they were refunded — silently, no alert, nothing to
// reconcile. The swap at charge time is the only thing standing between us and
// that, which is why it gets a test of its own.

import (
	"database/sql"
	"testing"
)

// insertReservedQueued builds the state promoteQueue acts on: a reserved order
// (card on file, nothing charged) that already holds a queue position.
func insertReservedQueued(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO orders (id, stripe_session_id, server_type, email, status, amount_cents,
		                     stripe_setup_intent_id, stripe_customer_id, stripe_payment_method_id,
		                     charge_attempts, auto_redeem, created_at)
		 VALUES (?, ?, 'minecraft', ?, 'reserved', 1400, 'seti_stub_1', 'cus_stub_1', 'pm_stub_card',
		         0, true, CURRENT_TIMESTAMP)`,
		id, "seti_"+id, id+"@example.com",
	); err != nil {
		t.Fatalf("insert reserved order %s: %v", id, err)
	}
	if _, err := db.Exec(
		`INSERT INTO servers (id, order_id, template, state, cpu_weight, memory_weight, created_at, share_token)
		 VALUES (?, ?, 'minecraft', 'queued', 1, 1024, CURRENT_TIMESTAMP, ?)`,
		"srv-"+id, id, "tok-"+id,
	); err != nil {
		t.Fatalf("insert queued server for %s: %v", id, err)
	}
	if _, err := db.Exec(
		`INSERT INTO queue (id, order_id, position, created_at) VALUES (?, ?, 1, CURRENT_TIMESTAMP)`,
		"q-"+id, id,
	); err != nil {
		t.Fatalf("insert queue row for %s: %v", id, err)
	}
	checkpoint(db)
}

// The whole feature in one test: a reserved order's card is charged at the
// moment its slot opens, and ONLY then does the order become paid.
func TestEvolution_Charge_ReservedOrderIsChargedOnPromotion(t *testing.T) {
	setStripeStubPIStatus("succeeded")
	t.Cleanup(func() { setStripeStubPIStatus("requires_capture") })

	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	insertReservedQueued(t, db, "chg-1")

	// Wait on the SERVER, not on orders.status: a successful charge sets `paid`
	// but the order races on through provisioning to `fulfilled` within the same
	// run of ticks, so polling for "paid" would miss it and time out on a flow
	// that actually worked.
	driveUntil(t, h, db, "reserved order to be charged and promoted", func() bool {
		var state string
		_ = db.QueryRow(`SELECT state FROM servers WHERE order_id = 'chg-1'`).Scan(&state)
		return state == "provisioning" || state == "active"
	})

	// charged_at is the double-charge guard, so assert it's set rather than
	// assert on its type: NOT NULL is the whole contract.
	var hasCharged bool
	if err := db.QueryRow(
		`SELECT charged_at IS NOT NULL FROM orders WHERE id = 'chg-1'`,
	).Scan(&hasCharged); err != nil {
		t.Fatalf("read charged_at: %v", err)
	}
	if !hasCharged {
		t.Errorf("charged_at must be stamped when the money moves; it is the double-charge guard")
	}

	// THE SWAP. Without it, three separate failure paths would later mark this
	// order refunded without refunding a cent.
	var ref string
	if err := db.QueryRow(`SELECT stripe_session_id FROM orders WHERE id = 'chg-1'`).Scan(&ref); err != nil {
		t.Fatalf("read stripe ref: %v", err)
	}
	if len(ref) < 3 || ref[:3] != "pi_" {
		t.Fatalf("stripe_session_id must be swapped to the real PaymentIntent id on a successful "+
			"charge, got %q — the pi_ prefix gates every refund path, so a provisioning failure "+
			"here would mark the customer refunded without paying them back", ref)
	}
}

// Money must move only when the server does. A reserved order sitting in the
// queue with no slot free must NOT be charged early.
func TestEvolution_Charge_ReservedOrderNotChargedWhileWaiting(t *testing.T) {
	setStripeStubPIStatus("succeeded")
	t.Cleanup(func() { setStripeStubPIStatus("requires_capture") })

	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)

	// Fill the fleet so nothing can be promoted: max_servers is 12 in the
	// harness config, so 12 active servers means no slot for our reserved order.
	for i := 0; i < 12; i++ {
		if _, err := db.Exec(
			`INSERT INTO servers (id, order_id, template, state, cpu_weight, memory_weight, created_at, share_token)
			 VALUES (?, ?, 'minecraft', 'active', 1, 1024, CURRENT_TIMESTAMP, ?)`,
			sqlID("full-srv", i), sqlID("full-ord", i), sqlID("full-tok", i),
		); err != nil {
			t.Fatalf("seed active server %d: %v", i, err)
		}
	}
	checkpoint(db)

	insertReservedQueued(t, db, "chg-2")

	// Pump ticks; the order must stay reserved and uncharged.
	for i := 0; i < 12; i++ {
		driveTick(h, db)
	}

	var status string
	var hasCharged bool
	if err := db.QueryRow(
		`SELECT status, charged_at IS NOT NULL FROM orders WHERE id = 'chg-2'`,
	).Scan(&status, &hasCharged); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if hasCharged {
		t.Errorf("a reserved order was charged while still waiting for a slot: money must move " +
			"only when the server goes live")
	}
	if status == "paid" {
		t.Errorf("status went paid without a server going live; want it to stay reserved, got %q", status)
	}
}

// A charged order must never be charged twice. charged_at is the guard: a
// promote that failed after the money moved must not re-charge on the next tick.
func TestEvolution_Charge_AlreadyChargedOrderIsNotChargedAgain(t *testing.T) {
	setStripeStubPIStatus("succeeded")
	t.Cleanup(func() { setStripeStubPIStatus("requires_capture") })

	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	insertReservedQueued(t, db, "chg-3")

	// Simulate "money already moved, promotion did not finish": charged_at set,
	// the real PI id already swapped in, but still queued.
	if _, err := db.Exec(
		`UPDATE orders SET charged_at = CURRENT_TIMESTAMP, stripe_session_id = 'pi_already', status = 'paid'
		 WHERE id = 'chg-3'`,
	); err != nil {
		t.Fatalf("pre-charge order: %v", err)
	}
	checkpoint(db)

	driveUntil(t, h, db, "already-charged order to promote", func() bool {
		var state string
		_ = db.QueryRow(`SELECT state FROM servers WHERE order_id = 'chg-3'`).Scan(&state)
		return state == "provisioning" || state == "active"
	})

	// The ref must still be the ORIGINAL PaymentIntent: a second charge would
	// have overwritten it with a new pi_stub id.
	var ref string
	if err := db.QueryRow(`SELECT stripe_session_id FROM orders WHERE id = 'chg-3'`).Scan(&ref); err != nil {
		t.Fatalf("read stripe ref: %v", err)
	}
	if ref != "pi_already" {
		t.Errorf("an already-charged order was charged again: stripe_session_id changed from "+
			"pi_already to %q", ref)
	}
}

func sqlID(prefix string, i int) string {
	return prefix + "-" + string(rune('a'+i))
}
