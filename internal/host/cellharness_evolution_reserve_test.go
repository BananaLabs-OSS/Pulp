package host

// "Reserve now, charge when live" — schema proofs (Evolution migration
// 00000002_reserve_columns).
//
// A full fleet must not refuse the sale: checkout runs a Stripe SetupIntent, the
// card sits at Stripe, and nothing is charged until promoteQueue brings the
// server live. These pin the DATA MODEL that flow stands on, before any of the
// payment behaviour exists.
//
// SCOPE — read this before trusting it. 00000002 has two branches and this file
// exercises exactly one:
//
//   - FRESH DB (here): the migrator runs the baseline first, whose createTables
//     builds `orders` from cellmodels.AllModels() — which already declares these
//     fields. So the columns exist and 00000002's loop skips every one. What
//     this proves is that the models, the baseline and the migration AGREE, and
//     that 00000002 is a clean no-op rather than a "duplicate column" boot
//     failure.
//   - EXISTING DB (prod, Crunchy Postgres): the columns are genuinely absent and
//     00000002 must ALTER them in. That branch is NOT covered here — the harness
//     always starts from an empty temp dir, StorageRoot isn't injectable, and
//     rebooting a cell onto a pre-seeded DB would mean changing shared harness
//     semantics. It is guarded instead by the migration's own post-condition
//     (re-introspect, hard error if a column is still missing), so the prod path
//     fails LOUDLY at boot rather than silently scanning into a missing column.
//     It still wants a real Postgres dry-run before deploy.

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

func TestEvolution_Reserve_ColumnsExistAfterMigration(t *testing.T) {
	_, db := startEvolutionDowntime(t)

	for _, col := range reserveColumns {
		if !ordersHasColumn(t, db, col) {
			t.Errorf("orders.%s missing after migrations: a reserved order cannot "+
				"record its Stripe references, so the charge-on-live flow has nowhere to live", col)
		}
	}
}

// The cell must BOOT with 00000002 registered. On a fresh DB the baseline has
// already created these columns, so a naive `ALTER TABLE ADD COLUMN` would fail
// with "duplicate column" and take the whole cell down on startup. Reaching a
// serving state at all is the proof.
func TestEvolution_Reserve_MigrationIsCleanOnFreshDB(t *testing.T) {
	h, db := startEvolutionDowntime(t)

	status, _ := h.Do("GET", "/health", nil, nil)
	if status != 200 {
		t.Fatalf("cell did not boot cleanly with migration 00000002: /health = %d", status)
	}

	// bun records applied versions in bun_migrations. 00000002 must be there:
	// if the migrator skipped it, prod would never get its ALTER.
	var applied int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM bun_migrations WHERE name LIKE '00000002%'`,
	).Scan(&applied); err != nil {
		t.Fatalf("read bun_migrations: %v", err)
	}
	if applied == 0 {
		t.Fatalf("migration 00000002 was never recorded as applied; prod would not get the reserve columns")
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
