package host

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/vmihailenco/msgpack/v5"
)

const serverReadsV2InternalSecret = "server-reads-v2-internal-secret"

const serverReadsV2FleetObservationCapability = "effect.fleet.observation"

type serverReadsV2ObservationIntent struct {
	Contract       string `msgpack:"contract"`
	ServerID       string `msgpack:"server_id"`
	NodeID         string `msgpack:"node_id"`
	ContainerID    string `msgpack:"container_id"`
	Field          string `msgpack:"field"`
	Generation     string `msgpack:"generation"`
	SourceRevision string `msgpack:"source_revision"`
}

type serverReadsV2ObservationStub struct {
	mu         sync.Mutex
	calls      map[string]int
	failFields map[string]bool
}

func newServerReadsV2ObservationStub() *serverReadsV2ObservationStub {
	return &serverReadsV2ObservationStub{
		calls:      map[string]int{},
		failFields: map[string]bool{},
	}
}

func (stub *serverReadsV2ObservationStub) callCount(field string) int {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return stub.calls[field]
}

func (stub *serverReadsV2ObservationStub) totalCalls() int {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	total := 0
	for _, count := range stub.calls {
		total += count
	}
	return total
}

func (stub *serverReadsV2ObservationStub) setFailure(field string, fail bool) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.failFields[field] = fail
}

func (stub *serverReadsV2ObservationStub) capability() ext.Capability {
	bind := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		execute := func(ctx context.Context, module api.Module, requestPtr, requestLen, responsePtrOut, responseLenOut uint32) uint32 {
			if module == nil || module.Memory() == nil {
				return 2
			}
			request, ok := module.Memory().Read(requestPtr, requestLen)
			if !ok {
				return 2
			}
			envelope, intent, ok := decodeServerReadsV2ObservationRequest(request)
			if !ok {
				return 3
			}
			data, ok := serverReadsV2ObservationData(intent)
			if !ok {
				return 4
			}

			stub.mu.Lock()
			stub.calls[intent.Field]++
			fail := stub.failFields[intent.Field]
			stub.mu.Unlock()
			if fail {
				return 5
			}

			result, err := msgpack.Marshal(map[string]any{
				"contract":        intent.Contract,
				"server_id":       intent.ServerID,
				"node_id":         intent.NodeID,
				"container_id":    intent.ContainerID,
				"field":           intent.Field,
				"generation":      intent.Generation,
				"source_revision": intent.SourceRevision,
				"observed_at":     "2026-07-26T12:00:00Z",
				"data":            data,
			})
			if err != nil {
				return 6
			}
			receipt := fleetRuntimeEffectReceipt{
				Version:        envelope.Version,
				IntentID:       envelope.ID,
				Kind:           envelope.Kind,
				IdempotencyKey: envelope.IdempotencyKey,
				Status:         "completed",
				Result:         result,
			}
			return writeStubMsgpack(ctx, module, receipt, responsePtrOut, responseLenOut)
		}
		builder.NewFunctionBuilder().WithFunc(execute).Export("fleet_observation_execute")
		return nil
	}
	return ext.Capability{
		Name:     serverReadsV2FleetObservationCapability,
		Provider: "evolution-deployment",
		Register: bind,
		Stub:     bind,
	}
}

func decodeServerReadsV2ObservationRequest(request []byte) (fleetRuntimeEffectIntent, serverReadsV2ObservationIntent, bool) {
	var envelopeFields map[string]msgpack.RawMessage
	if err := msgpack.Unmarshal(request, &envelopeFields); err != nil || len(envelopeFields) != 5 {
		return fleetRuntimeEffectIntent{}, serverReadsV2ObservationIntent{}, false
	}
	for name := range envelopeFields {
		switch name {
		case "version", "id", "kind", "idempotency_key", "payload":
		default:
			return fleetRuntimeEffectIntent{}, serverReadsV2ObservationIntent{}, false
		}
	}
	var envelope fleetRuntimeEffectIntent
	if err := msgpack.Unmarshal(request, &envelope); err != nil ||
		envelope.Version != "pulp.effect.v1" ||
		envelope.Kind != "pulp.effect.fleet.runtime-observation.execute.v1" ||
		envelope.ID == "" || envelope.IdempotencyKey == "" {
		return fleetRuntimeEffectIntent{}, serverReadsV2ObservationIntent{}, false
	}

	var intentFields map[string]msgpack.RawMessage
	if err := msgpack.Unmarshal(envelope.Payload, &intentFields); err != nil || len(intentFields) != 7 {
		return fleetRuntimeEffectIntent{}, serverReadsV2ObservationIntent{}, false
	}
	for name := range intentFields {
		switch name {
		case "contract", "server_id", "node_id", "container_id", "field", "generation", "source_revision":
		default:
			return fleetRuntimeEffectIntent{}, serverReadsV2ObservationIntent{}, false
		}
	}
	var intent serverReadsV2ObservationIntent
	if err := msgpack.Unmarshal(envelope.Payload, &intent); err != nil ||
		intent.Contract != "fleet.live-observation.v1" ||
		intent.ServerID == "" || intent.NodeID == "" || intent.ContainerID == "" ||
		intent.Generation == "" || intent.SourceRevision != intent.Generation {
		return fleetRuntimeEffectIntent{}, serverReadsV2ObservationIntent{}, false
	}
	return envelope, intent, true
}

func serverReadsV2ObservationData(intent serverReadsV2ObservationIntent) (map[string]any, bool) {
	switch intent.Field {
	case "settings":
		return map[string]any{"settings": map[string]string{
			"gamemode": "survival", "difficulty": "hard", "pvp": "true",
			"motd": "Sessions", "rcon.password": "must-never-leak",
		}}, true
	case "gamerules":
		return map[string]any{"gamerules": map[string]string{
			"keepInventory": "true", "mobGriefing": "false",
			"doDaylightCycle": "true", "doWeatherCycle": "false",
			"doInsomnia": "true", "playersSleepingPercentage": "50",
			"showDeathMessages": "true", "announceAdvancements": "false",
		}}, true
	case "player_history":
		return map[string]any{"player_history": []map[string]any{{
			"uuid": "12345678-1234-1234-1234-123456789abc", "name": "Alex",
			"expires_on": "2026-08-26 12:00:00 +0000",
		}}}, true
	case "access_snapshot":
		return map[string]any{"access_snapshot": map[string]any{
			"server_id": intent.ServerID,
			"whitelist": []map[string]any{{
				"uuid": "12345678-1234-1234-1234-123456789abc", "name": "Steve",
			}},
			"operators": []map[string]any{{
				"uuid": "22345678-1234-1234-1234-123456789abc", "name": "Alex",
				"level": 4, "bypasses_player_limit": true,
			}},
			"bans": []map[string]any{{
				"uuid": "32345678-1234-1234-1234-123456789abc", "name": "Griefer",
				"created": "2026-07-26", "source": "Server", "expires": "forever",
				"reason": "griefing",
			}},
			"updated_at": "2026-07-26T12:00:00Z",
		}}, true
	case "artifacts":
		return map[string]any{"artifacts": []map[string]any{
			{"name": "pack.zip", "kind": "datapack"},
			{"name": "mod.jar", "kind": "mod"},
		}}, true
	default:
		return nil, false
	}
}

func TestServerReadsV2ObservationStubAcceptsOnlyExactABI(t *testing.T) {
	payload := map[string]any{
		"contract": "fleet.live-observation.v1", "server_id": "server-1",
		"node_id": "node-1", "container_id": "container-1", "field": "settings",
		"generation": "revision-1", "source_revision": "revision-1",
	}
	payloadWire, err := msgpack.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	envelope := fleetRuntimeEffectIntent{
		Version: "pulp.effect.v1", ID: "observation-1",
		Kind:           "pulp.effect.fleet.runtime-observation.execute.v1",
		IdempotencyKey: "observation:server-1:settings", Payload: payloadWire,
	}
	wire, err := msgpack.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, intent, ok := decodeServerReadsV2ObservationRequest(wire); !ok || intent.Field != "settings" {
		t.Fatalf("exact observation ABI rejected: %#v, %t", intent, ok)
	}

	payload["command"] = "attacker-controlled"
	payloadWire, err = msgpack.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	envelope.Payload = payloadWire
	wire, err = msgpack.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := decodeServerReadsV2ObservationRequest(wire); ok {
		t.Fatal("observation payload with arbitrary command was accepted")
	}

	delete(payload, "command")
	payload["field"] = "arbitrary"
	payloadWire, err = msgpack.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	envelope.Payload = payloadWire
	wire, err = msgpack.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	_, intent, ok := decodeServerReadsV2ObservationRequest(wire)
	if !ok {
		t.Fatal("well-shaped unknown field should reach the field allowlist")
	}
	if _, allowed := serverReadsV2ObservationData(intent); allowed {
		t.Fatal("observation request for an arbitrary field was accepted")
	}
}

// startEvolutionServerReadsV2Harness is the application harness with an
// explicit storage root. The production harness intentionally allocates a new
// temp root per call; this local form lets the test stop every Pulp cell and
// boot the full composition again against the same owner databases.
func startEvolutionServerReadsV2Harness(t *testing.T, storageRoot string, observation *serverReadsV2ObservationStub, additional ...ext.Capability) *CellHarness {
	t.Helper()

	workspace, err := filepath.Abs(filepath.Join(evolutionSourceDir(), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	application, err := manifest.LoadApp(filepath.Join(workspace, "Evolution", "pulp-cell", "pulp.app.toml"))
	if err != nil {
		t.Fatal(err)
	}
	sources := evolutionApplicationHarnessSources(workspace)
	runtimes := make(map[string]*composedHarnessRuntime, len(application.Cells.Order))
	for _, spec := range application.Cells.Order {
		source, ok := sources[spec.Name]
		if !ok {
			t.Fatalf("Evolution application contains unmapped cell %q", spec.Name)
		}
		spec.WASMPath = BuildCell(t, source)
		if spec.Name == "evolution" {
			if spec.Config == nil {
				spec.Config = map[string]any{}
			}
			for key, value := range map[string]any{
				"internal_secret":      serverReadsV2InternalSecret,
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
				"resend_api_key":       "re_stub_server_reads",
			} {
				spec.Config[key] = value
			}
			spec.Config["legacy_owner_imports_single_app"] = true
		}
		runtimes[spec.Name] = &composedHarnessRuntime{spec: spec}
	}

	port := freePort(t)
	t.Setenv("HTTP_PORT", fmt.Sprintf("%d", port))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	overrides := evoDowntimeOverrides()
	overrides = append(overrides, observation.capability())
	overrides = append(overrides, additional...)
	capabilities := evolutionApplicationHarnessCapabilities(overrides)
	httpCapability := capabilities["transport.http.inbound"]
	if httpCapability.Name == "" {
		t.Fatal("transport.http.inbound capability not registered")
	}
	declared := map[string]bool{}
	for _, runtime := range runtimes {
		for _, name := range runtime.spec.Capabilities {
			if _, ok := capabilities[name]; !ok {
				t.Fatalf("cell %q declares unavailable capability %q", runtime.spec.Name, name)
			}
			declared[name] = true
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	harness := &CellHarness{
		URL:         fmt.Sprintf("http://127.0.0.1:%d", port),
		client:      &http.Client{Timeout: 5 * time.Second},
		cellsByName: make(map[string]*Cell, len(runtimes)),
		cancel:      cancel,
		t:           t,
		httpCap:     httpCapability,
		StorageRoot: storageRoot,
	}
	t.Cleanup(harness.stop)
	for name, capability := range capabilities {
		if !declared[name] {
			continue
		}
		if capability.Teardown != nil {
			harness.teardownCaps = append(harness.teardownCaps, capability)
		}
		if capability.Setup != nil {
			if err := capability.Setup(ext.SetupEnv{StorageRoot: storageRoot, Logger: logger}); err != nil {
				t.Fatalf("capability %q setup: %v", name, err)
			}
		}
	}

	registry := NewRegistry()
	for name, capability := range capabilities {
		if name != "pulp.sibling" {
			registry.Gated(capability)
		}
	}
	registry.Always(composedHarnessSiblingCapability(runtimes))
	for _, spec := range application.Cells.Order {
		configBytes, err := manifest.EncodeConfig(spec.Config)
		if err != nil {
			t.Fatalf("encode %s config: %v", spec.Name, err)
		}
		cell, err := Load(ctx, spec, registry, nil, logger)
		if err != nil {
			t.Fatalf("load %s cell: %v", spec.Name, err)
		}
		runtimes[spec.Name].cell = cell
		harness.cellsByName[spec.Name] = cell
		harness.cells = append(harness.cells, cell)
		if err := cell.Init(ctx, configBytes); err != nil {
			t.Fatalf("init %s cell: %v", spec.Name, err)
		}
	}
	evolution := runtimes["evolution"].cell
	if evolution == nil {
		t.Fatal("Evolution application did not load its HTTP adapter")
	}
	harness.cell = evolution
	harness.pumpWG.Add(1)
	go harness.pump(ctx)
	warmEvolution(t, harness)
	return harness
}

func enqueueFleetServerReadFixture(t *testing.T, h *CellHarness, id, serverID string, position int, createdAt time.Time) {
	t.Helper()
	request, err := msgpack.Marshal(map[string]any{
		"id": id,
		"entry": map[string]any{
			"id": id, "server_id": serverID, "position": position,
			"created_at": createdAt.UTC().Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.cellsByName["fleet"].Call(ctx, "fleet.v1.command.queue.enqueue", request); err != nil {
		t.Fatalf("enqueue Fleet server-read fixture: %v", err)
	}
}

func assertServerReadJSON(t *testing.T, h *CellHarness, path string, headers map[string]string, wantStatus int, want any) {
	t.Helper()
	status, body := h.Do("GET", path, headers, nil)
	if status != wantStatus {
		t.Fatalf("GET %s status = %d, want %d; body=%s", path, status, wantStatus, body)
	}
	var got any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("GET %s invalid JSON %q: %v", path, body, err)
	}
	wantWire, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var normalizedWant any
	if err := json.Unmarshal(wantWire, &normalizedWant); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, normalizedWant) {
		t.Fatalf("GET %s body = %s, want %s", path, body, wantWire)
	}
}

func TestEvolutionServerReadsV2RealHTTPParityAndHostMintedAuthorization(t *testing.T) {
	storageRoot := t.TempDir()
	observation := newServerReadsV2ObservationStub()
	h := startEvolutionServerReadsV2Harness(t, storageRoot, observation)
	now := time.Now().UTC().Truncate(time.Second)
	estimate := now.Add(2 * time.Hour)

	upsertFleetServerValue(t, h, map[string]any{
		"id": "read-expiring", "order_id": "order-read-expiring",
		"template": "minecraft", "status": "expiring",
		"expires_at": now.Add(time.Hour).Format(time.RFC3339),
		"created_at": now.Add(-time.Hour).Format(time.RFC3339),
		"updated_at": now.Format(time.RFC3339),
	})
	upsertFleetServerValue(t, h, map[string]any{
		"id": "read-estimate-slot", "order_id": "order-read-estimate-slot",
		"template": "minecraft", "status": "active",
		"expires_at": estimate.Format(time.RFC3339),
		"created_at": now.Add(-time.Hour).Format(time.RFC3339),
		"updated_at": now.Format(time.RFC3339),
	})
	upsertFleetServerValue(t, h, map[string]any{
		"id": "read-queued", "order_id": "order-read-queued",
		"template": "minecraft", "status": "queued",
		"created_at": now.Add(-time.Hour).Format(time.RFC3339),
		"updated_at": now.Format(time.RFC3339),
	})
	upsertFleetServerValue(t, h, map[string]any{
		"id": "read-alert", "order_id": "order-read-alert",
		"template": "minecraft", "status": "active", "health": "crash_loop",
		"crash_count": 3, "last_health_at": now.Format(time.RFC3339),
		"created_at": now.Add(-time.Hour).Format(time.RFC3339),
		"updated_at": now.Format(time.RFC3339),
	})
	upsertFleetServerValue(t, h, map[string]any{
		"id": "read-live", "order_id": "order-read-live",
		"template": "minecraft", "status": "active",
		"node_id": "game-node-a", "container_id": "minecraft-read-live",
		"settings":   map[string]string{"auto_restart": "daily"},
		"created_at": now.Add(-time.Hour).Format(time.RFC3339),
		"updated_at": now.Format(time.RFC3339),
	})
	upsertFleetServerValue(t, h, map[string]any{
		"id": "read-no-container", "order_id": "order-read-no-container",
		"template": "minecraft", "status": "active",
		"created_at": now.Add(-time.Hour).Format(time.RFC3339),
		"updated_at": now.Format(time.RFC3339),
	})
	enqueueFleetServerReadFixture(t, h, "read-queue-ticket", "read-queued", 1, now)

	trusted := map[string]string{"X-Internal-Secret": serverReadsV2InternalSecret}
	assertServerReadJSON(t, h, "/api/servers/read-expiring/extend-info", trusted, 200, map[string]any{
		"extendable": true, "mode": "queued", "cost": "1 session",
		"estimated_deploy": estimate.Format(time.RFC3339),
	})
	assertServerReadJSON(t, h, "/api/servers/read-alert/alerts", trusted, 200, []any{map[string]any{
		"type": "crash", "message": "Your server experienced an issue and was automatically restarted.",
		"count": 3, "time": now.Format(time.RFC3339),
	}})
	assertServerReadJSON(t, h, "/api/servers/read-expiring/alerts", trusted, 200, []any{})
	assertServerReadJSON(t, h, "/api/servers/read-alert/status", trusted, 200, map[string]any{
		"online": 0, "max": 5, "players": []any{},
	})
	assertServerReadJSON(t, h, "/api/servers/read-queued/status", trusted, 400, map[string]any{
		"error": "server is not running",
	})

	// Readiness is owner-preflighted before the privileged observation. Missing,
	// non-running, and unassigned servers must not touch the host capability.
	assertServerReadJSON(t, h, "/api/servers/read-missing/settings", trusted, 404, map[string]any{
		"error": "server not found",
	})
	assertServerReadJSON(t, h, "/api/servers/read-queued/settings", trusted, 400, map[string]any{
		"error": "server is not running",
	})
	assertServerReadJSON(t, h, "/api/servers/read-no-container/settings", trusted, 400, map[string]any{
		"error": "no container assigned",
	})
	if calls := observation.totalCalls(); calls != 0 {
		t.Fatalf("readiness preflight executed %d Fleet observation effects, want 0", calls)
	}

	assertServerReadJSON(t, h, "/api/servers/read-live/settings", trusted, 200, map[string]any{
		"gamemode": "survival", "difficulty": "hard", "pvp": "true",
		"motd": "Sessions", "auto_restart": "daily",
	})
	assertServerReadJSON(t, h, "/api/servers/read-live/gamerules", trusted, 200, map[string]any{
		"keep_inventory": "true", "mob_griefing": "false",
		"advance_time": "true", "advance_weather": "false",
		"spawn_phantoms": "true", "players_sleeping_percentage": "50",
		"show_death_messages": "true", "show_advancement_messages": "false",
	})
	assertServerReadJSON(t, h, "/api/servers/read-live/players", trusted, 200, []any{map[string]any{
		"uuid": "12345678-1234-1234-1234-123456789abc", "name": "Alex",
		"expiresOn": "2026-08-26 12:00:00 +0000",
	}})
	assertServerReadJSON(t, h, "/api/servers/read-live/whitelist", trusted, 200, []any{map[string]any{
		"uuid": "12345678-1234-1234-1234-123456789abc", "name": "Steve",
	}})
	assertServerReadJSON(t, h, "/api/servers/read-live/ops", trusted, 200, []any{map[string]any{
		"uuid": "22345678-1234-1234-1234-123456789abc", "name": "Alex",
		"level": 4, "bypassesPlayerLimit": true,
	}})
	assertServerReadJSON(t, h, "/api/servers/read-live/bans", trusted, 200, []any{map[string]any{
		"uuid": "32345678-1234-1234-1234-123456789abc", "name": "Griefer",
		"created": "2026-07-26", "source": "Server", "expires": "forever",
		"reason": "griefing",
	}})
	assertServerReadJSON(t, h, "/api/servers/read-live/datapacks", trusted, 200, map[string]any{
		"datapacks": []any{"pack.zip"},
	})
	for field, want := range map[string]int{
		"settings": 1, "gamerules": 1, "player_history": 1,
		"access_snapshot": 3, "artifacts": 1,
	} {
		if calls := observation.callCount(field); calls != want {
			t.Fatalf("Fleet observation %s calls = %d, want %d", field, calls, want)
		}
	}

	// A failed typed host effect is mapped to the audited legacy surface. There
	// is no owner, legacy database, file, Docker, or RCON fallback.
	observation.setFailure("settings", true)
	assertServerReadJSON(t, h, "/api/servers/read-live/settings", trusted, 502, map[string]any{
		"error": "failed to read settings",
	})
	observation.setFailure("settings", false)
	observation.setFailure("access_snapshot", true)
	assertServerReadJSON(t, h, "/api/servers/read-live/whitelist", trusted, 200, []any{})
	observation.setFailure("access_snapshot", false)
	observation.setFailure("artifacts", true)
	assertServerReadJSON(t, h, "/api/servers/read-live/datapacks", trusted, 200, map[string]any{
		"datapacks": []any{},
	})
	observation.setFailure("artifacts", false)

	// Supplying valid internal ingress auth is the only caller authority.
	// Same-named projection fields in headers/query are ignored; Evolution
	// constructs the exact internal-service envelope after middleware accepts.
	spoofed := map[string]string{
		"X-Internal-Secret":          serverReadsV2InternalSecret,
		"X-Pulp-Authorization-Mode":  "subject_ownership",
		"X-Pulp-Auth-Boundary":       "attacker",
		"X-Pulp-Auth-Audience":       "attacker",
		"X-Pulp-Auth-Service-ID":     "attacker",
		"X-Pulp-Auth-Verified-At":    "1970-01-01T00:00:00Z",
		"X-Pulp-Auth-Owned-Order-ID": "order-attacker",
	}
	assertServerReadJSON(t, h, "/api/servers/read-alert/status?authorization.mode=subject_ownership&authorization.internal_service.service_id=attacker", spoofed, 200, map[string]any{
		"online": 0, "max": 5, "players": []any{},
	})
	assertServerReadJSON(t, h, "/api/servers/read-live/settings?field=status&endpoint=http://attacker.invalid&effect_consumer_id=attacker&lease_duration_millis=1&observation_generation=attacker", spoofed, 200, map[string]any{
		"gamemode": "survival", "difficulty": "hard", "pvp": "true",
		"motd": "Sessions", "auto_restart": "daily",
	})
	assertServerReadJSON(t, h, "/api/servers/read-alert/status", nil, 401, map[string]any{"error": "unauthorized"})

	// Stop every cell and capability, then boot the full composition against
	// the same storage root. The owner projections—not Evolution's empty legacy
	// DB, a Docker file, RCON, or an in-memory fallback—must survive.
	h.stop()
	restarted := startEvolutionServerReadsV2Harness(t, storageRoot, observation)
	assertServerReadJSON(t, restarted, "/api/servers/read-expiring/extend-info", trusted, 200, map[string]any{
		"extendable": true, "mode": "queued", "cost": "1 session",
		"estimated_deploy": estimate.Format(time.RFC3339),
	})
	assertServerReadJSON(t, restarted, "/api/servers/read-alert/alerts", trusted, 200, []any{map[string]any{
		"type": "crash", "message": "Your server experienced an issue and was automatically restarted.",
		"count": 3, "time": now.Format(time.RFC3339),
	}})
	assertServerReadJSON(t, restarted, "/api/servers/read-alert/status", trusted, 200, map[string]any{
		"online": 0, "max": 5, "players": []any{},
	})
	assertServerReadJSON(t, restarted, "/api/servers/read-queued/status", trusted, 400, map[string]any{
		"error": "server is not running",
	})
	assertServerReadJSON(t, restarted, "/api/servers/read-live/settings", trusted, 200, map[string]any{
		"gamemode": "survival", "difficulty": "hard", "pvp": "true",
		"motd": "Sessions", "auto_restart": "daily",
	})
	assertServerReadJSON(t, restarted, "/api/servers/read-live/players", trusted, 200, []any{map[string]any{
		"uuid": "12345678-1234-1234-1234-123456789abc", "name": "Alex",
		"expiresOn": "2026-08-26 12:00:00 +0000",
	}})
	assertServerReadJSON(t, restarted, "/api/servers/read-live/datapacks", trusted, 200, map[string]any{
		"datapacks": []any{"pack.zip"},
	})
}
