package host

// FOUNDATION proof for the multi-node registry (adr/multi-node-registry-locked,
// build phase B1). Evolution must understand WHICH NODE each server sits on and
// treat capacity as NODE-OWNED: the node reports cpu/memory, Evolution sums it
// over the active nodes via nodeBudget(). This drives the real evolution.wasm
// cell and pins two things B1 must hold:
//
//   1. nodeBudget() returns the node's SELF-REPORTED budget. We seed one active
//      node whose bananagine_url routes to the "budgetnode" stub host (which
//      reports cpu_budget 14 / memory_budget 48 at /orchestration/stats) and
//      read the budget back off GET /api/calendar, the endpoint that surfaces
//      nodeBudget's cpu_budget/memory_budget verbatim.
//
//   2. servers carry node_id after the 0001_nodes backfill. The nodes table +
//      servers.node_id column are built on this fresh harness DB by bun's
//      baseline (from the cellmodels Node struct + Server.NodeID field), so a
//      server inserted WITHOUT a node_id (a pre-migration row) is stamped to
//      node-1 by the exact backfill UPDATE 0001_nodes runs, and reads back with
//      node_id = "node-1".
//
// SINGLE-NODE INVARIANT: one active node summed == that node's budget, which is
// why the 29 prod servers backfilling to node-1 is byte-identical to today.

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"
)

// budgetNodeURL routes to the "budgetnode" branch of the downtime Bananagine
// stub, which reports a real cpu_budget/memory_budget at /orchestration/stats.
const budgetNodeURL = "http://budgetnode:3000"

func TestEvolution_NodeRegistry_NodeBudgetSumsActiveNode(t *testing.T) {
	// cfg.BananagineURL points at the budget stub too, so nodeBudget yields the
	// same 14/48 whether it reads the seeded node row or (before the row is
	// visible / while the 60s cache is warm) the single-node bridge — the value
	// under test is stable regardless of cache timing.
	h, _ := startEvolutionDowntimeExtra(t, "", map[string]any{
		"bananagine_url": budgetNodeURL,
	})

	// No manual seed: the cell registers its own node-1 at boot (registerLocalNode)
	// from cfg.BananagineURL, which is budgetNodeURL here. So node-1 is already an
	// active node reporting 14/48 — this now also proves boot self-registration.
	status, body := h.Do("GET", "/api/calendar", nil, nil)
	if status != 200 {
		t.Fatalf("GET /api/calendar: want 200, got %d (%s)", status, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode calendar: %v (%s)", err, body)
	}
	cpu, _ := out["cpu_budget"].(float64)
	mem, _ := out["memory_budget"].(float64)
	if cpu != 14 {
		t.Errorf("cpu_budget = %v, want 14 (the active node's self-reported budget)", out["cpu_budget"])
	}
	if mem != 48 {
		t.Errorf("memory_budget = %v, want 48 (the active node's self-reported budget)", out["memory_budget"])
	}
}

func TestEvolution_NodeRegistry_ServersBackfillToNode1(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	_ = h

	// A server inserted with NO node_id — a pre-migration row, exactly what the
	// 29 prod servers look like before backfill. order_id is a plain notnull text
	// column (no enforced FK), so a placeholder id is sufficient here.
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO servers (id, order_id, template, state, cpu_weight, memory_weight, created_at)
		 VALUES ('srv-nr-1', 'ord-nr-1', 'minecraft', 'active', 1, 1024, ?)`, now,
	); err != nil {
		t.Fatalf("seed server: %v", err)
	}

	// Pre-backfill the column is empty (the model's default for a plain string).
	if got := serverNodeID(t, db, "srv-nr-1"); got != "" {
		t.Fatalf("pre-backfill node_id = %q, want empty", got)
	}

	// The exact backfill 0001_nodes runs.
	if _, err := db.Exec(
		`UPDATE servers SET node_id = 'node-1' WHERE node_id IS NULL OR node_id = ''`,
	); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got := serverNodeID(t, db, "srv-nr-1"); got != "node-1" {
		t.Errorf("post-backfill node_id = %q, want \"node-1\"", got)
	}
}

func serverNodeID(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var nodeID sql.NullString
	if err := db.QueryRow(`SELECT node_id FROM servers WHERE id = ?`, id).Scan(&nodeID); err != nil {
		t.Fatalf("read server node_id for %s: %v", id, err)
	}
	return nodeID.String
}
