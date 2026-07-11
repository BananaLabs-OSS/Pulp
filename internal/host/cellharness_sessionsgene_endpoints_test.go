package host

// LAYER C (continued) — the remaining gene-owned endpoint tests reached by
// LOADING THE REAL sessions-gene cell and calling gene.handle_route, the same
// wire the Evolution engine proxies on. Ports the schedule/swap/upgrade/
// availability business-logic cases from
// Evolution/internal/router/{voucher_flow_test.go,handlers_integration_test.go}
// that the coverage map lists as gene-owned. The owner gate itself is pinned by
// TestSessionsGene_OwnerGate_* (sibling file); these land on the handler logic
// past the gate, so every body carries the OWNER email.
//
// CELL-SEMANTICS NOTE: the native voucherAvailability router 404s a paid order
// (its WHERE clause filters to purchased/scheduled). The REAL gene handler finds
// the order first, then rejects a non-voucher status with 400 ("order is not a
// voucher"). Same invariant (a paid order is not reachable as a voucher), the
// status differs — asserted as the cell's real 400 below.

import (
	"testing"
	"time"
)

// seedGeneGameVisibility inserts an enabled game_visibility row (pointing at
// tierID for the pricing chain) so the gene's schedule/availability/swap
// template checks resolve.
func (h *geneHarness) seedGeneGameVisibility(template, tierID string) {
	h.t.Helper()
	if _, err := h.db.Exec(
		`INSERT INTO game_visibility (template, game_id, label, enabled, tier_id) VALUES (?, ?, ?, 1, ?)`,
		template, template, template, tierID,
	); err != nil {
		h.t.Fatalf("seed game_visibility %s: %v", template, err)
	}
}

// seedGeneTier inserts an enabled tier so upgrade/pricing lookups resolve.
func (h *geneHarness) seedGeneTier(id, name string, priceCents int, sortOrder int) {
	h.t.Helper()
	if _, err := h.db.Exec(
		`INSERT INTO tiers (id, name, label, price_cents, duration, max_cpu, max_ram_mb, enabled, sort_order, created_at)
		 VALUES (?, ?, ?, ?, '336h', 2.0, 4096, 1, ?, ?)`,
		id, name, name, priceCents, sortOrder, time.Now().UTC(),
	); err != nil {
		h.t.Fatalf("seed tier %s: %v", id, err)
	}
}

// --- schedule ---

// TestSessionsGene_VoucherSchedule_Success ports TestVoucherSchedule_Success:
// a purchased voucher scheduled to a valid future date with a valid template ->
// 200, order flipped to scheduled with scheduled_at set.
func TestSessionsGene_VoucherSchedule_Success(t *testing.T) {
	h := startSessionsGene(t)
	h.seedGeneGameVisibility("minecraft", "standard")
	const id = "vs-ok"
	h.seedOrder(id, sgOwnerEmail, "purchased")

	resp := h.handleRoute("POST", "/api/voucher/"+id+"/schedule", map[string]string{"id": id}, nil,
		map[string]any{"email": sgOwnerEmail, "date": futureDate(), "action": "new", "server_type": "minecraft"})
	if resp.Status != 200 {
		t.Fatalf("schedule success: want 200, got %d (%s)", resp.Status, resp.Body)
	}

	var status, scheduledAt string
	if err := h.db.QueryRow(`SELECT status, scheduled_at FROM orders WHERE id = ?`, id).Scan(&status, &scheduledAt); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if status != "scheduled" {
		t.Fatalf("expected status=scheduled, got %q", status)
	}
	if scheduledAt == "" {
		t.Fatalf("expected scheduled_at set, got empty")
	}
}

// TestSessionsGene_VoucherSchedule_NotFound ports TestVoucherSchedule_NotFound:
// a valid future date but a nonexistent voucher -> 404.
func TestSessionsGene_VoucherSchedule_NotFound(t *testing.T) {
	h := startSessionsGene(t)
	resp := h.handleRoute("POST", "/api/voucher/nope/schedule", map[string]string{"id": "nope"}, nil,
		map[string]any{"email": sgOwnerEmail, "date": futureDate(), "action": "new", "server_type": "minecraft"})
	if resp.Status != 404 {
		t.Fatalf("schedule not-found: want 404, got %d (%s)", resp.Status, resp.Body)
	}
}

// TestSessionsGene_VoucherSchedule_RejectsInvalidTemplate ports
// TestVoucherSchedule_RejectsInvalidTemplate: a purchased voucher scheduled to a
// template with no enabled game_visibility row -> 400.
func TestSessionsGene_VoucherSchedule_RejectsInvalidTemplate(t *testing.T) {
	h := startSessionsGene(t)
	const id = "vs-badtpl"
	h.seedOrder(id, sgOwnerEmail, "purchased")

	resp := h.handleRoute("POST", "/api/voucher/"+id+"/schedule", map[string]string{"id": id}, nil,
		map[string]any{"email": sgOwnerEmail, "date": futureDate(), "action": "new", "server_type": "fake-template"})
	if resp.Status != 400 {
		t.Fatalf("schedule invalid template: want 400, got %d (%s)", resp.Status, resp.Body)
	}
}

// --- unschedule ---

// TestSessionsGene_VoucherUnschedule_ExpiredRejected ports
// TestVoucherUnschedule_ExpiredRejected: a scheduled voucher whose
// voucher_expires_at is in the past cannot be unscheduled -> 400.
func TestSessionsGene_VoucherUnschedule_ExpiredRejected(t *testing.T) {
	h := startSessionsGene(t)
	const id = "vu-expired"
	h.seedOrder(id, sgOwnerEmail, "scheduled")
	past := time.Now().UTC().Add(-24 * time.Hour)
	if _, err := h.db.Exec(`UPDATE orders SET voucher_expires_at = ? WHERE id = ?`, past, id); err != nil {
		t.Fatalf("backdate voucher_expires_at: %v", err)
	}

	resp := h.handleRoute("POST", "/api/voucher/"+id+"/unschedule", map[string]string{"id": id}, nil,
		map[string]any{"email": sgOwnerEmail})
	if resp.Status != 400 {
		t.Fatalf("unschedule expired: want 400, got %d (%s)", resp.Status, resp.Body)
	}
}

// --- swap ---

// TestSessionsGene_VoucherSwap_FreeWhenNoUpcharge ports the free half of the
// voucher-swap logic: a target template whose price is <= the voucher's paid
// ceiling swaps inline (free:true) and updates server_type.
func TestSessionsGene_VoucherSwap_FreeWhenNoUpcharge(t *testing.T) {
	h := startSessionsGene(t)
	h.seedGeneGameVisibility("minecraft", "standard")
	h.seedGeneTier("standard", "session", 1400, 0)
	const id = "swap-free"
	h.seedOrder(id, sgOwnerEmail, "purchased") // seedOrder sets max_amount_cents = 1400

	resp := h.handleRoute("POST", "/api/voucher/"+id+"/swap", map[string]string{"id": id}, nil,
		map[string]any{"email": sgOwnerEmail, "target_template": "minecraft"})
	if resp.Status != 200 {
		t.Fatalf("swap free: want 200, got %d (%s)", resp.Status, resp.Body)
	}
	var serverType string
	if err := h.db.QueryRow(`SELECT server_type FROM orders WHERE id = ?`, id).Scan(&serverType); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if serverType != "minecraft" {
		t.Fatalf("expected server_type swapped to minecraft, got %q", serverType)
	}
}

// TestSessionsGene_VoucherSwap_PaymentRequiredOnUpcharge covers the paid half:
// a target template priced ABOVE the voucher's ceiling returns
// payment_required + the price diff, and does NOT change server_type.
func TestSessionsGene_VoucherSwap_PaymentRequiredOnUpcharge(t *testing.T) {
	h := startSessionsGene(t)
	h.seedGeneGameVisibility("minecraft-plus", "plus")
	h.seedGeneTier("plus", "session-plus", 9999, 1) // priced well above the 1400 ceiling
	const id = "swap-paid"
	h.seedOrder(id, sgOwnerEmail, "purchased")

	resp := h.handleRoute("POST", "/api/voucher/"+id+"/swap", map[string]string{"id": id}, nil,
		map[string]any{"email": sgOwnerEmail, "target_template": "minecraft-plus"})
	if resp.Status != 200 {
		t.Fatalf("swap upcharge: want 200, got %d (%s)", resp.Status, resp.Body)
	}
	var serverType string
	if err := h.db.QueryRow(`SELECT server_type FROM orders WHERE id = ?`, id).Scan(&serverType); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if serverType == "minecraft-plus" {
		t.Fatalf("upcharge swap must NOT change server_type before payment, got %q", serverType)
	}
}

// --- upgrade ---

// TestSessionsGene_UpgradeSession_InvalidTier covers upgradeSession's tier
// validation: an unknown target tier -> 400.
func TestSessionsGene_UpgradeSession_InvalidTier(t *testing.T) {
	h := startSessionsGene(t)
	const id = "up-badtier"
	h.seedOrder(id, sgOwnerEmail, "purchased")

	resp := h.handleRoute("POST", "/api/session/"+id+"/upgrade", map[string]string{"id": id}, nil,
		map[string]any{"email": sgOwnerEmail, "new_tier": "does-not-exist"})
	if resp.Status != 400 {
		t.Fatalf("upgrade invalid tier: want 400, got %d (%s)", resp.Status, resp.Body)
	}
}

// --- availability ---

// TestSessionsGene_VoucherAvailability_PurchasedVoucher ports
// TestVoucherAvailability_PurchasedVoucher: a purchased voucher -> 200.
func TestSessionsGene_VoucherAvailability_PurchasedVoucher(t *testing.T) {
	h := startSessionsGene(t)
	const id = "av-purchased"
	h.seedOrder(id, sgOwnerEmail, "purchased")

	resp := h.handleRoute("GET", "/api/voucher/"+id+"/availability", map[string]string{"id": id}, nil, nil)
	if resp.Status != 200 {
		t.Fatalf("availability purchased: want 200, got %d (%s)", resp.Status, resp.Body)
	}
}

// TestSessionsGene_VoucherAvailability_ScheduledIsVoucher ports
// TestVoucherAvailability_ScheduledIsVoucher: a scheduled voucher -> 200.
func TestSessionsGene_VoucherAvailability_ScheduledIsVoucher(t *testing.T) {
	h := startSessionsGene(t)
	const id = "av-scheduled"
	h.seedOrder(id, sgOwnerEmail, "scheduled")

	resp := h.handleRoute("GET", "/api/voucher/"+id+"/availability", map[string]string{"id": id}, nil, nil)
	if resp.Status != 200 {
		t.Fatalf("availability scheduled: want 200, got %d (%s)", resp.Status, resp.Body)
	}
}

// TestSessionsGene_VoucherAvailability_PaidNotVoucher ports
// TestVoucherAvailability_PaidNotVoucher: a paid (non-voucher) order is not
// reachable as a voucher -> 400 in the cell (native mini-router 404s; same
// invariant, see the file header).
func TestSessionsGene_VoucherAvailability_PaidNotVoucher(t *testing.T) {
	h := startSessionsGene(t)
	const id = "av-paid"
	h.seedOrder(id, sgOwnerEmail, "paid")

	resp := h.handleRoute("GET", "/api/voucher/"+id+"/availability", map[string]string{"id": id}, nil, nil)
	if resp.Status != 400 {
		t.Fatalf("availability paid-not-voucher: want 400, got %d (%s)", resp.Status, resp.Body)
	}
}

// TestSessionsGene_VoucherAvailability_NotFound ports
// TestVoucherAvailability_NotFound: an unknown voucher id -> 404.
func TestSessionsGene_VoucherAvailability_NotFound(t *testing.T) {
	h := startSessionsGene(t)
	resp := h.handleRoute("GET", "/api/voucher/nope/availability", map[string]string{"id": "nope"}, nil, nil)
	if resp.Status != 404 {
		t.Fatalf("availability not-found: want 404, got %d (%s)", resp.Status, resp.Body)
	}
}
