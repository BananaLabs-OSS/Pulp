package host

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

const reportingDispatchProvider = "orchestrator.dispatch"

type reportingDispatchRequest struct {
	Event   string `msgpack:"event"`
	Payload any    `msgpack:"payload,omitempty"`
}

type reportingDispatchResult struct {
	Value any `msgpack:"value,omitempty"`
}

type reportingHTTPProjection struct {
	Status  uint32            `msgpack:"status"`
	Headers map[string]string `msgpack:"headers"`
	Body    string            `msgpack:"body"`
}

func dispatchEvolutionReporting(
	t *testing.T,
	h *CellHarness,
	event string,
	payload map[string]any,
) reportingHTTPProjection {
	t.Helper()
	request, err := msgpack.Marshal(reportingDispatchRequest{Event: event, Payload: payload})
	if err != nil {
		t.Fatalf("encode %s: %v", event, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wire, err := h.cellsByName["lua-orchestrator"].Call(ctx, reportingDispatchProvider, request)
	if err != nil {
		t.Fatalf("dispatch %s: %v", event, err)
	}
	var result reportingDispatchResult
	if err := msgpack.Unmarshal(wire, &result); err != nil {
		t.Fatalf("decode %s dispatch result: %v", event, err)
	}
	projectionWire, err := msgpack.Marshal(result.Value)
	if err != nil {
		t.Fatalf("encode %s HTTP projection: %v", event, err)
	}
	var projection reportingHTTPProjection
	if err := msgpack.Unmarshal(projectionWire, &projection); err != nil {
		t.Fatalf("decode %s HTTP projection: %v", event, err)
	}
	return projection
}

func TestEvolution_ReportingAnalytics_ComposesThroughLuaOwners(t *testing.T) {
	h := startEvolution(t, "")
	warmEvolution(t, h)

	projection := dispatchEvolutionReporting(t, h,
		"evolution.sessions.commerce.admin.reporting.analytics.v1",
		map[string]any{
			"request_id": "reporting-analytics-e2e",
			"actor":      map[string]any{"id": "admin@example.test", "is_admin": true},
			"now_unix":   time.Now().UTC().Unix(),
		},
	)
	if projection.Status != 200 {
		t.Fatalf("analytics status = %d, body = %s", projection.Status, projection.Body)
	}
	if projection.Headers["Content-Type"] != "application/json" || projection.Body == "" {
		t.Fatalf("analytics projection = %#v", projection)
	}
}

func TestEvolution_ReportingExports_ComposeThroughLuaOwners(t *testing.T) {
	h := startEvolution(t, "")
	warmEvolution(t, h)

	for _, test := range []struct {
		event    string
		filename string
	}{
		{
			event:    "evolution.sessions.commerce.admin.reporting.orders-csv.v1",
			filename: "orders-2026-07-25.csv",
		},
		{
			event:    "evolution.sessions.commerce.admin.reporting.users-csv.v1",
			filename: "users-2026-07-25.csv",
		},
	} {
		projection := dispatchEvolutionReporting(t, h, test.event, map[string]any{
			"request_id":        "reporting-export-e2e:" + test.filename,
			"actor":             map[string]any{"id": "admin@example.test", "is_admin": true},
			"limit":             int64(10000),
			"download_filename": test.filename,
		})
		if projection.Status != 200 {
			t.Fatalf("%s status = %d, body = %s", test.event, projection.Status, projection.Body)
		}
		if !strings.Contains(projection.Headers["Content-Type"], "text/csv") ||
			!strings.Contains(projection.Headers["Content-Disposition"], test.filename) ||
			projection.Body == "" {
			t.Fatalf("%s projection = %#v", test.event, projection)
		}
	}
}

func TestEvolution_ReportingDispute_ComposesNotFoundThroughLuaOwners(t *testing.T) {
	h := startEvolution(t, "")
	warmEvolution(t, h)

	projection := dispatchEvolutionReporting(t, h,
		"evolution.sessions.commerce.admin.reporting.dispute-report.v1",
		map[string]any{
			"request_id":        "reporting-dispute-e2e",
			"actor":             map[string]any{"id": "admin@example.test", "is_admin": true},
			"generated_at_unix": time.Now().UTC().Unix(),
			"query":             map[string]any{"email": "missing@example.test"},
			"inline":            true,
			"download_filename": "dispute-report-missing-2026-07-25.json",
		},
	)
	if projection.Status != 404 {
		t.Fatalf("dispute status = %d, body = %s", projection.Status, projection.Body)
	}
	if projection.Headers["Content-Type"] != "application/json" || projection.Body == "" {
		t.Fatalf("dispute projection = %#v", projection)
	}
}

func TestEvolution_AdminReportingHTTP_UsesLuaComposition(t *testing.T) {
	const (
		secret = "reporting-admin-secret"
		email  = "${ADMIN_EMAILS}"
	)
	h := startEvolution(t, secret)
	warmEvolution(t, h)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("evolution-admin:" + email))
	headers := map[string]string{
		"Cookie": "admin_session=" + email + ":" + hex.EncodeToString(mac.Sum(nil)),
	}

	if status, body := h.Do(http.MethodGet, "/admin/api/analytics", headers, nil); status != http.StatusOK || len(body) == 0 {
		t.Fatalf("analytics HTTP status = %d, body = %s", status, body)
	}
	if status, body := h.Do(http.MethodGet, "/admin/api/export/orders.csv", headers, nil); status != http.StatusOK ||
		!strings.HasPrefix(string(body), "id,email,server_type,") {
		t.Fatalf("orders CSV HTTP status = %d, body = %s", status, body)
	}
	if status, body := h.Do(http.MethodGet, "/admin/api/export/users.csv", headers, nil); status != http.StatusOK ||
		!strings.HasPrefix(string(body), "email,total_orders,") {
		t.Fatalf("users CSV HTTP status = %d, body = %s", status, body)
	}
	if status, body := h.Do(http.MethodGet, "/admin/api/dispute-report?email=missing@example.test&inline=1", headers, nil); status != http.StatusNotFound {
		t.Fatalf("dispute HTTP status = %d, body = %s", status, body)
	}
}
