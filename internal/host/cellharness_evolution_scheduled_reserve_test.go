package host

// Scheduled orders are RESERVES (2026-07-17, Nick): a future-dated checkout must
// store the card without charging and park as `scheduled`, then charge only when
// the server actually goes live on its start day — the same "money moves when the
// server does" rule as a full-fleet reserve. Before this, a scheduled order was
// charged upfront at booking. These drive the real evolution.wasm cell.

import (
	"testing"
)

// The checkout half: a future-dated order on a NON-full fleet still reserves. The
// reserve comes from the SCHEDULE alone (not capacity), the card is stored via a
// SetupIntent, and the order parks as `scheduled` — never charged at booking.
func TestEvolution_Scheduled_StoresCardWithoutCharging(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)

	status, body := postCheckout(t, h, map[string]any{
		"server_type":  "minecraft",
		"email":        "sched@example.com",
		"auto_redeem":  false,
		"scheduled_at": "2099-01-01T00:00:00Z",
	})
	if status != 200 {
		t.Fatalf("scheduled checkout: want 200, got %d (%v)", status, body)
	}
	// reserved=true so the browser calls confirmSetup(), not confirmPayment():
	// a scheduled order's client_secret is a SetupIntent, the card is STORED.
	if body["reserved"] != true {
		t.Fatalf("scheduled checkout not reserved: a future-dated order must store the "+
			"card without charging, got %v", body)
	}
	if cs, _ := body["client_secret"].(string); cs == "" {
		t.Errorf("scheduled checkout returned no client_secret to confirmSetup() with (%v)", body)
	}
	if body["free"] == true {
		t.Errorf("paid scheduled order reported free=true (%v)", body)
	}
	// Parks as `scheduled` (waits for its day) — NOT `reserved` (which would deploy
	// the instant a slot frees) and NOT `paid` (charged upfront, the old behavior).
	if got := orderStatusByEmail(t, db, "sched@example.com"); got != "scheduled" {
		t.Fatalf("scheduled order status = %q, want \"scheduled\"", got)
	}
	// Card stored, nothing charged.
	var setupID string
	var charged bool
	if err := db.QueryRow(
		`SELECT COALESCE(stripe_setup_intent_id, ''), charged_at IS NOT NULL
		   FROM orders WHERE email = 'sched@example.com'`,
	).Scan(&setupID, &charged); err != nil {
		t.Fatalf("read scheduled order: %v", err)
	}
	if setupID == "" {
		t.Errorf("scheduled order has no stripe_setup_intent_id — no card stored to charge on the start day")
	}
	if charged {
		t.Errorf("scheduled order was CHARGED at booking; it must charge only when it goes live")
	}
}

// The full chain: on the start day the scheduled order activates to `reserved`,
// gets enqueued, and promoteQueue charges the stored card off-session as the
// server deploys. Money moves on the start day, not at booking.
func TestEvolution_Scheduled_ActivatesToReservedAndChargesOnStartDay(t *testing.T) {
	setStripeStubPIStatus("succeeded")
	t.Cleanup(func() { setStripeStubPIStatus("requires_capture") })
	setStripeStubSetupPM(t, "pm_stub_card") // customer completed the SetupIntent in-browser

	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)

	status, body := postCheckout(t, h, map[string]any{
		"server_type":  "minecraft",
		"email":        "schedchg@example.com",
		"auto_redeem":  false,
		"scheduled_at": "2099-01-01T00:00:00Z",
	})
	if status != 200 || body["reserved"] != true {
		t.Fatalf("scheduled checkout precondition: want 200 + reserved, got %d (%v)", status, body)
	}
	if got := orderStatusByEmail(t, db, "schedchg@example.com"); got != "scheduled" {
		t.Fatalf("precondition: scheduled order should park as `scheduled`, got %q", got)
	}

	// The start day arrives: backdate scheduled_at so activateScheduledOrders
	// promotes it on the next tick.
	if _, err := db.Exec(
		`UPDATE orders SET scheduled_at = '2020-01-01T00:00:00Z' WHERE email = 'schedchg@example.com'`,
	); err != nil {
		t.Fatalf("backdate scheduled_at: %v", err)
	}
	checkpoint(db)

	// scheduled -> reserved (activateScheduledOrders) -> enqueue -> promoteQueue
	// charges the stored card BEFORE flipping the server. Wait on the SERVER, not
	// on orders.status: a successful charge races through to provisioning.
	driveUntil(t, h, db, "scheduled order to activate, charge, and deploy", func() bool {
		var state string
		_ = db.QueryRow(
			`SELECT state FROM servers
			   WHERE order_id = (SELECT id FROM orders WHERE email = 'schedchg@example.com')`,
		).Scan(&state)
		return state == "provisioning" || state == "active"
	})

	// charged_at is the double-charge guard — NOT NULL means the money moved.
	var charged bool
	if err := db.QueryRow(
		`SELECT charged_at IS NOT NULL FROM orders WHERE email = 'schedchg@example.com'`,
	).Scan(&charged); err != nil {
		t.Fatalf("read charged_at: %v", err)
	}
	if !charged {
		t.Errorf("scheduled order deployed without charged_at set — the off-session charge " +
			"never fired on the start day")
	}
	// The pi_ swap, same refund-path guard as full-fleet reserve: a charged order
	// still carrying its seti_ id fails every refund path silently.
	var ref string
	if err := db.QueryRow(
		`SELECT stripe_session_id FROM orders WHERE email = 'schedchg@example.com'`,
	).Scan(&ref); err != nil {
		t.Fatalf("read stripe ref: %v", err)
	}
	if len(ref) < 3 || ref[:3] != "pi_" {
		t.Errorf("stripe_session_id not swapped to the real pi_ id after charge, got %q — "+
			"every refund path gates on the pi_ prefix", ref)
	}
}
