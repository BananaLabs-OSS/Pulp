package host

// Cell HTTP harness against the REAL deployed Hand cell (party system),
// proving the harness drives a sqlite + entropy.read cell end-to-end and
// pinning Hand's defensively-wired entropy bridge.
//
// Hand declares transport.http.inbound + storage.sqlite + entropy.read.
// The harness blank-imports ext-sqlite (per-cell data.db under a temp
// StorageRoot) and ext-entropy (crypto/rand bridge), runs their Setup, and
// Hand's bootstrap opens the pulp sql driver + migrates on Init.
//
// Audit context (batch-E Hand R1 — CLEAN, 0 CRIT/HIGH): the headline
// non-finding is that the entropy bridge is wired defensively — Hand
// imports pulp/entropy/cryptorand so crypto/rand draws from the host
// entropy.read capability instead of wazero's DETERMINISTIC default
// random_get. If that bridge silently regressed, every cell drawing from
// crypto/rand would emit predictable values. We pin it from the OUTSIDE:
// two freshly-created parties get invite codes from generateInviteCode
// (4 bytes of crypto/rand, hex). With the bridge wired they differ; a
// deterministic source would collide them. JWT auth + the open /health
// route are pinned alongside (proves the harness reaches authed handlers).

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// handSourceDir: Pulp/internal/host -> ../../../Hand/pulp-cell.
func handSourceDir() string {
	return filepath.Join("..", "..", "..", "Hand", "pulp-cell")
}

const handJWTSecret = "hand-harness-jwt-secret"

// mintJWT signs an HS256 token with the account_id claim Hand's JWTAuth
// middleware reads (matches Hand/pulp-cell/testtools/genjwt + the Claims
// shape in Fiber middleware.Claims).
func mintJWT(t *testing.T, secret, accountID string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"account_id": accountID,
		"session_id": "00000000-0000-0000-0000-000000000001",
		"exp":        time.Now().Add(time.Hour).Unix(),
		"iat":        time.Now().Unix(),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return signed
}

func startHand(t *testing.T) *CellHarness {
	return StartCellHTTP(t, CellHarnessConfig{
		SourceDir:    handSourceDir(),
		Name:         "hand",
		Capabilities: []string{"transport.http.inbound", "storage.sqlite", "entropy.read"},
		Config: map[string]any{
			"jwt_secret":    handJWTSecret,
			"service_token": "hand-harness-service-token",
		},
	})
}

func TestHand_HealthAndJWTAuth(t *testing.T) {
	h := startHand(t)

	// Open route works -> harness reaches the cell.
	if status, b := h.Do("GET", "/health", nil, nil); status != 200 {
		t.Fatalf("GET /health: want 200, got %d (%s)", status, b)
	}

	// Authed mutating route requires a valid Bearer JWT.
	if status, _ := h.Do("POST", "/parties", nil, []byte(`{}`)); status != 401 {
		t.Fatalf("POST /parties without JWT: want 401, got %d", status)
	}
	if status, _ := h.Do("POST", "/parties", map[string]string{"Authorization": "Bearer garbage"}, []byte(`{}`)); status != 401 {
		t.Fatalf("POST /parties with bad JWT: want 401, got %d", status)
	}
}

func TestHand_EntropyBridgeProducesDistinctInviteCodes(t *testing.T) {
	h := startHand(t)

	// Create two parties as two distinct accounts. Each CreateParty mints an
	// invite_code from 4 bytes of crypto/rand. With the entropy bridge wired
	// the two codes differ; a deterministic random_get would collide them.
	createParty := func(accountID string) string {
		tok := mintJWT(t, handJWTSecret, accountID)
		status, body := h.Do("POST", "/parties",
			map[string]string{"Authorization": "Bearer " + tok, "Content-Type": "application/json"},
			[]byte(`{}`))
		if status != 201 {
			t.Fatalf("CreateParty for %s: want 201, got %d (%s)", accountID, status, body)
		}
		var resp struct {
			InviteCode string `json:"invite_code"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode party body: %v (%s)", err, body)
		}
		if resp.InviteCode == "" {
			t.Fatalf("empty invite_code in response: %s", body)
		}
		return resp.InviteCode
	}

	code1 := createParty("11111111-1111-1111-1111-111111111111")
	code2 := createParty("22222222-2222-2222-2222-222222222222")

	if code1 == code2 {
		t.Fatalf("invite codes collided (%q == %q) — entropy bridge NOT wired; crypto/rand is deterministic", code1, code2)
	}
}
