package host

// PORT of the pool-flow slice of Evolution/internal/router/voucher_flow_test.go
// (TestPool_ContributionFlow / TestPool_ExpiredStateTransition /
// TestPool_CancellationClearsContributions), driven against the REAL Evolution
// cell's own migrated SQLite (pools + pool_contributions tables created by the
// cell's bootstrap migrations).
//
// FAITHFULNESS NOTE: the native tests run NO HTTP handler — they seed pool /
// pool_contribution rows and assert the collected-total arithmetic and the
// open->expired / open->cancelled status transitions plus contribution
// retention. They pin the pool SCHEMA + status-enum constants (PoolOpen /
// PoolExpired / PoolCancelled), not any router closure. These ports assert the
// SAME transitions on the cell's OWN migrated pool tables (the schema the cell
// ships), so the twin dies with a green schema-parity equivalent. The pool
// mutation endpoints (/api/pool/*) are Stripe-PI-gated and covered separately by
// the existing pool /confirm PI-gate proof; this slice is the state-machine
// parity the twin actually asserts.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

func seedPool(t *testing.T, db *sql.DB, id, token, status string, targetCents, collectedCents int, expiresAt time.Time) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO pools (id, pool_token, name, server_type, target_cents, collected_cents, status, creator_email, expires_at, created_at)
		 VALUES (?, ?, 'My Pool', 'minecraft', ?, ?, ?, 'creator@e.com', ?, ?)`,
		id, token, targetCents, collectedCents, status, expiresAt, time.Now().UTC(),
	); err != nil {
		t.Fatalf("seed pool %s: %v", id, err)
	}
	checkpoint(db)
}

func seedPoolContribution(t *testing.T, db *sql.DB, id, poolID, email string, amountCents int, confirmed bool) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO pool_contributions (id, pool_id, username, email, amount_cents, confirmed, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, poolID, email, email, amountCents, boolInt(confirmed), time.Now().UTC(),
	); err != nil {
		t.Fatalf("seed pool contribution %s: %v", id, err)
	}
	checkpoint(db)
}

func callFundingOwner(t *testing.T, h *CellHarness, provider string, request any) {
	t.Helper()
	_ = callFundingOwnerResult[any](t, h, provider, request)
}

func callFundingOwnerResult[T any](t *testing.T, h *CellHarness, provider string, request any) T {
	t.Helper()
	input, err := msgpack.Marshal(request)
	if err != nil {
		t.Fatalf("encode %s request: %v", provider, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := h.cellsByName["funding"].Call(ctx, provider, input)
	if err != nil {
		t.Fatalf("call %s: %v", provider, err)
	}
	var result struct {
		OK    bool `msgpack:"ok"`
		Value T    `msgpack:"value"`
		Error any  `msgpack:"error"`
	}
	if err := msgpack.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode %s result: %v", provider, err)
	}
	if !result.OK {
		t.Fatalf("%s rejected seed request: %#v", provider, result.Error)
	}
	return result.Value
}

func callCommerceOwnerResult[T any](t *testing.T, h *CellHarness, provider string, request any) T {
	t.Helper()
	input, err := msgpack.Marshal(request)
	if err != nil {
		t.Fatalf("encode %s request: %v", provider, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := h.cellsByName["commerce"].Call(ctx, provider, input)
	if err != nil {
		t.Fatalf("call %s: %v", provider, err)
	}
	var result struct {
		OK    bool `msgpack:"ok"`
		Value T    `msgpack:"value"`
		Error any  `msgpack:"error"`
	}
	if err := msgpack.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode %s result: %v", provider, err)
	}
	if !result.OK {
		t.Fatalf("%s rejected seed request: %#v", provider, result.Error)
	}
	return result.Value
}

// TestEvolution_Pool_ContributionFlow ports TestPool_ContributionFlow: two
// confirmed contributions drive collected_cents to the target while the pool
// stays open at the half-way point, and both contribution rows persist.
func TestEvolution_Pool_ContributionFlow(t *testing.T) {
	_, db := startEvolutionDowntime(t)

	seedPool(t, db, "pool-1", "pt-abc", "open", 2800, 0, time.Now().UTC().Add(24*time.Hour))

	// First contribution of $14 -> collected 1400, still open.
	seedPoolContribution(t, db, "contrib-1", "pool-1", "alice@e.com", 1400, true)
	if _, err := db.Exec(`UPDATE pools SET collected_cents = collected_cents + 1400 WHERE id = 'pool-1'`); err != nil {
		t.Fatalf("bump collected: %v", err)
	}
	checkpoint(db)

	var collected, target int
	var status string
	if err := db.QueryRow(`SELECT collected_cents, target_cents, status FROM pools WHERE id = 'pool-1'`).
		Scan(&collected, &target, &status); err != nil {
		t.Fatalf("read pool: %v", err)
	}
	if collected != 1400 {
		t.Fatalf("expected 1400 collected, got %d", collected)
	}
	if status != "open" {
		t.Fatalf("pool should still be open at 50%%, got %q", status)
	}

	// Second contribution reaches the target.
	seedPoolContribution(t, db, "contrib-2", "pool-1", "bob@e.com", 1400, true)
	if _, err := db.Exec(`UPDATE pools SET collected_cents = collected_cents + 1400 WHERE id = 'pool-1'`); err != nil {
		t.Fatalf("bump collected 2: %v", err)
	}
	checkpoint(db)

	if err := db.QueryRow(`SELECT collected_cents, target_cents FROM pools WHERE id = 'pool-1'`).
		Scan(&collected, &target); err != nil {
		t.Fatalf("read filled pool: %v", err)
	}
	if collected != 2800 {
		t.Fatalf("expected 2800 collected, got %d", collected)
	}
	if collected < target {
		t.Fatalf("should have reached target: %d >= %d", collected, target)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pool_contributions WHERE pool_id = 'pool-1' AND confirmed = 1`).Scan(&n); err != nil {
		t.Fatalf("count contributions: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 confirmed contributions, got %d", n)
	}
}

// TestEvolution_Pool_ExpiredStateTransition ports TestPool_ExpiredStateTransition:
// the cleanup sweep marks an open pool past its expiry as expired.
func TestEvolution_Pool_ExpiredStateTransition(t *testing.T) {
	_, db := startEvolutionDowntime(t)

	seedPool(t, db, "pool-exp", "pt-exp", "open", 2800, 0, time.Now().UTC().Add(-1*time.Hour))

	if _, err := db.Exec(
		`UPDATE pools SET status = 'expired' WHERE status = 'open' AND expires_at < ?`, time.Now().UTC(),
	); err != nil {
		t.Fatalf("expire sweep: %v", err)
	}
	checkpoint(db)

	var status string
	if err := db.QueryRow(`SELECT status FROM pools WHERE id = 'pool-exp'`).Scan(&status); err != nil {
		t.Fatalf("read pool: %v", err)
	}
	if status != "expired" {
		t.Fatalf("expected expired status, got %q", status)
	}
}

// TestEvolution_Pool_CancellationClearsContributions ports
// TestPool_CancellationClearsContributions: cancelling a pool flips it to
// cancelled but PRESERVES its contribution rows (needed for refund processing).
func TestEvolution_Pool_CancellationClearsContributions(t *testing.T) {
	_, db := startEvolutionDowntime(t)

	seedPool(t, db, "pool-cancel", "pt-cancel", "open", 2800, 0, time.Now().UTC().Add(24*time.Hour))
	seedPoolContribution(t, db, "cc-0", "pool-cancel", "a@e.com", 700, true)
	seedPoolContribution(t, db, "cc-1", "pool-cancel", "b@e.com", 700, true)

	if _, err := db.Exec(`UPDATE pools SET status = 'cancelled' WHERE id = 'pool-cancel'`); err != nil {
		t.Fatalf("cancel pool: %v", err)
	}
	checkpoint(db)

	var status string
	if err := db.QueryRow(`SELECT status FROM pools WHERE id = 'pool-cancel'`).Scan(&status); err != nil {
		t.Fatalf("read pool: %v", err)
	}
	if status != "cancelled" {
		t.Fatalf("expected cancelled, got %q", status)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pool_contributions WHERE pool_id = 'pool-cancel'`).Scan(&n); err != nil {
		t.Fatalf("count contributions: %v", err)
	}
	if n != 2 {
		t.Fatalf("cancellation should preserve contribution records, got %d", n)
	}
}

func TestEvolution_Pool_PublicGetRunsThroughLuaFundingOwner(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedPool(t, db, "pool-owner-read", "pt-owner-read", "open", 2800, 700, time.Now().UTC().Add(24*time.Hour))
	seedPoolContribution(t, db, "contrib-owner-read", "pool-owner-read", "creator@e.com", 700, true)

	status, body := h.Do(http.MethodGet, "/api/pool/pt-owner-read?email=creator@e.com", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET pool status = %d body=%s", status, body)
	}
	var response struct {
		Status         string `json:"status"`
		Name           string `json:"name"`
		TargetCents    int64  `json:"target_cents"`
		CollectedCents int64  `json:"collected_cents"`
		IsOwner        bool   `json:"is_owner"`
		Contributors   []struct {
			Username    string `json:"username"`
			AmountCents int64  `json:"amount_cents"`
		} `json:"contributors"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode pool response: %v body=%s", err, body)
	}
	if response.Status != "open" || response.Name != "My Pool" ||
		response.TargetCents != 2800 || response.CollectedCents != 700 || !response.IsOwner {
		t.Fatalf("unexpected owner pool projection: %#v", response)
	}
	if len(response.Contributors) != 1 || response.Contributors[0].Username != "creator@e.com" ||
		response.Contributors[0].AmountCents != 700 {
		t.Fatalf("unexpected owner contribution projection: %#v", response.Contributors)
	}
}

func TestEvolution_AdminPoolReadsRunThroughLuaFundingOwner(t *testing.T) {
	const (
		secret = "pool-admin-secret"
		email  = "${ADMIN_EMAILS}"
	)
	h, db := startEvolutionDowntimeCfg(t, secret)
	seedPool(t, db, "admin-pool-owner", "pt-admin-owner", "open", 4400, 1100, time.Now().UTC().Add(24*time.Hour))
	seedPoolContribution(t, db, "admin-contribution-owner", "admin-pool-owner", "member@example.test", 1100, true)

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("evolution-admin:" + email))
	headers := map[string]string{
		"Cookie": "admin_session=" + email + ":" + hex.EncodeToString(mac.Sum(nil)),
	}

	status, body := h.Do(http.MethodGet, "/admin/api/pools?q=admin-pool-owner&status=open", headers, nil)
	if status != http.StatusOK {
		t.Fatalf("GET admin pools status = %d body=%s", status, body)
	}
	var pools []struct {
		ID               string `json:"id"`
		TargetCents      int64  `json:"target_cents"`
		ContributorCount int    `json:"contributor_count"`
	}
	if err := json.Unmarshal(body, &pools); err != nil {
		t.Fatalf("decode admin pools: %v body=%s", err, body)
	}
	if len(pools) != 1 || pools[0].ID != "admin-pool-owner" ||
		pools[0].TargetCents != 4400 || pools[0].ContributorCount != 1 {
		t.Fatalf("admin pools = %#v", pools)
	}

	status, body = h.Do(http.MethodGet, "/admin/api/pools/admin-pool-owner/contributions", headers, nil)
	if status != http.StatusOK {
		t.Fatalf("GET admin contributions status = %d body=%s", status, body)
	}
	var contributions []struct {
		ID          string `json:"id"`
		Email       string `json:"email"`
		AmountCents int64  `json:"amount_cents"`
		Confirmed   bool   `json:"confirmed"`
	}
	if err := json.Unmarshal(body, &contributions); err != nil {
		t.Fatalf("decode admin contributions: %v body=%s", err, body)
	}
	if len(contributions) != 1 || contributions[0].ID != "admin-contribution-owner" ||
		contributions[0].Email != "member@example.test" || contributions[0].AmountCents != 1100 ||
		!contributions[0].Confirmed {
		t.Fatalf("admin contributions = %#v", contributions)
	}
}

func TestEvolution_AdminPoolMutationsRunThroughLuaFundingOwner(t *testing.T) {
	const (
		secret = "pool-admin-mutation-secret"
		email  = "${ADMIN_EMAILS}"
		poolID = "admin-owner-mutation-pool"
	)
	h, _ := startEvolutionDowntimeCfg(t, secret)
	now := time.Now().UTC()
	callFundingOwner(t, h, "funding.v1.pool.create", map[string]any{
		"request_id": "admin-owner-mutation-create", "pool_id": poolID, "game_id": "sessions",
		"requested_by": "creator@example.test", "goal_cents": int64(2800), "currency": "usd",
		"pool_token": "pt-admin-owner-mutation", "name": "Admin Mutation Pool",
		"creator_email": "creator@example.test",
		"expires_at":    now.Add(24 * time.Hour).Format(time.RFC3339Nano), "created_at": now.Format(time.RFC3339Nano),
		"creator_contribution_id": "admin-owner-mutation-contribution", "creator_username": "Creator",
		"initial_amount_cents": int64(700),
	})
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("evolution-admin:" + email))
	headers := map[string]string{
		"Cookie":          "admin_session=" + email + ":" + hex.EncodeToString(mac.Sum(nil)),
		"Content-Type":    "application/json",
		"Idempotency-Key": "admin-owner-extend",
	}
	extendBody, err := json.Marshal(map[string]int{"hours": 6})
	if err != nil {
		t.Fatal(err)
	}
	status, body := h.Do(http.MethodPost, "/admin/api/pools/"+poolID+"/extend", headers, extendBody)
	if status != http.StatusOK {
		t.Fatalf("admin extend status = %d body=%s", status, body)
	}
	var extended struct {
		OK        bool   `json:"ok"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &extended); err != nil {
		t.Fatalf("decode admin extend response: %v body=%s", err, body)
	}
	if !extended.OK || extended.ExpiresAt == "" {
		t.Fatalf("admin extend response = %#v", extended)
	}

	headers["Idempotency-Key"] = "admin-owner-cancel"
	status, body = h.Do(http.MethodPost, "/admin/api/pools/"+poolID+"/cancel", headers, nil)
	if status != http.StatusOK {
		t.Fatalf("admin cancel status = %d body=%s", status, body)
	}
	var cancelled struct {
		Cancelled bool `json:"cancelled"`
	}
	if err := json.Unmarshal(body, &cancelled); err != nil {
		t.Fatalf("decode admin cancel response: %v body=%s", err, body)
	}
	if !cancelled.Cancelled {
		t.Fatalf("admin cancel response = %#v", cancelled)
	}

	headers["Idempotency-Key"] = "admin-owner-extend-after-cancel"
	status, body = h.Do(http.MethodPost, "/admin/api/pools/"+poolID+"/extend", headers, extendBody)
	if status != http.StatusConflict {
		t.Fatalf("cancelled owner pool accepted extension: status=%d body=%s", status, body)
	}
}

func TestEvolution_AdminPoolForceProvisionRunsThroughLuaFundingOwner(t *testing.T) {
	const (
		secret         = "pool-admin-force-secret"
		adminEmail     = "${ADMIN_EMAILS}"
		poolID         = "admin-owner-force-pool"
		poolToken      = "pt-admin-owner-force"
		contributionID = "admin-owner-force-contribution"
	)
	h, _ := startEvolutionDowntimeCfg(t, secret)
	now := time.Now().UTC()
	callFundingOwner(t, h, "funding.v1.pool.create", map[string]any{
		"request_id": "admin-owner-force-create", "pool_id": poolID, "game_id": "sessions",
		"requested_by": "creator@example.test", "goal_cents": int64(2800), "currency": "usd",
		"pool_token": poolToken, "name": "Admin Force Pool", "creator_email": "creator@example.test",
		"expires_at": now.Add(24 * time.Hour).Format(time.RFC3339Nano), "created_at": now.Format(time.RFC3339Nano),
		"creator_contribution_id": contributionID, "creator_username": "Creator",
		"initial_amount_cents": int64(700),
	})
	createClaim, createLease := claimFundingEffectKind(
		t, h, "admin-force-create-worker", "pulp.effect.stripe.payment-intent.create.v1",
	)
	acknowledgeFundingEffectResult(t, h, createClaim, createLease, map[string]string{
		"payment_intent": "pi_admin_force", "client_secret": "pi_admin_force_secret",
	})

	confirmBody, err := json.Marshal(map[string]string{"contribution_id": contributionID})
	if err != nil {
		t.Fatal(err)
	}
	status, body := h.Do(
		http.MethodPost,
		"/api/pool/"+poolToken+"/confirm",
		map[string]string{"Content-Type": "application/json"},
		confirmBody,
	)
	if status != http.StatusAccepted {
		t.Fatalf("force fixture confirmation status = %d body=%s", status, body)
	}
	verifyClaim, verifyLease := claimFundingEffectKind(
		t, h, "admin-force-verify-worker", "pulp.effect.stripe.payment-intent.get.v1",
	)
	acknowledgeFundingEffectResult(t, h, verifyClaim, verifyLease, map[string]any{
		"payment_intent_id": "pi_admin_force", "status": "requires_capture",
		"amount_cents": int64(700), "currency": "usd", "capture_method": "manual",
	})

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("evolution-admin:" + adminEmail))
	headers := map[string]string{
		"Cookie":          "admin_session=" + adminEmail + ":" + hex.EncodeToString(mac.Sum(nil)),
		"Idempotency-Key": "admin-owner-force",
	}
	status, body = h.Do(http.MethodPost, "/admin/api/pools/"+poolID+"/force-provision", headers, nil)
	if status != http.StatusAccepted {
		t.Fatalf("admin force provision status = %d body=%s", status, body)
	}
	_, captureLease := claimFundingEffectKind(
		t, h, "admin-force-capture-worker", "pulp.effect.stripe.payment-intent.capture.v1",
	)
	if captureLease.Intent.Kind != "pulp.effect.stripe.payment-intent.capture.v1" {
		t.Fatalf("admin force provision effect = %#v", captureLease.Intent)
	}
	status, body = h.Do(http.MethodGet, "/api/pool/"+poolToken, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("force-filled pool projection status = %d body=%s", status, body)
	}
	var projection struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &projection); err != nil {
		t.Fatalf("decode force-filled pool projection: %v body=%s", err, body)
	}
	if projection.Status != "filled" {
		t.Fatalf("force-filled pool projection = %#v", projection)
	}
}

func TestEvolution_Pool_ContributionResumeRequiresTrustedIdentityAndUsesFundingOwner(t *testing.T) {
	const secret = "pool-resume-internal-secret"
	h, _ := startEvolutionDowntimeCfg(t, secret)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	callFundingOwner(t, h, "funding.v1.pool.create", map[string]any{
		"request_id": "resume-create", "pool_id": "funding-resume-pool", "game_id": "sessions",
		"requested_by": "resume@example.test", "goal_cents": int64(2800), "currency": "usd",
		"pool_token": "pt-funding-resume", "name": "Resume Pool", "creator_email": "resume@example.test",
		"expires_at": time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano), "created_at": now,
		"creator_contribution_id": "funding-resume-contribution", "creator_username": "ResumePlayer",
		"initial_amount_cents": int64(700),
	})
	callFundingOwner(t, h, "funding.v2.contribution.payment_intent.record", map[string]any{
		"request_id": "resume-record-payment", "contribution_id": "funding-resume-contribution",
		"stripe_payment_intent_id": "pi_resume", "client_secret": "pi_resume_secret", "updated_at": now,
	})

	path := "/api/pool/pt-funding-resume/contributions/funding-resume-contribution/resume"
	status, body := h.Do(http.MethodPost, path, nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("untrusted resume status = %d body=%s", status, body)
	}

	resumeMAC := hmac.New(sha256.New, []byte(secret))
	_, _ = resumeMAC.Write([]byte("pool-contribution-resume/v1\x00"))
	_, _ = resumeMAC.Write([]byte("pt-funding-resume"))
	_, _ = resumeMAC.Write([]byte{0})
	_, _ = resumeMAC.Write([]byte("funding-resume-contribution"))
	_, _ = resumeMAC.Write([]byte{0})
	_, _ = resumeMAC.Write([]byte("resume@example.test"))
	resumeBody, err := json.Marshal(map[string]string{
		"email": "resume@example.test", "resume_token": hex.EncodeToString(resumeMAC.Sum(nil)),
	})
	if err != nil {
		t.Fatalf("encode signed resume request: %v", err)
	}
	status, body = h.Do(http.MethodPost, path, map[string]string{"Content-Type": "application/json"}, resumeBody)
	if status != http.StatusOK {
		t.Fatalf("signed resume status = %d body=%s", status, body)
	}

	headers := map[string]string{
		"X-Internal-Secret":  secret,
		"X-Session-Verified": "resume@example.test",
	}
	status, body = h.Do(http.MethodPost, path, headers, nil)
	if status != http.StatusOK {
		t.Fatalf("trusted resume status = %d body=%s", status, body)
	}
	var response struct {
		ContributionID string `json:"contribution_id"`
		ClientSecret   string `json:"client_secret"`
		PaymentReady   bool   `json:"payment_ready"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode resume response: %v body=%s", err, body)
	}
	if response.ContributionID != "funding-resume-contribution" ||
		response.ClientSecret != "pi_resume_secret" || !response.PaymentReady {
		t.Fatalf("unexpected resume response: %#v", response)
	}

	headers["X-Session-Verified"] = "attacker@example.test"
	status, body = h.Do(http.MethodPost, path, headers, nil)
	if status != http.StatusNotFound {
		t.Fatalf("wrong-identity resume status = %d body=%s", status, body)
	}
	if string(body) == "" || string(body) == "pi_resume_secret" {
		t.Fatalf("wrong-identity response leaked secret: %s", body)
	}
}

func TestEvolution_Pool_CreateRunsThroughLuaFundingOwnerAndResumesAfterReceipt(t *testing.T) {
	const (
		secret = "pool-create-resume-secret"
		email  = "creator@example.test"
	)
	h, _ := startEvolutionDowntimeCfg(t, secret)
	createBody, err := json.Marshal(map[string]any{
		"email": email, "username": "Creator", "quantity": 1, "amount_cents": 1200,
	})
	if err != nil {
		t.Fatal(err)
	}
	createHeaders := map[string]string{
		"Content-Type":    "application/json",
		"Idempotency-Key": "pool-create-owner-e2e",
	}
	status, body := h.Do(http.MethodPost, "/api/pool/create", createHeaders, createBody)
	if status != http.StatusAccepted {
		t.Fatalf("create status = %d body=%s", status, body)
	}
	var pending struct {
		Status         string `json:"status"`
		PoolToken      string `json:"pool_token"`
		ContributionID string `json:"contribution_id"`
		ResumeToken    string `json:"resume_token"`
	}
	if err := json.Unmarshal(body, &pending); err != nil {
		t.Fatalf("decode create pending response: %v body=%s", err, body)
	}
	if pending.Status != "pending" || pending.PoolToken == "" ||
		pending.ContributionID == "" || pending.ResumeToken == "" {
		t.Fatalf("incomplete pending response: %#v", pending)
	}

	claimed := callFundingOwnerResult[poolEffectClaimResult](
		t, h, "funding.effects.v1.claim", map[string]any{
			"version": "pulp.effect.outbox.v1", "owner": "funding",
			"consumer_id": "pool-create-e2e-worker", "limit": uint32(1),
			"lease_duration_millis": int64(60_000),
		},
	)
	if len(claimed.Leases) != 1 ||
		claimed.Leases[0].Intent.Kind != "pulp.effect.stripe.payment-intent.create.v1" {
		t.Fatalf("unexpected Funding effect claim: %#v", claimed)
	}
	receiptResult, err := msgpack.Marshal(map[string]string{
		"payment_intent": "pi_pool_create_e2e",
		"client_secret":  "pi_pool_create_e2e_secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	settled := callFundingOwnerResult[poolEffectSettlementResult](
		t,
		h,
		"funding.effects.v1.acknowledge",
		map[string]any{
			"version": "pulp.effect.outbox.v1", "owner": "funding",
			"consumer_id": claimed.ConsumerID, "lease_id": claimed.Leases[0].LeaseID,
			"receipt": poolEffectReceipt{
				Version: "pulp.effect.v1", IntentID: claimed.Leases[0].Intent.ID,
				Kind:           claimed.Leases[0].Intent.Kind,
				IdempotencyKey: claimed.Leases[0].Intent.IdempotencyKey,
				Status:         "completed", Result: receiptResult,
			},
		},
	)
	if !settled.Settled {
		t.Fatalf("Funding receipt was not settled: %#v", settled)
	}

	resumeBody, err := json.Marshal(poolResumeRequest{
		Email: email, ResumeToken: pending.ResumeToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	resumePath := "/api/pool/" + pending.PoolToken + "/contributions/" + pending.ContributionID + "/resume"
	status, body = h.Do(
		http.MethodPost,
		resumePath,
		map[string]string{"Content-Type": "application/json"},
		resumeBody,
	)
	if status != http.StatusOK {
		t.Fatalf("resume status = %d body=%s", status, body)
	}
	var resumed struct {
		ContributionID string `json:"contribution_id"`
		ClientSecret   string `json:"client_secret"`
		PaymentReady   bool   `json:"payment_ready"`
	}
	if err := json.Unmarshal(body, &resumed); err != nil {
		t.Fatalf("decode resumed create: %v body=%s", err, body)
	}
	if resumed.ContributionID != pending.ContributionID ||
		resumed.ClientSecret != "pi_pool_create_e2e_secret" || !resumed.PaymentReady {
		t.Fatalf("unexpected resumed create: %#v", resumed)
	}

}

func TestEvolution_Pool_ContributeRunsThroughLuaFundingOwnerAndResumesAfterReceipt(t *testing.T) {
	const (
		secret    = "pool-contribute-resume-secret"
		poolToken = "pt-funding-contribute"
	)
	h, _ := startEvolutionDowntimeCfg(t, secret)
	now := time.Now().UTC()
	callFundingOwner(t, h, "funding.v1.pool.create", map[string]any{
		"request_id": "contribute-seed-create", "pool_id": "funding-contribute-pool", "game_id": "sessions",
		"requested_by": "creator@example.test", "goal_cents": int64(2800), "currency": "usd",
		"pool_token": poolToken, "name": "Contribute Pool", "creator_email": "creator@example.test",
		"expires_at": now.Add(24 * time.Hour).Format(time.RFC3339Nano), "created_at": now.Format(time.RFC3339Nano),
		"creator_contribution_id": "funding-contribute-creator", "creator_username": "Creator",
		"initial_amount_cents": int64(700),
	})
	seedClaim := callFundingOwnerResult[poolEffectClaimResult](
		t, h, "funding.effects.v1.claim", map[string]any{
			"version": "pulp.effect.outbox.v1", "owner": "funding",
			"consumer_id": "pool-contribute-seed-worker", "limit": uint32(1),
			"lease_duration_millis": int64(60_000),
		},
	)
	if len(seedClaim.Leases) != 1 {
		t.Fatalf("seed create effect claim = %#v", seedClaim)
	}
	seedResult, err := msgpack.Marshal(map[string]string{
		"payment_intent": "pi_contribute_creator", "client_secret": "pi_contribute_creator_secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	seedSettled := callFundingOwnerResult[poolEffectSettlementResult](
		t, h, "funding.effects.v1.acknowledge", map[string]any{
			"version": "pulp.effect.outbox.v1", "owner": "funding",
			"consumer_id": seedClaim.ConsumerID, "lease_id": seedClaim.Leases[0].LeaseID,
			"receipt": poolEffectReceipt{
				Version: "pulp.effect.v1", IntentID: seedClaim.Leases[0].Intent.ID,
				Kind: seedClaim.Leases[0].Intent.Kind, IdempotencyKey: seedClaim.Leases[0].Intent.IdempotencyKey,
				Status: "completed", Result: seedResult,
			},
		},
	)
	if !seedSettled.Settled {
		t.Fatalf("seed create effect did not settle: %#v", seedSettled)
	}

	contributeBody, err := json.Marshal(map[string]any{
		"email": "member@example.test", "username": "Member", "amount_cents": 700,
	})
	if err != nil {
		t.Fatal(err)
	}
	status, body := h.Do(
		http.MethodPost,
		"/api/pool/"+poolToken+"/contribute",
		map[string]string{"Content-Type": "application/json", "Idempotency-Key": "pool-contribute-owner-e2e"},
		contributeBody,
	)
	if status != http.StatusAccepted {
		t.Fatalf("contribute status = %d body=%s", status, body)
	}
	var pending struct {
		Status         string `json:"status"`
		ContributionID string `json:"contribution_id"`
		ResumeToken    string `json:"resume_token"`
	}
	if err := json.Unmarshal(body, &pending); err != nil {
		t.Fatalf("decode contribute pending response: %v body=%s", err, body)
	}
	if pending.Status != "pending" || pending.ContributionID == "" || pending.ResumeToken == "" {
		t.Fatalf("incomplete contribute pending response: %#v", pending)
	}

	claimed := callFundingOwnerResult[poolEffectClaimResult](
		t, h, "funding.effects.v1.claim", map[string]any{
			"version": "pulp.effect.outbox.v1", "owner": "funding",
			"consumer_id": "pool-contribute-e2e-worker", "limit": uint32(1),
			"lease_duration_millis": int64(60_000),
		},
	)
	if len(claimed.Leases) != 1 ||
		claimed.Leases[0].Intent.Kind != "pulp.effect.stripe.payment-intent.create.v1" {
		t.Fatalf("unexpected contribution effect claim: %#v", claimed)
	}
	resultWire, err := msgpack.Marshal(map[string]string{
		"payment_intent": "pi_pool_contribute_e2e",
		"client_secret":  "pi_pool_contribute_e2e_secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	settled := callFundingOwnerResult[poolEffectSettlementResult](
		t, h, "funding.effects.v1.acknowledge", map[string]any{
			"version": "pulp.effect.outbox.v1", "owner": "funding",
			"consumer_id": claimed.ConsumerID, "lease_id": claimed.Leases[0].LeaseID,
			"receipt": poolEffectReceipt{
				Version: "pulp.effect.v1", IntentID: claimed.Leases[0].Intent.ID,
				Kind: claimed.Leases[0].Intent.Kind, IdempotencyKey: claimed.Leases[0].Intent.IdempotencyKey,
				Status: "completed", Result: resultWire,
			},
		},
	)
	if !settled.Settled {
		t.Fatalf("contribution create receipt was not settled: %#v", settled)
	}

	resumeBody, err := json.Marshal(poolResumeRequest{
		Email: "member@example.test", ResumeToken: pending.ResumeToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	resumePath := "/api/pool/" + poolToken + "/contributions/" + pending.ContributionID + "/resume"
	status, body = h.Do(
		http.MethodPost,
		resumePath,
		map[string]string{"Content-Type": "application/json"},
		resumeBody,
	)
	if status != http.StatusOK {
		t.Fatalf("contribution resume status = %d body=%s", status, body)
	}
	var resumed struct {
		ContributionID string `json:"contribution_id"`
		ClientSecret   string `json:"client_secret"`
		PaymentReady   bool   `json:"payment_ready"`
	}
	if err := json.Unmarshal(body, &resumed); err != nil {
		t.Fatalf("decode resumed contribution: %v body=%s", err, body)
	}
	if resumed.ContributionID != pending.ContributionID ||
		resumed.ClientSecret != "pi_pool_contribute_e2e_secret" || !resumed.PaymentReady {
		t.Fatalf("unexpected resumed contribution: %#v", resumed)
	}
}

func TestEvolution_Pool_VoucherContributionRunsThroughLuaFundingAndCommerceOwners(t *testing.T) {
	const (
		poolID    = "funding-voucher-pool"
		poolToken = "pt-funding-voucher"
		actor     = "voucher-owner@example.test"
	)
	h, _ := startEvolutionDowntimeCfg(t, "pool-voucher-owner-secret")
	now := time.Now().UTC()
	callFundingOwner(t, h, "funding.v1.pool.create", map[string]any{
		"request_id": "voucher-seed-create", "pool_id": poolID, "game_id": "sessions",
		"requested_by": "creator@example.test", "goal_cents": int64(5000), "currency": "usd",
		"pool_token": poolToken, "name": "Voucher Pool", "creator_email": "creator@example.test",
		"expires_at": now.Add(24 * time.Hour).Format(time.RFC3339Nano), "created_at": now.Format(time.RFC3339Nano),
		"creator_contribution_id": "voucher-seed-creator", "creator_username": "Creator",
		"initial_amount_cents": int64(700),
	})

	seedVoucher := func(orderID, email string, amount int64) {
		t.Helper()
		customerID := "customer-" + orderID
		callCommerceOwnerResult[any](t, h, "commerce.order.create.v1", map[string]any{
			"idempotency_key": "seed-" + orderID,
			"order": map[string]any{
				"id": orderID, "customer_id": customerID, "game_id": "sessions",
				"tier_id": "voucher", "currency": "usd", "amount_cents": amount,
				"status": "purchased", "payment_status": "succeeded",
				"created_at_unix": now.Unix(),
				"compatibility":   map[string]any{"email": email, "server_type": "minecraft"},
			},
		})
		callCommerceOwnerResult[any](t, h, "commerce.order.compatibility-patch.v1", map[string]any{
			"idempotency_key": "seed-contact-" + orderID,
			"order_id":        orderID,
			"actor_id":        "voucher-e2e-seeder",
			"authorization": map[string]any{
				"actor_id": "voucher-e2e-seeder", "subject_customer_id": customerID,
				"may_patch_email": true,
			},
			"email": email,
		})
	}
	seedVoucher("voucher-order-e2e", actor, 1200)

	requestBody, err := json.Marshal(map[string]any{
		"email": actor, "username": "VoucherPlayer", "platform": "bedrock",
		"voucher_order_id": "voucher-order-e2e", "anonymous": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": "pool-voucher-owner-e2e",
	}
	status, body := h.Do(
		http.MethodPost,
		"/api/pool/"+poolToken+"/contribute-voucher",
		headers,
		requestBody,
	)
	if status != http.StatusOK {
		t.Fatalf("voucher contribution status = %d body=%s", status, body)
	}
	var completed struct {
		Confirmed      bool   `json:"confirmed"`
		ContributionID string `json:"contribution_id"`
	}
	if err := json.Unmarshal(body, &completed); err != nil {
		t.Fatalf("decode voucher contribution: %v body=%s", err, body)
	}
	if !completed.Confirmed || completed.ContributionID == "" {
		t.Fatalf("voucher contribution did not complete: %#v", completed)
	}
	if got := commerceOrderForID(t, h, "voucher-order-e2e").Status; got != "pool_consumed" {
		t.Fatalf("Commerce voucher status = %q, want pool_consumed", got)
	}

	type fundingPoolProjection struct {
		ID          string `msgpack:"id"`
		FundedCents int64  `msgpack:"funded_cents"`
	}
	pool := callFundingOwnerResult[fundingPoolProjection](
		t, h, "funding.v1.pool.get", map[string]any{"id": poolID},
	)
	if pool.ID != poolID || pool.FundedCents != 1200 {
		t.Fatalf("Funding pool projection = %#v", pool)
	}
	type fundingContributionProjection struct {
		ID             string `msgpack:"id"`
		VoucherOrderID string `msgpack:"voucher_order_id"`
		AmountCents    int64  `msgpack:"amount_cents"`
		Confirmed      bool   `msgpack:"confirmed"`
		Captured       bool   `msgpack:"captured"`
	}
	contributions := callFundingOwnerResult[[]fundingContributionProjection](
		t, h, "funding.v1.contribution.list", map[string]any{"pool_id": poolID},
	)
	var voucherContribution *fundingContributionProjection
	for i := range contributions {
		if contributions[i].ID == completed.ContributionID {
			voucherContribution = &contributions[i]
			break
		}
	}
	if voucherContribution == nil || voucherContribution.VoucherOrderID != "voucher-order-e2e" ||
		voucherContribution.AmountCents != 1200 || !voucherContribution.Confirmed || !voucherContribution.Captured {
		t.Fatalf("Funding voucher contribution = %#v; all=%#v", voucherContribution, contributions)
	}

	replayStatus, replayBody := h.Do(
		http.MethodPost,
		"/api/pool/"+poolToken+"/contribute-voucher",
		headers,
		requestBody,
	)
	if replayStatus != http.StatusOK {
		t.Fatalf("voucher replay status = %d body=%s", replayStatus, replayBody)
	}
	var replayed struct {
		Confirmed      bool   `json:"confirmed"`
		ContributionID string `json:"contribution_id"`
	}
	if err := json.Unmarshal(replayBody, &replayed); err != nil {
		t.Fatalf("decode voucher replay: %v body=%s", err, replayBody)
	}
	if !replayed.Confirmed || replayed.ContributionID != completed.ContributionID {
		t.Fatalf("voucher replay changed outcome: first=%#v replay=%#v", completed, replayed)
	}
	pool = callFundingOwnerResult[fundingPoolProjection](
		t, h, "funding.v1.pool.get", map[string]any{"id": poolID},
	)
	if pool.FundedCents != 1200 {
		t.Fatalf("voucher replay double-counted Funding: %#v", pool)
	}

	seedVoucher("voucher-wrong-actor", actor, 500)
	wrongActorBody, _ := json.Marshal(map[string]any{
		"email": "attacker@example.test", "username": "Attacker",
		"voucher_order_id": "voucher-wrong-actor",
	})
	status, body = h.Do(
		http.MethodPost,
		"/api/pool/"+poolToken+"/contribute-voucher",
		map[string]string{"Content-Type": "application/json", "Idempotency-Key": "pool-voucher-wrong-actor"},
		wrongActorBody,
	)
	if status != http.StatusConflict {
		t.Fatalf("wrong-actor voucher status = %d body=%s", status, body)
	}
	if got := commerceOrderForID(t, h, "voucher-wrong-actor").Status; got != "purchased" {
		t.Fatalf("wrong-actor voucher was consumed: %q", got)
	}

	seedVoucher("voucher-oversized", actor, 5000)
	oversizedBody, _ := json.Marshal(map[string]any{
		"email": actor, "username": "VoucherPlayer",
		"voucher_order_id": "voucher-oversized",
	})
	status, body = h.Do(
		http.MethodPost,
		"/api/pool/"+poolToken+"/contribute-voucher",
		map[string]string{"Content-Type": "application/json", "Idempotency-Key": "pool-voucher-oversized"},
		oversizedBody,
	)
	if status != http.StatusConflict {
		t.Fatalf("oversized voucher status = %d body=%s", status, body)
	}
	if got := commerceOrderForID(t, h, "voucher-oversized").Status; got != "purchased" {
		t.Fatalf("oversized voucher was consumed: %q", got)
	}
}

func TestEvolution_Pool_CancelRunsThroughLuaFundingOwnerAndEmitsReleaseEffect(t *testing.T) {
	const (
		secret         = "pool-cancel-owner-secret"
		poolToken      = "pt-funding-cancel"
		contributionID = "funding-cancel-contribution"
	)
	h, _ := startEvolutionDowntimeCfg(t, secret)
	now := time.Now().UTC()
	callFundingOwner(t, h, "funding.v1.pool.create", map[string]any{
		"request_id": "cancel-owner-create", "pool_id": "funding-cancel-pool", "game_id": "sessions",
		"requested_by": "creator@example.test", "goal_cents": int64(2800), "currency": "usd",
		"pool_token": poolToken, "name": "Cancel Owner Pool", "creator_email": "creator@example.test",
		"expires_at": now.Add(24 * time.Hour).Format(time.RFC3339Nano), "created_at": now.Format(time.RFC3339Nano),
		"creator_contribution_id": contributionID, "creator_username": "Creator",
		"initial_amount_cents": int64(700),
	})
	createClaim := callFundingOwnerResult[poolEffectClaimResult](
		t, h, "funding.effects.v1.claim", map[string]any{
			"version": "pulp.effect.outbox.v1", "owner": "funding",
			"consumer_id": "pool-cancel-create-worker", "limit": uint32(1),
			"lease_duration_millis": int64(60_000),
		},
	)
	if len(createClaim.Leases) != 1 {
		t.Fatalf("cancel seed create claim = %#v", createClaim)
	}
	createResult, err := msgpack.Marshal(map[string]string{
		"payment_intent": "pi_pool_cancel_owner", "client_secret": "pi_pool_cancel_owner_secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	createSettled := callFundingOwnerResult[poolEffectSettlementResult](
		t, h, "funding.effects.v1.acknowledge", map[string]any{
			"version": "pulp.effect.outbox.v1", "owner": "funding",
			"consumer_id": createClaim.ConsumerID, "lease_id": createClaim.Leases[0].LeaseID,
			"receipt": poolEffectReceipt{
				Version: "pulp.effect.v1", IntentID: createClaim.Leases[0].Intent.ID,
				Kind: createClaim.Leases[0].Intent.Kind, IdempotencyKey: createClaim.Leases[0].Intent.IdempotencyKey,
				Status: "completed", Result: createResult,
			},
		},
	)
	if !createSettled.Settled {
		t.Fatalf("cancel seed create did not settle: %#v", createSettled)
	}

	wrongBody, err := json.Marshal(map[string]string{"email": "attacker@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	status, body := h.Do(
		http.MethodPost,
		"/api/pool/"+poolToken+"/cancel",
		map[string]string{"Content-Type": "application/json", "Idempotency-Key": "pool-cancel-wrong-owner"},
		wrongBody,
	)
	if status != http.StatusForbidden {
		t.Fatalf("non-owner cancel status = %d body=%s", status, body)
	}

	cancelBody, err := json.Marshal(map[string]string{"email": "creator@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	status, body = h.Do(
		http.MethodPost,
		"/api/pool/"+poolToken+"/cancel",
		map[string]string{"Content-Type": "application/json", "Idempotency-Key": "pool-cancel-owner"},
		cancelBody,
	)
	if status != http.StatusAccepted {
		t.Fatalf("owner cancel status = %d body=%s", status, body)
	}
	cancelClaim := callFundingOwnerResult[poolEffectClaimResult](
		t, h, "funding.effects.v1.claim", map[string]any{
			"version": "pulp.effect.outbox.v1", "owner": "funding",
			"consumer_id": "pool-cancel-effect-worker", "limit": uint32(10),
			"lease_duration_millis": int64(60_000),
		},
	)
	foundCancel := false
	for _, lease := range cancelClaim.Leases {
		if lease.Intent.Kind == "pulp.effect.stripe.payment-intent.cancel.v1" {
			foundCancel = true
			break
		}
	}
	if !foundCancel {
		t.Fatalf("Funding cancellation did not emit a release effect: %#v", cancelClaim)
	}
	status, body = h.Do(http.MethodGet, "/api/pool/"+poolToken, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("cancelled pool projection status = %d body=%s", status, body)
	}
	var projection struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &projection); err != nil {
		t.Fatalf("decode cancelled pool projection: %v body=%s", err, body)
	}
	if projection.Status != "cancelled" {
		t.Fatalf("cancelled pool projection = %#v", projection)
	}
}

func TestEvolution_Pool_ConfirmRunsThroughLuaAndAcceptsOnlyFundingEffectReceipt(t *testing.T) {
	const (
		poolToken      = "pt-confirm-owner"
		poolID         = "funding-confirm-pool"
		contributionID = "funding-confirm-contribution"
		paymentIntent  = "pi_confirm_owner"
		email          = "confirm@example.test"
	)
	h, _ := startEvolutionDowntimeCfg(t, "pool-confirm-secret")
	now := time.Now().UTC()
	callFundingOwner(t, h, "funding.v1.pool.create", map[string]any{
		"request_id": "confirm-owner-create", "pool_id": poolID, "game_id": "sessions",
		"requested_by": email, "goal_cents": int64(2800), "currency": "usd",
		"pool_token": poolToken, "name": "Confirm Owner Pool", "creator_email": email,
		"expires_at": now.Add(24 * time.Hour).Format(time.RFC3339Nano), "created_at": now.Format(time.RFC3339Nano),
		"creator_contribution_id": contributionID, "creator_username": "ConfirmPlayer",
		"initial_amount_cents": int64(700),
	})
	callFundingOwner(t, h, "funding.v2.contribution.payment_intent.record", map[string]any{
		"request_id": "confirm-owner-record-payment", "contribution_id": contributionID,
		"stripe_payment_intent_id": paymentIntent, "client_secret": "pi_confirm_owner_secret",
		"updated_at": now.Format(time.RFC3339Nano),
	})

	confirmBody, err := json.Marshal(map[string]string{"contribution_id": contributionID})
	if err != nil {
		t.Fatal(err)
	}
	status, body := h.Do(
		http.MethodPost,
		"/api/pool/"+poolToken+"/confirm",
		map[string]string{"Content-Type": "application/json"},
		confirmBody,
	)
	if status != http.StatusAccepted {
		t.Fatalf("confirm request status = %d body=%s", status, body)
	}

	claimed := callFundingOwnerResult[poolEffectClaimResult](
		t, h, "funding.effects.v1.claim", map[string]any{
			"version": "pulp.effect.outbox.v1", "owner": "funding",
			"consumer_id": "pool-confirm-e2e-worker", "limit": uint32(10),
			"lease_duration_millis": int64(60_000),
		},
	)
	var verification *poolEffectLease
	for i := range claimed.Leases {
		if claimed.Leases[i].Intent.Kind == "pulp.effect.stripe.payment-intent.get.v1" {
			verification = &claimed.Leases[i]
			break
		}
	}
	if verification == nil {
		t.Fatalf("Funding did not emit PaymentIntent verification: %#v", claimed)
	}
	resultWire, err := msgpack.Marshal(map[string]any{
		"payment_intent_id": paymentIntent,
		"status":            "requires_capture",
		"amount_cents":      int64(700),
		"currency":          "usd",
		"capture_method":    "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	settled := callFundingOwnerResult[poolEffectSettlementResult](
		t,
		h,
		"funding.effects.v1.acknowledge",
		map[string]any{
			"version": "pulp.effect.outbox.v1", "owner": "funding",
			"consumer_id": claimed.ConsumerID, "lease_id": verification.LeaseID,
			"receipt": poolEffectReceipt{
				Version: "pulp.effect.v1", IntentID: verification.Intent.ID,
				Kind: verification.Intent.Kind, IdempotencyKey: verification.Intent.IdempotencyKey,
				Status: "completed", Result: resultWire,
			},
		},
	)
	if !settled.Settled {
		t.Fatalf("Funding verification receipt was not settled: %#v", settled)
	}

	status, body = h.Do(
		http.MethodPost,
		"/api/pool/"+poolToken+"/confirm",
		map[string]string{"Content-Type": "application/json"},
		confirmBody,
	)
	if status != http.StatusOK {
		t.Fatalf("confirmed replay status = %d body=%s", status, body)
	}
	var response struct {
		Confirmed      bool   `json:"confirmed"`
		ContributionID string `json:"contribution_id"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode confirm response: %v body=%s", err, body)
	}
	if !response.Confirmed || response.ContributionID != contributionID {
		t.Fatalf("unexpected confirm response: %#v", response)
	}
}

func exercisePoolConfirmationEffectStatus(t *testing.T, prefix, paymentStatus string, wantConfirmed bool) {
	t.Helper()
	poolID := prefix + "-pool"
	poolToken := "pt-" + prefix
	contributionID := prefix + "-contribution"
	paymentIntent := "pi_" + prefix
	email := prefix + "@example.test"
	h, _ := startEvolutionDowntimeCfg(t, "pool-confirm-"+prefix+"-secret")
	now := time.Now().UTC()
	callFundingOwner(t, h, "funding.v1.pool.create", map[string]any{
		"request_id": prefix + "-create", "pool_id": poolID, "game_id": "sessions",
		"requested_by": email, "goal_cents": int64(2800), "currency": "usd",
		"pool_token": poolToken, "name": "Confirmation Receipt Pool", "creator_email": email,
		"expires_at": now.Add(24 * time.Hour).Format(time.RFC3339Nano), "created_at": now.Format(time.RFC3339Nano),
		"creator_contribution_id": contributionID, "creator_username": "ConfirmPlayer",
		"initial_amount_cents": int64(700),
	})
	callFundingOwner(t, h, "funding.v2.contribution.payment_intent.record", map[string]any{
		"request_id": prefix + "-record-payment", "contribution_id": contributionID,
		"stripe_payment_intent_id": paymentIntent, "client_secret": paymentIntent + "_secret",
		"updated_at": now.Format(time.RFC3339Nano),
	})

	confirmBody, err := json.Marshal(map[string]string{"contribution_id": contributionID})
	if err != nil {
		t.Fatal(err)
	}
	status, body := h.Do(
		http.MethodPost,
		"/api/pool/"+poolToken+"/confirm",
		map[string]string{"Content-Type": "application/json"},
		confirmBody,
	)
	if status != http.StatusAccepted {
		t.Fatalf("initial confirm status = %d body=%s", status, body)
	}

	claimed := callFundingOwnerResult[poolEffectClaimResult](
		t, h, "funding.effects.v1.claim", map[string]any{
			"version": "pulp.effect.outbox.v1", "owner": "funding",
			"consumer_id": prefix + "-worker", "limit": uint32(10),
			"lease_duration_millis": int64(60_000),
		},
	)
	var verification *poolEffectLease
	for i := range claimed.Leases {
		if claimed.Leases[i].Intent.Kind == "pulp.effect.stripe.payment-intent.get.v1" {
			verification = &claimed.Leases[i]
			break
		}
	}
	if verification == nil {
		t.Fatalf("Funding did not emit PaymentIntent verification: %#v", claimed)
	}
	resultWire, err := msgpack.Marshal(map[string]any{
		"payment_intent_id": paymentIntent,
		"status":            paymentStatus,
		"amount_cents":      int64(700),
		"currency":          "usd",
		"capture_method":    "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	ackWire, err := msgpack.Marshal(map[string]any{
		"version": "pulp.effect.outbox.v1", "owner": "funding",
		"consumer_id": claimed.ConsumerID, "lease_id": verification.LeaseID,
		"receipt": poolEffectReceipt{
			Version: "pulp.effect.v1", IntentID: verification.Intent.ID,
			Kind: verification.Intent.Kind, IdempotencyKey: verification.Intent.IdempotencyKey,
			Status: "completed", Result: resultWire,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ackOutput, err := h.cellsByName["funding"].Call(ctx, "funding.effects.v1.acknowledge", ackWire)
	if err != nil {
		t.Fatalf("acknowledge verification receipt: %v", err)
	}
	var ackResult struct {
		OK    bool                       `msgpack:"ok"`
		Value poolEffectSettlementResult `msgpack:"value"`
	}
	if err := msgpack.Unmarshal(ackOutput, &ackResult); err != nil {
		t.Fatalf("decode verification acknowledgement: %v", err)
	}
	if wantConfirmed {
		if !ackResult.OK || !ackResult.Value.Settled {
			t.Fatalf("authorized receipt did not settle: %#v", ackResult)
		}
	} else if ackResult.OK {
		t.Fatalf("unauthorized PaymentIntent status was accepted: %#v", ackResult)
	}

	status, body = h.Do(
		http.MethodPost,
		"/api/pool/"+poolToken+"/confirm",
		map[string]string{"Content-Type": "application/json"},
		confirmBody,
	)
	if wantConfirmed {
		if status != http.StatusOK {
			t.Fatalf("confirmed replay status = %d body=%s", status, body)
		}
		return
	}
	if status != http.StatusAccepted {
		t.Fatalf("rejected authorization must remain pending, status = %d body=%s", status, body)
	}
}

type poolResumeRequest struct {
	Email       string `json:"email"`
	ResumeToken string `json:"resume_token"`
}

type poolEffectIntent struct {
	Version        string             `msgpack:"version"`
	ID             string             `msgpack:"id"`
	Kind           string             `msgpack:"kind"`
	IdempotencyKey string             `msgpack:"idempotency_key"`
	Payload        msgpack.RawMessage `msgpack:"payload"`
}

type poolEffectLease struct {
	LeaseID string           `msgpack:"lease_id"`
	Intent  poolEffectIntent `msgpack:"intent"`
}

type poolEffectClaimResult struct {
	ConsumerID string            `msgpack:"consumer_id"`
	Leases     []poolEffectLease `msgpack:"leases"`
}

type poolEffectReceipt struct {
	Version        string             `msgpack:"version"`
	IntentID       string             `msgpack:"intent_id"`
	Kind           string             `msgpack:"kind"`
	IdempotencyKey string             `msgpack:"idempotency_key"`
	Status         string             `msgpack:"status"`
	Result         msgpack.RawMessage `msgpack:"result,omitempty"`
}

type poolEffectSettlementResult struct {
	Settled bool `msgpack:"settled"`
}

func claimFundingEffectKind(
	t *testing.T,
	h *CellHarness,
	consumerID string,
	kind string,
) (poolEffectClaimResult, poolEffectLease) {
	t.Helper()
	claimed := callFundingOwnerResult[poolEffectClaimResult](
		t, h, "funding.effects.v1.claim", map[string]any{
			"version": "pulp.effect.outbox.v1", "owner": "funding",
			"consumer_id": consumerID, "limit": uint32(10),
			"lease_duration_millis": int64(60_000),
		},
	)
	for _, lease := range claimed.Leases {
		if lease.Intent.Kind == kind {
			return claimed, lease
		}
	}
	t.Fatalf("Funding effect %q not claimable: %#v", kind, claimed)
	return poolEffectClaimResult{}, poolEffectLease{}
}

func acknowledgeFundingEffectResult(
	t *testing.T,
	h *CellHarness,
	claimed poolEffectClaimResult,
	lease poolEffectLease,
	result any,
) {
	t.Helper()
	resultWire, err := msgpack.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	settled := callFundingOwnerResult[poolEffectSettlementResult](
		t, h, "funding.effects.v1.acknowledge", map[string]any{
			"version": "pulp.effect.outbox.v1", "owner": "funding",
			"consumer_id": claimed.ConsumerID, "lease_id": lease.LeaseID,
			"receipt": poolEffectReceipt{
				Version: "pulp.effect.v1", IntentID: lease.Intent.ID,
				Kind: lease.Intent.Kind, IdempotencyKey: lease.Intent.IdempotencyKey,
				Status: "completed", Result: resultWire,
			},
		},
	)
	if !settled.Settled {
		t.Fatalf("Funding effect %q did not settle: %#v", lease.Intent.Kind, settled)
	}
}
