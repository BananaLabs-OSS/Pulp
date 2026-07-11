package host

// PORT of the pool-flow slice of Evolution/internal/router/voucher_flow_test.go
// (TestPool_ContributionFlow / TestPool_ExpiredStateTransition /
// TestPool_CancellationClearsContributions), driven against the REAL Evolution
// cell's own migrated SQLite (pools + pool_contributions tables created by the
// cell's bootstrap migrations).
//
// FAITHFULNESS NOTE: the native tests run NO HTTP handler — they seed pool /
// pool_contribution rows and assert the collected-total arithmetic and the
// open->expired / open->cancelled status transitions plus contribution
// retention. They pin the pool SCHEMA + status-enum constants (PoolOpen /
// PoolExpired / PoolCancelled), not any router closure. These ports assert the
// SAME transitions on the cell's OWN migrated pool tables (the schema the cell
// ships), so the twin dies with a green schema-parity equivalent. The pool
// mutation endpoints (/api/pool/*) are Stripe-PI-gated and covered separately by
// the existing pool /confirm PI-gate proof; this slice is the state-machine
// parity the twin actually asserts.

import (
	"database/sql"
	"testing"
	"time"
)

func seedPool(t *testing.T, db *sql.DB, id, token, status string, targetCents, collectedCents int, expiresAt time.Time) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO pools (id, pool_token, name, server_type, target_cents, collected_cents, status, creator_email, expires_at, created_at)
		 VALUES (?, ?, 'My Pool', 'minecraft', ?, ?, ?, 'creator@e.com', ?, ?)`,
		id, token, targetCents, collectedCents, status, expiresAt, time.Now().UTC(),
	); err != nil {
		t.Fatalf("seed pool %s: %v", id, err)
	}
	checkpoint(db)
}

func seedPoolContribution(t *testing.T, db *sql.DB, id, poolID, email string, amountCents int, confirmed bool) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO pool_contributions (id, pool_id, username, email, amount_cents, confirmed, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, poolID, email, email, amountCents, boolInt(confirmed), time.Now().UTC(),
	); err != nil {
		t.Fatalf("seed pool contribution %s: %v", id, err)
	}
	checkpoint(db)
}

// TestEvolution_Pool_ContributionFlow ports TestPool_ContributionFlow: two
// confirmed contributions drive collected_cents to the target while the pool
// stays open at the half-way point, and both contribution rows persist.
func TestEvolution_Pool_ContributionFlow(t *testing.T) {
	_, db := startEvolutionDowntime(t)

	seedPool(t, db, "pool-1", "pt-abc", "open", 2800, 0, time.Now().UTC().Add(24*time.Hour))

	// First contribution of $14 -> collected 1400, still open.
	seedPoolContribution(t, db, "contrib-1", "pool-1", "alice@e.com", 1400, true)
	if _, err := db.Exec(`UPDATE pools SET collected_cents = collected_cents + 1400 WHERE id = 'pool-1'`); err != nil {
		t.Fatalf("bump collected: %v", err)
	}
	checkpoint(db)

	var collected, target int
	var status string
	if err := db.QueryRow(`SELECT collected_cents, target_cents, status FROM pools WHERE id = 'pool-1'`).
		Scan(&collected, &target, &status); err != nil {
		t.Fatalf("read pool: %v", err)
	}
	if collected != 1400 {
		t.Fatalf("expected 1400 collected, got %d", collected)
	}
	if status != "open" {
		t.Fatalf("pool should still be open at 50%%, got %q", status)
	}

	// Second contribution reaches the target.
	seedPoolContribution(t, db, "contrib-2", "pool-1", "bob@e.com", 1400, true)
	if _, err := db.Exec(`UPDATE pools SET collected_cents = collected_cents + 1400 WHERE id = 'pool-1'`); err != nil {
		t.Fatalf("bump collected 2: %v", err)
	}
	checkpoint(db)

	if err := db.QueryRow(`SELECT collected_cents, target_cents FROM pools WHERE id = 'pool-1'`).
		Scan(&collected, &target); err != nil {
		t.Fatalf("read filled pool: %v", err)
	}
	if collected != 2800 {
		t.Fatalf("expected 2800 collected, got %d", collected)
	}
	if collected < target {
		t.Fatalf("should have reached target: %d >= %d", collected, target)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pool_contributions WHERE pool_id = 'pool-1' AND confirmed = 1`).Scan(&n); err != nil {
		t.Fatalf("count contributions: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 confirmed contributions, got %d", n)
	}
}

// TestEvolution_Pool_ExpiredStateTransition ports TestPool_ExpiredStateTransition:
// the cleanup sweep marks an open pool past its expiry as expired.
func TestEvolution_Pool_ExpiredStateTransition(t *testing.T) {
	_, db := startEvolutionDowntime(t)

	seedPool(t, db, "pool-exp", "pt-exp", "open", 2800, 0, time.Now().UTC().Add(-1*time.Hour))

	if _, err := db.Exec(
		`UPDATE pools SET status = 'expired' WHERE status = 'open' AND expires_at < ?`, time.Now().UTC(),
	); err != nil {
		t.Fatalf("expire sweep: %v", err)
	}
	checkpoint(db)

	var status string
	if err := db.QueryRow(`SELECT status FROM pools WHERE id = 'pool-exp'`).Scan(&status); err != nil {
		t.Fatalf("read pool: %v", err)
	}
	if status != "expired" {
		t.Fatalf("expected expired status, got %q", status)
	}
}

// TestEvolution_Pool_CancellationClearsContributions ports
// TestPool_CancellationClearsContributions: cancelling a pool flips it to
// cancelled but PRESERVES its contribution rows (needed for refund processing).
func TestEvolution_Pool_CancellationClearsContributions(t *testing.T) {
	_, db := startEvolutionDowntime(t)

	seedPool(t, db, "pool-cancel", "pt-cancel", "open", 2800, 0, time.Now().UTC().Add(24*time.Hour))
	seedPoolContribution(t, db, "cc-0", "pool-cancel", "a@e.com", 700, true)
	seedPoolContribution(t, db, "cc-1", "pool-cancel", "b@e.com", 700, true)

	if _, err := db.Exec(`UPDATE pools SET status = 'cancelled' WHERE id = 'pool-cancel'`); err != nil {
		t.Fatalf("cancel pool: %v", err)
	}
	checkpoint(db)

	var status string
	if err := db.QueryRow(`SELECT status FROM pools WHERE id = 'pool-cancel'`).Scan(&status); err != nil {
		t.Fatalf("read pool: %v", err)
	}
	if status != "cancelled" {
		t.Fatalf("expected cancelled, got %q", status)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pool_contributions WHERE pool_id = 'pool-cancel'`).Scan(&n); err != nil {
		t.Fatalf("count contributions: %v", err)
	}
	if n != 2 {
		t.Fatalf("cancellation should preserve contribution records, got %d", n)
	}
}
