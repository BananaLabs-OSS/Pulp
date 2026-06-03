package host

// Cell HTTP harness against the REAL deployed Hytale-Auth cell (OAuth
// device-code client that mints live Hytale server credentials), pinning
// its audit fixes.
//
// Hytale-Auth declares transport.http.inbound + transport.http.outbound +
// storage.fs. On Init its bootstrap runs the OAuth device-code flow, which
// makes an OUTBOUND fetch to oauth.accounts.hytale.com. The real ext-http
// outbound would attempt a live network call (and the egress guard / lack
// of network makes it fail), so bootstrap would error and Init would never
// complete. We therefore swap transport.http.outbound for a deterministic
// in-memory stub (CellHarnessConfig.CapabilityOverrides) that answers the
// device-auth endpoint with a canned RFC-8628 response. That drives the
// cell into setupMode=true so BOTH audit assertions are reachable:
//
//   go-hytale-auth-r1 HIGH — GET /tokens was UNAUTHENTICATED, minting live
//   bearer credentials to any caller. The enforce-when-set fix gates it on
//   X-Service-Token when SERVICE_TOKEN is set. Pinned: SERVICE_TOKEN set +
//   no/wrong X-Service-Token -> 401 (decided in middleware before any mint).
//
//   go-hytale-auth-r1 MED — the device user_code (a short-lived shared
//   secret per RFC 8628) must NOT be disclosed on the open GET / route.
//   Fix: status handler returns "See cell logs" instead of the code. Pinned:
//   during setupMode the GET / body never contains the canned user_code (nor
//   the pre-filled verification_uri_complete).

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/abi"
	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// hytaleAuthSourceDir: Pulp/internal/host -> ../../../Hytale-Auth/pulp-cell.
func hytaleAuthSourceDir() string {
	return filepath.Join("..", "..", "..", "Hytale-Auth", "pulp-cell")
}

// The canned device-auth values the stub returns. The test asserts the cell
// never leaks these on the open status route.
const (
	hytaleStubUserCode    = "WXYZ-7777"
	hytaleStubVerifyURI   = "https://verify.example.test/device?code=WXYZ-7777"
	hytaleStubDeviceCode  = "device-code-abc123"
)

// stubOutboundCapability builds an in-memory transport.http.outbound that
// answers Hytale-Auth's device-auth POST with a canned RFC-8628 body and
// 503s anything else (so the poll never "authorizes" and setupMode stays
// true). It mirrors ext-http's httpOutboundRegister ABI: decode the
// msgpack HTTPFetchRequest, encode an HTTPResponse, hand it back via
// pulp_alloc. Local to this harness instance via CapabilityOverrides — the
// real outbound is untouched for sibling tests.
func stubOutboundCapability() ext.Capability {
	register := func(b wazero.HostModuleBuilder, _ ext.Cell) error {
		b.NewFunctionBuilder().
			WithFunc(func(ctx context.Context, m api.Module, reqPtr, reqLen, respPtrOut, respLenOut uint32) uint32 {
				if reqLen == 0 {
					return 1
				}
				data, ok := m.Memory().Read(reqPtr, reqLen)
				if !ok {
					return 2
				}
				req, err := abi.DecodeHTTPFetchRequest(data)
				if err != nil {
					return 3
				}

				var resp abi.HTTPResponse
				switch {
				case strings.Contains(req.URL, "/oauth2/device/auth"):
					resp = abi.HTTPResponse{
						Status:  200,
						Headers: map[string]string{"Content-Type": "application/json"},
						Body: []byte(`{"device_code":"` + hytaleStubDeviceCode +
							`","user_code":"` + hytaleStubUserCode +
							`","verification_uri_complete":"` + hytaleStubVerifyURI +
							`","interval":5}`),
					}
				default:
					// Token poll / refresh / session: keep the cell in setupMode
					// (authorization_pending) so the user_code-disclosure path is
					// the one exercised, and no live mint is attempted.
					resp = abi.HTTPResponse{
						Status:  200,
						Headers: map[string]string{"Content-Type": "application/json"},
						Body:    []byte(`{"error":"authorization_pending"}`),
					}
				}

				respBytes, err := abi.EncodeHTTPResponse(resp)
				if err != nil {
					return 5
				}
				allocFn := m.ExportedFunction("pulp_alloc")
				if allocFn == nil {
					return 6
				}
				results, err := allocFn.Call(ctx, uint64(len(respBytes)))
				if err != nil || len(results) == 0 {
					return 7
				}
				respPtr := uint32(results[0])
				if respPtr == 0 {
					return 7
				}
				if !m.Memory().Write(respPtr, respBytes) {
					return 8
				}
				if !m.Memory().WriteUint32Le(respPtrOut, respPtr) {
					return 8
				}
				if !m.Memory().WriteUint32Le(respLenOut, uint32(len(respBytes))) {
					return 8
				}
				return 0
			}).
			Export("http_fetch")

		// Hytale-Auth only uses the unary http_fetch; stub the streaming
		// trio so the import set still resolves if the module references them.
		for _, name := range []string{"http_fetch_begin", "http_fetch_read", "http_fetch_close"} {
			b.NewFunctionBuilder().
				WithFunc(func(_ context.Context, _ api.Module, _, _, _, _, _ uint32) uint32 { return 99 }).
				Export(name)
		}
		return nil
	}
	return ext.Capability{Name: "transport.http.outbound", Register: register, Stub: register}
}

func startHytaleAuth(t *testing.T, serviceToken string) *CellHarness {
	// service_token is delivered via the manifest [config] table (the PRIMARY
	// source) — NOT an env var. The Pulp host forwards only HTTP_PORT/TZ into
	// the WASI sandbox, so an env-only token would read empty inside the cell
	// and the /tokens auth gate could never engage. (See the resolveServiceToken
	// audit fix in Hytale-Auth/pulp-cell/main.go.)
	return StartCellHTTP(t, CellHarnessConfig{
		SourceDir:           hytaleAuthSourceDir(),
		Name:                "hytale-auth",
		Capabilities:        []string{"transport.http.inbound", "transport.http.outbound", "storage.fs"},
		Config:              map[string]any{"service_token": serviceToken},
		CapabilityOverrides: []ext.Capability{stubOutboundCapability()},
	})
}

func TestHytaleAuth_TokensRequiresAuthWhenTokenSet(t *testing.T) {
	const token = "hytale-harness-secret"
	h := startHytaleAuth(t, token)

	// /health is always open (proves the harness reached the cell).
	if status, b := h.Do("GET", "/health", nil, nil); status != 200 {
		t.Fatalf("GET /health: want 200, got %d (%s)", status, b)
	}

	// Credential-minting /tokens must be gated when SERVICE_TOKEN is set.
	if status, _ := h.Do("GET", "/tokens", nil, nil); status != 401 {
		t.Fatalf("GET /tokens without X-Service-Token: want 401, got %d", status)
	}
	if status, _ := h.Do("GET", "/tokens", map[string]string{"X-Service-Token": "wrong"}, nil); status != 401 {
		t.Fatalf("GET /tokens with wrong token: want 401, got %d", status)
	}
}

func TestHytaleAuth_StatusDoesNotDiscloseUserCode(t *testing.T) {
	// Token empty here: we are exercising the OPEN GET / route, and the
	// concern is information disclosure, not auth. The stubbed device flow
	// puts the cell in setupMode with a known user_code; GET / must not leak
	// it (nor the pre-filled verification URI).
	h := startHytaleAuth(t, "")

	status, body := h.Do("GET", "/", nil, nil)
	if status != 200 {
		t.Fatalf("GET /: want 200, got %d (%s)", status, body)
	}
	s := string(body)
	if strings.Contains(s, hytaleStubUserCode) {
		t.Fatalf("GET / leaked the device user_code %q: %s", hytaleStubUserCode, s)
	}
	if strings.Contains(s, hytaleStubVerifyURI) {
		t.Fatalf("GET / leaked the verification_uri_complete %q: %s", hytaleStubVerifyURI, s)
	}
}
