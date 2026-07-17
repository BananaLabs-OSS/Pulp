package host

import (
	"strings"
	"testing"
	"time"
)

// A reserved order (fleet full: card on file, nothing charged) must appear on
// the customer's own /servers page. Before the fix, getSessions filtered
// status IN (paid, fulfilled, purchased, scheduled), so a held spot was
// invisible — the customer's reservation looked like it had vanished.
//
// Once enqueued, a reserved order carries a `queued` server row, so it should
// list as its SERVER state (queued), needing no new response status or frontend
// badge. This pins that.
func seedQueuedServer(h *geneHarness, orderID, serverID string) {
	h.t.Helper()
	now := time.Now().UTC()
	if _, err := h.db.Exec(
		`INSERT INTO servers (id, order_id, template, state, ip, port, ports_json, share_token, display_name, expires_at, created_at, cpu_weight, memory_weight, restart_count, total_paused_ms, operating)
		 VALUES (?, ?, 'minecraft-session', 'queued', '', 0, '', ?, 'Realm', ?, ?, 0.33, 3, 0, 0, 0)`,
		serverID, orderID, "tok-"+serverID, now.Add(48*time.Hour), now,
	); err != nil {
		h.t.Fatalf("seed queued server: %v", err)
	}
}

func TestSessionsGene_GetSessions_ShowsReservedHeldSpot(t *testing.T) {
	h := startSessionsGene(t)
	const email = "reserver@example.com"

	// A reserved order with its enqueued queued server, and a
	// payment_action_required one (declined, still holding position).
	h.seedOrder("ord-res-1", email, "reserved")
	seedQueuedServer(h, "ord-res-1", "srv-res-1")
	h.seedOrder("ord-par-1", email, "payment_action_required")
	seedQueuedServer(h, "ord-par-1", "srv-par-1")

	resp := h.handleRoute("GET", "/api/sessions", nil, map[string]string{"email": email}, nil)
	if resp.Status != 200 {
		t.Fatalf("getSessions: want 200, got %d (%s)", resp.Status, resp.Body)
	}
	body := string(resp.Body)

	// Both held spots must appear (previously they were filtered out entirely).
	if !strings.Contains(body, "ord-res-1") {
		t.Errorf("a reserved held spot is missing from the owner's /servers list: %s", body)
	}
	if !strings.Contains(body, "ord-par-1") {
		t.Errorf("a payment_action_required held spot is missing from /servers: %s", body)
	}
	// They surface as their server state (queued), not a raw order status the
	// frontend can't render.
	if !strings.Contains(body, `"queued"`) {
		t.Errorf("held spot should list as its queued server state, got: %s", body)
	}
	if strings.Contains(body, `"reserved"`) || strings.Contains(body, `"payment_action_required"`) {
		t.Errorf("raw reserve status leaked to the response; the frontend renders server state, not order status: %s", body)
	}
}
