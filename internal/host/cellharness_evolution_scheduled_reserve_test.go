package host

// Scheduled orders store a card without charging, then the owner-driven Lua
// lifecycle holds Fleet capacity, charges, records the Commerce receipt, and
// promotes on the start day. These proofs query the real owner cells rather
// than Evolution's retired compatibility tables.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

func TestEvolution_Scheduled_StoresCardWithoutCharging(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)

	const idempotencyKey = "scheduled-store-card"
	status, body := postCheckoutWithHeaders(t, h, map[string]any{
		"server_type":  "minecraft",
		"email":        "sched@example.com",
		"auto_redeem":  false,
		"scheduled_at": "2099-01-01T00:00:00Z",
	}, map[string]string{"Idempotency-Key": idempotencyKey})
	if status != 200 || body["reserved"] != true {
		t.Fatalf("scheduled checkout: want 200 + reserved, got %d (%v)", status, body)
	}
	if cs, _ := body["client_secret"].(string); cs == "" {
		t.Errorf("scheduled checkout returned no SetupIntent client_secret (%v)", body)
	}
	if body["free"] == true {
		t.Errorf("paid scheduled order reported free=true (%v)", body)
	}

	orderID := deterministicHarnessIdentifier("order", idempotencyKey)
	order := commerceOrderForID(t, h, orderID)
	if order.Status != "scheduled" {
		t.Fatalf("scheduled order status = %q, want scheduled", order.Status)
	}
	if order.PaymentStatus != "pending" || !strings.HasPrefix(order.StripePaymentID, "seti_") {
		t.Fatalf("scheduled order charged instead of storing a card: %#v", order)
	}
}

func TestEvolution_Scheduled_ActivatesToReservedAndChargesOnStartDay(t *testing.T) {
	setStripeStubPIStatus("succeeded")
	t.Cleanup(func() { setStripeStubPIStatus("requires_capture") })
	setStripeStubSetupPM(t, "pm_stub_card")

	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)

	const idempotencyKey = "scheduled-activate-charge"
	status, body := postCheckoutWithHeaders(t, h, map[string]any{
		"server_type":  "minecraft",
		"email":        "schedchg@example.com",
		"auto_redeem":  false,
		"scheduled_at": time.Now().UTC().Add(3 * time.Second).Format(time.RFC3339),
	}, map[string]string{"Idempotency-Key": idempotencyKey})
	if status != 200 || body["reserved"] != true {
		t.Fatalf("scheduled checkout precondition: want 200 + reserved, got %d (%v)", status, body)
	}
	orderID := deterministicHarnessIdentifier("order", idempotencyKey)
	if got := commerceOrderForID(t, h, orderID).Status; got != "scheduled" {
		t.Fatalf("precondition: scheduled order should park as scheduled, got %q", got)
	}

	serverID := orderID + "-server"
	driveUntil(t, h, db, "scheduled owner order to charge and promote", func() bool {
		state, _ := fleetServerState(h, serverID)
		return state == "provisioning" || state == "active"
	})

	order := commerceOrderForID(t, h, orderID)
	if order.Status != "paid" || order.PaymentStatus != "succeeded" {
		t.Fatalf("scheduled order promoted without durable Commerce payment: %#v", order)
	}
	if !strings.HasPrefix(order.StripePaymentID, "pi_") {
		t.Fatalf("scheduled charge did not persist the refundable PaymentIntent: %#v", order)
	}
}

func fleetServerState(h *CellHarness, serverID string) (string, bool) {
	fleetCell := h.cellsByName["fleet"]
	if fleetCell == nil {
		return "", false
	}
	wire, err := msgpack.Marshal(map[string]string{"id": serverID})
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := fleetCell.Call(ctx, "fleet.v1.query.server.get", wire)
	if err != nil {
		return "", false
	}
	var server struct {
		Status string `msgpack:"status"`
	}
	if msgpack.Unmarshal(response, &server) != nil {
		return "", false
	}
	return server.Status, server.Status != ""
}
