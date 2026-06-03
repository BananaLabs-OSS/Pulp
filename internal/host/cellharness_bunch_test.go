package host

// Cell HTTP harness against the REAL deployed Bunch cell (social graph:
// friends / blocks / presence), pinning its authz posture.
//
// Bunch declares transport.http.inbound + transport.ws.inbound +
// storage.sqlite. The harness blank-imports ext-sqlite (per-cell data.db)
// and ext-http already provides transport.ws.inbound; Bunch's bootstrap
// opens the pulp sql driver + migrates on Init and registers the /ws route.
//
// Audit context (batch-E Bunch R1 — CLEAN after downgrade, 0 CRIT/HIGH):
// the headline non-finding is "No IDOR — every op scoped by claim
// account_id." These tests pin that authz contract from the outside:
//
//   1. A mutating route (POST /friends/request) requires a valid Bearer JWT
//      -> 401 unauthenticated / bad token.
//   2. With a valid JWT the mutating route works -> 201 (proves the harness
//      drives the authed sqlite-backed handler end-to-end).
//   3. The actor identity comes from the JWT CLAIM, not the request body:
//      a self-friend request (friend_id == the token's account_id) is
//      rejected 400 "self_friend". This is the observable proof that the
//      handler scopes the operation by uuid.MustParse(account_id) from the
//      token — the property the audit relied on to rule out IDOR.

import (
	"path/filepath"
	"testing"
)

// bunchSourceDir: Pulp/internal/host -> ../../../Bunch/pulp-cell.
func bunchSourceDir() string {
	return filepath.Join("..", "..", "..", "Bunch", "pulp-cell")
}

const bunchJWTSecret = "bunch-harness-jwt-secret"

func startBunch(t *testing.T) *CellHarness {
	return StartCellHTTP(t, CellHarnessConfig{
		SourceDir:    bunchSourceDir(),
		Name:         "bunch",
		Capabilities: []string{"transport.http.inbound", "transport.ws.inbound", "storage.sqlite"},
		Config: map[string]any{
			"jwt_secret":     bunchJWTSecret,
			"service_secret": "bunch-harness-service-secret",
		},
	})
}

func TestBunch_MutatingRouteRequiresJWT(t *testing.T) {
	h := startBunch(t)

	if status, b := h.Do("GET", "/health", nil, nil); status != 200 {
		t.Fatalf("GET /health: want 200, got %d (%s)", status, b)
	}

	body := []byte(`{"friend_id":"22222222-2222-2222-2222-222222222222"}`)
	if status, _ := h.Do("POST", "/friends/request", nil, body); status != 401 {
		t.Fatalf("POST /friends/request without JWT: want 401, got %d", status)
	}
	if status, _ := h.Do("POST", "/friends/request",
		map[string]string{"Authorization": "Bearer garbage"}, body); status != 401 {
		t.Fatalf("POST /friends/request with bad JWT: want 401, got %d", status)
	}
}

func TestBunch_AuthedMutatingRouteWorks(t *testing.T) {
	h := startBunch(t)

	tok := mintJWT(t, bunchJWTSecret, "11111111-1111-1111-1111-111111111111")
	body := []byte(`{"friend_id":"22222222-2222-2222-2222-222222222222"}`)
	status, b := h.Do("POST", "/friends/request",
		map[string]string{"Authorization": "Bearer " + tok, "Content-Type": "application/json"}, body)
	if status != 201 {
		t.Fatalf("authed POST /friends/request: want 201, got %d (%s)", status, b)
	}
}

func TestBunch_ActorScopedByJWTClaimNotBody(t *testing.T) {
	// The handler reads the actor from uuid.MustParse(account_id) (the JWT
	// claim), then rejects friend_id == accountID as self_friend. We send a
	// friend_id EQUAL to the token's account_id: the only way the server can
	// detect "self" is by sourcing the actor from the claim, not the body.
	// A 400 self_friend therefore pins the claim-scoping the audit's no-IDOR
	// conclusion rests on.
	h := startBunch(t)

	const self = "33333333-3333-3333-3333-333333333333"
	tok := mintJWT(t, bunchJWTSecret, self)
	body := []byte(`{"friend_id":"` + self + `"}`)
	status, b := h.Do("POST", "/friends/request",
		map[string]string{"Authorization": "Bearer " + tok, "Content-Type": "application/json"}, body)
	if status != 400 {
		t.Fatalf("self-friend request: want 400 (claim-scoped self-check), got %d (%s)", status, b)
	}
}
