package host

// PORT of Evolution/internal/poller/validate_modfit_test.go + validate_test.go +
// the resolveDiscount slice of internal/router/resolve_discount_test.go /
// handlers_integration_test.go (TestValidateCode_*), driven THROUGH the real
// Evolution cell under the Pulp host.
//
// The native tests call p.ValidateModFit / parseModsJSON / resolveDiscount
// directly. The cell exposes the same logic through two real customer endpoints:
//   - POST /api/validate-config -> validateModFit(db, template, tierID, mods, email)
//   - GET  /api/validate-code?code=... -> resolveDiscount(db, code)
// so these ports seed the tier/coupon reference rows on the cell's own SQLite and
// assert the endpoint's verdict — the same behaviour the mirror pins, reached
// through the cell's real handler rather than a direct method call.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func postValidateConfig(t *testing.T, h *CellHarness, modsJSON string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"template":  "minecraft",
		"mods_json": modsJSON,
		"email":     "cfg@example.com",
	})
	status, b := h.Do("POST", "/api/validate-config",
		map[string]string{"Content-Type": "application/json"}, body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return status, out
}

// TestEvolution_ValidateConfig_EmptyModsOK ports TestValidateModFit_Empty /
// TestParseModsJSON_Empty: no mods -> status "ok".
func TestEvolution_ValidateConfig_EmptyModsOK(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db) // one enabled tier (standard, 2.0cpu/4096mb) + minecraft gv

	status, out := postValidateConfig(t, h, "")
	if status != 200 {
		t.Fatalf("validate-config empty mods: want 200, got %d (%v)", status, out)
	}
	if out["status"] != "ok" {
		t.Fatalf("empty mods should be ok, got %v", out["status"])
	}
}

// TestEvolution_ValidateConfig_TooHeavy ports TestValidateModFit_TooHeavy_NoUpgradeAvailable:
// mods over the only tier's ceiling, with no higher tier, -> too_heavy.
func TestEvolution_ValidateConfig_TooHeavy(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)

	// The seeded tier is 2.0 CPU / 4096 MB. A 3.0-CPU mod exceeds it, and there
	// is no higher tier to upgrade to -> too_heavy.
	status, out := postValidateConfig(t, h, `[{"id":"massive","cpu_weight":3.0,"ram_weight_mb":6000}]`)
	if status != 200 {
		t.Fatalf("validate-config heavy mods: want 200, got %d (%v)", status, out)
	}
	if out["status"] != "too_heavy" {
		t.Fatalf("over-ceiling mods with no upgrade should be too_heavy, got %v", out["status"])
	}
}

// TestEvolution_ValidateConfig_InvalidJSON ports TestValidateModFit_InvalidJSON /
// TestParseModsJSON_InvalidJSON: malformed mods_json -> 400.
func TestEvolution_ValidateConfig_InvalidJSON(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)

	status, out := postValidateConfig(t, h, `{not json`)
	if status != 400 {
		t.Fatalf("validate-config invalid json: want 400, got %d (%v)", status, out)
	}
}

// seedCoupon inserts a coupon reference row on the cell's connection.
func seedCoupon(t *testing.T, db *sql.DB, code string, discountCents, maxUses, uses int, expiresAt *time.Time) {
	t.Helper()
	now := time.Now().UTC()
	var exp any
	if expiresAt != nil {
		exp = *expiresAt
	}
	if _, err := db.Exec(
		`INSERT INTO coupons (id, code, discount_cents, max_uses, uses, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"cpn-"+code, code, discountCents, maxUses, uses, exp, now,
	); err != nil {
		t.Fatalf("seed coupon %s: %v", code, err)
	}
	checkpoint(db)
}

func getValidateCode(t *testing.T, h *CellHarness, code string) (int, map[string]any) {
	t.Helper()
	status, b := h.Do("GET", "/api/validate-code?code="+code, nil, nil)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return status, out
}

// TestEvolution_ValidateCode_Missing ports TestValidateCode_Missing: no code -> 400.
func TestEvolution_ValidateCode_Missing(t *testing.T) {
	h, _ := startEvolutionDowntime(t)
	if status, out := getValidateCode(t, h, ""); status != 400 {
		t.Fatalf("validate-code missing: want 400, got %d (%v)", status, out)
	}
}

// TestEvolution_ValidateCode_Unknown ports TestValidateCode_Invalid /
// TestResolveDiscount_NonexistentCode: unknown code -> 404 valid:false.
func TestEvolution_ValidateCode_Unknown(t *testing.T) {
	h, _ := startEvolutionDowntime(t)
	status, out := getValidateCode(t, h, "NOPE")
	if status != 404 {
		t.Fatalf("validate-code unknown: want 404, got %d (%v)", status, out)
	}
	if out["valid"] != false {
		t.Fatalf("unknown code should be valid:false, got %v", out["valid"])
	}
}

// TestEvolution_ValidateCode_Valid ports TestValidateCode_Valid /
// TestResolveDiscount_ActivePromotion(coupon path): a real coupon -> 200 with
// its discount_cents.
func TestEvolution_ValidateCode_Valid(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedCoupon(t, db, "SAVE5", 500, 0, 0, nil)

	status, out := getValidateCode(t, h, "SAVE5")
	if status != 200 {
		t.Fatalf("validate-code valid: want 200, got %d (%v)", status, out)
	}
	if fmt.Sprintf("%v", out["discount_cents"]) != "500" {
		t.Fatalf("expected discount_cents 500, got %v", out["discount_cents"])
	}
}

// TestEvolution_ValidateCode_ExpiredRejected ports TestResolveDiscount_ExpiredCouponRejected:
// an expired coupon -> 404.
func TestEvolution_ValidateCode_ExpiredRejected(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	past := time.Now().UTC().Add(-1 * time.Hour)
	seedCoupon(t, db, "OLD", 500, 0, 0, &past)

	if status, out := getValidateCode(t, h, "OLD"); status != 404 {
		t.Fatalf("validate-code expired: want 404, got %d (%v)", status, out)
	}
}

// TestEvolution_ValidateCode_ExhaustedRejected ports TestResolveDiscount_ExhaustedCouponRejected:
// a coupon with uses >= max_uses -> 404.
func TestEvolution_ValidateCode_ExhaustedRejected(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedCoupon(t, db, "USEDUP", 500, 1, 1, nil)

	if status, out := getValidateCode(t, h, "USEDUP"); status != 404 {
		t.Fatalf("validate-code exhausted: want 404, got %d (%v)", status, out)
	}
}
