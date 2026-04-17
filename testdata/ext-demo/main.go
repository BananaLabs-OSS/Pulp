// ext-demo — minimal plugin that declares storage.s3 + payment.stripe
// capabilities. Proves the deployment binary successfully links both
// extensions and the plugin can instantiate against them.
//
// We do NOT actually call the host imports here (that would require
// real AWS / Stripe credentials). The test is: does the plugin load
// under a Pulp binary that links both extensions? Under a Pulp binary
// that links NEITHER, the host should stub the imports with error 99
// wrappers and the plugin still loads; under a Pulp binary that links
// ONE but declares BOTH, the undeclared one gets stubbed too.
package main

import "unsafe"

func main() {}

//go:wasmimport pulp s3_put
func hostS3Put(ptr, ln uint32) uint32

//go:wasmimport pulp stripe_webhook_verify
func hostStripeWebhookVerify(ptr, ln uint32) uint32

//go:wasmexport pulp_alloc
func pulpAlloc(size uint32) uint32 {
	if size == 0 {
		return 0
	}
	buf := make([]byte, size)
	return uint32(uintptr(unsafe.Pointer(&buf[0])))
}

//go:wasmexport pulp_free
func pulpFree(_, _ uint32) {}

//go:wasmexport pulp_init
func pulpInit(_, _ uint32) int32 {
	// Reference the host imports so DCE doesn't strip them. We do not
	// invoke them — no real credentials in this test.
	_ = hostS3Put
	_ = hostStripeWebhookVerify
	return 0
}

//go:wasmexport pulp_step
func pulpStep(_, _ uint32) int32 { return 0 }

//go:wasmexport pulp_shutdown
func pulpShutdown() int32 { return 0 }
