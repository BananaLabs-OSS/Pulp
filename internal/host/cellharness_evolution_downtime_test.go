package host

// PROOF SLICE — the downtime-compensation reconcile, driven THROUGH the real
// Evolution cell (evolution.wasm) under the Pulp host, WITHOUT the native
// internal/poller mirror.
//
// This is the parity proof for [[kill-native-twin-plan]] step (1): the native
// mirror's Evolution/internal/poller/downtime_comp_test.go asserts the HOURS-
// granular auto-extend-on-downtime behaviour by calling p.healthCheckActive()
// directly on a native *poller. The cell links only under GOOS=wasip1 + the
// Pulp host, so it cannot be `go test`-ed that way. This harness instead:
//
//   - builds the Evolution cell to wasm and Inits it under a test Pulp host
//     (StartCellHTTP + the shared stripe/s3/docker/sibling stubs), so bootstrap
//     runs the real migrations against a host-provided temp SQLite (the test
//     opens that same data.db to inspect it directly + seed deployment reference
//     data — the tier/game_visibility catalog no cell endpoint creates);
//   - drives a server to ACTIVE entirely through the cell's REAL customer
//     endpoints (provisionActiveServer): POST /api/checkout creates the paid
//     order, POST /api/webhooks/stripe flips it to `paid` (the stub's webhook
//     verify passes; with no sessions gene the cell's fallback marks it paid),
//     and pumped ticks run enqueueNewOrders -> promoteQueue -> provision ->
//     createContainer against the Bananagine stub, so the ACTIVE row is written
//     by the cell's OWN poller — not seeded on a side connection;
//   - stubs the cell's OUTBOUND HTTP (Bananagine) with a canned responder whose
//     container status is a live atomic — /health=200, a POST create that 201s
//     with an id/ip/ports, and a flippable stopped<->running status;
//   - captures every outbound Resend email by decoding the workers.Submit
//     payload (the async http.fetch the cell enqueues for email); and
//   - DRIVES THE POLLER TICK via driveTick: every inbound request runs the
//     cell's OnStep -> poll.tickIfDue -> mainTick -> healthCheckActive ->
//     settleDowntimeCompensation. NOTE (corrective, see ADR cell-test-harness):
//     the harness's idle step-pump does NOT reach OnStep, so mainTick only
//     advances on a request — that, not any "cross-connection SQLite
//     invisibility", is why the earlier settle proofs never saw the poller act.
//     Cross-connection reads work fine (the poller sees the checkout-written
//     order and the test-backdated first_seen).
//
// CLOCK NOTE (why the window is opened THROUGH the cell, then backdated): the
// settle path measures the outage as time.Since(first_seen) using the CELL's
// clock. Under wazero the cell's wall clock is NOT the test process's clock, so
// a first_seen written from the host's time.Now() yields a wrong (often
// sub-floor) outage. The native test sidesteps this by opening the window with
// a real "stopped" tick (recordAlert stamps first_seen in the poller's own
// clock) and THEN backdating first_seen by the simulated age. This harness does
// the same: flip Bananagine to "stopped" so one tick opens the window in the
// cell's clock, read that first_seen back, subtract the simulated age, then
// flip to "running" so the next tick settles. Outage measurement is therefore
// entirely in the cell's clock frame — clock-consistent and deterministic.
//
// It then asserts the SAME reconciled behaviour the native mirror pins:
//   - a ~90-min outage credits EXACTLY 2h and fires ONE compensation email
//     whose subject carries the server display name; the body renders "2 hours";
//   - a ~40-min outage (below the 1-hour floor) credits NOTHING, no email;
//   - a >=24h outage renders the credit in DAYS ("1 day"), never hours;
//   - a repeat recovery tick does NOT double-credit and sends no 2nd email.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

const downtimeCompSubject = "We Added Time Back for Downtime"

// ---- controllable Bananagine outbound stub -------------------------------
//
// Replaces transport.http.outbound for the downtime harness. It answers the
// endpoints the health path touches, reading the container status from a live
// atomic so the test can flip stopped <-> running (mirrors the native test's
// fakeBananagine httptest.Server).

var evoBananagineStatus atomic.Value // string: "running" | "stopped"

func setEvoBananagineStatus(s string) { evoBananagineStatus.Store(s) }

func evoBananagineOutboundStub() ext.Capability {
	bind := func(b wazero.HostModuleBuilder, _ ext.Cell) error {
		fetch := func(ctx context.Context, m api.Module, reqPtr, reqLen, op, ol uint32) uint32 {
			var req struct {
				Method string `msgpack:"method"`
				URL    string `msgpack:"url"`
			}
			_ = readStubMsgpack(m, reqPtr, reqLen, &req)
			status, body := evoBananagineResponse(req.Method, req.URL)
			resp := struct {
				Status  uint32            `msgpack:"status"`
				Headers map[string]string `msgpack:"headers"`
				Body    []byte            `msgpack:"body"`
			}{Status: status, Headers: map[string]string{"content-type": "application/json"}, Body: body}
			return writeStubMsgpack(ctx, m, resp, op, ol)
		}
		begin := func(_ context.Context, _ api.Module, _, _, _, _ uint32) uint32 { return 5 }
		read := func(_ context.Context, _ api.Module, _, _, _, _, _ uint32) uint32 { return 5 }
		closeFn := func(_ context.Context, _ api.Module, _, _ uint32) uint32 { return 0 }
		b.NewFunctionBuilder().WithFunc(fetch).Export("http_fetch")
		b.NewFunctionBuilder().WithFunc(begin).Export("http_fetch_begin")
		b.NewFunctionBuilder().WithFunc(read).Export("http_fetch_read")
		b.NewFunctionBuilder().WithFunc(closeFn).Export("http_fetch_close")
		return nil
	}
	return ext.Capability{Name: "transport.http.outbound", Register: bind, Stub: bind}
}

// evoBananagineResponse mirrors the native fakeBananagine mux. Method is read
// so POST /orchestration/servers (createContainer) and GET /orchestration/servers
// (reconcileOrphanContainers list) — which share a URL — return different bodies.
func evoBananagineResponse(method, rawURL string) (uint32, []byte) {
	path := rawURL
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	switch {
	// Mojang Java-UUID lookup (resolveJavaUUID, reached via POST
	// /api/servers/:id/whitelist with platform=java). A "Nobody" name 404s
	// (player-not-found); any other name returns Notch's canonical 32-hex id so
	// the handler can dash-format and echo it back.
	case strings.Contains(path, "/users/profiles/minecraft/"):
		if strings.HasSuffix(path, "/Nobody") {
			return 404, []byte(`{}`)
		}
		return 200, []byte(`{"id":"069a79f444e94726a5befca90e38aaf5","name":"Notch"}`)
	// GeyserMC Bedrock-UUID lookup (resolveBedrockUUID, platform=bedrock).
	case strings.Contains(path, "/v2/utils/uuid/bedrock_or_java/"):
		return 200, []byte(`{"id":"00000000000000000009000005ccdde3"}`)
	case strings.HasSuffix(path, "/health"):
		return 200, []byte(`{}`)
	case strings.HasSuffix(path, "/orchestration/stats"):
		// nodeBudget's per-node capacity report. A node whose bananagine_url
		// routes to the "budgetnode" host reports a REAL budget so the node-
		// registry test can prove nodeBudget sums it; every other host (the
		// default localhost the other proofs use) reports an empty object, i.e.
		// cpu_budget/memory_budget 0 — byte-identical to the pre-registry stub,
		// which had no /stats case and fell through to `{}`.
		if strings.Contains(rawURL, "budgetnode") {
			return 200, []byte(`{"node":{"cpu_budget":14,"memory_budget":48}}`)
		}
		return 200, []byte(`{}`)
	case strings.HasSuffix(path, "/orchestration/servers"):
		if method == "POST" {
			// createContainer — 201 with an id/name/ip/ports so provision()
			// can flip the server to active. Deterministic identifiers keyed
			// off the count so retries/adopt don't collide.
			n := evoContainerNext.Add(1)
			return 201, []byte(fmt.Sprintf(
				`{"id":"cont-%d","name":"srv-%d","ip":"10.0.0.1","ports":{"java":25565,"bedrock":19132}}`, n, n))
		}
		// reconcileOrphanContainers list — empty => no orphans to reap.
		return 200, []byte(`[]`)
	case strings.Contains(path, "/orchestration/servers/") &&
		(strings.HasSuffix(path, "/restart") || strings.HasSuffix(path, "/exec")):
		return 200, []byte(`{}`)
	case strings.Contains(path, "/orchestration/servers/"):
		// getContainerStatus / adoptExistingContainer probe. A GET for a
		// concrete container id reports the flippable live status.
		st, _ := evoBananagineStatus.Load().(string)
		if st == "" {
			st = "running"
		}
		return 200, []byte(fmt.Sprintf(`{"id":%q,"status":%q,"ip":"10.0.0.1","ports":{"java":25565}}`,
			strings.TrimPrefix(path[strings.LastIndex(path, "/")+1:], ""), st))
	default:
		return 200, []byte(`{}`)
	}
}

// evoContainerNext mints unique container ids for the createContainer stub.
var evoContainerNext atomic.Uint32

// ---- email-capturing workers stub ----------------------------------------
//
// The cell enqueues transactional email as an async workers.Submit http.fetch
// to api.resend.com. This stub decodes that submit payload, records the Resend
// body (to/subject/html), and returns a valid task id (>=100 so the cell's
// workers.Submit treats it as accepted). workers_result stays "pending" so the
// cell neither resubmits (one capture per email) nor decodes a result body.

type evoCapturedEmail struct{ to, subject, html string }

var (
	evoEmailMu    sync.Mutex
	evoEmails     []evoCapturedEmail
	evoWorkerNext atomic.Uint32 // task ids start at 100
)

func resetEvoEmails() {
	evoEmailMu.Lock()
	evoEmails = nil
	evoEmailMu.Unlock()
	evoWorkerNext.Store(100)
}

// evoCompEmailsFor returns captured downtime-compensation emails to `to`.
func evoCompEmailsFor(to string) []evoCapturedEmail {
	evoEmailMu.Lock()
	defer evoEmailMu.Unlock()
	var out []evoCapturedEmail
	for _, e := range evoEmails {
		if e.to == to && strings.Contains(e.subject, downtimeCompSubject) {
			out = append(out, e)
		}
	}
	return out
}

func evoRecordSubmit(m api.Module, reqPtr, reqLen uint32) {
	var req struct {
		Type string `msgpack:"type"`
		URL  string `msgpack:"url"`
		Body []byte `msgpack:"body"`
	}
	if !readStubMsgpack(m, reqPtr, reqLen, &req) {
		return
	}
	if !strings.Contains(req.URL, "resend.com") {
		return
	}
	var payload struct {
		To      []string `json:"to"`
		Subject string   `json:"subject"`
		HTML    string   `json:"html"`
	}
	if json.Unmarshal(req.Body, &payload) != nil || len(payload.To) == 0 {
		return
	}
	evoEmailMu.Lock()
	evoEmails = append(evoEmails, evoCapturedEmail{to: payload.To[0], subject: payload.Subject, html: payload.HTML})
	evoEmailMu.Unlock()
}

func evoCapturingWorkersStub() ext.Capability {
	bind := func(b wazero.HostModuleBuilder, _ ext.Cell) error {
		submit := func(_ context.Context, m api.Module, reqPtr, reqLen uint32) uint32 {
			evoRecordSubmit(m, reqPtr, reqLen)
			return evoWorkerNext.Add(1) // >=101, a valid accepted task id
		}
		fire := func(_ context.Context, m api.Module, reqPtr, reqLen uint32) uint32 {
			evoRecordSubmit(m, reqPtr, reqLen)
			return 0
		}
		// statusPending(0): the cell keeps the task in flight and never
		// resubmits, so each email is captured exactly once at submit.
		result := func(_ context.Context, _ api.Module, _, _, _ uint32) uint32 { return 0 }
		b.NewFunctionBuilder().WithFunc(submit).Export("workers_submit")
		b.NewFunctionBuilder().WithFunc(fire).Export("workers_submit_fire")
		b.NewFunctionBuilder().WithFunc(result).Export("workers_result")
		return nil
	}
	return ext.Capability{Name: "workers", Register: bind, Stub: bind}
}

// evoDowntimeOverrides is the override set for the downtime harness: the shared
// stripe/s3/docker/sibling stubs plus the controllable Bananagine outbound and
// the email-capturing workers stub (both replace their ext.All() cap by name).
func evoDowntimeOverrides() []ext.Capability {
	return []ext.Capability{
		stripeStubCapability(),
		s3StubCapability(),
		dockerStubCapability(),
		siblingStubCapability(),
		evoBananagineOutboundStub(),
		evoCapturingWorkersStub(),
	}
}

// ---- harness --------------------------------------------------------------

func startEvolutionDowntime(t *testing.T) (*CellHarness, *sql.DB) {
	return startEvolutionDowntimeCfg(t, "")
}

// startEvolutionDowntimeCfg is startEvolutionDowntime with a configurable
// internal_secret. An empty secret leaves the internal-auth'd routes
// (/api/servers/*) fail-closed (503) as the downtime proofs expect; a non-empty
// secret opens them so a test can drive an internal endpoint by sending the
// matching X-Internal-Secret header (see the whitelist-add UUID ports).
func startEvolutionDowntimeCfg(t *testing.T, internalSecret string) (*CellHarness, *sql.DB) {
	return startEvolutionDowntimeExtra(t, internalSecret, nil)
}

// startEvolutionDowntimeExtra is startEvolutionDowntimeCfg with extra cell
// config keys merged over the defaults. The capacity proofs need it: at the
// default cpu_budget/memory_budget of 0, Marrow's canFitTemplate skips the
// check entirely (`if opts.CPUBudget > 0 && ...`), so the deploy-gate kernel
// can never return DenyCapacityFull and the branch under test is unreachable.
func startEvolutionDowntimeExtra(t *testing.T, internalSecret string, extra map[string]any) (*CellHarness, *sql.DB) {
	t.Helper()
	setEvoBananagineStatus("running")
	resetEvoEmails()

	cellCfg := map[string]any{
		"internal_secret":      internalSecret,
		"frontend_url":         "https://sessions.gg",
		"max_servers":          12,
		"poll_interval":        "50ms",
		"server_lifetime":      "336h",
		"refund_threshold":     "10m",
		"db_dialect":           "",
		"r2_account_id":        "stub-account",
		"r2_access_key_id":     "stub-key",
		"r2_secret_access_key": "stub-secret",
		"r2_bucket":            "stub-bucket",
		// Non-empty so enqueueEmail does not short-circuit ("no API key").
		"resend_api_key": "re_stub_downtime",
		// bananagine_url left default (http://localhost:3000); the outbound
		// stub answers by path suffix regardless of host.
	}
	for k, v := range extra {
		cellCfg[k] = v
	}

	h := StartCellHTTP(t, CellHarnessConfig{
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
		},
		Config:              cellCfg,
		CapabilityOverrides: evoDowntimeOverrides(),
	})
	warmEvolution(t, h)

	// Match ext-sqlite's path byte-for-byte (filepath.Join). SQLite keys the
	// WAL shared-memory region on the path string, so a "/" vs "\" mismatch on
	// Windows would put this connection on a SEPARATE WAL view from the cell's
	// (writes invisible in both directions) — the exact split we must avoid.
	dbPath := filepath.Join(h.StorageRoot, "evolution", "data.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open cell db: %v", err)
	}
	db.SetMaxOpenConns(1)
	for _, p := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(p); err != nil {
			t.Fatalf("db pragma %q: %v", p, err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return h, db
}

// seedDowntimeCatalog inserts the ONE enabled tier + the minecraft
// game_visibility row that /api/checkout's deploy-gate kernel requires (an
// enabled tier + a gv row for the template — else it denies "unknown template").
// This is deployment REFERENCE data: on a real box it comes from the
// seed-fresh-db tool (and, for a template's first gv row, from Bananagine's
// template sync). No cell endpoint creates a template's FIRST gv row, so the
// harness seeds it directly via this connection — reference config, NOT a
// test-only write path into the production cell (zero production surface, same
// pattern the harness already used for orders/servers). A synchronous cell
// handler reads it fine (proven: checkout succeeds), because the blocker the
// ADR pinned was never cross-connection invisibility — it was that the poller's
// mainTick wasn't being driven (see driveUntil / the ADR update).
func seedDowntimeCatalog(t *testing.T, db *sql.DB) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO tiers (id, name, label, price_cents, duration, enabled, sort_order, max_cpu, max_ram_mb, created_at)
		 VALUES ('standard','session','Session',1400,'336h',1,0,2.0,4096,?)`, now,
	); err != nil {
		t.Fatalf("seed tier: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO game_visibility (template, tier_id, game_id, enabled)
		 VALUES ('minecraft','standard','minecraft',1)`,
	); err != nil {
		t.Fatalf("seed game_visibility: %v", err)
	}
	checkpoint(db)
}

// driveTick fires one GET /health at the cell. Every inbound request runs the
// cell's OnStep hook (DrainAdminAsyncQueue + poll.tickIfDue) BEFORE dispatch, so
// a request is what advances the poller's mainTick in the harness — the idle
// step-pump alone does not reach OnStep, which is why the earlier settle proofs
// (which waited on background ticks) never saw the poller act. checkpoint()
// flushes this connection's WAL frames so the read-back is immediate.
func driveTick(h *CellHarness, db *sql.DB) {
	h.Do("GET", "/health", nil, nil)
	checkpoint(db)
}

// driveUntil pumps poller ticks (via driveTick) until cond holds or the deadline
// elapses. Replaces the passive waitFor for anything that depends on the poller
// mainTick running (enqueue, provision, health-check, settle).
func driveUntil(t *testing.T, h *CellHarness, db *sql.DB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		driveTick(h, db)
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out driving poller ticks for: %s", what)
}

// provisionActiveServer drives a brand-new server to ACTIVE entirely through the
// cell's REAL customer endpoints, so the ACTIVE row is written by the cell's OWN
// poller (createContainer against the Bananagine stub) — not seeded on a side
// connection. Flow: POST /api/checkout (creates a pending paid-intent order) ->
// POST /api/webhooks/stripe payment_intent.succeeded (the stub's webhook_verify
// passes; with no sessions gene loaded the cell's fallback flips the order
// straight to `paid`) -> pump ticks so enqueueNewOrders + promoteQueue +
// provision create + start the container and mark it active. Returns the
// poller-assigned server id, order id, generated display name, and the initial
// expires_at (the credit baseline).
func provisionActiveServer(t *testing.T, h *CellHarness, db *sql.DB, email string) (serverID, orderID, displayName string, origExpiry time.Time) {
	t.Helper()
	seedDowntimeCatalog(t, db)

	body, _ := json.Marshal(map[string]any{
		"server_type":   "minecraft",
		"email":         email,
		"age_confirmed": true,
		"tos_accepted":  true,
		"eula_accepted": true,
	})
	if s, b := h.Do("POST", "/api/checkout", map[string]string{"Content-Type": "application/json"}, body); s != 200 {
		t.Fatalf("checkout: want 200, got %d (%s)", s, b)
	}
	// The stripe stub mints a PaymentIntent id "pi_stub_<amount_cents>"; the
	// default minecraft price is 1400 with no discount.
	const pi = "pi_stub_1400"
	wh := []byte(fmt.Sprintf(
		`{"id":"evt-%s","type":"payment_intent.succeeded","data":{"object":{"id":%q,"amount_received":1400}}}`,
		email, pi))
	if s, b := h.Do("POST", "/api/webhooks/stripe",
		map[string]string{"Content-Type": "application/json", "Stripe-Signature": "t=1,v1=stub"}, wh); s != 200 {
		t.Fatalf("stripe webhook: want 200, got %d (%s)", s, b)
	}

	driveUntil(t, h, db, "checkout order provisioned to an active server", func() bool {
		return db.QueryRow(
			`SELECT s.id, s.order_id, s.display_name
			   FROM servers s JOIN orders o ON o.id = s.order_id
			  WHERE o.stripe_session_id = ? AND s.state = 'active'`, pi,
		).Scan(&serverID, &orderID, &displayName) == nil
	})
	if serverID == "" {
		t.Fatal("no active server after provisioning")
	}
	origExpiry = readExpiresAt(t, db, serverID)
	return serverID, orderID, displayName, origExpiry
}

// checkpoint forces this connection's WAL frames into the main db file so the
// cell's separate connection reliably observes the write on its next tick.
// Under WAL two in-process connections share the -wal, but a TRUNCATE
// checkpoint removes any ambiguity about visibility timing.
func checkpoint(db *sql.DB) {
	var a, b, c int
	_ = db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&a, &b, &c)
}

// openBackdatedWindow opens a downtime_window THROUGH the cell (so first_seen is
// stamped in the cell's own clock), then backdates it by ageSeconds to simulate
// an outage of that length, then flips the container back to "running" so the
// next settle tick credits it. This keeps the whole outage measurement in the
// cell's clock frame (see the CLOCK NOTE at the top of the file).
//
// It drives poller ticks (driveTick) while waiting for the window to open —
// healthCheckActive runs on the poller's mainTick, which the harness advances
// via inbound requests, not the idle step-pump. The server it acts on is the
// REAL active server provisionActiveServer created through the cell, so
// healthCheckActive sees it on the first stopped tick and recordAlert opens the
// window; the test then backdates first_seen and the recovery tick settles.
func openBackdatedWindow(t *testing.T, h *CellHarness, db *sql.DB, serverID string, ageSeconds int) {
	t.Helper()
	setEvoBananagineStatus("stopped")
	var fs string
	driveUntil(t, h, db, "downtime_window opened by healthCheckActive", func() bool {
		return db.QueryRow(
			`SELECT first_seen FROM alerts WHERE server_id=? AND type='downtime_window' AND resolved=0`,
			serverID,
		).Scan(&fs) == nil && fs != ""
	})
	cellFirst := parseDBTime(t, fs)
	back := cellFirst.Add(-time.Duration(ageSeconds) * time.Second)
	if _, err := db.Exec(
		`UPDATE alerts SET first_seen=?, last_seen=? WHERE server_id=? AND type='downtime_window' AND resolved=0`,
		back, back, serverID,
	); err != nil {
		t.Fatalf("backdate window for %s: %v", serverID, err)
	}
	// Reset the restart bookkeeping the stopped tick advanced, then recover.
	if _, err := db.Exec(`UPDATE servers SET restart_count=0 WHERE id=?`, serverID); err != nil {
		t.Fatalf("reset restart_count for %s: %v", serverID, err)
	}
	checkpoint(db)
	setEvoBananagineStatus("running")
}

func readExpiresAt(t *testing.T, db *sql.DB, serverID string) time.Time {
	t.Helper()
	var raw string
	if err := db.QueryRow(`SELECT expires_at FROM servers WHERE id = ?`, serverID).Scan(&raw); err != nil {
		t.Fatalf("read expires_at for %s: %v", serverID, err)
	}
	return parseDBTime(t, raw)
}

func parseDBTime(t *testing.T, s string) time.Time {
	t.Helper()
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05",
	} {
		if ts, err := time.Parse(layout, s); err == nil {
			return ts.UTC()
		}
	}
	t.Fatalf("unparseable db time %q", s)
	return time.Time{}
}

func countOpenWindows(t *testing.T, db *sql.DB, serverID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM alerts WHERE server_id = ? AND type = 'downtime_window' AND resolved = 0`,
		serverID,
	).Scan(&n); err != nil {
		t.Fatalf("count open windows for %s: %v", serverID, err)
	}
	return n
}

// ===========================================================================
// THE PROOF
// ===========================================================================

func TestEvolution_DowntimeCompensation_CreditsRoundedUpHourOnce(t *testing.T) {
	h, db := startEvolutionDowntime(t)

	const email = "player2h@example.com"
	srvID, _, display, origStored := provisionActiveServer(t, h, db, email)

	// ~90-minute outage: above the 1h floor, rounds UP to exactly 2h.
	openBackdatedWindow(t, h, db, srvID, 90*60)

	// A running-container tick settles the window: +2h and one email.
	driveUntil(t, h, db, "expires_at extended by the downtime credit", func() bool {
		return readExpiresAt(t, db, srvID).Sub(origStored) > time.Minute
	})

	added := readExpiresAt(t, db, srvID).Sub(origStored)
	if added < 2*time.Hour-time.Minute || added > 2*time.Hour+time.Minute {
		t.Fatalf("expected ~2h credit for a ~90m outage, got %s", added)
	}
	if n := countOpenWindows(t, db, srvID); n != 0 {
		t.Fatalf("expected outage window resolved, %d still open", n)
	}

	driveUntil(t, h, db, "compensation email captured", func() bool {
		return len(evoCompEmailsFor(email)) >= 1
	})
	emails := evoCompEmailsFor(email)
	if len(emails) != 1 {
		t.Fatalf("expected exactly 1 compensation email to %s, got %d: %+v", email, len(emails), emails)
	}
	got := emails[0]
	if !strings.Contains(got.subject, display) {
		t.Fatalf("subject = %q, want it to carry display name %q", got.subject, display)
	}
	if !strings.Contains(got.html, "2 hours") {
		t.Fatalf("email body should render \"2 hours\", got:\n%s", got.html)
	}

	// Repeat recovery ticks must NOT double-credit and send no 2nd email.
	afterCredit := readExpiresAt(t, db, srvID)
	for i := 0; i < 8; i++ { // several running-container settle ticks
		driveTick(h, db)
		time.Sleep(50 * time.Millisecond)
	}
	if again := readExpiresAt(t, db, srvID); again.Sub(afterCredit) > time.Second {
		t.Fatalf("double-credit on repeat tick: %s -> %s", afterCredit, again)
	}
	if n := len(evoCompEmailsFor(email)); n != 1 {
		t.Fatalf("second compensation email on repeat tick: total to %s = %d", email, n)
	}
}

func TestEvolution_DowntimeCompensation_BelowHourFloorNoCredit(t *testing.T) {
	h, db := startEvolutionDowntime(t)

	const email = "player40m@example.com"
	srvID, _, _, origStored := provisionActiveServer(t, h, db, email)

	// 40-minute outage: below the 1h meaningful-downtime floor.
	openBackdatedWindow(t, h, db, srvID, 40*60)

	// The window is settled (resolved) on recovery even below the floor.
	driveUntil(t, h, db, "outage window resolved even below floor", func() bool {
		return countOpenWindows(t, db, srvID) == 0
	})

	// Below-floor: no credit, no email.
	if added := readExpiresAt(t, db, srvID).Sub(origStored); added > time.Second {
		t.Fatalf("below-floor outage must not credit; expiry moved by %s", added)
	}
	for i := 0; i < 6; i++ { // give any (erroneous) email time to land
		driveTick(h, db)
		time.Sleep(50 * time.Millisecond)
	}
	if n := len(evoCompEmailsFor(email)); n != 0 {
		t.Fatalf("below-floor outage must not send a compensation email; captured %d", n)
	}
}

func TestEvolution_DowntimeCompensation_DayWording(t *testing.T) {
	h, db := startEvolutionDowntime(t)

	const email = "playerday@example.com"
	srvID, _, _, origStored := provisionActiveServer(t, h, db, email)

	// 24.5h outage: rounds up to exactly 25h -> ">=24h renders in DAYS".
	openBackdatedWindow(t, h, db, srvID, 24*3600+30*60)

	driveUntil(t, h, db, "expires_at extended by the day-scale credit", func() bool {
		return readExpiresAt(t, db, srvID).Sub(origStored) > 20*time.Hour
	})
	added := readExpiresAt(t, db, srvID).Sub(origStored)
	if added < 25*time.Hour-time.Minute || added > 25*time.Hour+time.Minute {
		t.Fatalf("expected ~25h credit for a 24.5h outage, got %s", added)
	}

	driveUntil(t, h, db, "day-wording email captured", func() bool {
		return len(evoCompEmailsFor(email)) >= 1
	})
	got := evoCompEmailsFor(email)[0]
	if !strings.Contains(got.html, "1 day") {
		t.Fatalf("email body should render days (\"1 day\"), got:\n%s", got.html)
	}
	if strings.Contains(got.html, "24 hours") || strings.Contains(got.html, "25 hours") {
		t.Fatalf("email must not render a >=24h credit in hours, got:\n%s", got.html)
	}
}

// TestEvolution_DowntimeHarness_Smoke proves the harness INFRASTRUCTURE works
// end-to-end:
//
//  1. the Evolution cell builds to wasm, Inits under the Pulp host (real
//     migrations), and serves /health — via warmEvolution in startEvolution*;
//  2. the test opens the cell's host-provided data.db and observes the cell's
//     boot-seeded config (cross-connection reads work — the ADR's "cross-
//     connection invisibility" was a MISDIAGNOSIS; see the corrective note);
//  3. a paid order committed by THIS connection IS picked up by the cell's
//     enqueueNewOrders tick and provisioned to a server row — once the poller's
//     mainTick is actually driven (an inbound request runs OnStep -> tickIfDue;
//     the idle step-pump does not, which is what made the earlier settle proofs
//     appear "blocked"). driveTick supplies that drive.
func TestEvolution_DowntimeHarness_Smoke(t *testing.T) {
	h, db := startEvolutionDowntime(t)

	// (2) The cell booted and seeded baseline config on ITS connection; the test
	// connection observes it — cross-connection reads work in both directions.
	var nsc int
	if err := db.QueryRow(`SELECT COUNT(*) FROM site_config`).Scan(&nsc); err != nil || nsc == 0 {
		t.Fatalf("cell->test visibility broken: site_config=%d err=%v", nsc, err)
	}

	// (3) A paid order this connection commits (with the catalog reference rows
	// the enqueue path needs) is enqueued + provisioned by the poller ONCE ticks
	// are driven — refuting the old "the cell never observes external commits"
	// conclusion. This is the corrective infra guard.
	seedDowntimeCatalog(t, db)
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO orders (id, stripe_session_id, server_type, tier_id, email, status, auto_redeem, created_at)
		 VALUES ('smoke-ord','ss_smoke','minecraft','standard','s@e.com','paid',0,?)`, now,
	); err != nil {
		t.Fatalf("seed paid order: %v", err)
	}
	checkpoint(db)

	driveUntil(t, h, db, "poller enqueues + provisions the committed paid order", func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM servers WHERE order_id='smoke-ord'`).Scan(&n)
		return n > 0
	})
	t.Log("confirmed: the poller observes this connection's committed order and provisions it once mainTick is driven (no cross-connection blocker)")
}
