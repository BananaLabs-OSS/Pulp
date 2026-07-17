package host

// A FULL FLEET RESERVES, IT NEVER REFUSES (Nick, 2026-07-17).
//
// The deploy-gate kernel's capacity check is the LAST gate it runs (ban,
// engine×tier fit, template availability and mod fit have all already passed),
// so a DenyCapacityFull means "this order is good, there's just no room right
// now". Checkout used to turn that into a 503 and refuse the customer ~200
// lines before the reserve branch could ever see it. It now falls through and
// reserves: card stored via SetupIntent, charged when the server goes live.
//
// WHY THESE TESTS EXIST AT ALL — the bug was DORMANT, not absent. Marrow's
// canFitTemplate gates on `if opts.CPUBudget > 0 && ...`, and pulp.cell.toml
// ships cpu_budget = 0, so DenyCapacityFull is unreachable in production today.
// Anyone restoring cpu_budget = 14 would silently switch checkout back to
// 503ing with no error anywhere. These proofs pin the behaviour WITH a real
// budget configured, so that config change can never resurrect the refusal.
//
// The harness therefore passes a real cpu_budget/memory_budget (see
// startEvolutionDowntimeExtra); at the default 0 the kernel simply cannot deny
// and there would be nothing to test.

import (
	"database/sql"
	"testing"
)

// capacityFullCfg is a budget smaller than one server. seedDowntimeCatalog's
// `standard` tier declares max_cpu 2.0 / max_ram_mb 4096, and Marrow's
// templateResource reads the template's resource cost straight off that tier —
// so with a 1.0-core / 1.0-GiB budget, canFitTemplate's `usedCPU+tCPU >
// CPUBudget` is true for the very FIRST server and the kernel returns
// DenyCapacityFull. No servers need to exist: an empty fleet is already "full"
// when one server cannot fit the budget.
var capacityFullCfg = map[string]any{
	"cpu_budget":    1.0,
	"memory_budget": 1.0,
}

// orderStatusFor reads the status the cell wrote for a claim token's order.
func orderStatusByEmail(t *testing.T, db *sql.DB, email string) string {
	t.Helper()
	var status string
	if err := db.QueryRow(
		`SELECT status FROM orders WHERE email = ? ORDER BY created_at DESC LIMIT 1`, email,
	).Scan(&status); err != nil {
		t.Fatalf("read order status for %s: %v", email, err)
	}
	return status
}

// CONTROL — the discriminator for the proof below.
//
// Without this, TestEvolution_Checkout_CapacityFull_PaidOrderReserves would be
// worthless: `isReserved` is ORed from cachedQueueStatus too, so a checkout
// could come back reserved for reasons that have nothing to do with the kernel
// gate this change touches. This pins that with the SAME catalog and NO budget,
// the same checkout is NOT reserved — so any "reserved" seen below is
// attributable to the capacity deny and nothing else.
func TestEvolution_Checkout_CapacityFull_ControlUnbudgetedDoesNotReserve(t *testing.T) {
	h, db := startEvolutionDowntime(t) // no cpu_budget → kernel cannot deny
	seedDowntimeCatalog(t, db)

	status, body := postCheckout(t, h, map[string]any{
		"server_type": "minecraft",
		"email":       "control@example.com",
	})
	if status != 200 {
		t.Fatalf("control checkout: want 200, got %d (%v)", status, body)
	}
	if body["reserved"] == true {
		t.Fatalf("control checkout came back reserved with no capacity budget — "+
			"the reserve proof below would prove nothing, since it could not "+
			"distinguish the kernel gate from cachedQueueStatus (%v)", body)
	}
}

// THE PROOF. A capacity-denied, NON-free checkout must RESERVE, not 503.
//
// Before the fix this returned 503 "All servers are currently in use" from the
// kernel gate and the customer was refused outright.
func TestEvolution_Checkout_CapacityFull_PaidOrderReserves(t *testing.T) {
	h, db := startEvolutionDowntimeExtra(t, "", capacityFullCfg)
	seedDowntimeCatalog(t, db)

	status, body := postCheckout(t, h, map[string]any{
		"server_type": "minecraft",
		"email":       "reserved@example.com",
	})

	if status == 503 {
		t.Fatalf("full fleet REFUSED a paying customer with 503 (%v) — a full "+
			"fleet must reserve, not refuse: the capacity deny is no longer "+
			"falling through to the reserve branch", body)
	}
	if status != 200 {
		t.Fatalf("capacity-denied paid checkout: want 200, got %d (%v)", status, body)
	}
	if body["reserved"] != true {
		t.Fatalf("capacity-denied paid checkout was not reserved: want reserved=true, got %v", body)
	}
	// A SetupIntent client_secret, not a PaymentIntent one: the card is STORED,
	// nothing is captured until promoteQueue charges it on go-live.
	if cs, _ := body["client_secret"].(string); cs == "" {
		t.Errorf("reserved checkout returned no client_secret — the browser has "+
			"nothing to confirmSetup() with, so the card never gets stored (%v)", body)
	}
	if body["free"] == true {
		t.Errorf("paid order reported free=true (%v)", body)
	}
	// `reserved`, not `pending`: no Stripe webhook is coming for a SetupIntent,
	// so a pending order would sit unfulfilled forever.
	if got := orderStatusByEmail(t, db, "reserved@example.com"); got != "reserved" {
		t.Errorf("order status = %q, want \"reserved\"", got)
	}
}

// A FREE order under the same capacity deny must STILL 503 — and in the same
// shape, since PaymentSection.tsx keys off capacity_full.
//
// There is no card to store on a $0 order and nothing to charge when the server
// goes live, so there is nothing to reserve. isFree is only known after pricing
// runs, which is why the router captures the deny and defers this decision.
func TestEvolution_Checkout_CapacityFull_FreeOrderStill503s(t *testing.T) {
	h, db := startEvolutionDowntimeExtra(t, "", capacityFullCfg)
	seedDowntimeCatalog(t, db)
	// Discount == the full 1400 minecraft price → amount 0 → isFree.
	seedCoupon(t, db, "FREE100", 1400, 0, 0, nil)

	status, body := postCheckout(t, h, map[string]any{
		"server_type": "minecraft",
		"email":       "freeloader@example.com",
		"promo_code":  "FREE100",
	})

	if status != 503 {
		t.Fatalf("free order under a capacity deny: want 503 (nothing to "+
			"reserve — no card, no later charge), got %d (%v)", status, body)
	}
	if body["capacity_full"] != true {
		t.Errorf("503 body lost capacity_full — PaymentSection.tsx parses it (%v)", body)
	}
	if body["reserved"] == true {
		t.Errorf("free order was reserved; there is no card to charge on go-live (%v)", body)
	}
}

// GUARD — the fall-through must be exactly one deny reason wide.
//
// A banned user hits the same kernel with the same full fleet. Capacity is the
// kernel's LAST check and the ban is its FIRST, so a ban must still 403 and must
// never be laundered into a reserved order with a card on file.
func TestEvolution_Checkout_CapacityFull_BannedUserStill403s(t *testing.T) {
	h, db := startEvolutionDowntimeExtra(t, "", capacityFullCfg)
	seedDowntimeCatalog(t, db)
	seedUserBan(t, db, "ban-cap-1", "banned@example.com", nil)

	status, body := postCheckout(t, h, map[string]any{
		"server_type": "minecraft",
		"email":       "banned@example.com",
	})

	if status != 403 {
		t.Fatalf("banned user under a capacity deny: want 403, got %d (%v) — "+
			"the capacity fall-through has been widened past DenyCapacityFull", status, body)
	}
	if body["reserved"] == true {
		t.Errorf("banned user got a reserved order (%v)", body)
	}
}
