package host

// Proof of the cell HTTP harness against a REAL deployed cell (Bananasplit)
// exercising an AUDIT-FIXED path.
//
// Audit finding go-bananasplit-r1 [CRITICAL]: the deployed pulp-cell had NO
// auth on any mutating route — the May-27 service-token commit hardened only
// the dead native tree. Fix 30a8bc0 ("service-token auth on deployed cell
// mutating routes (enforce-when-set)") added X-Service-Token gating that
// turns ON only when service_token is configured. These tests PIN that fix so
// it cannot silently re-break:
//
//   1. token SET + missing X-Service-Token  -> 401  (auth enforced)
//   2. token SET + wrong   X-Service-Token  -> 401  (constant-time mismatch)
//   3. token SET + correct X-Service-Token  -> not 401 (passes the gate)
//   4. token EMPTY (default)                -> not 401 (no outage for callers)
//   5. /health is always open regardless of token
//
// The cell lives in a sibling module (bananasplit-cell); go build resolves it
// via that dir's own go.mod, so no module wiring is needed here.

import (
	"path/filepath"
	"testing"
)

// bananasplitSourceDir is the deployed cell's source (relative to this
// package: Pulp/internal/host -> ../../../Bananasplit/pulp-cell).
func bananasplitSourceDir() string {
	return filepath.Join("..", "..", "..", "Bananasplit", "pulp-cell")
}

// startBananasplit boots the cell with the given service token. peel_url and
// bananagine_url are left as defaults; the auth gate is decided before any
// route handler body runs, so the outbound deps are never reached for the
// auth-path assertions (a 401 aborts in middleware).
func startBananasplit(t *testing.T, serviceToken string) *CellHarness {
	return StartCellHTTP(t, CellHarnessConfig{
		SourceDir:    bananasplitSourceDir(),
		Name:         "bananasplit",
		Capabilities: []string{"transport.http.inbound", "transport.http.outbound"},
		Config: map[string]any{
			"service_token": serviceToken,
			"peel_url":      "",
		},
	})
}

func TestBananasplit_AuthEnforcedWhenTokenSet(t *testing.T) {
	const token = "harness-secret-token"
	h := startBananasplit(t, token)

	body := []byte(`{"uuid":"u1","mode":"duo"}`)

	// 1. No X-Service-Token -> 401.
	if status, b := h.Do("POST", "/queue/join", nil, body); status != 401 {
		t.Fatalf("missing token: want 401, got %d (%s)", status, b)
	}

	// 2. Wrong token -> 401.
	if status, b := h.Do("POST", "/queue/join", map[string]string{"X-Service-Token": "nope"}, body); status != 401 {
		t.Fatalf("wrong token: want 401, got %d (%s)", status, b)
	}

	// 3. Correct token -> passes the auth gate (must NOT be 401). The
	//    handler itself returns 200 with a queued position.
	if status, b := h.Do("POST", "/queue/join", map[string]string{"X-Service-Token": token}, body); status == 401 {
		t.Fatalf("correct token: must pass auth gate, got 401 (%s)", b)
	} else if status != 200 {
		t.Fatalf("correct token: want 200 from handler, got %d (%s)", status, b)
	}
}

func TestBananasplit_HealthAlwaysOpenWithTokenSet(t *testing.T) {
	h := startBananasplit(t, "harness-secret-token")
	// /health must never require the service token.
	if status, b := h.Do("GET", "/health", nil, nil); status != 200 {
		t.Fatalf("/health with auth on: want 200, got %d (%s)", status, b)
	}
}

func TestBananasplit_NoAuthWhenTokenEmpty(t *testing.T) {
	// Default posture (empty token): callers that send NO X-Service-Token
	// must keep working — no 401, no outage. Pins the "enforce-when-set"
	// half of the fix (it must NOT fail-closed when unconfigured).
	h := startBananasplit(t, "")
	body := []byte(`{"uuid":"u1","mode":"duo"}`)
	if status, b := h.Do("POST", "/queue/join", nil, body); status == 401 {
		t.Fatalf("empty token must not gate routes, got 401 (%s)", b)
	} else if status != 200 {
		t.Fatalf("empty token: want 200 from handler, got %d (%s)", status, b)
	}
}
