package host

// "Reserve now, charge when live" through the real composed application.
// These tests assert Commerce and Fleet owner contracts, not Evolution's
// retired orders/servers/queue tables.

import (
	"database/sql"
	"strings"
	"testing"
)

func reservedOwnerCheckout(t *testing.T, h *CellHarness, db *sql.DB, key, email string) string {
	t.Helper()
	status, body := postCheckoutWithHeaders(t, h, map[string]any{
		"server_type": "minecraft",
		"email":       email,
	}, map[string]string{"Idempotency-Key": key})
	if status != 200 || body["reserved"] != true {
		t.Fatalf("reserved checkout %s: want 200 + reserved, got %d (%v)", key, status, body)
	}
	orderID := deterministicHarnessIdentifier("order", key)
	order := commerceOrderForID(t, h, orderID)
	if order.Status != "reserved" || order.PaymentStatus != "pending" ||
		!strings.HasPrefix(order.StripePaymentID, "seti_") {
		t.Fatalf("reserved Commerce precondition = %#v", order)
	}
	return orderID
}

func openHarnessFleetCapacity(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(
		`UPDATE nodes SET cpu_budget=16,memory_budget=32 WHERE id='node-1'`,
	); err != nil {
		t.Fatalf("open harness capacity: %v", err)
	}
	checkpoint(db)
}

func TestEvolution_Charge_ReservedOrderIsChargedOnPromotion(t *testing.T) {
	setStripeStubPIStatus("succeeded")
	t.Cleanup(func() { setStripeStubPIStatus("requires_capture") })
	setStripeStubSetupPM(t, "pm_stub_card")

	h, db := startEvolutionDowntimeExtra(t, "", capacityFullCfg)
	seedDowntimeCatalog(t, db)
	orderID := reservedOwnerCheckout(t, h, db, "charge-reserved-promotion", "chg-1@example.com")

	openHarnessFleetCapacity(t, db)
	serverID := orderID + "-server"
	driveUntil(t, h, db, "reserved order to charge and promote", func() bool {
		state, _ := fleetServerState(h, serverID)
		return state == "provisioning" || state == "active"
	})

	order := commerceOrderForID(t, h, orderID)
	if order.Status != "paid" || order.PaymentStatus != "succeeded" ||
		!strings.HasPrefix(order.StripePaymentID, "pi_") {
		t.Fatalf("reserved order lacks durable refundable charge = %#v", order)
	}
}

func TestEvolution_Charge_ReservedOrderNotChargedWhileWaiting(t *testing.T) {
	setStripeStubPIStatus("succeeded")
	t.Cleanup(func() { setStripeStubPIStatus("requires_capture") })
	setStripeStubSetupPM(t, "pm_stub_card")

	h, db := startEvolutionDowntimeExtra(t, "", capacityFullCfg)
	seedDowntimeCatalog(t, db)
	orderID := reservedOwnerCheckout(t, h, db, "charge-waits-for-capacity", "chg-2@example.com")

	for i := 0; i < 12; i++ {
		driveTick(h, db)
	}
	order := commerceOrderForID(t, h, orderID)
	if order.Status != "reserved" || order.PaymentStatus != "pending" ||
		!strings.HasPrefix(order.StripePaymentID, "seti_") {
		t.Fatalf("waiting order moved money without capacity = %#v", order)
	}
	if state, found := fleetServerState(h, orderID+"-server"); !found || state != "queued" {
		t.Fatalf("waiting order did not retain its Fleet queue position: state=%q found=%v", state, found)
	}
}

func TestEvolution_Charge_AlreadyChargedOrderIsNotChargedAgain(t *testing.T) {
	setStripeStubPIStatus("succeeded")
	t.Cleanup(func() { setStripeStubPIStatus("requires_capture") })
	setStripeStubSetupPM(t, "pm_stub_card")

	h, db := startEvolutionDowntimeExtra(t, "", capacityFullCfg)
	seedDowntimeCatalog(t, db)
	orderID := reservedOwnerCheckout(t, h, db, "charge-idempotent-replay", "chg-3@example.com")
	openHarnessFleetCapacity(t, db)
	driveUntil(t, h, db, "reserved order first promotion", func() bool {
		state, _ := fleetServerState(h, orderID+"-server")
		return state == "provisioning" || state == "active"
	})
	first := commerceOrderForID(t, h, orderID)
	if !strings.HasPrefix(first.StripePaymentID, "pi_") {
		t.Fatalf("first charge did not settle: %#v", first)
	}

	for i := 0; i < 12; i++ {
		driveTick(h, db)
	}
	replayed := commerceOrderForID(t, h, orderID)
	if replayed.StripePaymentID != first.StripePaymentID ||
		replayed.PaymentStatus != "succeeded" {
		t.Fatalf("settled order was charged again: first=%#v replay=%#v", first, replayed)
	}
}
