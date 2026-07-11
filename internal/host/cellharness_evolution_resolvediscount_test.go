package host

// PORT of the remaining resolveDiscount edges from
// Evolution/internal/router/resolve_discount_test.go
// (PromoPriorityOverCoupon / ZeroDiscountPromoFallsToCoupon /
// PromotionWithoutStripeID), driven THROUGH the real Evolution cell's
// GET /api/validate-code (which calls the cell's resolveDiscount and returns
// {valid, discount_cents}).
//
// The native tests call resolveDiscount(ctx, db, code) directly and inspect the
// (discount, couponID, stripePromoID) tuple. The cell endpoint surfaces only
// discount_cents, so these ports assert the DISCOUNT the resolver returns —
// which is the load-bearing choice each native case pins (promo wins over
// coupon; a zero-discount promo is skipped; a pre-cutover promo with no Stripe
// id still resolves its discount). The couponID / stripePromoID tuple fields
// are the Stripe-attach plumbing the endpoint does not echo; they are not a
// separate behavior, just the same row read the discount assertion already
// proves is selected.

import (
	"fmt"
	"testing"
)

// TestEvolution_ValidateCode_PromoPriorityOverCoupon ports
// TestResolveDiscount_PromoPriorityOverCoupon: with a promo AND a coupon under
// the same code, the active promo's discount wins.
func TestEvolution_ValidateCode_PromoPriorityOverCoupon(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedPromotion(t, db, "DOUBLE", 1000, true)
	seedCoupon(t, db, "DOUBLE", 200, 0, 0, nil)

	status, out := getValidateCode(t, h, "DOUBLE")
	if status != 200 {
		t.Fatalf("validate-code promo-priority: want 200, got %d (%v)", status, out)
	}
	if fmt.Sprintf("%v", out["discount_cents"]) != "1000" {
		t.Fatalf("promo should win over coupon: expected discount_cents 1000, got %v", out["discount_cents"])
	}
}

// TestEvolution_ValidateCode_ZeroDiscountPromoFallsToCoupon ports
// TestResolveDiscount_ZeroDiscountPromoFallsToCoupon: a promo with 0 discount is
// skipped (resolveDiscount filters discount_cents > 0), so the coupon resolves.
func TestEvolution_ValidateCode_ZeroDiscountPromoFallsToCoupon(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedPromotion(t, db, "ZERO", 0, true) // active but 0 discount -> skipped
	seedCoupon(t, db, "ZERO", 50, 0, 0, nil)

	status, out := getValidateCode(t, h, "ZERO")
	if status != 200 {
		t.Fatalf("validate-code zero-promo: want 200, got %d (%v)", status, out)
	}
	if fmt.Sprintf("%v", out["discount_cents"]) != "50" {
		t.Fatalf("zero-discount promo should fall through to coupon 50, got %v", out["discount_cents"])
	}
}

// TestEvolution_ValidateCode_PromotionWithoutStripeID ports
// TestResolveDiscount_PromotionWithoutStripeID: a pre-cutover promo row that was
// never backfilled with a Stripe promotion-code id still resolves its discount
// (the local discount math is authoritative; the empty Stripe id is the caller's
// fallback signal, not an error).
func TestEvolution_ValidateCode_PromotionWithoutStripeID(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedPromotion(t, db, "LEGACY", 250, true) // seedPromotion inserts an empty stripe id

	status, out := getValidateCode(t, h, "LEGACY")
	if status != 200 {
		t.Fatalf("validate-code legacy-promo: want 200, got %d (%v)", status, out)
	}
	if fmt.Sprintf("%v", out["discount_cents"]) != "250" {
		t.Fatalf("pre-cutover promo should still resolve discount 250, got %v", out["discount_cents"])
	}
}
