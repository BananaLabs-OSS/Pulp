package host

// PORT of the DB-effect slice of Evolution/internal/router/stripe_webhook_test.go
// (CouponUsesIncrementsOnSuccess, DisputeFlagsOrder), driven THROUGH the real
// Evolution cell's POST /api/webhooks/stripe. The native test reconstructs the
// webhook handler; this drives the cell's REAL handler (signature verify ->
// stripe_events dedup -> event dispatch) and asserts its OWN DB writes.
//
// The shared stripe stub's webhook_verify returns "valid" by default (Layer D's
// opt-in verification is left OFF here), so these ports exercise the post-verify
// business logic exactly as the native tests do with a correctly-signed body.

import (
	"testing"
	"time"
)

func postStripeWebhook(t *testing.T, h *CellHarness, payload string) (int, []byte) {
	t.Helper()
	return h.Do("POST", "/api/webhooks/stripe",
		map[string]string{"Content-Type": "application/json", "Stripe-Signature": "t=1,v1=stub"},
		[]byte(payload))
}

// TestEvolution_Webhook_DisputeFlagsOrder ports TestWebhook_DisputeFlagsOrder:
// a charge.dispute.created for a known order flags it as disputed.
func TestEvolution_Webhook_DisputeFlagsOrder(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO orders (id, stripe_session_id, server_type, email, status, auto_redeem, amount_cents, created_at)
		 VALUES ('ord-disp','pi_dispute','minecraft','d@example.com','fulfilled',1,1400,?)`, now,
	); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	checkpoint(db)

	status, b := postStripeWebhook(t, h,
		`{"id":"evt-disp-1","type":"charge.dispute.created","data":{"object":{"payment_intent":{"id":"pi_dispute"}}}}`)
	if status != 200 {
		t.Fatalf("dispute webhook: want 200, got %d (%s)", status, b)
	}

	var st string
	if err := db.QueryRow(`SELECT status FROM orders WHERE id = 'ord-disp'`).Scan(&st); err != nil {
		t.Fatalf("read order status: %v", err)
	}
	if st != "disputed" {
		t.Fatalf("expected order flagged disputed, got %q", st)
	}
}

// TestEvolution_Webhook_CouponUsesIncrements ports
// TestWebhook_CouponUsesIncrementsOnSuccess: a payment_intent.succeeded for a
// pending auto-redeem order that carries a coupon increments the coupon's uses
// (in the pending->purchased tx) before the no-gene fallback flips it to paid.
func TestEvolution_Webhook_CouponUsesIncrements(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO coupons (id, code, discount_cents, max_uses, uses, created_at, stripe_promotion_code_id, stripe_coupon_id)
		 VALUES ('cpn-w','TEST5',200,5,0,?,'','')`, now,
	); err != nil {
		t.Fatalf("seed coupon: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO orders (id, stripe_session_id, server_type, email, status, auto_redeem, amount_cents, coupon_id, created_at)
		 VALUES ('ord-cpn','pi_coupon','minecraft','c@example.com','pending',1,100,'cpn-w',?)`, now,
	); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	checkpoint(db)

	status, b := postStripeWebhook(t, h,
		`{"id":"evt-cpn-1","type":"payment_intent.succeeded","data":{"object":{"id":"pi_coupon","amount_received":100}}}`)
	if status != 200 {
		t.Fatalf("coupon webhook: want 200, got %d (%s)", status, b)
	}

	var uses int
	if err := db.QueryRow(`SELECT uses FROM coupons WHERE id = 'cpn-w'`).Scan(&uses); err != nil {
		t.Fatalf("read coupon uses: %v", err)
	}
	if uses != 1 {
		t.Fatalf("expected coupon uses=1 after success, got %d", uses)
	}
}
