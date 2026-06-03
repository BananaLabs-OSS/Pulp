package host

// Cell HTTP harness against the REAL deployed Evolution cell — the heaviest,
// highest-value cell in the portfolio (orders, queues, servers, pools, gifts,
// coupons, admin desk, SSE, Stripe, Resend, R2). Evolution declares the full
// capability set (transport.http.{inbound,outbound}, transport.sse,
// storage.{fs,sqlite,s3}, spawn.docker, payment.stripe, workers, entropy.read).
//
// The external-backend caps (stripe → Stripe API, s3 → R2, docker → daemon,
// workers → host pool) are wired to deterministic in-memory stubs via
// CapabilityOverrides (see cellharness_evostubs_test.go). storage.sqlite backs
// onto a temp data.db (ext-sqlite) so the cell migrates + serves for real;
// the postgres dialect quirks are out of harness scope (see needs-live below).
//
// AUDIT FIXES PINNED (all provable with an HTTP request + assertion, no real
// backend):
//
//   1. Finalize-DoS throttle (R7/R8 audit, router.go:7468/7577/7709). The
//      /api/upload-*/finalize routes carry their OWN finalizeRL burst limiter
//      (15/60s). After 15 calls from one IP the 16th gets 429. A re-call of an
//      already-validated upload short-circuits (idempotency) — and an unknown
//      upload_id 400s on the cheap DB lookup before any R2 fetch.
//
//   2. internalAuth fail-CLOSED (router.go:395). With INTERNAL_SECRET unset and
//      no insecure opt-in, every /internal/* + gated endpoint returns 503
//      ("disabled"), never falls open. With the secret set, a missing/wrong
//      X-Internal-Secret is 401 and the correct one passes.
//
//   3. Pool /confirm requires real PI authorization (router.go:4969/5010). The
//      fix gates the pool credit on the contribution's PaymentIntent being in
//      "requires_capture"; a client flag is no longer trusted. With the stripe
//      stub returning a non-requires_capture status, /confirm rejects 400
//      ("payment not authorized"); with requires_capture it is accepted.
//
// NEEDS-LIVE-INTEGRATION (documented, NOT faked — see report):
//   - Stripe webhook HMAC verification (real signed payload + secret).
//   - julianday()/date math under real Postgres vs sqlite.
//   - GDPR R2-object delete durability (real bucket bytes).
//   - wallet-index PG boot path (postgres dialect index DDL).

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// warmEvolution polls /health with a tolerant client until the cell answers
// 200. Evolution's FIRST step tick runs heavy synchronous bootstrap work
// (DB backup dump, Resend/Bananagine health probes, queue-status rebuild) on
// the single-threaded step loop; a request that lands mid-tick can exceed the
// harness's 5s client timeout. Warming up lets that one-shot tick drain before
// the pinned assertions fire, so the timeouts are deterministic boot latency,
// not a real handler stall.
func warmEvolution(t *testing.T, h *CellHarness) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(h.URL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("evolution cell did not become healthy within 45s")
}

// evolutionSourceDir: Pulp/internal/host -> ../../../Evolution/pulp-cell.
func evolutionSourceDir() string {
	return filepath.Join("..", "..", "..", "Evolution", "pulp-cell")
}

// startEvolution boots the Evolution cell with the external caps stubbed.
// internalSecret is configurable so the fail-closed vs enforced internalAuth
// paths can each be pinned. R2 creds are non-empty so cfg.R2Enabled() is true
// and the upload/finalize routes register (the s3 stub serves the presigns).
// bananagine_url / minecraft_sidecar_url are left empty so bootstrap skips the
// reachability probe + sidecar warmup (no outbound calls at Init).
func startEvolution(t *testing.T, internalSecret string) *CellHarness {
	return StartCellHTTP(t, CellHarnessConfig{
		SourceDir: evolutionSourceDir(),
		Name:      "evolution",
		Capabilities: []string{
			"transport.http.inbound",
			"transport.http.outbound",
			"transport.sse",
			"storage.fs",
			"storage.sqlite",
			"storage.s3",
			"payment.stripe",
			"workers",
			"entropy.read",
			// spawn.docker is intentionally NOT declared: the override's Stub
			// binds its imports, and no pinned path reaches docker. Declaring
			// it is unnecessary and keeps the harness honest about what runs.
		},
		Config: map[string]any{
			"internal_secret":     internalSecret,
			"frontend_url":        "https://sessions.gg",
			"max_servers":         12,
			"poll_interval":       "15s",
			"server_lifetime":     "336h",
			"refund_threshold":    "10m",
			"db_dialect":          "", // sqlite dialect (ext-sqlite backend)
			"r2_account_id":       "stub-account",
			"r2_access_key_id":    "stub-key",
			"r2_secret_access_key": "stub-secret",
			"r2_bucket":           "stub-bucket",
		},
		CapabilityOverrides: evolutionStubOverrides(),
	})
}

// TestEvolution_HealthAndBoot proves the harness drives the heaviest cell:
// it builds to wasm, Inits (migrations on the temp sqlite, gene discovery,
// poller, route registration all succeed), and serves an open route.
func TestEvolution_HealthAndBoot(t *testing.T) {
	h := startEvolution(t, "")
	warmEvolution(t, h)
	if status, b := h.Do("GET", "/health", nil, nil); status != 200 {
		t.Fatalf("GET /health: want 200, got %d (%s)", status, b)
	}
}

// TestEvolution_InternalAuthFailsClosed pins the internalAuth fail-closed fix
// (router.go:395). INTERNAL_SECRET unset + no insecure opt-in => every gated
// endpoint must 503, never fall open.
func TestEvolution_InternalAuthFailsClosed(t *testing.T) {
	h := startEvolution(t, "") // secret empty, ALLOW_INSECURE_INTERNAL unset
	warmEvolution(t, h)

	// A REGISTERED /internal route (group middleware only runs on a matched
	// route). With no secret the internalAuth middleware must 503 "disabled"
	// — never fall open to the handler, never 401.
	status, b := h.Do("GET", "/internal/versions/minecraft", nil, nil)
	if status != 503 {
		t.Fatalf("internal route with no secret: want 503 (fail-closed), got %d (%s)", status, b)
	}
}

// TestEvolution_InternalAuthEnforcedWhenSet pins the enforced half: with the
// secret configured, a missing/wrong X-Internal-Secret is 401 and the correct
// one passes the gate (no longer 503/401).
func TestEvolution_InternalAuthEnforcedWhenSet(t *testing.T) {
	const secret = "evo-harness-internal-secret"
	h := startEvolution(t, secret)
	warmEvolution(t, h)

	const route = "/internal/versions/minecraft"
	// Missing header -> 401 (not 503: secret IS configured).
	if status, b := h.Do("GET", route, nil, nil); status != 401 {
		t.Fatalf("internal route, secret set, no header: want 401, got %d (%s)", status, b)
	}
	// Wrong header -> 401 (constant-time mismatch).
	if status, b := h.Do("GET", route, map[string]string{"X-Internal-Secret": "wrong"}, nil); status != 401 {
		t.Fatalf("internal route, wrong secret: want 401, got %d (%s)", status, b)
	}
	// Correct header -> passes the gate (handler runs; must NOT be 401/503).
	// The handler itself may 404 (no version-state row for minecraft) or 200,
	// but it must NOT be blocked by the auth middleware.
	status, b := h.Do("GET", route, map[string]string{"X-Internal-Secret": secret}, nil)
	if status == 401 || status == 503 {
		t.Fatalf("internal route, correct secret: must pass auth gate, got %d (%s)", status, b)
	}
}

// TestEvolution_FinalizeDoSThrottle pins the finalize burst limiter
// (router.go:7468, finalizeRL 15/60s). The 16th finalize from the same IP in
// the window is rejected 429. Earlier calls return 400 (unknown upload_id —
// the cheap-reject DB lookup before any R2 fetch), proving the limiter, not a
// backend error, is what produces the 429.
func TestEvolution_FinalizeDoSThrottle(t *testing.T) {
	h := startEvolution(t, "")
	warmEvolution(t, h)

	body := []byte(`{"upload_id":"does-not-exist"}`)
	hdr := map[string]string{"Content-Type": "application/json"}

	// finalizeRL is 15/60s. The first 15 calls pass the limiter (and 400 on
	// the unknown upload_id); the 16th trips the limiter -> 429.
	var got429 bool
	for i := 0; i < 16; i++ {
		status, b := h.Do("POST", "/api/upload-world/finalize", hdr, body)
		if i < 15 {
			// Pre-limit: must NOT be 429. Unknown upload_id -> 400.
			if status == 429 {
				t.Fatalf("call %d tripped the limiter early (429) — finalizeRL window too tight (%s)", i+1, b)
			}
			if status != 400 {
				t.Fatalf("call %d: want 400 (unknown upload_id, cheap-reject), got %d (%s)", i+1, status, b)
			}
		} else {
			if status != 429 {
				t.Fatalf("call %d: want 429 (finalize DoS throttle), got %d (%s)", i+1, status, b)
			}
			got429 = true
		}
	}
	if !got429 {
		t.Fatal("finalize throttle never produced a 429 within the burst window")
	}
}

// TestEvolution_FinalizeThrottleIsPerEndpointLimiter proves the world +
// datapack + bedrock finalizes share the SAME finalizeRL (R8 fix: a single
// dedicated 15/60s limiter, separate from presign's uploadRL). Hammering
// /upload-world/finalize to exhaustion also 429s /upload-datapack/finalize.
func TestEvolution_FinalizeThrottleSharedAcrossAssets(t *testing.T) {
	h := startEvolution(t, "")
	warmEvolution(t, h)
	hdr := map[string]string{"Content-Type": "application/json"}
	body := []byte(`{"upload_id":"x"}`)

	// Burn the 15-call budget on the world finalize.
	for i := 0; i < 15; i++ {
		h.Do("POST", "/api/upload-world/finalize", hdr, body)
	}
	// The shared limiter is now exhausted for this IP — datapack finalize 429s.
	if status, b := h.Do("POST", "/api/upload-datapack/finalize", hdr, body); status != 429 {
		t.Fatalf("datapack finalize after world budget burned: want 429 (shared finalizeRL), got %d (%s)", status, b)
	}
}

// poolCreateResp / contributeResp decode the JSON the pool endpoints return.
type poolCreateResp struct {
	PoolToken      string `json:"pool_token"`
	ContributionID string `json:"contribution_id"`
}

// createSessionPool drives POST /api/pool/create for a session pool (no
// server_type => session pool; quantity sets the target). Returns the pool
// token + the creator contribution id. The stripe stub mints the creator PI.
func createSessionPool(t *testing.T, h *CellHarness, email string) poolCreateResp {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"email":        email,
		"username":     "creator",
		"quantity":     1,
		"amount_cents": 1200,
	})
	status, b := h.Do("POST", "/api/pool/create",
		map[string]string{"Content-Type": "application/json"}, body)
	if status != 200 {
		t.Fatalf("POST /api/pool/create: want 200, got %d (%s)", status, b)
	}
	var out poolCreateResp
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode pool create resp: %v (%s)", err, b)
	}
	if out.PoolToken == "" || out.ContributionID == "" {
		t.Fatalf("pool create returned empty token/contribution: %s", b)
	}
	return out
}

// TestEvolution_PoolConfirmRejectsUnauthorizedPI pins the /confirm PI-verify
// fix (router.go:5010). The stripe stub returns a PI whose status is NOT
// requires_capture; /confirm must reject 400 "payment not authorized" rather
// than crediting the pool on a trusted-client flag.
func TestEvolution_PoolConfirmRejectsUnauthorizedPI(t *testing.T) {
	// Stub all PIs to an un-authorized status BEFORE booting so the creator
	// PI minted at pool-create also carries it.
	setStripeStubPIStatus("requires_payment_method")
	defer setStripeStubPIStatus("requires_capture")

	h := startEvolution(t, "")
	warmEvolution(t, h)
	pool := createSessionPool(t, h, "reject@example.com")

	confirmBody, _ := json.Marshal(map[string]string{"contribution_id": pool.ContributionID})
	status, b := h.Do("POST", "/api/pool/"+pool.PoolToken+"/confirm",
		map[string]string{"Content-Type": "application/json"}, confirmBody)
	if status != 400 {
		t.Fatalf("pool confirm with non-requires_capture PI: want 400 (payment not authorized), got %d (%s)", status, b)
	}
}

// TestEvolution_PoolConfirmAcceptsAuthorizedPI is the positive half: with the
// stub PI in requires_capture, /confirm passes the audit gate (credits the
// pool — 200). Proves the 400 above is the PI-status check firing, not a
// blanket reject.
func TestEvolution_PoolConfirmAcceptsAuthorizedPI(t *testing.T) {
	setStripeStubPIStatus("requires_capture") // explicit (default), for clarity
	h := startEvolution(t, "")
	warmEvolution(t, h)
	pool := createSessionPool(t, h, "accept@example.com")

	confirmBody, _ := json.Marshal(map[string]string{"contribution_id": pool.ContributionID})
	status, b := h.Do("POST", "/api/pool/"+pool.PoolToken+"/confirm",
		map[string]string{"Content-Type": "application/json"}, confirmBody)
	if status != 200 {
		t.Fatalf("pool confirm with requires_capture PI: want 200 (gate passed), got %d (%s)", status, b)
	}
}
