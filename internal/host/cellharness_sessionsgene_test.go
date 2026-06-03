package host

// Cell harness against the REAL deployed Sessions-Gene cell, pinning the
// CRITICAL C2 owner gate (verifyOrderEmail) and the read-IDOR fix (02e3888).
//
// WHY A DEDICATED HARNESS (not StartCellHTTP)
// -------------------------------------------
// Sessions-Gene is NOT a transport.http.inbound cell. It is an Evolution
// *gene*: it declares `provides = ["sessions"]` and registers its handlers as
// sibling functions via gene.Register (Fiber/pulp/gene). In production the
// Evolution engine receives a browser request, matches the path to a gene
// route, and proxies it as a msgpack-encoded gene.HTTPRequest through a sibling
// call to the gene's pulp_on_call export (function "gene.handle_route"). There
// is no net listener in front of the gene, so StartCellHTTP (which keys on a
// real port + transport.http.inbound) does not apply.
//
// This harness drives the cell exactly the way the engine does: load + Init the
// wasm cell, then cell.Call(ctx, "gene.handle_route", <msgpack HTTPRequest>)
// and decode the msgpack HTTPResponse. That is the real wire, so the owner gate
// is exercised end-to-end.
//
// NO Fiber/pulp/gene IMPORT
// -------------------------
// We must NOT import Fiber/pulp/gene here: it transitively imports Fiber/pulp
// (core), whose //go:wasmimport functions have no body and only compile under
// GOOS=wasip1 — importing it would break this native host test build. The gene
// wire contract is just two msgpack structs + a string const, mirrored below.
//
// CAPABILITIES
// ------------
// Sessions-Gene declares storage.sqlite, spawn.docker, payment.stripe,
// workers, transport.http.outbound, entropy.read. ext-http/-sqlite/-entropy are
// already imported by cellharness_test.go (they supply transport.http.outbound,
// storage.sqlite, entropy.read). The three external-backend caps are wired to
// the in-memory stubs already defined for the Evolution harness
// (cellharness_evostubs_test.go: stripeStubCapability / dockerStubCapability /
// workersStubCapability), which export exactly the host-import symbols this
// cell's wasm references. The owner gate runs BEFORE any stripe/docker/workers/
// outbound call on every path under test, so no live backend is ever reached —
// the stubs only need to exist so wazero can instantiate the module.
//
// SEEDING
// -------
// ext-sqlite backs the cell's DB at <storageRoot>/<cellName>/data.db (a real
// file opened eagerly at Register). The harness knows storageRoot + the cell
// name, opens that same file with modernc.org/sqlite, creates the schema the
// gene's bun queries expect (the cell's bootstrap only OPENS the DB; Evolution
// runs migrations in prod), and seeds rows with plain SQL. Single connection +
// WAL + busy_timeout match ext-sqlite, and seeding happens before any Call
// (serially), so there is no contention.

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/vmihailenco/msgpack/v5"
	_ "modernc.org/sqlite"
)

// ---- gene wire types (mirror of Fiber/pulp/gene; see header) --------------

const fnHandleRoute = "gene.handle_route"

type geneHTTPRequest struct {
	Method  string            `msgpack:"method"`
	Path    string            `msgpack:"path"`
	Params  map[string]string `msgpack:"params,omitempty"`
	Query   map[string]string `msgpack:"query,omitempty"`
	Headers map[string]string `msgpack:"headers,omitempty"`
	Body    []byte            `msgpack:"body,omitempty"`
}

type geneHTTPResponse struct {
	Status  uint32            `msgpack:"status"`
	Headers map[string]string `msgpack:"headers,omitempty"`
	Cookies []string          `msgpack:"cookies,omitempty"`
	Body    []byte            `msgpack:"body,omitempty"`
}

// ---- harness --------------------------------------------------------------

const sessionsGeneCellName = "sessions"

func sessionsGeneSourceDir() string {
	// Pulp/internal/host -> ../../../Sessions-Gene/pulp-cell
	return filepath.Join("..", "..", "..", "Sessions-Gene", "pulp-cell")
}

type geneHarness struct {
	t      *testing.T
	cell   *Cell
	db     *sql.DB
	cancel context.CancelFunc
	caps   []ext.Capability
}

func startSessionsGene(t *testing.T) *geneHarness {
	t.Helper()

	wasmPath := BuildCell(t, sessionsGeneSourceDir())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Resolve caps: ext.All() (http/sqlite/entropy/...) + the stripe/docker/
	// workers stubs the Evolution harness already defines, by name.
	caps := map[string]ext.Capability{}
	for _, c := range ext.All() {
		caps[c.Name] = c
	}
	for _, c := range []ext.Capability{stripeStubCapability(), dockerStubCapability(), workersStubCapability()} {
		caps[c.Name] = c
	}

	declared := []string{
		"storage.sqlite", "spawn.docker", "payment.stripe",
		"workers", "transport.http.outbound", "entropy.read",
	}
	declaredSet := map[string]bool{}
	for _, n := range declared {
		declaredSet[n] = true
	}

	storageRoot := t.TempDir()
	for name, c := range caps {
		if declaredSet[name] && c.Setup != nil {
			if err := c.Setup(ext.SetupEnv{StorageRoot: storageRoot, Logger: logger}); err != nil {
				t.Fatalf("capability %q setup: %v", name, err)
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	spec := &manifest.CellSpec{
		SchemaVersion: manifest.CurrentSchemaVersion,
		Name:          sessionsGeneCellName,
		Version:       "0.0.0-test",
		Capabilities:  declared,
		Config: map[string]any{
			"voucher_lifetime_hours": 336,
			"currency":               "usd",
			// Empty evolution_url/internal_secret/resend: the owner gate runs
			// before any outbound kernel/email call, so they're never reached.
		},
		WASMPath: wasmPath,
	}

	registry := NewRegistry()
	for _, c := range caps {
		registry.Gated(c)
	}

	configBytes, err := manifest.EncodeConfig(spec.Config)
	if err != nil {
		cancel()
		t.Fatalf("encode config: %v", err)
	}

	cell, err := Load(ctx, spec, registry, nil, logger)
	if err != nil {
		cancel()
		t.Fatalf("load sessions-gene cell: %v", err)
	}
	if err := cell.Init(ctx, configBytes); err != nil {
		cell.Close(context.Background())
		cancel()
		t.Fatalf("init sessions-gene cell: %v", err)
	}

	// Open the SAME data.db ext-sqlite created for this cell to seed it.
	dbPath := filepath.Join(storageRoot, sessionsGeneCellName, "data.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		cell.Close(context.Background())
		cancel()
		t.Fatalf("open seed db: %v", err)
	}
	db.SetMaxOpenConns(1)
	for _, p := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(p); err != nil {
			t.Fatalf("seed db pragma %q: %v", p, err)
		}
	}
	if _, err := db.Exec(sessionsGeneSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	h := &geneHarness{t: t, cell: cell, db: db, cancel: cancel}
	for name, c := range caps {
		if declaredSet[name] {
			h.caps = append(h.caps, c)
		}
	}
	t.Cleanup(h.stop)
	return h
}

func (h *geneHarness) stop() {
	if h.db != nil {
		_ = h.db.Close()
	}
	if h.cell != nil {
		_ = h.cell.Shutdown(context.Background())
		_ = h.cell.Close(context.Background())
	}
	h.cancel()
	for _, c := range h.caps {
		if c.Teardown != nil {
			_ = c.Teardown(context.Background())
		}
	}
}

// handleRoute drives one gene HTTP request through the cell's pulp_on_call
// (gene.handle_route) — exactly how the Evolution engine proxies a gene route.
func (h *geneHarness) handleRoute(method, path string, params, query map[string]string, body any) geneHTTPResponse {
	h.t.Helper()
	var bodyBytes []byte
	switch b := body.(type) {
	case nil:
	case []byte:
		bodyBytes = b
	default:
		var err error
		bodyBytes, err = json.Marshal(b)
		if err != nil {
			h.t.Fatalf("marshal body: %v", err)
		}
	}
	req := geneHTTPRequest{Method: method, Path: path, Params: params, Query: query, Body: bodyBytes}
	args, err := msgpack.Marshal(req)
	if err != nil {
		h.t.Fatalf("marshal request: %v", err)
	}
	out, err := h.cell.Call(context.Background(), fnHandleRoute, args)
	if err != nil {
		h.t.Fatalf("cell.Call(%s %s): %v", method, path, err)
	}
	var resp geneHTTPResponse
	if err := msgpack.Unmarshal(out, &resp); err != nil {
		h.t.Fatalf("unmarshal response: %v", err)
	}
	return resp
}

// seedOrder inserts a minimal order row (the gate only needs id/email/status).
func (h *geneHarness) seedOrder(id, email, status string) {
	h.t.Helper()
	_, err := h.db.Exec(
		`INSERT INTO orders (id, stripe_session_id, server_type, email, status, auto_redeem, max_amount_cents, resolve_cache_json, ip_address, created_at)
		 VALUES (?, ?, 'minecraft-session', ?, ?, 1, 1400, '', '', ?)`,
		id, "ss_"+id, email, status, time.Now().UTC(),
	)
	if err != nil {
		h.t.Fatalf("seed order %s: %v", id, err)
	}
}

// ===========================================================================
// THE OWNER GATE (CRITICAL C2 / the load-bearing invariant the Evolution
// voucher-IDOR refutation depends on)
// ===========================================================================
//
// Every mutating gene handler loads the order by :id then runs
// verifyOrderEmail(body.Email, order.Email). A mismatch (or any empty side)
// MUST 403 before any state change. We pin both directions for each handler:
//
//   REJECT — body email != order email -> 403 "not authorized".
//   ACCEPT — body email == order email -> NOT 403 (gate passed). We seed a
//            status the handler then rejects 400, so the accept assertion never
//            needs a live Stripe/Docker/kernel backend.

type mutatingCase struct {
	name    string
	method  string
	pathFor func(id string) string
	body    func(email string) map[string]any
	// acceptSeedStatus: order.Status that PASSES the gate but trips the
	// handler's next guard (-> 400), so the accept path lands on a non-403
	// without reaching a backend.
	acceptSeedStatus string
}

func sessionsGeneMutatingCases() []mutatingCase {
	return []mutatingCase{
		{
			name:             "deploy",
			method:           "POST",
			pathFor:          func(id string) string { return "/api/session/" + id + "/deploy" },
			body:             func(e string) map[string]any { return map[string]any{"email": e, "server_type": "minecraft-session"} },
			acceptSeedStatus: "paid", // wants "purchased" -> 400 after gate
		},
		{
			name:    "reconfigure",
			method:  "POST",
			pathFor: func(id string) string { return "/api/session/" + id + "/reconfigure" },
			body:    func(e string) map[string]any { return map[string]any{"email": e} },
			// reconfigure wants paid|fulfilled; "purchased" -> 400 after gate.
			acceptSeedStatus: "purchased",
		},
		{
			name:    "schedule",
			method:  "POST",
			pathFor: func(id string) string { return "/api/voucher/" + id + "/schedule" },
			body: func(e string) map[string]any {
				// date is parsed BEFORE the gate -> must be valid + future.
				return map[string]any{"email": e, "date": time.Now().UTC().AddDate(0, 0, 7).Format("2006-01-02")}
			},
			acceptSeedStatus: "paid", // wants "purchased" -> 400 after gate
		},
		{
			name:             "unschedule",
			method:           "POST",
			pathFor:          func(id string) string { return "/api/voucher/" + id + "/unschedule" },
			body:             func(e string) map[string]any { return map[string]any{"email": e} },
			acceptSeedStatus: "paid", // wants "scheduled" -> 400 after gate
		},
		{
			name:             "swap",
			method:           "POST",
			pathFor:          func(id string) string { return "/api/voucher/" + id + "/swap" },
			body:             func(e string) map[string]any { return map[string]any{"email": e, "target_template": "minecraft-session"} },
			acceptSeedStatus: "paid", // wants "purchased"|"scheduled" -> 400 after gate
		},
		{
			name:             "config",
			method:           "POST",
			pathFor:          func(id string) string { return "/api/voucher/" + id + "/config" },
			body:             func(e string) map[string]any { return map[string]any{"email": e, "motd": "hello"} },
			acceptSeedStatus: "paid", // wants "purchased"|"scheduled" -> 400 after gate
		},
		{
			name:             "upgrade",
			method:           "POST",
			pathFor:          func(id string) string { return "/api/session/" + id + "/upgrade" },
			body:             func(e string) map[string]any { return map[string]any{"email": e, "new_tier": "session-plus"} },
			acceptSeedStatus: "paid", // wants "purchased"|"scheduled" -> 400 after gate
		},
	}
}

const (
	sgOwnerEmail    = "owner@example.com"
	sgImposterEmail = "attacker@evil.com"
)

func TestSessionsGene_OwnerGate_RejectsImposter(t *testing.T) {
	h := startSessionsGene(t)
	for _, c := range sessionsGeneMutatingCases() {
		t.Run(c.name, func(t *testing.T) {
			id := "ord-rej-" + c.name
			// Seed in a status the handler ACCEPTS past the gate, so a 403 can
			// ONLY originate from the gate (never a later status check).
			seedStatus := "purchased"
			if c.name == "reconfigure" {
				seedStatus = "fulfilled"
			}
			h.seedOrder(id, sgOwnerEmail, seedStatus)

			resp := h.handleRoute(c.method, c.pathFor(id), map[string]string{"id": id}, nil, c.body(sgImposterEmail))
			if resp.Status != 403 {
				t.Fatalf("%s imposter email: want 403 (owner gate), got %d (%s)", c.name, resp.Status, resp.Body)
			}

			resp = h.handleRoute(c.method, c.pathFor(id), map[string]string{"id": id}, nil, c.body(""))
			if resp.Status != 403 {
				t.Fatalf("%s empty email: want 403 (fail-closed), got %d (%s)", c.name, resp.Status, resp.Body)
			}
		})
	}
}

func TestSessionsGene_OwnerGate_AcceptsOwner(t *testing.T) {
	h := startSessionsGene(t)
	for _, c := range sessionsGeneMutatingCases() {
		t.Run(c.name, func(t *testing.T) {
			id := "ord-acc-" + c.name
			h.seedOrder(id, sgOwnerEmail, c.acceptSeedStatus)

			// Exact owner email -> gate passed -> NOT 403.
			resp := h.handleRoute(c.method, c.pathFor(id), map[string]string{"id": id}, nil, c.body(sgOwnerEmail))
			if resp.Status == 403 {
				t.Fatalf("%s owner email: gate REJECTED the real owner (403: %s)", c.name, resp.Body)
			}

			// Normalized owner email (case + whitespace) -> also NOT 403.
			// Proves the gate uses wallet.NormalizeEmail, not raw equality.
			id2 := "ord-acc2-" + c.name
			h.seedOrder(id2, sgOwnerEmail, c.acceptSeedStatus)
			noisy := "  OWNER@Example.COM  "
			resp = h.handleRoute(c.method, c.pathFor(id2), map[string]string{"id": id2}, nil, c.body(noisy))
			if resp.Status == 403 {
				t.Fatalf("%s normalized owner email %q: gate REJECTED a legit owner (403: %s)", c.name, noisy, resp.Body)
			}
		})
	}
}

// ===========================================================================
// READ-IDOR FIX (02e3888) — getSessions must not leak ShareToken/IP/ports to a
// caller who has not proven ownership via a live claim token.
// ===========================================================================

func (h *geneHarness) seedActiveServer(orderID, serverID, ip, shareTok string) {
	h.t.Helper()
	now := time.Now().UTC()
	_, err := h.db.Exec(
		`INSERT INTO servers (id, order_id, template, state, ip, port, ports_json, share_token, display_name, expires_at, created_at, cpu_weight, memory_weight, restart_count, total_paused_ms, operating)
		 VALUES (?, ?, 'minecraft-session', 'active', ?, 25565, '{"bedrock":19132}', ?, 'Realm', ?, ?, 0.33, 3, 0, 0, 0)`,
		serverID, orderID, ip, shareTok, now.Add(48*time.Hour), now,
	)
	if err != nil {
		h.t.Fatalf("seed server: %v", err)
	}
}

func TestSessionsGene_GetSessions_RedactsSecretsWithoutClaimToken(t *testing.T) {
	h := startSessionsGene(t)

	const email = "reader@example.com"
	const orderID = "ord-read-1"
	const shareTok = "SHARE-SECRET-XYZ"
	const serverIP = "203.0.113.7"

	h.seedOrder(orderID, email, "fulfilled")
	h.seedActiveServer(orderID, "srv-read-1", serverIP, shareTok)

	// --- Unproven: ?email= only, no token -> secrets stripped. ---
	resp := h.handleRoute("GET", "/api/sessions", nil, map[string]string{"email": email}, nil)
	if resp.Status != 200 {
		t.Fatalf("getSessions (unproven): want 200, got %d (%s)", resp.Status, resp.Body)
	}
	body := string(resp.Body)
	if strings.Contains(body, shareTok) {
		t.Fatalf("READ-IDOR REGRESSION: ShareToken leaked to non-owner: %s", body)
	}
	if strings.Contains(body, serverIP) {
		t.Fatalf("READ-IDOR REGRESSION: server IP leaked to non-owner: %s", body)
	}
	// The dashboard row must still render (order id present) — degrade to
	// redacted, not empty.
	if !strings.Contains(body, orderID) {
		t.Fatalf("getSessions (unproven): order row should still render, got %s", body)
	}

	// --- Proven: a live claim token bound to the same email -> secrets shown. ---
	now := time.Now().UTC()
	if _, err := h.db.Exec(
		`INSERT INTO claim_tokens (id, email, token, claimed, expires_at, created_at, ip_address)
		 VALUES ('ct-read-1', ?, 'live-claim-token-123', 0, ?, ?, '')`,
		email, now.Add(2*time.Hour), now,
	); err != nil {
		t.Fatalf("seed claim token: %v", err)
	}
	resp = h.handleRoute("GET", "/api/sessions", nil, map[string]string{"email": email, "token": "live-claim-token-123"}, nil)
	if resp.Status != 200 {
		t.Fatalf("getSessions (proven): want 200, got %d (%s)", resp.Status, resp.Body)
	}
	proven := string(resp.Body)
	if !strings.Contains(proven, shareTok) {
		t.Fatalf("getSessions (proven): owner with live claim token MUST see ShareToken, got %s", proven)
	}
	if !strings.Contains(proven, serverIP) {
		t.Fatalf("getSessions (proven): owner with live claim token MUST see server IP, got %s", proven)
	}
}

func TestSessionsGene_GetSessions_ExpiredClaimTokenStaysRedacted(t *testing.T) {
	h := startSessionsGene(t)

	const email = "reader2@example.com"
	const orderID = "ord-read-2"
	const shareTok = "SHARE-SECRET-EXPIRED"

	h.seedOrder(orderID, email, "fulfilled")
	h.seedActiveServer(orderID, "srv-read-2", "203.0.113.9", shareTok)

	now := time.Now().UTC()
	// Expired claim token: must NOT prove ownership (fail-closed on expiry).
	if _, err := h.db.Exec(
		`INSERT INTO claim_tokens (id, email, token, claimed, expires_at, created_at, ip_address)
		 VALUES ('ct-read-2', ?, 'expired-token-456', 0, ?, ?, '')`,
		email, now.Add(-1*time.Hour), now.Add(-3*time.Hour),
	); err != nil {
		t.Fatalf("seed expired claim token: %v", err)
	}
	resp := h.handleRoute("GET", "/api/sessions", nil, map[string]string{"email": email, "token": "expired-token-456"}, nil)
	if resp.Status != 200 {
		t.Fatalf("getSessions (expired token): want 200, got %d (%s)", resp.Status, resp.Body)
	}
	if strings.Contains(string(resp.Body), shareTok) {
		t.Fatalf("READ-IDOR REGRESSION: expired claim token leaked ShareToken: %s", resp.Body)
	}
}
