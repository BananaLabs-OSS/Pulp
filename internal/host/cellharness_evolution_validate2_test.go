package host

// PORT (second slice) of Evolution/internal/poller/validate_modfit_test.go +
// validate_test.go + the resolveDiscount slice of
// internal/router/resolve_discount_test.go, driven THROUGH the real Evolution
// cell's two public endpoints:
//   - POST /api/validate-config -> validateModFit (Marrow validate.ModFit)
//   - GET  /api/validate-code?code=... -> resolveDiscount
//
// CELL-SEMANTICS NOTES (why some native cases map differently or are gaps):
//   - The cell's /api/validate-config defaults tier_id to the lowest-sort_order
//     ENABLED tier and resolves the ceiling from that tier's max_cpu/max_ram_mb
//     (Marrow templateResource), NOT from a hardcoded per-template map. So the
//     native fake-map limits (2.5 CPU / 4.5 GiB for minecraft, 0.33/3 for an
//     unknown template) do not apply — the cell's minecraft ceiling here is the
//     seeded standard tier (2.0 CPU / 4096 MB).
//   - The cross-product catalog recommends the SAME template at a HIGHER tier
//     (recommended_tier_id), not a different template — so UpgradeNeeded asserts
//     recommended_tier_id, not the native recommended_tier=="minecraft-plus".
//   - Marrow's ModFit dropped the voucher-wallet lookup, so
//     HasVoucherForRecommended is never set through this endpoint
//     (VoucherDetection / NoVoucher are recorded as GAPs in the deliverable).

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// seedUpgradeTier adds a second, higher tier ('plus', 3.0 CPU / 6144 MB, sort
// 1) so nextTierTemplate has an upgrade rung above the seedDowntimeCatalog base
// tier ('standard', 2.0 / 4096, sort 0).
func seedUpgradeTier(t *testing.T, db *sql.DB) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO tiers (id, name, label, price_cents, duration, enabled, sort_order, max_cpu, max_ram_mb, created_at)
		 VALUES ('plus','session-plus','Session+',2000,'336h',1,1,3.0,6144,?)`, now,
	); err != nil {
		t.Fatalf("seed upgrade tier: %v", err)
	}
	checkpoint(db)
}

// postValidateConfigT posts /api/validate-config for an explicit template.
func postValidateConfigT(t *testing.T, h *CellHarness, template, modsJSON string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"template":  template,
		"mods_json": modsJSON,
		"email":     "cfg@example.com",
	})
	status, b := h.Do("POST", "/api/validate-config",
		map[string]string{"Content-Type": "application/json"}, body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return status, out
}

// TestEvolution_ValidateConfig_OkLightMods ports TestValidateModFit_Ok_LightMods:
// light mods well under the ceiling -> ok.
func TestEvolution_ValidateConfig_OkLightMods(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db) // minecraft ceiling = standard tier 2.0 CPU / 4096 MB

	status, out := postValidateConfig(t, h, `[{"id":"m1","name":"Light","cpu_weight":0.5,"ram_weight_mb":1000}]`)
	if status != 200 {
		t.Fatalf("validate-config light mods: want 200, got %d (%v)", status, out)
	}
	if out["status"] != "ok" {
		t.Fatalf("light mods should be ok, got %v", out["status"])
	}
}

// TestEvolution_ValidateConfig_Degraded ports TestValidateModFit_Degraded_NearLimit:
// mods above the 80% comfort threshold but still fitting the tier -> degraded.
func TestEvolution_ValidateConfig_Degraded(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)

	// 1.7 CPU is 85% of the 2.0 ceiling (>= 0.80 threshold) yet still fits.
	status, out := postValidateConfig(t, h, `[{"id":"m1","name":"Heavy","cpu_weight":1.7,"ram_weight_mb":2000}]`)
	if status != 200 {
		t.Fatalf("validate-config near-limit: want 200, got %d (%v)", status, out)
	}
	if out["status"] != "degraded" {
		t.Fatalf("near-limit mods should be degraded, got %v", out["status"])
	}
}

// TestEvolution_ValidateConfig_EmptyTemplate ports TestValidateModFit_EmptyTemplate:
// an empty template is rejected by validateModFit ("template is required") -> 400.
func TestEvolution_ValidateConfig_EmptyTemplate(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)

	if status, out := postValidateConfigT(t, h, "", ""); status != 400 {
		t.Fatalf("validate-config empty template: want 400, got %d (%v)", status, out)
	}
}

// TestEvolution_ValidateConfig_UpgradeNeeded ports TestValidateModFit_UpgradeNeeded:
// mods exceed the base tier but fit a higher tier -> upgrade_needed, with the
// higher tier surfaced as recommended_tier_id (cell cross-product semantics).
func TestEvolution_ValidateConfig_UpgradeNeeded(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db) // base tier standard (2.0 / 4096, sort 0)
	seedUpgradeTier(t, db)     // higher tier plus (3.0 / 6144, sort 1)

	// 2.8 CPU / 5000 MB exceeds standard, fits plus.
	status, out := postValidateConfig(t, h, `[{"id":"m1","name":"Medium","cpu_weight":2.8,"ram_weight_mb":5000}]`)
	if status != 200 {
		t.Fatalf("validate-config upgrade: want 200, got %d (%v)", status, out)
	}
	if out["status"] != "upgrade_needed" {
		t.Fatalf("over-base mods that fit a higher tier should be upgrade_needed, got %v", out["status"])
	}
	if out["recommended_tier_id"] != "plus" {
		t.Fatalf("expected recommended_tier_id=plus, got %v", out["recommended_tier_id"])
	}
}

// TestEvolution_ValidateConfig_TooHeavyWithUpgradeAvailable ports
// TestValidateModFit_TooHeavy: mods exceed even the highest tier -> too_heavy.
func TestEvolution_ValidateConfig_TooHeavyWithUpgradeAvailable(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	seedUpgradeTier(t, db)

	// 5.0 CPU exceeds even the plus tier (3.0).
	status, out := postValidateConfig(t, h, `[{"id":"m1","name":"Massive","cpu_weight":5.0,"ram_weight_mb":8000}]`)
	if status != 200 {
		t.Fatalf("validate-config too-heavy: want 200, got %d (%v)", status, out)
	}
	if out["status"] != "too_heavy" {
		t.Fatalf("mods over the top tier should be too_heavy, got %v", out["status"])
	}
}

// --- resolveDiscount edges via GET /api/validate-code ---

// TestEvolution_ValidateCode_Whitespace ports TestResolveDiscount_WhitespaceCode:
// a whitespace-only code trims to empty; resolveDiscount returns (0,"") with no
// error, and the endpoint surfaces valid:true discount 0. (The endpoint only
// 400s when the code query param is entirely absent — see the Missing port.)
func TestEvolution_ValidateCode_Whitespace(t *testing.T) {
	h, _ := startEvolutionDowntime(t)
	status, out := getValidateCode(t, h, "%20%20")
	if status != 200 {
		t.Fatalf("validate-code whitespace: want 200 (trims to empty, no error), got %d (%v)", status, out)
	}
	if out["valid"] != true {
		t.Fatalf("whitespace code should resolve valid:true discount 0, got %v", out)
	}
	if fmt.Sprintf("%v", out["discount_cents"]) != "0" {
		t.Fatalf("whitespace code discount should be 0, got %v", out["discount_cents"])
	}
}

// TestEvolution_ValidateCode_CaseInsensitive ports
// TestResolveDiscount_CaseInsensitive: a lowercase request matches an
// uppercase-stored coupon (resolveDiscount uppercases both sides).
func TestEvolution_ValidateCode_CaseInsensitive(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedCoupon(t, db, "SAVEME", 750, 0, 0, nil)

	status, out := getValidateCode(t, h, "saveme")
	if status != 200 {
		t.Fatalf("validate-code case-insensitive: want 200, got %d (%v)", status, out)
	}
	if fmt.Sprintf("%v", out["discount_cents"]) != "750" {
		t.Fatalf("expected discount_cents 750, got %v", out["discount_cents"])
	}
}

// TestEvolution_ValidateCode_UnlimitedCouponAllowed ports
// TestResolveDiscount_UnlimitedCouponAllowed: a coupon with max_uses=0 is never
// exhausted regardless of uses.
func TestEvolution_ValidateCode_UnlimitedCouponAllowed(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedCoupon(t, db, "UNLIMITED", 300, 0, 999, nil) // max_uses 0 => unlimited

	status, out := getValidateCode(t, h, "UNLIMITED")
	if status != 200 {
		t.Fatalf("validate-code unlimited: want 200, got %d (%v)", status, out)
	}
	if fmt.Sprintf("%v", out["discount_cents"]) != "300" {
		t.Fatalf("expected discount_cents 300, got %v", out["discount_cents"])
	}
}

// TestEvolution_ValidateCode_NearExpiryAllowed ports
// TestResolveDiscount_NearExpiryCouponAllowed: a coupon whose expiry is still in
// the future (even barely) resolves.
func TestEvolution_ValidateCode_NearExpiryAllowed(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	future := time.Now().UTC().Add(30 * time.Minute)
	seedCoupon(t, db, "SOON", 400, 0, 0, &future)

	status, out := getValidateCode(t, h, "SOON")
	if status != 200 {
		t.Fatalf("validate-code near-expiry: want 200, got %d (%v)", status, out)
	}
	if fmt.Sprintf("%v", out["discount_cents"]) != "400" {
		t.Fatalf("expected discount_cents 400, got %v", out["discount_cents"])
	}
}

// TestEvolution_ValidateCode_ActivePromotion ports
// TestResolveDiscount_ActivePromotion: an active promotion with discount>0
// resolves ahead of the coupon table.
func TestEvolution_ValidateCode_ActivePromotion(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedPromotion(t, db, "LAUNCH", 1000, true)

	status, out := getValidateCode(t, h, "LAUNCH")
	if status != 200 {
		t.Fatalf("validate-code active promo: want 200, got %d (%v)", status, out)
	}
	if fmt.Sprintf("%v", out["discount_cents"]) != "1000" {
		t.Fatalf("expected discount_cents 1000, got %v", out["discount_cents"])
	}
}

// TestEvolution_ValidateCode_InactivePromoRejected ports the inactive-promotion
// half of TestResolveDiscount_InactivePromoFallsToCoupon: an inactive promotion
// with no matching coupon does not resolve -> 404.
func TestEvolution_ValidateCode_InactivePromoRejected(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedPromotion(t, db, "OLDPROMO", 1000, false) // active=false

	if status, out := getValidateCode(t, h, "OLDPROMO"); status != 404 {
		t.Fatalf("validate-code inactive promo: want 404, got %d (%v)", status, out)
	}
}

// seedPromotion inserts a promotions reference row on the cell's connection.
func seedPromotion(t *testing.T, db *sql.DB, code string, discountCents int, active bool) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO promotions (id, message, code, discount_cents, require_email, active, created_at, stripe_promotion_code_id, stripe_coupon_id)
		 VALUES (?, ?, ?, ?, 0, ?, ?, '', '')`,
		"promo-"+code, code+" promo", code, discountCents, boolInt(active), now,
	); err != nil {
		t.Fatalf("seed promotion %s: %v", code, err)
	}
	checkpoint(db)
}
