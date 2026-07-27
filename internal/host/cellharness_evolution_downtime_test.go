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
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	"github.com/vmihailenco/msgpack/v5"
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

// evoMinecraftProfileStub is the deterministic implementation of the narrow
// identity.minecraft-profile.resolve capability used by the composed Evolution
// harness. UUID resolution used to share transport.http.outbound, but it is now
// intentionally brokered by Pulp-ext-http so a guest cannot choose an arbitrary
// egress URL. Keep this separate from evoBananagineOutboundStub: production
// continues to use the origin-bound profile authority while the harness has no
// network dependency.
func evoMinecraftProfileStub() ext.Capability {
	bind := func(b wazero.HostModuleBuilder, _ ext.Cell) error {
		resolve := func(ctx context.Context, m api.Module, reqPtr, reqLen, respPtrOut, respLenOut uint32) uint32 {
			var request struct {
				PlayerName string `msgpack:"player_name"`
				Platform   string `msgpack:"platform"`
			}
			if !readStubMsgpack(m, reqPtr, reqLen, &request) {
				return 3
			}
			if request.Platform == "java" && request.PlayerName == "Nobody" {
				return 7 // Fiber profile.Resolve maps this to pulp.ErrNotFound.
			}
			var uuid string
			switch request.Platform {
			case "java":
				uuid = "069a79f4-44e9-4726-a5be-fca90e38aaf5"
			case "bedrock":
				uuid = "00000000-0000-0000-0009-000005ccdde3"
			default:
				return 3
			}
			return writeStubMsgpack(ctx, m, struct {
				UUID   string `msgpack:"uuid"`
				Name   string `msgpack:"name"`
				Source string `msgpack:"source"`
			}{UUID: uuid, Name: request.PlayerName, Source: request.Platform}, respPtrOut, respLenOut)
		}
		b.NewFunctionBuilder().WithFunc(resolve).Export("minecraft_profile_resolve")
		return nil
	}
	return ext.Capability{Name: "identity.minecraft-profile.resolve", Register: bind, Stub: bind}
}

// evoServerMutationEffectStub executes only a fenced whitelist-add claim for
// the composed harness. It is intentionally narrower than production's
// privileged extension: callers cannot supply an endpoint, credential, or
// arbitrary operation.
func evoServerMutationEffectStub() ext.Capability {
	type opaqueRef struct {
		Version string `msgpack:"version"`
		Value   string `msgpack:"value"`
	}
	type fence struct {
		Version string    `msgpack:"version"`
		Intent  opaqueRef `msgpack:"intent"`
		Lease   opaqueRef `msgpack:"lease"`
		Attempt uint32    `msgpack:"attempt"`
	}
	type intent struct {
		Payload []byte `msgpack:"payload"`
	}
	type lease struct {
		Intent intent `msgpack:"intent"`
		Fence  fence  `msgpack:"fence"`
	}
	type request struct {
		Version string `msgpack:"version"`
		Owner   string `msgpack:"owner"`
		Claim   []byte `msgpack:"claim"`
	}
	bind := func(b wazero.HostModuleBuilder, _ ext.Cell) error {
		execute := func(ctx context.Context, m api.Module, reqPtr, reqLen, respPtrOut, respLenOut uint32) uint32 {
			var input request
			if !readStubMsgpack(m, reqPtr, reqLen, &input) || input.Version != "server-mutation-host.v4" || input.Owner != "runtime-control" {
				return 3
			}
			var claimed lease
			if len(input.Claim) == 0 || msgpack.Unmarshal(input.Claim, &claimed) != nil {
				return 4
			}
			var envelope struct {
				Body struct {
					Operation string `msgpack:"operation"`
					Payload   struct {
						Action   string `msgpack:"action"`
						Name     string `msgpack:"name"`
						UUID     string `msgpack:"uuid"`
						Platform string `msgpack:"platform"`
					} `msgpack:"payload"`
				} `msgpack:"body"`
			}
			if len(claimed.Intent.Payload) == 0 || msgpack.Unmarshal(claimed.Intent.Payload, &envelope) != nil || envelope.Body.Operation != "minecraft.access.apply" || envelope.Body.Payload.Action != "whitelist.add" || envelope.Body.Payload.Name == "" || envelope.Body.Payload.UUID == "" || (envelope.Body.Payload.Platform != "java" && envelope.Body.Payload.Platform != "bedrock") {
				return 4
			}
			added := envelope.Body.Payload.Name
			if envelope.Body.Payload.Platform == "bedrock" {
				added = "." + added
			}
			output, err := msgpack.Marshal(map[string]any{"version": "server-mutation-whitelist-result.v1", "kind": "whitelist.add", "status": "executed", "added": added, "uuid": envelope.Body.Payload.UUID})
			if err != nil {
				return 5
			}
			digest := sha256.Sum256(output)
			completed := time.Now().UTC().Format(time.RFC3339Nano)
			genericReceipt, err := msgpack.Marshal(map[string]any{"version": "contracts.v1", "fence": claimed.Fence, "succeeded": true, "output_sha256": hex.EncodeToString(digest[:]), "completed_at": completed})
			if err != nil {
				return 5
			}
			operationReceipt, err := msgpack.Marshal(map[string]any{"version": "contracts.v1", "operation": "minecraft.access.apply", "fence": claimed.Fence, "output": output, "output_sha256": hex.EncodeToString(digest[:]), "completed_at": completed})
			if err != nil {
				return 5
			}
			return writeStubMsgpack(ctx, m, map[string]any{"version": "server-mutation-host.v4", "owner": input.Owner, "generic_receipt": genericReceipt, "operation_receipt": operationReceipt}, respPtrOut, respLenOut)
		}
		b.NewFunctionBuilder().WithFunc(execute).Export("server_mutation_execute_v4")
		return nil
	}
	return ext.Capability{Name: "effect.server-mutation.v4", Register: bind, Stub: bind}
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
// stripe/s3/docker/sibling stubs plus deterministic Bananagine, player identity,
// and worker capabilities. Each replaces its matching host capability by name.
func evoDowntimeOverrides() []ext.Capability {
	return []ext.Capability{
		stripeStubCapability(),
		capacityObservationStubCapability(),
		s3StubCapability(),
		dockerStubCapability(),
		siblingStubCapability(),
		crossApplicationHarnessStubCapability(),
		evoBananagineOutboundStub(),
		evoMinecraftProfileStub(),
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
	capacityCPU, capacityMemory := int64(0), int64(0)
	if strings.Contains(fmt.Sprint(cellCfg["bananagine_url"]), "budgetnode") {
		capacityCPU, capacityMemory = 14, 48
	}
	if v, ok := extra["cpu_budget"].(float64); ok && v >= 0 {
		capacityCPU = int64(v)
	}
	if v, ok := extra["memory_budget"].(float64); ok && v >= 0 {
		capacityMemory = int64(v)
	}
	setEvoCapacityObservationBudget(capacityCPU*1000, capacityMemory*(1024*1024*1024))
	t.Cleanup(func() { setEvoCapacityObservationBudget(0, 0) })

	h := StartEvolutionApplicationHTTP(t, CellHarnessConfig{
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
	// The composed checkout path reads capacity from Fleet, not Evolution's
	// retired node table. Seed one deterministic Fleet node for the harness;
	// capacity-specific tests can override its budgets through extra config.
	if fleet := h.cellsByName["fleet"]; fleet != nil {
		cpu := int64(8)
		memory := int64(16384)
		if strings.Contains(fmt.Sprint(cellCfg["bananagine_url"]), "budgetnode") {
			cpu, memory = 14, 48
		}
		if v, ok := extra["cpu_budget"].(float64); ok && v > 0 {
			cpu = int64(v)
		}
		if v, ok := extra["memory_budget"].(float64); ok && v > 0 {
			memory = int64(v)
		}
		args, err := msgpack.Marshal(map[string]any{
			"id":   "harness-fleet-node-upsert",
			"node": map[string]any{"id": "node-1", "name": "Harness Fleet Node", "cpu_capacity": cpu, "cpu_capacity_millis": cpu * 1000, "memory_capacity": memory, "status": "active"},
		})
		if err != nil {
			t.Fatalf("encode Fleet harness node: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = fleet.Call(ctx, "fleet.v1.command.node.upsert", args)
		cancel()
		if err != nil {
			t.Fatalf("seed Fleet harness node: %v", err)
		}
	}

	// Match ext-sqlite's path byte-for-byte (filepath.Join). SQLite keys the
	// WAL shared-memory region on the path string, so a "/" vs "\" mismatch on
	// Windows would put this connection on a SEPARATE WAL view from the cell's
	// (writes invisible in both directions) — the exact split we must avoid.
	dbPath := filepath.Join(h.StorageRoot, "evolution", "data.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open cell db: %v", err)
	}
	if strings.Contains(fmt.Sprint(cellCfg["bananagine_url"]), "budgetnode") {
		if _, err := db.Exec(`UPDATE nodes SET cpu_budget=14,memory_budget=48 WHERE id='node-1'`); err != nil {
			t.Fatalf("seed budget-node capacity: %v", err)
		}
		checkpoint(db)
	}
	db.SetMaxOpenConns(1)
	for _, p := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(p); err != nil {
			t.Fatalf("db pragma %q: %v", p, err)
		}
	}
	cpuBudget, cpuBudgetSet := extra["cpu_budget"].(float64)
	memoryBudget, memoryBudgetSet := extra["memory_budget"].(float64)
	if cpuBudgetSet || memoryBudgetSet {
		now := time.Now().UTC()
		if _, err := db.Exec(
			`INSERT INTO nodes (id, name, bananagine_url, region, cpu_budget, memory_budget, state, registered_at, reported_at, last_seen_at)
			 VALUES ('node-1','Harness capacity node','http://bananagine.invalid','test',?,?,'active',?,?,?)
			 ON CONFLICT(id) DO UPDATE SET
			   cpu_budget=excluded.cpu_budget,
			   memory_budget=excluded.memory_budget,
			   reported_at=excluded.reported_at,
			   last_seen_at=excluded.last_seen_at`,
			cpuBudget, memoryBudget, now, now, now,
		); err != nil {
			t.Fatalf("seed capacity node: %v", err)
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
		`INSERT INTO games (id, name, price_cents, duration, config_json, primary_template, visible)
		 VALUES ('minecraft','Minecraft',1400,'336h','{}','minecraft',1)`,
	); err != nil {
		t.Fatalf("seed game: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO game_version_state (game, state_json, image_tag, auto_deploy)
		 VALUES ('minecraft','{}','paper-server:1.21@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',1)`,
	); err != nil {
		t.Fatalf("seed game version: %v", err)
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

func ownerDowntimeFixture(
	t *testing.T, h *CellHarness, key, email string, now time.Time, age time.Duration,
) (string, time.Time) {
	t.Helper()
	orderID, order := ownerCheckoutPending(t, h, key, email, nil)
	settleOwnerCheckout(t, h, "evt-"+key, order)
	serverID := orderID + "-server"
	expires := now.Add(30 * 24 * time.Hour).UTC()
	upsertFleetServerValue(t, h, map[string]any{
		"id": serverID, "order_id": orderID, "template": "minecraft",
		"display_name": "Realm " + key, "status": "active", "health": "unhealthy",
		"last_health_at": now.Add(-age).UTC().Format(time.RFC3339Nano),
		"expires_at":     expires.Format(time.RFC3339Nano),
	})
	return serverID, expires
}

func dispatchDowntimePolicy(t *testing.T, h *CellHarness, requestID string, now time.Time) {
	t.Helper()
	dispatchFleetMaintenanceRequest(t, h, map[string]any{
		"request_id":                      requestID,
		"now":                             now.UTC().Format(time.RFC3339Nano),
		"limit":                           uint32(100),
		"max_downtime_credit_seconds":     int64(7 * 24 * 60 * 60),
		"min_downtime_credit_seconds":     int64(60 * 60),
		"downtime_credit_quantum_seconds": int64(60 * 60),
	})
}

type notificationEmailValue struct {
	Recipients []string `msgpack:"recipients"`
	Subject    string   `msgpack:"subject"`
	HTML       string   `msgpack:"html"`
}

func claimedNotificationEmail(t *testing.T, h *CellHarness, consumer string) notificationEmailValue {
	t.Helper()
	intents := claimNotifications(t, h, consumer)
	if len(intents) != 1 {
		t.Fatalf("notification intents = %d, want 1: %#v", len(intents), intents)
	}
	if intents[0].Kind != "pulp.effect.notification.email.send.v1" {
		t.Fatalf("notification kind = %q", intents[0].Kind)
	}
	var email notificationEmailValue
	if err := msgpack.Unmarshal(intents[0].Payload, &email); err != nil {
		t.Fatalf("decode notification email: %v", err)
	}
	return email
}

// ===========================================================================
// THE PROOF
// ===========================================================================

func TestEvolution_DowntimeCompensation_CreditsRoundedUpHourOnce(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	const email = "player2h@example.com"
	now := time.Now().UTC()
	serverID, originalExpiry := ownerDowntimeFixture(t, h, "downtime-rounded", email, now, 90*time.Minute)
	dispatchDowntimePolicy(t, h, "maintenance-downtime-rounded", now)

	server := fleetServerForID(t, h, serverID)
	creditedExpiry, err := time.Parse(time.RFC3339Nano, server.ExpiresAt)
	if err != nil {
		t.Fatalf("parse Fleet credited expiry: %v", err)
	}
	if added := creditedExpiry.Sub(originalExpiry); added != 2*time.Hour ||
		server.DowntimeCreditSeconds != int64((2*time.Hour)/time.Second) {
		t.Fatalf("expected exact 2h Fleet credit, got expiry delta=%s server=%#v", added, server)
	}

	notification := claimedNotificationEmail(t, h, "downtime-rounded-consumer")
	if len(notification.Recipients) != 1 || notification.Recipients[0] != email {
		t.Fatalf("downtime notification recipients = %#v", notification.Recipients)
	}
	if !strings.Contains(notification.Subject, "Realm downtime-rounded") ||
		!strings.Contains(notification.HTML, "2 hours") {
		t.Fatalf("downtime notification lost owner facts: %#v", notification)
	}

	dispatchDowntimePolicy(t, h, "maintenance-downtime-rounded-retry", now)
	replayed := fleetServerForID(t, h, serverID)
	if replayed.ExpiresAt != server.ExpiresAt ||
		replayed.DowntimeCreditSeconds != server.DowntimeCreditSeconds {
		t.Fatalf("downtime credit replay changed Fleet state: first=%#v replay=%#v", server, replayed)
	}
	if intents := claimNotifications(t, h, "downtime-rounded-replay-consumer"); len(intents) != 0 {
		t.Fatalf("downtime replay enqueued another notification: %#v", intents)
	}
}

func TestEvolution_DowntimeCompensation_BelowHourFloorNoCredit(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	const email = "player40m@example.com"
	now := time.Now().UTC()
	serverID, originalExpiry := ownerDowntimeFixture(t, h, "downtime-floor", email, now, 40*time.Minute)
	dispatchDowntimePolicy(t, h, "maintenance-downtime-floor", now)
	server := fleetServerForID(t, h, serverID)
	if server.ExpiresAt != originalExpiry.Format(time.RFC3339Nano) ||
		server.DowntimeCreditSeconds != 0 {
		t.Fatalf("below-floor outage changed Fleet credit: %#v", server)
	}
	if intents := claimNotifications(t, h, "downtime-floor-consumer"); len(intents) != 0 {
		t.Fatalf("below-floor outage enqueued notification: %#v", intents)
	}
}

func TestEvolution_DowntimeCompensation_DayWording(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	const email = "playerday@example.com"
	now := time.Now().UTC()
	serverID, originalExpiry := ownerDowntimeFixture(t, h, "downtime-day", email, now, 24*time.Hour+30*time.Minute)
	dispatchDowntimePolicy(t, h, "maintenance-downtime-day", now)
	server := fleetServerForID(t, h, serverID)
	creditedExpiry, err := time.Parse(time.RFC3339Nano, server.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if added := creditedExpiry.Sub(originalExpiry); added != 25*time.Hour {
		t.Fatalf("expected exact 25h Fleet credit, got %s", added)
	}
	notification := claimedNotificationEmail(t, h, "downtime-day-consumer")
	if !strings.Contains(notification.HTML, "1 day") ||
		strings.Contains(notification.HTML, "24 hours") ||
		strings.Contains(notification.HTML, "25 hours") {
		t.Fatalf("day-scale notification wording = %#v", notification)
	}
}

// TestEvolution_DowntimeHarness_Smoke proves the composed harness reaches
// Commerce and Fleet owner state through the real application route.
/*
Legacy harness history:
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
*/
func TestEvolution_DowntimeHarness_Smoke(t *testing.T) {
	h, db := startEvolutionDowntime(t)

	// (2) The cell booted and seeded baseline config on ITS connection; the test
	// connection observes it — cross-connection reads work in both directions.
	seedDowntimeCatalog(t, db)
	orderID, order := ownerCheckoutPending(t, h, "downtime-owner-smoke", "smoke@example.test", nil)
	settleOwnerCheckout(t, h, "evt-downtime-owner-smoke", order)
	settled := commerceOrderForID(t, h, orderID)
	if settled.Status != "paid" || settled.PaymentStatus != "succeeded" {
		t.Fatalf("Commerce smoke order did not settle: %#v", settled)
	}
	server := fleetServerForID(t, h, orderID+"-server")
	if server.OrderID != orderID || server.NodeID == "" ||
		(server.Status != "provisioning" && server.Status != "ready") {
		t.Fatalf("owner smoke order did not reach Fleet: %#v", server)
	}

	// (3) A paid order this connection commits (with the catalog reference rows
	// the enqueue path needs) is enqueued + provisioned by the poller ONCE ticks
	// are driven — refuting the old "the cell never observes external commits"
	// conclusion. This is the corrective infra guard.
}
