package host

// Cell HTTP harness against the REAL deployed Peel cell (UDP relay +
// HTTP control API), pinning Peel's audit fixes.
//
// Peel needs network.udp (the relay binds an inbound UDP socket on init)
// + transport.http.{inbound,outbound}. The harness blank-imports ext-udp;
// its default UDP_BIND_ALLOW ("0.0.0.0,::") admits the relay's wildcard
// inbound listener, so init succeeds. We point listen_addr at an ephemeral
// loopback port so the relay never collides with a real :5520.
//
// Audit findings pinned (batch-C Peel R1, fixes c57e222 / service-token):
//
//   go-peel-r1 HIGH #1 — mutating control API (POST /routes, DELETE
//   /routes/:ip, DELETE /sessions/:ip) was unauthenticated. The reconciled
//   "enforce-when-set" fix gates them on X-Service-Token ONLY when
//   SERVICE_TOKEN is configured. Pinned:
//     1. token SET + no   X-Service-Token  -> 401
//     2. token SET + wrong X-Service-Token -> 401
//     3. token SET + right X-Service-Token -> not 401 (passes the gate)
//     4. token EMPTY -> cell STARTS and serves the route unauthenticated
//        (no outage for today's header-less Bananasplit/Potassium callers)
//     5. GET /health + GET /routes are always open
//
//   go-peel-r1 HIGH #2 — PEEL-M1 backend-address validation landed only on
//   the dead native tree; the deployed setRoute first-write path stored
//   req.Backend unvalidated. Fix calls validBackendAddr in pulp-cell
//   setRoute. Pinned: a first-write POST /routes with a garbage backend is
//   rejected 400 (and a well-formed one is accepted).

import (
	"path/filepath"
	"testing"
)

// peelSourceDir: Pulp/internal/host -> ../../../Peel/pulp-cell.
func peelSourceDir() string {
	return filepath.Join("..", "..", "..", "Peel", "pulp-cell")
}

// startPeel boots the Peel cell with the given service token. listen_addr
// is an ephemeral loopback UDP port so the relay's inbound bind succeeds
// without colliding with a real deployment's :5520. bananasplit_url is left
// at its default; the auth gate + backend validation are decided before any
// outbound lookup, so Bananasplit is never reached for these assertions.
func startPeel(t *testing.T, serviceToken string) *CellHarness {
	return StartCellHTTP(t, CellHarnessConfig{
		SourceDir:    peelSourceDir(),
		Name:         "peel",
		Capabilities: []string{"transport.http.inbound", "transport.http.outbound", "network.udp"},
		Config: map[string]any{
			"service_token": serviceToken,
			"listen_addr":   "127.0.0.1:0",
		},
	})
}

func TestPeel_ControlAPIAuthEnforcedWhenTokenSet(t *testing.T) {
	const token = "peel-harness-secret"
	h := startPeel(t, token)

	body := []byte(`{"player_ip":"203.0.113.50","backend":"10.0.50.2:5521"}`)

	// 1. No X-Service-Token -> 401.
	if status, b := h.Do("POST", "/routes", nil, body); status != 401 {
		t.Fatalf("missing token: want 401, got %d (%s)", status, b)
	}

	// 2. Wrong token -> 401.
	if status, b := h.Do("POST", "/routes", map[string]string{"X-Service-Token": "nope"}, body); status != 401 {
		t.Fatalf("wrong token: want 401, got %d (%s)", status, b)
	}

	// 3. Correct token -> passes the gate (handler returns 200 with {"status":"ok"}).
	if status, b := h.Do("POST", "/routes", map[string]string{"X-Service-Token": token}, body); status == 401 {
		t.Fatalf("correct token: must pass auth gate, got 401 (%s)", b)
	} else if status != 200 {
		t.Fatalf("correct token: want 200 from handler, got %d (%s)", status, b)
	}

	// DELETE routes are gated too.
	if status, _ := h.Do("DELETE", "/routes/203.0.113.50", nil, nil); status != 401 {
		t.Fatalf("DELETE /routes without token: want 401, got %d", status)
	}
	if status, _ := h.Do("DELETE", "/sessions/203.0.113.50", nil, nil); status != 401 {
		t.Fatalf("DELETE /sessions without token: want 401, got %d", status)
	}
}

func TestPeel_ObservabilityRoutesAlwaysOpenWithTokenSet(t *testing.T) {
	h := startPeel(t, "peel-harness-secret")
	// GET /health and GET /routes must never require the token.
	if status, b := h.Do("GET", "/health", nil, nil); status != 200 {
		t.Fatalf("GET /health with auth on: want 200, got %d (%s)", status, b)
	}
	if status, b := h.Do("GET", "/routes", nil, nil); status != 200 {
		t.Fatalf("GET /routes with auth on: want 200, got %d (%s)", status, b)
	}
}

func TestPeel_NoAuthWhenTokenEmpty(t *testing.T) {
	// Default posture (empty token): the cell STARTS (proven by reaching
	// here — Init binds the relay + control API) and serves the mutating
	// route WITHOUT a token. Pins the enforce-when-set half: an unconfigured
	// token must not fail-closed and must not 401 header-less callers.
	h := startPeel(t, "")
	body := []byte(`{"player_ip":"203.0.113.50","backend":"10.0.50.2:5521"}`)
	if status, b := h.Do("POST", "/routes", nil, body); status == 401 {
		t.Fatalf("empty token must not gate routes, got 401 (%s)", b)
	} else if status != 200 {
		t.Fatalf("empty token: want 200 from handler, got %d (%s)", status, b)
	}
}

func TestPeel_FirstWriteRejectsBadBackend(t *testing.T) {
	// PEEL-M1 fix: validBackendAddr runs on the first-write/create path, not
	// just on backend-change. A garbage backend on a brand-new route must be
	// rejected 400 before it is ever stored. Token empty so auth is off and
	// the assertion isolates the validation behavior.
	h := startPeel(t, "")

	// Garbage backend (no host:port, unresolvable) -> 400.
	bad := []byte(`{"player_ip":"203.0.113.99","backend":"not-a-valid-addr"}`)
	if status, b := h.Do("POST", "/routes", nil, bad); status != 400 {
		t.Fatalf("first-write bad backend: want 400, got %d (%s)", status, b)
	}

	// A well-formed backend on the same (still-new) player_ip -> 200, proving
	// the 400 above was the validator firing, not a blanket reject.
	good := []byte(`{"player_ip":"203.0.113.99","backend":"10.0.50.2:5521"}`)
	if status, b := h.Do("POST", "/routes", nil, good); status != 200 {
		t.Fatalf("first-write good backend: want 200, got %d (%s)", status, b)
	}
}
