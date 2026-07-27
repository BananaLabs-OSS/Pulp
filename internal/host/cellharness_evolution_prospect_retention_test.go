package host

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

func TestEvolution_ProspectRetention_RunsThroughLuaCommerceOwner(t *testing.T) {
	h := startEvolution(t, "")
	warmEvolution(t, h)

	status, body := h.Do(http.MethodPost, "/api/prospect",
		map[string]string{"Content-Type": "application/json"}, []byte(`{}`))
	if status != http.StatusOK {
		t.Fatalf("create prospect status = %d, body = %s", status, body)
	}
	var created struct {
		ProspectID string `json:"prospect_id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ProspectID == "" {
		t.Fatalf("create prospect body = %s, err = %v", body, err)
	}

	request, err := msgpack.Marshal(reportingDispatchRequest{
		Event: "commerce.workflow.maintenance.prospect.retain.v1",
		Payload: map[string]any{"command": map[string]any{
			"idempotency_key":   "prospect-retention-e2e",
			"now_unix":          time.Now().UTC().Add(time.Hour).Unix(),
			"retention_seconds": int64(0),
			"limit":             int64(500),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.cellsByName["lua-orchestrator"].Call(ctx, reportingDispatchProvider, request); err != nil {
		t.Fatalf("dispatch prospect retention: %v", err)
	}

	status, body = h.Do(http.MethodGet, "/api/prospect/"+created.ProspectID+"/stream", nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("retained prospect status = %d, body = %s", status, body)
	}
}
