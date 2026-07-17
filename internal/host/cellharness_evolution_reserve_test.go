package host

// "Reserve now, charge when live" — schema proofs.
//
// A full fleet must not refuse the sale: checkout runs a Stripe SetupIntent, the
// card sits at Stripe, and nothing is charged until promoteQueue brings the
// server live. These pin the DATA MODEL that flow stands on.
//
// SCOPE — read this before trusting it. There is deliberately NO migration file
// (Nick, 2026-07-17): Evolution is not on sow, and a hand-written bun migration
// was dropped in favour of ALTERing the DB directly. So the columns arrive by two
// different routes and this file can only prove one:
//
//   - FRESH DB (covered here): the baseline's createTables builds `orders` from
//     cellmodels.AllModels(), which declares these fields — so a new deploy and
//     this harness get them automatically. What that proves is the model and the
//     schema have not drifted.
//   - EXISTING DB (prod, Crunchy Postgres): CREATE TABLE IF NOT EXISTS never
//     alters an existing table, so prod gets these columns ONLY when someone runs
//     the ALTER by hand. Nothing here can prove that happened. If it is skipped,
//     bun scans into columns that do not exist and the failure lands far from the
//     cause. The DDL is recorded in the reserve-charge-on-live ADR.

import (
	"database/sql"
	"testing"
)

// reserveColumns are the Stripe REFERENCE columns a reserved order needs.
// Stripe holds the card; the DB holds ids only.
var reserveColumns = []string{
	"stripe_setup_intent_id",
	"stripe_customer_id",
	"stripe_payment_method_id",
	"charged_at",
	"charge_attempts",
	"last_charge_attempt_at", // drives the decline backoff; without it retries storm
	"grace_until",
	"last_charge_error",
}

func ordersHasColumn(t *testing.T, db *sql.DB, col string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('orders') WHERE name = ?`, col,
	).Scan(&n); err != nil {
		t.Fatalf("introspect orders.%s: %v", col, err)
	}
	return n > 0
}

func TestEvolution_Reserve_ColumnsExist(t *testing.T) {
	_, db := startEvolutionDowntime(t)

	for _, col := range reserveColumns {
		if !ordersHasColumn(t, db, col) {
			t.Errorf("orders.%s missing: a reserved order cannot "+
				"record its Stripe references, so the charge-on-live flow has nowhere to live", col)
		}
	}
}

// There is deliberately NO migration file for the reserve columns (Nick,
// 2026-07-17): a fresh DB gets them from the models via the baseline's
// createTables, and an existing DB is ALTERed directly by hand. This pins the
// half that is automatic — the cell must boot and build the columns from the
// models alone, with no migration involved.
//
// The other half has no test and cannot have one here: an EXISTING prod DB only
// gets these columns if someone runs the ALTER. See the reserve-charge-on-live
// ADR for the exact DDL. If that is skipped, bun scans into columns that do not
// exist and the failure lands far from the cause.
func TestEvolution_Reserve_FreshDBGetsColumnsFromModelsAlone(t *testing.T) {
	h, db := startEvolutionDowntime(t)

	status, _ := h.Do("GET", "/health", nil, nil)
	if status != 200 {
		t.Fatalf("cell did not boot: /health = %d", status)
	}

	for _, col := range reserveColumns {
		if !ordersHasColumn(t, db, col) {
			t.Errorf("orders.%s absent on a fresh DB: the baseline builds orders from "+
				"cellmodels.AllModels(), so a field missing here means the model and the "+
				"schema have drifted", col)
		}
	}
}

// A reserved order is one with a card on file and NO money taken. The point of
// the whole feature is that these two facts can be true at once, so the schema
// has to be able to express it: Stripe references present, charged_at NULL.
func TestEvolution_Reserve_OrderCanHoldCardRefWithoutBeingCharged(t *testing.T) {
	_, db := startEvolutionDowntime(t)

	if _, err := db.Exec(
		// auto_redeem is notnull with no default, so it has to be stated.
		`INSERT INTO orders (id, stripe_session_id, server_type, email, status,
		                     amount_cents, stripe_setup_intent_id, stripe_customer_id,
		                     stripe_payment_method_id, charge_attempts, auto_redeem, created_at)
		 VALUES ('res-1', 'seti_stub_1', 'minecraft', 'reserved@example.com', 'reserved',
		         1400, 'seti_stub_1', 'cus_stub_1', 'pm_stub_1', 0, true, CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("insert reserved order: %v", err)
	}
	checkpoint(db)

	var status, customer, pm string
	var chargedAt sql.NullTime
	var graceUntil sql.NullTime
	var attempts int
	if err := db.QueryRow(
		`SELECT status, stripe_customer_id, stripe_payment_method_id, charged_at, grace_until, charge_attempts
		 FROM orders WHERE id = 'res-1'`,
	).Scan(&status, &customer, &pm, &chargedAt, &graceUntil, &attempts); err != nil {
		t.Fatalf("read reserved order: %v", err)
	}

	if status != "reserved" {
		t.Errorf("status: want reserved, got %q", status)
	}
	if customer == "" || pm == "" {
		t.Errorf("card references not persisted: customer=%q pm=%q", customer, pm)
	}
	// The whole promise of the feature: reserved means NOT charged.
	if chargedAt.Valid {
		t.Errorf("charged_at must be NULL on a reserved order, got %v", chargedAt.Time)
	}
	// Grace only starts on the first decline, so it is unset until then.
	if graceUntil.Valid {
		t.Errorf("grace_until must be NULL until a charge is declined, got %v", graceUntil.Time)
	}
	if attempts != 0 {
		t.Errorf("charge_attempts: want 0, got %d", attempts)
	}
}

// The reserved order must survive a round-trip through the cell's OWN bun
// mapping, not just raw SQL. bun errors on unmapped columns when scanning
// SELECT * via TableExpr("orders") — which the gifts list and several admin
// queries do — so a column added to the DB but missed in the model would break
// unrelated endpoints. Driving a real endpoint that lists orders proves the
// mapping holds.
func TestEvolution_Reserve_ReservedOrderDoesNotBreakOrderScans(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)

	if _, err := db.Exec(
		`INSERT INTO orders (id, stripe_session_id, server_type, email, status,
		                     amount_cents, stripe_setup_intent_id, stripe_customer_id,
		                     stripe_payment_method_id, charge_attempts, auto_redeem, created_at)
		 VALUES ('res-2', 'seti_stub_2', 'minecraft', 'scan@example.com', 'reserved',
		         1400, 'seti_stub_2', 'cus_stub_2', 'pm_stub_2', 0, true, CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("insert reserved order: %v", err)
	}
	checkpoint(db)

	// A checkout runs the same order-table machinery. If the new columns broke
	// the model mapping, this is where it would surface.
	status, out := postCheckout(t, h, map[string]any{
		"email":       "scan@example.com",
		"server_type": "minecraft",
	})
	if status != 200 {
		t.Fatalf("checkout broke with a reserved order present: %d (%v)", status, out)
	}
}
