package host

// LAYER C PROOF — gene-owned endpoint tests reached by LOADING THE REAL
// sessions-gene cell.
//
// The coverage map lists ~25 sessions-gene-owned endpoint tests (/api/sessions,
// /api/voucher/*, extend/schedule, ...) as unreachable "because the harness
// stubs the gene out". That is true of the ENGINE-side harness (StartCellHTTP +
// siblingStubCapability, which answers pulp_call with code 4). It is NOT true of
// the DEDICATED gene harness in cellharness_sessionsgene_test.go: startSessions
// Gene() builds + Inits the real Sessions-Gene/pulp-cell wasm under the Pulp
// host and drives gene-owned routes exactly the way the Evolution engine
// proxies them — cell.Call(ctx, "gene.handle_route", <msgpack HTTPRequest>).
//
// So the gene-owned endpoint tests ARE portable today: they load the gene cell
// directly and call handle_route, rather than trying to reach the gene through
// the engine's stubbed sibling. These ports of
// Evolution/internal/router/voucher_flow_test.go's schedule/unschedule cases
// prove the gene's BUSINESS LOGIC (not just the owner gate the existing gene
// harness already pins) is reachable end-to-end. Every other voucher_flow /
// handlers_integration gene-owned case is now-PORTABLE by the same primitive.
//
// All bodies carry the OWNER email so the C2 owner gate passes and the assertion
// lands on the handler's business logic (the gate itself is covered by
// TestSessionsGene_OwnerGate_* in the sibling file).

import (
	"testing"
	"time"
)

// TestSessionsGene_VoucherSchedule_InvalidDateFormat ports
// TestVoucherSchedule_InvalidDateFormat: a malformed date -> 400 (rejected
// before the DB is even touched).
func TestSessionsGene_VoucherSchedule_InvalidDateFormat(t *testing.T) {
	h := startSessionsGene(t)
	const id = "vs-baddate"
	h.seedOrder(id, sgOwnerEmail, "purchased")

	resp := h.handleRoute("POST", "/api/voucher/"+id+"/schedule", map[string]string{"id": id}, nil,
		map[string]any{"email": sgOwnerEmail, "date": "not-a-date"})
	if resp.Status != 400 {
		t.Fatalf("invalid date: want 400, got %d (%s)", resp.Status, resp.Body)
	}
}

// TestSessionsGene_VoucherSchedule_PastDate ports TestVoucherSchedule_PastDate:
// a date in the past -> 400.
func TestSessionsGene_VoucherSchedule_PastDate(t *testing.T) {
	h := startSessionsGene(t)
	const id = "vs-past"
	h.seedOrder(id, sgOwnerEmail, "purchased")

	resp := h.handleRoute("POST", "/api/voucher/"+id+"/schedule", map[string]string{"id": id}, nil,
		map[string]any{"email": sgOwnerEmail, "date": "2000-01-01"})
	if resp.Status != 400 {
		t.Fatalf("past date: want 400, got %d (%s)", resp.Status, resp.Body)
	}
}

// TestSessionsGene_VoucherSchedule_OnlyPurchasedAllowed ports
// TestVoucherSchedule_OnlyPurchasedAllowed: a non-purchased voucher (here
// `paid`) passes the owner gate + date checks but is not schedulable -> 400.
func TestSessionsGene_VoucherSchedule_OnlyPurchasedAllowed(t *testing.T) {
	h := startSessionsGene(t)
	const id = "vs-notpurchased"
	h.seedOrder(id, sgOwnerEmail, "paid") // not `purchased`

	resp := h.handleRoute("POST", "/api/voucher/"+id+"/schedule", map[string]string{"id": id}, nil,
		map[string]any{"email": sgOwnerEmail, "date": futureDate()})
	if resp.Status != 400 {
		t.Fatalf("non-purchased schedule: want 400 (not schedulable), got %d (%s)", resp.Status, resp.Body)
	}
}

// TestSessionsGene_VoucherUnschedule_NotScheduled ports
// TestVoucherUnschedule_NotScheduled: unscheduling a voucher that is not in the
// scheduled state -> 400.
func TestSessionsGene_VoucherUnschedule_NotScheduled(t *testing.T) {
	h := startSessionsGene(t)
	const id = "vu-notsched"
	h.seedOrder(id, sgOwnerEmail, "purchased") // not `scheduled`

	resp := h.handleRoute("POST", "/api/voucher/"+id+"/unschedule", map[string]string{"id": id}, nil,
		map[string]any{"email": sgOwnerEmail})
	if resp.Status != 400 {
		t.Fatalf("unschedule not-scheduled: want 400, got %d (%s)", resp.Status, resp.Body)
	}
}

// TestSessionsGene_VoucherUnschedule_Success ports TestVoucherUnschedule_Success:
// a scheduled voucher (unexpired) unschedules cleanly -> 200.
func TestSessionsGene_VoucherUnschedule_Success(t *testing.T) {
	h := startSessionsGene(t)
	const id = "vu-ok"
	h.seedOrder(id, sgOwnerEmail, "scheduled") // voucher_expires_at unset => not expired

	resp := h.handleRoute("POST", "/api/voucher/"+id+"/unschedule", map[string]string{"id": id}, nil,
		map[string]any{"email": sgOwnerEmail})
	if resp.Status != 200 {
		t.Fatalf("unschedule scheduled voucher: want 200, got %d (%s)", resp.Status, resp.Body)
	}
}

// futureDate returns a YYYY-MM-DD safely in the future for schedule bodies.
func futureDate() string {
	return time.Now().UTC().AddDate(0, 0, 30).Format("2006-01-02")
}
