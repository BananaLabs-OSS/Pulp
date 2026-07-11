package host

// PORT of the remaining payment_intent.succeeded / event-dispatch cases from
// Evolution/internal/router/stripe_webhook_test.go, driven THROUGH the real
// Evolution cell's POST /api/webhooks/stripe. The native test reconstructs the
// webhook handler (buildWebhookRouter); this drives the cell's REAL handler
// (signature verify -> stripe_events dedup -> event dispatch) and asserts its
// OWN DB writes.
//
// CELL-SEMANTICS NOTE (why VoucherPathMarksPurchased is a GAP, not a port):
// the native buildWebhookRouter reconstructs a voucher branch that flips a
// non-auto-redeem order to `purchased` and stamps voucher_expires_at ~1 year
// out. The REAL cell handler routes the voucher's fulfillment through the
// sessions gene's deploySession; with no gene loaded (the engine harness stubs
// the sibling), the cell's fallback flips the order straight to `paid`
// (router.go: "sessions gene not loaded, fallback: direct status=paid"). So the
// cell never lands a non-auto-redeem order on `purchased` + a 1yr voucher expiry
// on this path — that shape is the reconstruction's, not the cell's. Recorded
// as a GAP in the deliverable. Everything else below is the cell's real writes.

import (
	"database/sql"
	"testing"
	"time"
)

// seedWebhookOrder seeds a pending-ish order the webhook keys on by
// stripe_session_id, with the auto_redeem / scheduled_at / amount the case needs.
func seedWebhookOrder(t *testing.T, db *sql.DB, id, pi, status string, autoRedeem bool, amountCents int, scheduledAt *time.Time) {
	t.Helper()
	now := time.Now().UTC()
	var sched any
	if scheduledAt != nil {
		sched = *scheduledAt
	}
	if _, err := db.Exec(
		`INSERT INTO orders (id, stripe_session_id, server_type, tier_id, email, status, auto_redeem, amount_cents, scheduled_at, created_at)
		 VALUES (?, ?, 'minecraft', 'standard', ?, ?, ?, ?, ?, ?)`,
		id, pi, id+"@e.com", status, boolInt(autoRedeem), amountCents, sched, now,
	); err != nil {
		t.Fatalf("seed webhook order %s: %v", id, err)
	}
	checkpoint(db)
}

func orderStatus(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var st string
	if err := db.QueryRow(`SELECT status FROM orders WHERE id = ?`, id).Scan(&st); err != nil {
		t.Fatalf("read order %s status: %v", id, err)
	}
	return st
}

// TestEvolution_Webhook_AutoRedeemMarksPaid ports TestWebhook_AutoRedeemMarksPaid:
// a pending auto-redeem order, on payment_intent.succeeded, is flipped to `paid`
// (the no-gene fallback path — the real cell fulfillment target for auto-redeem).
func TestEvolution_Webhook_AutoRedeemMarksPaid(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedWebhookOrder(t, db, "ord-ar", "pi_ar", "pending", true, 1400, nil)

	status, b := postStripeWebhook(t, h,
		`{"id":"evt-ar","type":"payment_intent.succeeded","data":{"object":{"id":"pi_ar","amount_received":1400}}}`)
	if status != 200 {
		t.Fatalf("auto-redeem webhook: want 200, got %d (%s)", status, b)
	}
	if st := orderStatus(t, db, "ord-ar"); st != "paid" {
		t.Fatalf("auto-redeem order should be paid, got %q", st)
	}
}

// TestEvolution_Webhook_ScheduledVoucherStaysScheduled ports
// TestWebhook_ScheduledVoucherStaysScheduled: a non-auto-redeem order whose
// scheduled_at is in the future is parked as `scheduled` (deploy deferred), not
// deployed now.
func TestEvolution_Webhook_ScheduledVoucherStaysScheduled(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	future := time.Now().UTC().AddDate(0, 0, 7)
	seedWebhookOrder(t, db, "ord-sch", "pi_sch", "pending", false, 1400, &future)

	status, b := postStripeWebhook(t, h,
		`{"id":"evt-sch","type":"payment_intent.succeeded","data":{"object":{"id":"pi_sch","amount_received":1400}}}`)
	if status != 200 {
		t.Fatalf("scheduled webhook: want 200, got %d (%s)", status, b)
	}
	if st := orderStatus(t, db, "ord-sch"); st != "scheduled" {
		t.Fatalf("future-scheduled order should stay scheduled, got %q", st)
	}
}

// TestEvolution_Webhook_DuplicateIgnored ports TestWebhook_DuplicateIgnored:
// a payment_intent.succeeded for an order that is NOT pending (already
// processed by a prior delivery) is acknowledged 200 and NOT re-processed —
// the handler's `order.Status != OrderPending` early-return guard.
//
// The native case seeds `paid` as the already-processed state, but in the cell
// `paid` is not terminal — the poller enqueues+provisions a paid order to
// `fulfilled` on the next driven tick, so it can't stand in for "already
// processed and untouched". We seed the terminal `fulfilled` state instead
// (which no poller advances) and assert the duplicate webhook leaves it there.
func TestEvolution_Webhook_DuplicateIgnored(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedWebhookOrder(t, db, "ord-dup", "pi_dup", "fulfilled", true, 1400, nil)

	status, b := postStripeWebhook(t, h,
		`{"id":"evt-dup","type":"payment_intent.succeeded","data":{"object":{"id":"pi_dup","amount_received":1400}}}`)
	if status != 200 {
		t.Fatalf("duplicate webhook: want 200, got %d (%s)", status, b)
	}
	if st := orderStatus(t, db, "ord-dup"); st != "fulfilled" {
		t.Fatalf("already-processed order must be left untouched on duplicate delivery, got %q", st)
	}
}

// TestEvolution_Webhook_UnknownOrderIgnored ports TestWebhook_UnknownOrderIDIgnored:
// a payment_intent.succeeded whose PI matches no order is acknowledged 200 (Stripe
// best practice — never 4xx an unknown PI, or Stripe retries forever).
func TestEvolution_Webhook_UnknownOrderIgnored(t *testing.T) {
	h, _ := startEvolutionDowntime(t)

	status, b := postStripeWebhook(t, h,
		`{"id":"evt-unk","type":"payment_intent.succeeded","data":{"object":{"id":"pi_nonexistent","amount_received":1400}}}`)
	if status != 200 {
		t.Fatalf("unknown-order webhook: want 200, got %d (%s)", status, b)
	}
}

// TestEvolution_Webhook_UnknownEventTypeAcknowledged ports
// TestWebhook_UnknownEventTypeAcknowledged: an event type the handler does not
// branch on falls through to a 200 acknowledgement.
func TestEvolution_Webhook_UnknownEventTypeAcknowledged(t *testing.T) {
	h, _ := startEvolutionDowntime(t)

	status, b := postStripeWebhook(t, h,
		`{"id":"evt-sub","type":"customer.subscription.created","data":{"object":{"id":"cs_1"}}}`)
	if status != 200 {
		t.Fatalf("unknown event type: want 200, got %d (%s)", status, b)
	}
}
