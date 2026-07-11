package host

// PORT of Evolution/internal/router/checkout_gates_test.go, driven THROUGH the
// real Evolution cell's POST /api/checkout. The native test reconstructs the
// pre-Stripe gate logic in a mini gin router; this drives the cell's REAL
// handler (unified deploy-gate kernel + discount resolution + order/PI
// creation against the stripe stub) and asserts the same gate outcomes.
//
// Every checkout body carries age_confirmed / tos_accepted / eula_accepted so
// the compliance gates (which run before the gates under test) pass — the
// native mini-router had no such gates, so this is faithful setup, not a
// weakened assertion.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// postCheckout posts a checkout body with the compliance flags set, letting the
// caller override server_type / promo_code / mods_json / email.
func postCheckout(t *testing.T, h *CellHarness, fields map[string]any) (int, map[string]any) {
	t.Helper()
	body := map[string]any{
		"email":         "buyer@example.com",
		"age_confirmed": true,
		"tos_accepted":  true,
		"eula_accepted": true,
	}
	for k, v := range fields {
		body[k] = v
	}
	raw, _ := json.Marshal(body)
	status, b := h.Do("POST", "/api/checkout",
		map[string]string{"Content-Type": "application/json"}, raw)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return status, out
}

func seedUserBan(t *testing.T, db *sql.DB, id, email string, expiresAt *time.Time) {
	t.Helper()
	now := time.Now().UTC()
	var exp any
	if expiresAt != nil {
		exp = *expiresAt
	}
	if _, err := db.Exec(
		`INSERT INTO user_bans (id, email, reason, banned_by, expires_at, created_at)
		 VALUES (?, ?, 'fraud', 'admin', ?, ?)`,
		id, email, exp, now,
	); err != nil {
		t.Fatalf("seed user ban %s: %v", id, err)
	}
	checkpoint(db)
}

// TestEvolution_Checkout_MissingEmail ports TestCheckout_MissingEmail.
func TestEvolution_Checkout_MissingEmail(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)

	status, out := postCheckout(t, h, map[string]any{"email": ""})
	if status != 400 {
		t.Fatalf("missing email: want 400, got %d (%v)", status, out)
	}
}

// TestEvolution_Checkout_BannedUserRejected ports TestCheckout_BannedUserRejected:
// a currently-banned email is rejected by the kernel with 403.
func TestEvolution_Checkout_BannedUserRejected(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	seedUserBan(t, db, "ban-1", "banned@example.com", nil)

	status, out := postCheckout(t, h, map[string]any{"email": "banned@example.com"})
	if status != 403 {
		t.Fatalf("banned user: want 403, got %d (%v)", status, out)
	}
}

// TestEvolution_Checkout_ExpiredBanAllows ports TestCheckout_ExpiredBanAllowsCheckout:
// an expired ban must not block checkout.
func TestEvolution_Checkout_ExpiredBanAllows(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	past := time.Now().UTC().Add(-1 * time.Hour)
	seedUserBan(t, db, "ban-old", "formerly@example.com", &past)

	status, out := postCheckout(t, h, map[string]any{"email": "formerly@example.com"})
	if status == 403 {
		t.Fatalf("expired ban should not block checkout, got 403 (%v)", out)
	}
}

// TestEvolution_Checkout_InvalidPromo ports TestCheckout_InvalidPromoCode.
func TestEvolution_Checkout_InvalidPromo(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)

	status, out := postCheckout(t, h, map[string]any{
		"email":      "u@example.com",
		"promo_code": "NOPE_INVALID",
	})
	if status != 400 {
		t.Fatalf("invalid promo: want 400, got %d (%v)", status, out)
	}
}

// TestEvolution_Checkout_ValidPromoDiscount ports TestCheckout_ValidPromoProceedsWithDiscount:
// a real coupon proceeds and the response carries its discount_cents.
func TestEvolution_Checkout_ValidPromoDiscount(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	seedCoupon(t, db, "SAVE5", 500, 0, 0, nil)

	status, out := postCheckout(t, h, map[string]any{
		"email":      "u@example.com",
		"promo_code": "SAVE5",
	})
	if status != 200 {
		t.Fatalf("valid promo: want 200, got %d (%v)", status, out)
	}
	if fmt.Sprintf("%v", out["discount_cents"]) != "500" {
		t.Fatalf("expected discount_cents 500, got %v", out["discount_cents"])
	}
}

// TestEvolution_Checkout_ModsOverBudgetRejected ports TestCheckout_ModsOverBudgetRejected:
// mods that exceed the tier ladder are rejected by the kernel with 400.
func TestEvolution_Checkout_ModsOverBudgetRejected(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	seedUpgradeTier(t, db)

	status, out := postCheckout(t, h, map[string]any{
		"email":     "u@example.com",
		"mods_json": `[{"id":"big","cpu_weight":2.8,"ram_weight_mb":5000}]`,
	})
	if status != 400 {
		t.Fatalf("over-budget mods: want 400, got %d (%v)", status, out)
	}
}

// TestEvolution_Checkout_DefaultsServerType ports TestCheckout_DefaultsServerTypeToMinecraft:
// with no server_type the handler defaults to minecraft and proceeds.
func TestEvolution_Checkout_DefaultsServerType(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)

	status, out := postCheckout(t, h, map[string]any{"email": "u@example.com"})
	if status != 200 {
		t.Fatalf("default server_type: want 200, got %d (%v)", status, out)
	}
	if out["claim_token"] == nil || out["claim_token"] == "" {
		t.Fatalf("expected a claim_token on success, got %v", out)
	}
}

// TestEvolution_Checkout_HappyPath ports TestCheckout_HappyPathReturnsAmount:
// an explicit minecraft checkout creates a pending order at the catalog price.
func TestEvolution_Checkout_HappyPath(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)

	status, out := postCheckout(t, h, map[string]any{
		"email":       "happy@example.com",
		"server_type": "minecraft",
	})
	if status != 200 {
		t.Fatalf("happy path: want 200, got %d (%v)", status, out)
	}
	// A pending order was created at the standard price (1400) for this email.
	var amount int
	if err := db.QueryRow(
		`SELECT amount_cents FROM orders WHERE email = 'happy@example.com' AND status = 'pending'`,
	).Scan(&amount); err != nil {
		t.Fatalf("expected a pending order for the checkout: %v", err)
	}
	if amount != 1400 {
		t.Fatalf("expected order amount 1400 (minecraft price), got %d", amount)
	}
}
