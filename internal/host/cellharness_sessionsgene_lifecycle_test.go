package host

import "testing"

// The engine persists server=active and order=fulfilled before invoking this
// hook. Sessions-Gene may react to that transition, but it must not write the
// shared engine lifecycle row itself.
func TestSessionsGene_OnServerReady_DoesNotMutateEngineOrder(t *testing.T) {
	h := startSessionsGene(t)
	const id = "ready-read-only"
	h.seedOrder(id, sgOwnerEmail, "paid")

	h.onServerReady(id, "server-ready-1")

	var status string
	if err := h.db.QueryRow(`SELECT status FROM orders WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if status != "paid" {
		t.Fatalf("gene mutated engine-owned lifecycle state: status=%q", status)
	}
}
