package host

// LAYER D — PORT of the two Stripe-signature reject tests
// (Evolution/internal/router/stripe_webhook_test.go: TestWebhook_RejectsBad
// Signature, TestWebhook_RejectsMissingSignature), driven THROUGH the real
// Evolution cell's POST /api/webhooks/stripe.
//
// The cell verifies the webhook via Fiber/pulp/stripe.VerifyWebhook -> the host
// `stripe_webhook_verify` import. The shared harness stub returns "valid" for
// every signature by default, which makes these two rejects unreachable. Rather
// than flag NEEDS-LIVE, we wire the stub to actually verify: setStripeStubVerify
// Sig(true) turns on real Stripe-style HMAC verification against
// stripeStubWebhookSecret, so a bad/missing signature is rejected through the
// cell's REAL webhook handler (code 6 -> HTTP 400) — and a correctly-signed
// payload still passes (the positive control), proving the gate is two-sided,
// not always-reject.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

// signStripeHarness produces a valid Stripe-Signature header for payload using
// the stub's webhook secret — mirrors the native signStripePayload helper.
func signStripeHarness(payload []byte) string {
	ts := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte(stripeStubWebhookSecret))
	mac.Write([]byte(fmt.Sprintf("%d.%s", ts, string(payload))))
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

// TestEvolution_Webhook_RejectsMissingSignature ports
// TestWebhook_RejectsMissingSignature: no Stripe-Signature header -> 400.
func TestEvolution_Webhook_RejectsMissingSignature(t *testing.T) {
	setStripeStubVerifySig(true)
	t.Cleanup(func() { setStripeStubVerifySig(false) })

	h, _ := startEvolutionDowntime(t)
	// No Stripe-Signature header at all.
	status, b := h.Do("POST", "/api/webhooks/stripe",
		map[string]string{"Content-Type": "application/json"}, []byte(`{}`))
	if status != 400 {
		t.Fatalf("missing signature: want 400, got %d (%s)", status, b)
	}
}

// TestEvolution_Webhook_RejectsBadSignature ports TestWebhook_RejectsBadSignature:
// a signature that does not validate against the payload -> 400.
func TestEvolution_Webhook_RejectsBadSignature(t *testing.T) {
	setStripeStubVerifySig(true)
	t.Cleanup(func() { setStripeStubVerifySig(false) })

	h, _ := startEvolutionDowntime(t)
	status, b := h.Do("POST", "/api/webhooks/stripe",
		map[string]string{"Content-Type": "application/json", "Stripe-Signature": "t=123,v1=deadbeef"},
		[]byte(`{"type":"payment_intent.succeeded"}`))
	if status != 400 {
		t.Fatalf("bad signature: want 400, got %d (%s)", status, b)
	}
}

// TestEvolution_Webhook_AcceptsValidSignature is the positive control: with
// verification ON, a correctly-signed payload passes the signature gate (the
// cell processes it — an unknown-order payment_intent.succeeded is acknowledged
// 200, never 400). Proves the reject path is a real two-sided verify, not an
// always-reject.
func TestEvolution_Webhook_AcceptsValidSignature(t *testing.T) {
	setStripeStubVerifySig(true)
	t.Cleanup(func() { setStripeStubVerifySig(false) })

	h, _ := startEvolutionDowntime(t)
	payload := []byte(`{"id":"evt-sig-ok","type":"payment_intent.succeeded","data":{"object":{"id":"pi_unknown_sig"}}}`)
	status, b := h.Do("POST", "/api/webhooks/stripe",
		map[string]string{"Content-Type": "application/json", "Stripe-Signature": signStripeHarness(payload)},
		payload)
	if status == 400 {
		t.Fatalf("valid signature was rejected: got 400 (%s)", b)
	}
	if status != 200 {
		t.Fatalf("valid signature, unknown order: want 200, got %d (%s)", status, b)
	}
}
