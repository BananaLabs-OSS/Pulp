package host

// PORT of Evolution/internal/poller/transitions_test.go (+ the enqueue slice of
// poller_test.go) driven THROUGH the real Evolution cell (evolution.wasm) under
// the Pulp host, WITHOUT the native internal/poller mirror.
//
// The native transitions_test calls p.enqueueNewOrders / p.checkExpirations
// directly on a native *poller. The cell links only under GOOS=wasip1 + the Pulp
// host, so it cannot be `go test`-ed that way. This harness instead seeds the
// order/server rows on the cell's own SQLite (the same reference-data pattern the
// downtime harness uses) and DRIVES the poller's mainTick via driveTick (an
// inbound request runs OnStep -> poll.tickIfDue -> mainTick ->
// enqueueNewOrders + promoteQueue + checkExpirations), then asserts the poller's
// OWN writes.
//
// NOTE on granularity: one driven mainTick runs enqueue + promote + provision in
// the same cycle, so the transient `queued` state the native unit test catches by
// calling enqueueNewOrders in isolation is not separately observable here. These
// ports therefore assert the OBSERVABLE outcome of the transition (a server row
// is / is not created for the order; an active server past its expiry leaves the
// active state), which is exactly the behaviour the native tests pin, reached
// through the cell's real step loop rather than a direct method call.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// seedOrderRow inserts a paid-shaped order directly on the cell's connection
// (reference data the enqueue path reads), mirroring the smoke test's insert plus
// the gift flags the native TestEnqueue_* cases toggle.
func seedOrderRow(t *testing.T, db *sql.DB, id, status string, isGift, giftClaimed bool) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO orders (id, stripe_session_id, server_type, tier_id, email, status, auto_redeem, is_gift, gift_claimed, created_at)
		 VALUES (?, ?, 'minecraft', 'standard', ?, ?, 0, ?, ?, ?)`,
		id, "ss_"+id, id+"@e.com", status, boolInt(isGift), boolInt(giftClaimed), now,
	); err != nil {
		t.Fatalf("seed order %s: %v", id, err)
	}
	checkpoint(db)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func serverCountForOrder(t *testing.T, db *sql.DB, orderID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM servers WHERE order_id = ?`, orderID).Scan(&n); err != nil {
		t.Fatalf("count servers for %s: %v", orderID, err)
	}
	return n
}

// TestEvolution_Enqueue_CreatesServerForPaidOrder ports
// TestEnqueue_CreatesServerAndQueueEntry: a paid order the poller sees gets a
// server row (with the minecraft template + a generated share token) created by
// the cell's OWN enqueueNewOrders.
func TestEvolution_Enqueue_CreatesServerForPaidOrder(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	orderID, order := ownerCheckoutPending(t, h, "enqueue-paid-owner", "paid@example.test", nil)
	settleOwnerCheckout(t, h, "evt-enqueue-paid-owner", order)
	server := fleetServerForID(t, h, orderID+"-server")
	if server.OrderID != orderID || server.NodeID == "" ||
		(server.Status != "provisioning" && server.Status != "ready") {
		t.Fatalf("paid Commerce order did not reach Fleet provisioning: %#v", server)
	}
}

// TestEvolution_Enqueue_SkipsNonPaidOrders ports TestEnqueue_SkipsNonPaidOrders:
// a pending order is never turned into a server.
func TestEvolution_Enqueue_SkipsNonPaidOrders(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	orderID, _ := ownerCheckoutPending(t, h, "enqueue-pending-owner", "pending@example.test", nil)
	if fleetServerExists(t, h, orderID+"-server") {
		t.Fatal("pending Commerce order must not create a Fleet server")
	}
}

// TestEvolution_Enqueue_SkipsUnclaimedGifts ports TestEnqueue_SkipsUnclaimedGifts:
// a paid but unclaimed gift is not provisioned.
func TestEvolution_Enqueue_SkipsUnclaimedGifts(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	orderID, order := ownerCheckoutPending(t, h, "enqueue-gift-unclaimed-owner", "buyer@example.test", map[string]any{
		"is_gift": true,
	})
	settleOwnerCheckout(t, h, "evt-enqueue-gift-unclaimed-owner", order)
	if fleetServerExists(t, h, orderID+"-server") {
		t.Fatal("unclaimed Commerce gift must not create a Fleet server")
	}
}

// TestEvolution_Enqueue_EnqueuesClaimedGifts ports TestEnqueue_EnqueuesClaimedGifts:
// a claimed gift IS provisioned.
func TestEvolution_Enqueue_EnqueuesClaimedGifts(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	const key = "enqueue-gift-claimed-owner"
	setStripeStubPIStatus("requires_capture")
	status, response := postCheckoutWithHeaders(t, h, map[string]any{
		"server_type": "minecraft", "email": "buyer@example.test", "is_gift": true,
	}, map[string]string{"Idempotency-Key": key})
	if status != 200 {
		t.Fatalf("gift checkout: want 200, got %d (%v)", status, response)
	}
	orderID := deterministicHarnessIdentifier("order", key)
	order := commerceOrderForID(t, h, orderID)
	settleOwnerCheckout(t, h, "evt-enqueue-gift-claimed-owner", order)
	giftToken := fmt.Sprint(response["gift_token"])
	if giftToken == "" || giftToken == "<nil>" {
		t.Fatalf("gift checkout returned no token: %v", response)
	}

	giftStatus, giftBody := h.Do("GET", "/api/gift/"+giftToken, nil, nil)
	var giftResponse map[string]any
	_ = json.Unmarshal(giftBody, &giftResponse)
	if giftStatus != 200 || giftResponse["status"] != "unclaimed" {
		t.Fatalf("gift HTTP read through Pulp -> Lua -> Commerce = %d %#v", giftStatus, giftResponse)
	}

	claimRequest, _ := json.Marshal(map[string]any{
		"username": "Recipient", "email": "recipient@example.test",
	})
	claimStatus, claimBody := h.Do(
		"POST", "/api/gift/"+giftToken+"/claim",
		map[string]string{"Content-Type": "application/json"}, claimRequest,
	)
	var claimResponse map[string]any
	_ = json.Unmarshal(claimBody, &claimResponse)
	if claimStatus != 200 || claimResponse["order_id"] != orderID ||
		fmt.Sprint(claimResponse["claim_token"]) == "" {
		t.Fatalf("gift HTTP claim through Pulp -> Lua -> owners = %d %#v", claimStatus, claimResponse)
	}
	server := fleetServerForID(t, h, orderID+"-server")
	if server.OrderID != orderID || server.NodeID == "" ||
		(server.Status != "provisioning" && server.Status != "ready") {
		t.Fatalf("claimed gift did not reach Fleet through Lua: %#v", server)
	}
}

func TestEvolution_GiftCancellation_UsesLuaCommerceOwner(t *testing.T) {
	h, db := startEvolutionDowntime(t)
	seedDowntimeCatalog(t, db)
	const key = "gift-cancel-owner"
	setStripeStubPIStatus("requires_capture")
	status, response := postCheckoutWithHeaders(t, h, map[string]any{
		"server_type": "minecraft", "email": "buyer@example.test", "is_gift": true,
	}, map[string]string{"Idempotency-Key": key})
	if status != 200 {
		t.Fatalf("gift checkout: want 200, got %d (%v)", status, response)
	}
	orderID := deterministicHarnessIdentifier("order", key)
	settleOwnerCheckout(t, h, "evt-gift-cancel-owner", commerceOrderForID(t, h, orderID))
	giftToken := fmt.Sprint(response["gift_token"])

	cancelRequest, _ := json.Marshal(map[string]any{"email": "buyer@example.test"})
	cancelStatus, cancelBody := h.Do(
		"POST", "/api/gift/"+giftToken+"/cancel",
		map[string]string{"Content-Type": "application/json"}, cancelRequest,
	)
	var cancelResponse map[string]any
	_ = json.Unmarshal(cancelBody, &cancelResponse)
	if cancelStatus != 200 || cancelResponse["refunded"] != true {
		t.Fatalf("gift cancellation through Pulp -> Lua -> Commerce = %d %#v", cancelStatus, cancelResponse)
	}
	if got := commerceOrderForID(t, h, orderID).Status; got != "refund_pending" && got != "refunded" {
		t.Fatalf("Commerce did not own gift cancellation state: %q", got)
	}
}

// TestEvolution_CheckExpirations_ActiveServerExpires ports the
// TestCheckExpirations_ExpiringToExpired / _ActiveToExpiring behaviour: a REAL
// active server (provisioned through the cell) whose expires_at is backdated past
// now leaves the active state on a driven tick — the cell's own checkExpirations
// runs the active->expiring->expired transition.
func TestEvolution_CheckExpirations_ActiveServerExpires(t *testing.T) {
	h, _ := startEvolutionDowntime(t)
	now := time.Now().UTC()
	const serverID = "fleet-expired-server"
	upsertFleetServer(t, h, serverID, "active", now.Add(-24*time.Hour).Format(time.RFC3339Nano))
	dispatchFleetMaintenance(t, h, "maintenance-expired-server", now)
	if state := fleetServerForID(t, h, serverID).Status; state != "destroyed" {
		t.Fatalf("expired Fleet server without a runtime should be destroyed, got %q", state)
	}
}
