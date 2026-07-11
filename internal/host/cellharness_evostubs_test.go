package host

// In-memory stub capabilities for the Evolution cell harness.
//
// Evolution declares payment.stripe, storage.s3, spawn.docker, and workers
// in addition to the light-cell set. Those four caps' real exts
// (Pulp-ext-{stripe,s3,docker,workers}) talk to live backends (Stripe API,
// R2, the Docker daemon, the host worker pool) that this harness cannot and
// should not reach. The audit fixes we pin here are decided in the cell's
// HTTP handlers BEFORE (or instead of) any real backend call, so canned /
// no-op host-fn bindings are sufficient to get the cell to Init and to drive
// the testable endpoints.
//
// These are wired into the harness via CellHarnessConfig.CapabilityOverrides,
// so they only replace the caps for the Evolution harness instances — sibling
// tests keep whatever ext.All() registered. Each capability binds EXACTLY the
// host-import names the Evolution wasm references (discovered by dumping the
// module's imported functions); an unbound import would fail wazero
// instantiation, and an extra export is harmless.
//
// Controllable behavior (read by the test):
//   - stripeStubPIStatus governs the status returned by
//     stripe_payment_intent_{create,get}. The pool /confirm audit fix gates
//     the pool credit on the PI being in "requires_capture"; flipping this
//     var lets the test prove both the accept and the reject branch.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/vmihailenco/msgpack/v5"
)

// stripeStubPIStatus is the status the stripe stub stamps on every
// PaymentIntent it creates and returns. Default "requires_capture" so the
// pool happy-path (create→contribute→confirm) clears the audit gate; the
// reject test flips it to a non-requires_capture value. Guarded by a mutex
// because the harness pump goroutine and the test goroutine both touch it.
var (
	stripeStubMu       sync.Mutex
	stripeStubPIStatus = "requires_capture"
)

func setStripeStubPIStatus(s string) {
	stripeStubMu.Lock()
	stripeStubPIStatus = s
	stripeStubMu.Unlock()
}

func getStripeStubPIStatus() string {
	stripeStubMu.Lock()
	defer stripeStubMu.Unlock()
	return stripeStubPIStatus
}

// ---- Layer D: opt-in webhook-signature verification ----------------------
//
// The stripe stub's webhook verify returns "valid" (code 0) for EVERY inbound
// signature by default, so the existing checkout->webhook->provision proofs can
// drive the cell with a placeholder Stripe-Signature. That default makes the
// two native signature-reject tests (RejectsBadSignature / RejectsMissing
// signature) unreachable — nothing the harness sends is ever rejected.
//
// stripeStubVerifySig makes the stub ACTUALLY verify the Stripe-style HMAC when
// a test opts in (default false preserves every existing test's behaviour). The
// host-side ext owns the signing secret in production; in-harness the STUB owns
// it, so a test that opts in signs its payload with stripeStubWebhookSecret and
// the stub validates against the same secret — a bad or missing signature then
// returns code 6 (ErrStripeSignatureInvalid), which the cell's real webhook
// handler surfaces as HTTP 400, exactly the native contract.
const stripeStubWebhookSecret = "whsec_harness_test"

var (
	stripeStubVerifySigMu sync.Mutex
	stripeStubVerifySig   = false
)

func setStripeStubVerifySig(on bool) {
	stripeStubVerifySigMu.Lock()
	stripeStubVerifySig = on
	stripeStubVerifySigMu.Unlock()
}

func getStripeStubVerifySig() bool {
	stripeStubVerifySigMu.Lock()
	defer stripeStubVerifySigMu.Unlock()
	return stripeStubVerifySig
}

// verifyStripeStubSignature reimplements Stripe's HMAC-SHA256 scheme: the header
// is "t=<unix>,v1=<hex-hmac>" and the signed payload is "<t>.<raw-body>". An
// empty header (missing signature) or any mismatch is invalid.
func verifyStripeStubSignature(header string, payload []byte) bool {
	if header == "" {
		return false
	}
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			ts = kv[1]
		case "v1":
			sig = kv[1]
		}
	}
	if ts == "" || sig == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(stripeStubWebhookSecret))
	mac.Write([]byte(ts + "." + string(payload)))
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(sig))
}

// writeStubMsgpack marshals v and hands it back to the guest using the same
// pulp_alloc + (ptrOut,lenOut) contract every real ext uses (mirrors
// Pulp-ext-stripe.writeMsgpackResponse). Returns the ext error code.
func writeStubMsgpack(ctx context.Context, m api.Module, v any, respPtrOut, respLenOut uint32) uint32 {
	encoded, err := msgpack.Marshal(v)
	if err != nil {
		return 5
	}
	allocFn := m.ExportedFunction("pulp_alloc")
	if allocFn == nil {
		return 7
	}
	var ptr uint32
	if len(encoded) > 0 {
		res, err := allocFn.Call(ctx, uint64(len(encoded)))
		if err != nil || len(res) == 0 || res[0] == 0 {
			return 7
		}
		ptr = uint32(res[0])
		if !m.Memory().Write(ptr, encoded) {
			return 8
		}
	}
	if !m.Memory().WriteUint32Le(respPtrOut, ptr) {
		return 8
	}
	if !m.Memory().WriteUint32Le(respLenOut, uint32(len(encoded))) {
		return 8
	}
	return 0
}

func readStubMsgpack(m api.Module, reqPtr, reqLen uint32, out any) bool {
	if reqLen == 0 {
		return false
	}
	data, ok := m.Memory().Read(reqPtr, reqLen)
	if !ok {
		return false
	}
	return msgpack.Unmarshal(data, out) == nil
}

// ---- stripe stub ---------------------------------------------------------
//
// Mirrors the wire shapes in Pulp-ext-stripe / Fiber/pulp/stripe for the
// handful of calls the pinned paths drive. payment_intent_{create,get} carry
// real-ish responses (a stable id + the controllable status) so the pool
// flow can be driven end-to-end; everything else returns a benign canned
// object or ok so the cell never wedges on a missing binding.

type stubPaymentIntent struct {
	ID            string            `msgpack:"id"`
	Status        string            `msgpack:"status"`
	Amount        int64             `msgpack:"amount"`
	Currency      string            `msgpack:"currency"`
	ClientSecret  string            `msgpack:"client_secret,omitempty"`
	ReceiptEmail  string            `msgpack:"receipt_email,omitempty"`
	CaptureMethod string            `msgpack:"capture_method,omitempty"`
	LatestCharge  string            `msgpack:"latest_charge,omitempty"`
	LastErrorMsg  string            `msgpack:"last_error,omitempty"`
	LastErrorCode string            `msgpack:"last_error_code,omitempty"`
	Metadata      map[string]string `msgpack:"metadata"`
}

func stripeStubCapability() ext.Capability {
	bind := func(b wazero.HostModuleBuilder, _ ext.Cell) error {
		// payment_intent_create — mint a deterministic PI id from the
		// idempotency key (so create→get is consistent) carrying the
		// controllable status.
		piCreate := func(ctx context.Context, m api.Module, reqPtr, reqLen, op, ol uint32) uint32 {
			var req struct {
				AmountCents   int64  `msgpack:"amount_cents"`
				Currency      string `msgpack:"currency"`
				CaptureMethod string `msgpack:"capture_method,omitempty"`
			}
			_ = readStubMsgpack(m, reqPtr, reqLen, &req)
			pi := stubPaymentIntent{
				ID:            "pi_stub_" + fmt.Sprintf("%d", req.AmountCents),
				Status:        getStripeStubPIStatus(),
				Amount:        req.AmountCents,
				Currency:      req.Currency,
				ClientSecret:  "pi_stub_secret",
				CaptureMethod: req.CaptureMethod,
				Metadata:      map[string]string{},
			}
			return writeStubMsgpack(ctx, m, pi, op, ol)
		}
		piGet := func(ctx context.Context, m api.Module, reqPtr, reqLen, op, ol uint32) uint32 {
			var req struct {
				ID string `msgpack:"id"`
			}
			_ = readStubMsgpack(m, reqPtr, reqLen, &req)
			pi := stubPaymentIntent{
				ID:            req.ID,
				Status:        getStripeStubPIStatus(),
				Amount:        1200,
				Currency:      "usd",
				CaptureMethod: "manual",
				Metadata:      map[string]string{},
			}
			return writeStubMsgpack(ctx, m, pi, op, ol)
		}
		// generic ok-with-empty-object for the create/get-shaped fns we don't
		// drive (4 ptr args): return an empty msgpack object.
		okObj := func(ctx context.Context, m api.Module, _, _, op, ol uint32) uint32 {
			return writeStubMsgpack(ctx, m, map[string]any{}, op, ol)
		}
		// webhook_verify is a 2-arg fn returning a bare code (0=valid,
		// 6=ErrStripeSignatureInvalid). Default: always valid so existing
		// proofs can drive the webhook with a placeholder signature. When a
		// test opts into stripeStubVerifySig, actually verify the Stripe-style
		// HMAC over the WebhookVerifyRequest{payload, signature_header}.
		verify := func(_ context.Context, m api.Module, reqPtr, reqLen uint32) uint32 {
			if !getStripeStubVerifySig() {
				return 0
			}
			var req struct {
				Payload         []byte `msgpack:"payload"`
				SignatureHeader string `msgpack:"signature_header"`
			}
			_ = readStubMsgpack(m, reqPtr, reqLen, &req)
			if verifyStripeStubSignature(req.SignatureHeader, req.Payload) {
				return 0
			}
			return 6
		}

		b.NewFunctionBuilder().WithFunc(piCreate).Export("stripe_payment_intent_create")
		b.NewFunctionBuilder().WithFunc(piGet).Export("stripe_payment_intent_get")
		b.NewFunctionBuilder().WithFunc(okObj).Export("stripe_payment_intent_capture")
		b.NewFunctionBuilder().WithFunc(okObj).Export("stripe_payment_intent_cancel")
		b.NewFunctionBuilder().WithFunc(okObj).Export("stripe_refund_create")
		b.NewFunctionBuilder().WithFunc(okObj).Export("stripe_customer_create")
		b.NewFunctionBuilder().WithFunc(okObj).Export("stripe_invoice_create")
		b.NewFunctionBuilder().WithFunc(okObj).Export("stripe_invoice_finalize")
		b.NewFunctionBuilder().WithFunc(okObj).Export("stripe_invoice_item_create")
		b.NewFunctionBuilder().WithFunc(okObj).Export("stripe_balance_get")
		b.NewFunctionBuilder().WithFunc(okObj).Export("stripe_coupon_create")
		b.NewFunctionBuilder().WithFunc(okObj).Export("stripe_promotion_code_create")
		b.NewFunctionBuilder().WithFunc(okObj).Export("stripe_promotion_code_lookup")
		b.NewFunctionBuilder().WithFunc(verify).Export("stripe_webhook_verify")
		return nil
	}
	return ext.Capability{Name: "payment.stripe", Register: bind, Stub: bind}
}

// ---- s3 stub -------------------------------------------------------------
//
// presign / presign_put return a canned URL so /upload-* can register and
// hand out a slot; head/get/list return empty; mutating ops return ok. None
// of the pinned assertions depend on real R2 bytes — the finalize DoS gate
// trips on the rate limiter and the cheap-reject DB lookup, both BEFORE any
// S3 fetch.

func s3StubCapability() ext.Capability {
	bind := func(b wazero.HostModuleBuilder, _ ext.Cell) error {
		presign := func(ctx context.Context, m api.Module, _, _, op, ol uint32) uint32 {
			return writeStubMsgpack(ctx, m, map[string]string{"url": "https://stub.r2.local/presigned"}, op, ol)
		}
		emptyObj := func(ctx context.Context, m api.Module, _, _, op, ol uint32) uint32 {
			return writeStubMsgpack(ctx, m, map[string]any{}, op, ol)
		}
		ok2 := func(_ context.Context, _ api.Module, _, _ uint32) uint32 { return 0 }

		b.NewFunctionBuilder().WithFunc(presign).Export("s3_presign")
		b.NewFunctionBuilder().WithFunc(presign).Export("s3_presign_put")
		b.NewFunctionBuilder().WithFunc(emptyObj).Export("s3_head")
		b.NewFunctionBuilder().WithFunc(emptyObj).Export("s3_get")
		b.NewFunctionBuilder().WithFunc(emptyObj).Export("s3_list")
		b.NewFunctionBuilder().WithFunc(emptyObj).Export("s3_put_multipart_init")
		b.NewFunctionBuilder().WithFunc(emptyObj).Export("s3_put_multipart_part")
		b.NewFunctionBuilder().WithFunc(ok2).Export("s3_put")
		b.NewFunctionBuilder().WithFunc(ok2).Export("s3_copy")
		b.NewFunctionBuilder().WithFunc(ok2).Export("s3_delete")
		b.NewFunctionBuilder().WithFunc(ok2).Export("s3_put_multipart_complete")
		b.NewFunctionBuilder().WithFunc(ok2).Export("s3_put_multipart_abort")
		return nil
	}
	return ext.Capability{Name: "storage.s3", Register: bind, Stub: bind}
}

// ---- docker stub ---------------------------------------------------------
//
// spawn.docker is declared but never reached on a pinned HTTP path (the
// audit fixes here decide in middleware / pre-backend handler logic). Bind
// the four docker imports the Evolution wasm references as no-ops returning
// an error code so any stray call degrades gracefully rather than wedging.

func dockerStubCapability() ext.Capability {
	bind := func(b wazero.HostModuleBuilder, _ ext.Cell) error {
		nop4 := func(_ context.Context, _ api.Module, _, _, _, _ uint32) uint32 { return 4 }
		nop2 := func(_ context.Context, _ api.Module, _, _ uint32) uint32 { return 4 }
		b.NewFunctionBuilder().WithFunc(nop4).Export("docker_exec")
		b.NewFunctionBuilder().WithFunc(nop4).Export("docker_files_read")
		b.NewFunctionBuilder().WithFunc(nop2).Export("docker_files_write")
		b.NewFunctionBuilder().WithFunc(nop2).Export("docker_restart")
		return nil
	}
	return ext.Capability{Name: "spawn.docker", Register: bind, Stub: bind}
}

// ---- workers stub --------------------------------------------------------
//
// workers backs Evolution's async http.fetch queue (emails, status push,
// world archival). Those run on poller ticks OFF the pinned HTTP paths.
// submit returns 0 (a benign zero task id) and result returns "not ready"
// so the consume loops simply find nothing — no panic, no wedge.

func workersStubCapability() ext.Capability {
	bind := func(b wazero.HostModuleBuilder, _ ext.Cell) error {
		submit := func(_ context.Context, _ api.Module, _, _ uint32) uint32 { return 0 }
		// statusPending (0): consume loops treat every submitted task as
		// still in-flight and never try to decode a (non-existent) result
		// body — no panic on the async paths the pinned tests don't exercise.
		result := func(_ context.Context, _ api.Module, _, _, _ uint32) uint32 { return 0 }
		b.NewFunctionBuilder().WithFunc(submit).Export("workers_submit")
		b.NewFunctionBuilder().WithFunc(submit).Export("workers_submit_fire")
		b.NewFunctionBuilder().WithFunc(result).Export("workers_result")
		return nil
	}
	return ext.Capability{Name: "workers", Register: bind, Stub: bind}
}

// ---- sibling-call stub ---------------------------------------------------
//
// Evolution `consumes = ["sessions"]`, so the Evolution wasm imports
// pulp_call (Fiber/pulp/sibling). In production that import is bound by the
// run package's pulp.sibling capability, which routes B->A sibling calls. The
// internal/host harness loads a single cell with no sibling registry, so we
// bind pulp_call to a stub returning code 4 ("call failed"): gene discovery
// degrades gracefully (main.go logs the warning and boots; gene-owned routes
// 404) while every pinned route here — pool, finalize, internal — is engine-
// owned and unaffected.

func siblingStubCapability() ext.Capability {
	bind := func(b wazero.HostModuleBuilder, _ ext.Cell) error {
		pulpCall := func(_ context.Context, _ api.Module,
			_, _, _, _, _, _, _, _ uint32) uint32 {
			return 4
		}
		b.NewFunctionBuilder().WithFunc(pulpCall).Export("pulp_call")
		return nil
	}
	return ext.Capability{Name: "pulp.sibling", Register: bind, Stub: bind}
}

// ---- http.outbound stub --------------------------------------------------
//
// transport.http.outbound's real ext makes live network calls behind a
// deny-all-private SSRF guard, so it can't reach an in-test sidecar. This stub
// replaces ONLY the outbound capability (inbound stays real so h.Do still
// drives the cell) and serves canned sidecar responses keyed by URL path. It
// lets the P1-7 generic-proxy harness drive the cell's outbound fetches
// (/capabilities at boot, then /versions //mods //client-mods //preflight)
// without a real sidecar. Bodies are stable per path so legacy and generic
// routes proxying the SAME path get byte-identical responses.

func cannedSidecarResponse(rawURL string) (uint32, []byte) {
	path := rawURL
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	switch {
	case strings.HasSuffix(path, "/capabilities"):
		return 200, []byte(`{"game":"minecraft","endpoints":[` +
			`{"id":"versions","method":"GET","core":"/api/:game/versions","sidecar":"/versions","cache_s":300,"forward_query":true},` +
			`{"id":"mods","method":"GET","core":"/api/:game/mods","sidecar":"/mods","cache_s":300,"forward_query":true},` +
			`{"id":"client-mods","method":"POST","core":"/api/:game/client-mods","sidecar":"/client-mods","ratelimit":"heavy","cache":"body-hash","max_body":65536},` +
			`{"id":"preflight-jre","method":"POST","core":"/api/:game/preflight/jre","sidecar":"/preflight/jre","ratelimit":"heavy","cache":"body-hash","max_body":65536}` +
			`]}`)
	case strings.HasSuffix(path, "/game-meta"):
		return 200, []byte(`{"game":"minecraft","eula":{"required":true,"name":"Minecraft EULA","url":"https://aka.ms/MinecraftEULA"}}`)
	case strings.HasSuffix(path, "/versions"):
		return 200, []byte(`{"versions":["1.21.4","1.21.3"],"latest":"1.21.4","crossplay":true}`)
	case strings.HasSuffix(path, "/mods"):
		return 200, []byte(`{"mods":[{"id":"fabric-api","name":"Fabric API"}]}`)
	case strings.HasSuffix(path, "/client-mods"):
		return 200, []byte(`{"client_mods":[{"id":"sodium","url":"https://stub/sodium.jar"}]}`)
	case strings.HasSuffix(path, "/preflight/jre"):
		return 200, []byte(`{"jre":"17","ok":true}`)
	case strings.HasSuffix(path, "/deps/health"):
		return 200, []byte(`{"overall":"ok","deps":[]}`)
	default:
		return 200, []byte(`{}`)
	}
}

func httpOutboundStubCapability() ext.Capability {
	bind := func(b wazero.HostModuleBuilder, _ ext.Cell) error {
		fetch := func(ctx context.Context, m api.Module, reqPtr, reqLen, op, ol uint32) uint32 {
			var req struct {
				URL string `msgpack:"url"`
			}
			_ = readStubMsgpack(m, reqPtr, reqLen, &req)
			status, body := cannedSidecarResponse(req.URL)
			resp := struct {
				Status  uint32            `msgpack:"status"`
				Headers map[string]string `msgpack:"headers"`
				Body    []byte            `msgpack:"body"`
			}{Status: status, Headers: map[string]string{"content-type": "application/json"}, Body: body}
			return writeStubMsgpack(ctx, m, resp, op, ol)
		}
		// Streaming fetch is unused by the proxy endpoints; bind the imports so
		// instantiation succeeds, returning a benign error/no-op.
		begin := func(_ context.Context, _ api.Module, _, _, _, _ uint32) uint32 { return 5 }
		read := func(_ context.Context, _ api.Module, _, _, _, _, _ uint32) uint32 { return 5 }
		closeFn := func(_ context.Context, _ api.Module, _, _ uint32) uint32 { return 0 }
		b.NewFunctionBuilder().WithFunc(fetch).Export("http_fetch")
		b.NewFunctionBuilder().WithFunc(begin).Export("http_fetch_begin")
		b.NewFunctionBuilder().WithFunc(read).Export("http_fetch_read")
		b.NewFunctionBuilder().WithFunc(closeFn).Export("http_fetch_close")
		return nil
	}
	return ext.Capability{Name: "transport.http.outbound", Register: bind, Stub: bind}
}

// evolutionStubOverrides is the full override set the Evolution harness wires.
func evolutionStubOverrides() []ext.Capability {
	return []ext.Capability{
		stripeStubCapability(),
		s3StubCapability(),
		dockerStubCapability(),
		workersStubCapability(),
		siblingStubCapability(),
		httpOutboundStubCapability(),
	}
}
