package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/vmihailenco/msgpack/v5"
)

type serverRestartV1Payload struct {
	Extension   string `msgpack:"extension"`
	ServerID    string `msgpack:"server_id"`
	NodeID      string `msgpack:"node_id"`
	ContainerID string `msgpack:"container_id"`
	Reason      string `msgpack:"reason"`
}

type serverRestartV1Stub struct {
	mu       sync.Mutex
	calls    map[string]int
	failNext bool
}

func newServerRestartV1Stub() *serverRestartV1Stub {
	return &serverRestartV1Stub{calls: map[string]int{}}
}

func (stub *serverRestartV1Stub) totalCalls() int {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	total := 0
	for _, count := range stub.calls {
		total += count
	}
	return total
}

func (stub *serverRestartV1Stub) failOnce() {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.failNext = true
}

func (stub *serverRestartV1Stub) capability() ext.Capability {
	bind := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		execute := func(ctx context.Context, module api.Module, requestPtr, requestLen, responsePtrOut, responseLenOut uint32) uint32 {
			if module == nil || module.Memory() == nil {
				return 2
			}
			request, ok := module.Memory().Read(requestPtr, requestLen)
			if !ok {
				return 2
			}
			envelope, payload, ok := decodeServerRestartV1Intent(request)
			if !ok {
				return 3
			}
			stub.mu.Lock()
			stub.calls[envelope.ID]++
			fail := stub.failNext
			stub.failNext = false
			stub.mu.Unlock()
			if fail {
				return 5
			}
			result, err := msgpack.Marshal(map[string]string{
				"server_id": payload.ServerID, "node_id": payload.NodeID,
				"container_id": payload.ContainerID, "operation": "restart",
				"status": "restarted", "completed_at": "2026-07-26T12:00:00Z",
			})
			if err != nil {
				return 6
			}
			return writeStubMsgpack(ctx, module, fleetRuntimeEffectReceipt{
				Version: envelope.Version, IntentID: envelope.ID, Kind: envelope.Kind,
				IdempotencyKey: envelope.IdempotencyKey, Status: "completed", Result: result,
			}, responsePtrOut, responseLenOut)
		}
		builder.NewFunctionBuilder().WithFunc(execute).Export("fleet_effect_execute")
		return nil
	}
	return ext.Capability{
		Name: fleetRuntimeEffectCapability, Provider: "server-restart-v1-test",
		Register: bind, Stub: bind,
	}
}

func decodeServerRestartV1Intent(request []byte) (fleetRuntimeEffectIntent, serverRestartV1Payload, bool) {
	var fields map[string]msgpack.RawMessage
	if err := msgpack.Unmarshal(request, &fields); err != nil || len(fields) != 5 {
		return fleetRuntimeEffectIntent{}, serverRestartV1Payload{}, false
	}
	for name := range fields {
		switch name {
		case "version", "id", "kind", "idempotency_key", "payload":
		default:
			return fleetRuntimeEffectIntent{}, serverRestartV1Payload{}, false
		}
	}
	var envelope fleetRuntimeEffectIntent
	if err := msgpack.Unmarshal(request, &envelope); err != nil ||
		envelope.Version != "pulp.effect.v1" ||
		envelope.Kind != fleetRuntimeEffectExtension ||
		envelope.ID == "" || envelope.IdempotencyKey == "" {
		return fleetRuntimeEffectIntent{}, serverRestartV1Payload{}, false
	}
	var payloadFields map[string]msgpack.RawMessage
	if err := msgpack.Unmarshal(envelope.Payload, &payloadFields); err != nil || len(payloadFields) != 5 {
		return fleetRuntimeEffectIntent{}, serverRestartV1Payload{}, false
	}
	for name := range payloadFields {
		switch name {
		case "extension", "server_id", "node_id", "container_id", "reason":
		default:
			return fleetRuntimeEffectIntent{}, serverRestartV1Payload{}, false
		}
	}
	var payload serverRestartV1Payload
	if err := msgpack.Unmarshal(envelope.Payload, &payload); err != nil ||
		payload.Extension != "restart" || payload.Reason != "customer-restart" ||
		payload.ServerID == "" || payload.NodeID == "" || payload.ContainerID == "" {
		return fleetRuntimeEffectIntent{}, serverRestartV1Payload{}, false
	}
	return envelope, payload, true
}

func assertServerRestartJSON(t *testing.T, h *CellHarness, path string, headers map[string]string, body any, wantStatus int, want any) {
	t.Helper()
	var requestBody []byte
	if body != nil {
		var err error
		requestBody, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	status, wire := h.Do("POST", path, headers, requestBody)
	if status != wantStatus {
		t.Fatalf("POST %s status = %d, want %d; body=%s", path, status, wantStatus, wire)
	}
	var got any
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("POST %s invalid JSON %q: %v", path, wire, err)
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
		t.Fatalf("POST %s body = %s, want %s", path, wire, wantWire)
	}
}

func serverRestartFleetState(t *testing.T, h *CellHarness, serverID string) (bool, string) {
	t.Helper()
	request, err := msgpack.Marshal(map[string]any{"id": serverID})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := h.cellsByName["fleet"].Call(ctx, "fleet.v1.query.server.get", request)
	if err != nil {
		t.Fatal(err)
	}
	var server struct {
		Operating bool   `msgpack:"operating"`
		Health    string `msgpack:"health"`
	}
	if err := msgpack.Unmarshal(response, &server); err != nil {
		t.Fatal(err)
	}
	return server.Operating, server.Health
}

func TestEvolutionServerRestartV1RealHTTPReplayRaceAndFullRestart(t *testing.T) {
	storageRoot := t.TempDir()
	restart := newServerRestartV1Stub()
	observation := newServerReadsV2ObservationStub()
	h := startEvolutionServerReadsV2Harness(t, storageRoot, observation, restart.capability())
	now := time.Now().UTC()
	upsertFleetServerValue(t, h, map[string]any{
		"id": "restart-live", "order_id": "order-restart-live",
		"template": "minecraft", "status": "active",
		"node_id": "game-node-a", "container_id": "minecraft-restart-live",
		"created_at": now.Add(-time.Hour).Format(time.RFC3339),
		"updated_at": now.Format(time.RFC3339),
	})
	upsertFleetServerValue(t, h, map[string]any{
		"id": "restart-stopped", "order_id": "order-restart-stopped",
		"template": "minecraft", "status": "queued",
		"created_at": now.Add(-time.Hour).Format(time.RFC3339),
		"updated_at": now.Format(time.RFC3339),
	})
	upsertFleetServerValue(t, h, map[string]any{
		"id": "restart-unassigned", "order_id": "order-restart-unassigned",
		"template": "minecraft", "status": "active",
		"created_at": now.Add(-time.Hour).Format(time.RFC3339),
		"updated_at": now.Format(time.RFC3339),
	})

	trusted := map[string]string{
		"X-Internal-Secret": serverReadsV2InternalSecret,
		"Idempotency-Key":   "restart-replay-1",
	}
	assertServerRestartJSON(t, h, "/api/servers/restart-missing/restart", trusted, nil, 404, map[string]any{"error": "server not found"})
	assertServerRestartJSON(t, h, "/api/servers/restart-stopped/restart", trusted, nil, 400, map[string]any{"error": "server is not running"})
	assertServerRestartJSON(t, h, "/api/servers/restart-unassigned/restart", trusted, nil, 400, map[string]any{"error": "no container assigned"})
	if calls := restart.totalCalls(); calls != 0 {
		t.Fatalf("failed preflight executed %d restart effects", calls)
	}
	assertServerRestartJSON(t, h, "/api/servers/restart-live/restart", nil, nil, 401, map[string]any{"error": "unauthorized"})
	assertServerRestartJSON(t, h, "/api/servers/restart-live/restart", trusted, map[string]any{
		"effect_id": "attacker", "runtime_endpoint": "https://attacker.invalid",
	}, 400, map[string]any{"error": "invalid request"})
	assertServerRestartJSON(t, h, "/api/servers/restart-live/restart?effect_id=attacker&lease_duration_millis=1", trusted, nil, 400, map[string]any{"error": "invalid request"})
	assertServerRestartJSON(t, h, "/api/servers/restart-live/restart", trusted, nil, 200, map[string]any{"status": "restarted"})
	assertServerRestartJSON(t, h, "/api/servers/restart-live/restart", trusted, nil, 200, map[string]any{"status": "restarted"})
	if calls := restart.totalCalls(); calls != 1 {
		t.Fatalf("same operation executed host %d times, want 1", calls)
	}

	const racers = 12
	raceHeaders := map[string]string{
		"X-Internal-Secret": serverReadsV2InternalSecret,
		"Idempotency-Key":   "restart-race-2",
	}
	var group sync.WaitGroup
	failures := make(chan string, racers)
	for index := 0; index < racers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			status, body := h.Do("POST", "/api/servers/restart-live/restart", raceHeaders, nil)
			if status != http.StatusOK || string(body) != `{"status":"restarted"}` {
				failures <- fmt.Sprintf("status=%d body=%s", status, body)
			}
		}()
	}
	group.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	if calls := restart.totalCalls(); calls != 2 {
		t.Fatalf("raced operation executed host %d total times, want 2", calls)
	}

	h.stop()
	replayed := startEvolutionServerReadsV2Harness(t, storageRoot, observation, restart.capability())
	assertServerRestartJSON(t, replayed, "/api/servers/restart-live/restart", raceHeaders, nil, 200, map[string]any{"status": "restarted"})
	if calls := restart.totalCalls(); calls != 2 {
		t.Fatalf("full Pulp restart replay executed host %d total times, want 2", calls)
	}

	distinct := map[string]string{
		"X-Internal-Secret": serverReadsV2InternalSecret,
		"Idempotency-Key":   "restart-distinct-3",
	}
	assertServerRestartJSON(t, replayed, "/api/servers/restart-live/restart", distinct, nil, 200, map[string]any{"status": "restarted"})
	if calls := restart.totalCalls(); calls != 3 {
		t.Fatalf("distinct restart executed host %d total times, want 3", calls)
	}
	if operating, _ := serverRestartFleetState(t, replayed, "restart-live"); operating {
		t.Fatal("settled successful restart left Fleet operating")
	}

	restart.failOnce()
	failed := map[string]string{
		"X-Internal-Secret": serverReadsV2InternalSecret,
		"Idempotency-Key":   "restart-failure-4",
	}
	assertServerRestartJSON(t, replayed, "/api/servers/restart-live/restart", failed, nil, http.StatusBadGateway, map[string]any{"error": "restart failed"})
	if operating, health := serverRestartFleetState(t, replayed, "restart-live"); operating || health != "degraded" {
		t.Fatalf("settled failed restart state = operating %t health %q", operating, health)
	}
}
