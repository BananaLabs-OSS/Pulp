package host

// Remaining Stripe webhook compatibility cases through authoritative owner
// state. No case seeds or inspects Evolution's retired order projection.

import (
	"testing"
	"time"
)

func TestEvolution_Webhook_AutoRedeemMarksPaid(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	orderID, order := ownerCheckoutPending(t, h, "webhook-auto-redeem", "ar@example.com", nil)

	settleOwnerCheckout(t, h, "evt-ar", order)
	settled := commerceOrderForID(t, h, orderID)
	if settled.Status != "paid" || settled.PaymentStatus != "succeeded" {
		t.Fatalf("auto-redeem Commerce order should be paid, got %#v", settled)
	}
	server := fleetServerForID(t, h, orderID+"-server")
	if server.ID != orderID+"-server" || server.OrderID != orderID ||
		server.NodeID == "" || (server.Status != "provisioning" && server.Status != "ready") {
		t.Fatalf("paid checkout did not reach Fleet provisioning through Lua: %#v", server)
	}
}

func TestEvolution_Webhook_ScheduledVoucherStaysScheduled(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)

	const key = "webhook-scheduled-owner"
	status, response := postCheckoutWithHeaders(t, h, map[string]any{
		"server_type":  "minecraft",
		"email":        "scheduled@example.com",
		"auto_redeem":  false,
		"scheduled_at": time.Now().UTC().AddDate(0, 0, 7).Format(time.RFC3339),
	}, map[string]string{"Idempotency-Key": key})
	if status != 200 || response["reserved"] != true {
		t.Fatalf("scheduled checkout: want 200 + reserved, got %d (%v)", status, response)
	}
	orderID := deterministicHarnessIdentifier("order", key)
	before := commerceOrderForID(t, h, orderID)
	if before.Status != "scheduled" {
		t.Fatalf("scheduled Commerce precondition = %#v", before)
	}

	// A PaymentIntent event cannot settle an order whose durable payment
	// authority is a SetupIntent awaiting its start-day lifecycle.
	status, body := postStripeWebhook(t, h, paymentWebhookPayload(
		t, "evt-sch", "payment_intent.succeeded", "pi_unrelated_schedule", 1400,
	))
	if status != 200 {
		t.Fatalf("unrelated scheduled webhook: want 200, got %d (%s)", status, body)
	}
	after := commerceOrderForID(t, h, orderID)
	if after.Status != "scheduled" || after.StripePaymentID != before.StripePaymentID {
		t.Fatalf("scheduled order changed after unrelated PI event: before=%#v after=%#v", before, after)
	}
}

func TestEvolution_Webhook_DuplicateIgnored(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	orderID, order := ownerCheckoutPending(t, h, "webhook-duplicate-owner", "dup@example.com", nil)
	payload := paymentWebhookPayload(t, "evt-dup", "payment_intent.succeeded", order.StripePaymentID, order.AmountCents)

	status, body := postStripeWebhook(t, h, payload)
	if status != 200 {
		t.Fatalf("first webhook: want 200, got %d (%s)", status, body)
	}
	first := commerceOrderForID(t, h, orderID)
	status, body = postStripeWebhook(t, h, payload)
	if status != 200 {
		t.Fatalf("duplicate webhook: want 200, got %d (%s)", status, body)
	}
	replayed := commerceOrderForID(t, h, orderID)
	if replayed.Status != first.Status || replayed.PaymentStatus != first.PaymentStatus ||
		replayed.StripePaymentID != first.StripePaymentID {
		t.Fatalf("duplicate webhook changed Commerce state: first=%#v replay=%#v", first, replayed)
	}
}

func TestEvolution_Webhook_UnknownOrderIgnored(t *testing.T) {
	h, _ := startEvolutionDowntime(t)

	status, body := postStripeWebhook(t, h, paymentWebhookPayload(
		t, "evt-unk", "payment_intent.succeeded", "pi_nonexistent", 1400,
	))
	if status != 200 {
		t.Fatalf("unknown-order webhook: want 200, got %d (%s)", status, body)
	}
}

func TestEvolution_Webhook_UnknownEventTypeAcknowledged(t *testing.T) {
	h, _ := startEvolutionDowntime(t)

	status, body := postStripeWebhook(t, h,
		`{"id":"evt-sub","type":"customer.subscription.created","data":{"object":{"id":"cs_1"}}}`)
	if status != 200 {
		t.Fatalf("unknown event type: want 200, got %d (%s)", status, body)
	}
}
