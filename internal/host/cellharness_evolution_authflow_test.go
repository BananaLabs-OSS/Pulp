package host

// PORT of Evolution/internal/router/auth_flow_test.go, driven THROUGH the real
// Evolution cell's GET /api/claim and POST /api/magic-link. The native test
// reconstructs these handlers in a mini gin router; this drives the cell's REAL
// handlers over the harness's inbound HTTP and asserts the same outcomes,
// seeding claim_tokens / orders on the cell's own SQLite.

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"
)

func seedClaimToken(t *testing.T, db *sql.DB, token, email string, claimed bool, expiresAt time.Time) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO claim_tokens (id, email, token, claimed, expires_at, created_at, ip_address)
		 VALUES (?, ?, ?, ?, ?, ?, '')`,
		"ct-"+token, email, token, boolInt(claimed), expiresAt, now,
	); err != nil {
		t.Fatalf("seed claim token %s: %v", token, err)
	}
	checkpoint(db)
}

func getClaim(t *testing.T, h *CellHarness, token string) (int, map[string]any) {
	t.Helper()
	path := "/api/claim"
	if token != "" {
		path += "?token=" + token
	}
	status, b := h.Do("GET", path, nil, nil)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return status, out
}

func postMagicLink(t *testing.T, h *CellHarness, email string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"email": email})
	status, b := h.Do("POST", "/api/magic-link",
		map[string]string{"Content-Type": "application/json"}, raw)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return status, out
}

func claimTokenCountForEmail(t *testing.T, db *sql.DB, email string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claim_tokens WHERE LOWER(email) = LOWER(?)`, email).Scan(&n); err != nil {
		t.Fatalf("count claim tokens for %s: %v", email, err)
	}
	return n
}

// --- /api/claim ---

// TestEvolution_Claim_MissingToken ports TestClaim_MissingToken: no token -> 400.
func TestEvolution_Claim_MissingToken(t *testing.T) {
	h, _ := startEvolutionDowntime(t)
	if status, out := getClaim(t, h, ""); status != 400 {
		t.Fatalf("claim missing token: want 400, got %d (%v)", status, out)
	}
}

// TestEvolution_Claim_InvalidToken ports TestClaim_InvalidToken: unknown token -> 401.
func TestEvolution_Claim_InvalidToken(t *testing.T) {
	h, _ := startEvolutionDowntime(t)
	if status, out := getClaim(t, h, "does-not-exist"); status != 401 {
		t.Fatalf("claim invalid token: want 401, got %d (%v)", status, out)
	}
}

// TestEvolution_Claim_ExpiredToken ports TestClaim_ExpiredTokenRejected: an
// expired (but unclaimed) token -> 401.
func TestEvolution_Claim_ExpiredToken(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedClaimToken(t, db, "expired-tok", "a@example.com", false, time.Now().UTC().Add(-1*time.Hour))
	if status, out := getClaim(t, h, "expired-tok"); status != 401 {
		t.Fatalf("claim expired token: want 401, got %d (%v)", status, out)
	}
}

// TestEvolution_Claim_AlreadyClaimed ports TestClaim_AlreadyClaimedRejected: a
// token already marked claimed -> 401.
func TestEvolution_Claim_AlreadyClaimed(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedClaimToken(t, db, "used-tok", "b@example.com", true, time.Now().UTC().Add(1*time.Hour))
	if status, out := getClaim(t, h, "used-tok"); status != 401 {
		t.Fatalf("claim already-claimed token: want 401, got %d (%v)", status, out)
	}
}

// TestEvolution_Claim_ValidTokenReturnsEmail ports TestClaim_ValidTokenReturnsEmail:
// a valid token -> 200 with the associated email.
func TestEvolution_Claim_ValidTokenReturnsEmail(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedClaimToken(t, db, "good-tok", "owner@example.com", false, time.Now().UTC().Add(1*time.Hour))
	status, out := getClaim(t, h, "good-tok")
	if status != 200 {
		t.Fatalf("claim valid token: want 200, got %d (%v)", status, out)
	}
	if out["email"] != "owner@example.com" {
		t.Fatalf("claim should return the token email, got %v", out["email"])
	}
}

// TestEvolution_Claim_TokenIsConsumed ports TestClaim_TokenIsConsumed: a token
// works once, then is consumed (second claim -> 401).
func TestEvolution_Claim_TokenIsConsumed(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedClaimToken(t, db, "once-tok", "once@example.com", false, time.Now().UTC().Add(1*time.Hour))

	if status, _ := getClaim(t, h, "once-tok"); status != 200 {
		t.Fatalf("first claim should succeed, got %d", status)
	}
	if status, out := getClaim(t, h, "once-tok"); status != 401 {
		t.Fatalf("second claim of a consumed token: want 401, got %d (%v)", status, out)
	}
}

// --- /api/magic-link ---

// TestEvolution_MagicLink_MissingEmail ports TestMagicLink_MissingEmail: -> 400.
func TestEvolution_MagicLink_MissingEmail(t *testing.T) {
	h, _ := startEvolutionDowntime(t)
	if status, out := postMagicLink(t, h, ""); status != 400 {
		t.Fatalf("magic-link missing email: want 400, got %d (%v)", status, out)
	}
}

// TestEvolution_MagicLink_UnknownEmailSilentSuccess ports
// TestMagicLink_UnknownEmailSilentSuccess: an email with no footprint returns
// {sent:true} (anti-enumeration) but creates NO token.
func TestEvolution_MagicLink_UnknownEmailSilentSuccess(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	status, out := postMagicLink(t, h, "nobody@example.com")
	if status != 200 || out["sent"] != true {
		t.Fatalf("unknown email: want 200 {sent:true}, got %d (%v)", status, out)
	}
	if n := claimTokenCountForEmail(t, db, "nobody@example.com"); n != 0 {
		t.Fatalf("no token should be created for an unknown email, got %d", n)
	}
}

// TestEvolution_MagicLink_CreatesTokenForKnownUser ports
// TestMagicLink_CreatesTokenForKnownUser: an email that owns an order gets a
// claim token minted.
func TestEvolution_MagicLink_CreatesTokenForKnownUser(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO orders (id, stripe_session_id, server_type, email, status, auto_redeem, created_at)
		 VALUES ('ord-known','ss_known','minecraft','known@example.com','fulfilled',0,?)`, now,
	); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	checkpoint(db)

	status, out := postMagicLink(t, h, "known@example.com")
	if status != 200 || out["sent"] != true {
		t.Fatalf("known user: want 200 {sent:true}, got %d (%v)", status, out)
	}
	if n := claimTokenCountForEmail(t, db, "known@example.com"); n != 1 {
		t.Fatalf("expected exactly 1 token minted for a known user, got %d", n)
	}
}

// TestEvolution_MagicLink_CaseInsensitive ports TestMagicLink_CaseInsensitive:
// an upper-cased request matches a lower-cased stored order email.
func TestEvolution_MagicLink_CaseInsensitive(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO orders (id, stripe_session_id, server_type, email, status, auto_redeem, created_at)
		 VALUES ('ord-ci','ss_ci','minecraft','user@example.com','fulfilled',0,?)`, now,
	); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	checkpoint(db)

	status, out := postMagicLink(t, h, "USER@EXAMPLE.COM")
	if status != 200 || out["sent"] != true {
		t.Fatalf("case-insensitive: want 200 {sent:true}, got %d (%v)", status, out)
	}
	if n := claimTokenCountForEmail(t, db, "user@example.com"); n != 1 {
		t.Fatalf("expected a token minted for the case-insensitive match, got %d", n)
	}
}
