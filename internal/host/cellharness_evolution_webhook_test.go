package host

// Stripe webhook compatibility through the real Pulp -> Lua -> Commerce path.
// Test setup creates orders through checkout and assertions query Commerce, so
// these proofs cannot accidentally pass against Evolution's retired tables.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

func postStripeWebhook(t *testing.T, h *CellHarness, payload string) (int, []byte) {
	t.Helper()
	return h.Do("POST", "/api/webhooks/stripe",
		map[string]string{"Content-Type": "application/json", "Stripe-Signature": "t=1,v1=stub"},
		[]byte(payload))
}

func ownerCheckoutPending(t *testing.T, h *CellHarness, key, email string, fields map[string]any) (string, commerceOrderValue) {
	t.Helper()
	setStripeStubPIStatus("requires_capture")
	body := map[string]any{"server_type": "minecraft", "email": email}
	for name, value := range fields {
		body[name] = value
	}
	status, response := postCheckoutWithHeaders(t, h, body, map[string]string{"Idempotency-Key": key})
	if status != 200 {
		t.Fatalf("owner checkout %s: want 200, got %d (%v)", key, status, response)
	}
	orderID := deterministicHarnessIdentifier("order", key)
	order := commerceOrderForID(t, h, orderID)
	if order.Status != "checkout_pending" || order.PaymentStatus != "pending" ||
		order.StripePaymentID == "" {
		t.Fatalf("owner checkout %s did not persist a pending payment: %#v", key, order)
	}
	return orderID, order
}

func paymentWebhookPayload(t *testing.T, eventID, eventType, paymentIntentID string, amount int64) string {
	t.Helper()
	object := map[string]any{"id": paymentIntentID}
	if amount > 0 {
		object["amount_received"] = amount
	}
	if eventType == "charge.dispute.created" {
		object = map[string]any{"payment_intent": paymentIntentID}
	}
	wire, err := json.Marshal(map[string]any{
		"id": eventID, "type": eventType,
		"data": map[string]any{"object": object},
	})
	if err != nil {
		t.Fatalf("encode Stripe webhook: %v", err)
	}
	return string(wire)
}

func settleOwnerCheckout(t *testing.T, h *CellHarness, eventID string, order commerceOrderValue) {
	t.Helper()
	status, body := postStripeWebhook(t, h, paymentWebhookPayload(
		t, eventID, "payment_intent.succeeded", order.StripePaymentID, order.AmountCents,
	))
	if status != 200 {
		t.Fatalf("payment success webhook: want 200, got %d (%s)", status, body)
	}
}

func TestEvolution_Webhook_DisputeFlagsOrder(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	orderID, order := ownerCheckoutPending(t, h, "webhook-dispute-owner", "d@example.com", nil)
	settleOwnerCheckout(t, h, "evt-disp-paid", order)

	status, body := postStripeWebhook(t, h, paymentWebhookPayload(
		t, "evt-disp-1", "charge.dispute.created", order.StripePaymentID, 0,
	))
	if status != 200 {
		t.Fatalf("dispute webhook: want 200, got %d (%s)", status, body)
	}
	if got := commerceOrderForID(t, h, orderID).Status; got != "disputed" {
		t.Fatalf("expected Commerce order flagged disputed, got %q", got)
	}
}

type commerceCouponListResult struct {
	OK    bool `msgpack:"ok"`
	Value []struct {
		Code string `msgpack:"code"`
		Uses int64  `msgpack:"uses"`
	} `msgpack:"value"`
}

type fleetServerValue struct {
	ID                      string `msgpack:"id"`
	OrderID                 string `msgpack:"order_id"`
	NodeID                  string `msgpack:"node_id"`
	DisplayName             string `msgpack:"display_name"`
	Status                  string `msgpack:"status"`
	ExpiresAt               string `msgpack:"expires_at"`
	DowntimeCreditSeconds   int64  `msgpack:"downtime_credit_seconds"`
	DowntimeCreditedThrough string `msgpack:"downtime_credited_through"`
}

func fleetServerForID(t *testing.T, h *CellHarness, serverID string) fleetServerValue {
	t.Helper()
	cell := h.cellsByName["fleet"]
	if cell == nil {
		t.Fatal("composed application has no Fleet owner")
	}
	request, err := msgpack.Marshal(map[string]any{"id": serverID})
	if err != nil {
		t.Fatalf("encode Fleet server query: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := cell.Call(ctx, "fleet.v1.query.server.get", request)
	if err != nil {
		t.Fatalf("query Fleet server %s: %v", serverID, err)
	}
	var server fleetServerValue
	if err := msgpack.Unmarshal(response, &server); err != nil {
		t.Fatalf("decode Fleet server %s: %v", serverID, err)
	}
	return server
}

func fleetServerExists(t *testing.T, h *CellHarness, serverID string) bool {
	t.Helper()
	cell := h.cellsByName["fleet"]
	if cell == nil {
		t.Fatal("composed application has no Fleet owner")
	}
	request, err := msgpack.Marshal(map[string]any{"id": serverID})
	if err != nil {
		t.Fatalf("encode Fleet server query: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = cell.Call(ctx, "fleet.v1.query.server.get", request)
	return err == nil
}

func commerceCouponUses(t *testing.T, h *CellHarness, code string) int64 {
	t.Helper()
	cell := h.cellsByName["commerce"]
	if cell == nil {
		t.Fatal("composed application has no Commerce owner")
	}
	request, err := msgpack.Marshal(map[string]any{})
	if err != nil {
		t.Fatalf("encode coupon list: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := cell.Call(ctx, "commerce.admin.coupon.list-legacy.v1", request)
	if err != nil {
		t.Fatalf("query Commerce coupons: %v", err)
	}
	var result commerceCouponListResult
	if err := msgpack.Unmarshal(response, &result); err != nil || !result.OK {
		t.Fatalf("decode Commerce coupons: result=%#v err=%v", result, err)
	}
	for _, coupon := range result.Value {
		if coupon.Code == code {
			return coupon.Uses
		}
	}
	t.Fatalf("Commerce coupon %q not found", code)
	return 0
}

func TestEvolution_Webhook_CouponUsesIncrements(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	expires := time.Now().UTC().Add(24 * time.Hour)
	seedCoupon(t, db, "TEST5", 200, 5, 0, &expires)

	_, order := ownerCheckoutPending(t, h, "webhook-coupon-owner", "c@example.com", map[string]any{
		"promo_code": "TEST5",
	})
	if order.AmountCents != 1200 {
		t.Fatalf("coupon checkout amount = %d, want 1200", order.AmountCents)
	}
	settleOwnerCheckout(t, h, "evt-cpn-1", order)
	if uses := commerceCouponUses(t, h, "TEST5"); uses != 1 {
		t.Fatalf("expected Commerce coupon uses=1 after success, got %d", uses)
	}
}
