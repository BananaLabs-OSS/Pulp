package host

// PORT of TestCheckout_LargeDiscountMakesFree from
// Evolution/internal/router/checkout_gates_test.go, driven THROUGH the real
// Evolution cell's POST /api/checkout.
//
// A coupon whose discount exceeds the catalog price clamps the amount to 0 and
// takes the FREE path: the cell skips PaymentIntent creation, sends the
// confirmation inline, and (auto-redeem default) flips the order to paid via the
// no-gene fallback. The response carries amount_cents 0 + free:true + order_id.
//
// STUB NOTE: unlike the ADR's earlier framing, this cell's free path does NOT
// mint a Stripe invoice — sendOrderConfirmed + the no-gene fallback complete the
// flow with the existing stripe stub, so no invoice-aware stub is needed. The
// $0 free path is fully reachable.

import (
	"testing"
)

// TestEvolution_Checkout_LargeDiscountMakesFree ports
// TestCheckout_LargeDiscountMakesFree: a discount larger than the price -> $0 +
// free. The cell's free-path response carries free:true + order_id (it has no
// separate amount_cents/is_free wire fields the native mini-router synthesized),
// so we assert free:true on the response and read the clamped amount off the
// created order — the same $0 outcome the native case pins.
func TestEvolution_Checkout_LargeDiscountMakesFree(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	seedCoupon(t, db, "FREE", 99999, 0, 0, nil) // discount >> 1400 price

	status, out := postCheckout(t, h, map[string]any{
		"email":      "u@example.com",
		"promo_code": "FREE",
	})
	if status != 200 {
		t.Fatalf("large-discount checkout: want 200, got %d (%v)", status, out)
	}
	if out["free"] != true {
		t.Fatalf("expected free:true, got %v (%v)", out["free"], out)
	}
	orderID, _ := out["order_id"].(string)
	if orderID == "" {
		t.Fatalf("expected order_id on a free checkout, got %v", out)
	}
	var amount int
	if err := db.QueryRow(`SELECT amount_cents FROM orders WHERE id = ?`, orderID).Scan(&amount); err != nil {
		t.Fatalf("read free order amount: %v", err)
	}
	if amount != 0 {
		t.Fatalf("large discount should clamp order amount to 0, got %d", amount)
	}
}
