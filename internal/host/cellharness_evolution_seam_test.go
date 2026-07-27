package host

// P1-7 game-capability seam harness (DESIGN-GAME-CAPABILITY-SEAM).
//
// Proves the generic declaration-driven proxy (/api/:game/...) returns the
// exact response produced by the in-host minecraft-resolver WASM package. The
// retired standalone sidecar is no longer a source of truth.

import (
	"context"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

// startEvolutionWithSidecar boots the cell with a (stub-served) minecraft
// sidecar configured, so GameSidecarURLs["minecraft"] is set and both the
// hardcoded MC routes and the generic /capabilities-driven routes register.
func startEvolutionWithSidecar(t *testing.T) *CellHarness {
	return StartEvolutionApplicationHTTP(t, CellHarnessConfig{
		SourceDir: evolutionSourceDir(),
		Name:      "evolution",
		Capabilities: []string{
			"transport.http.inbound",
			"transport.http.outbound",
			"transport.sse",
			"storage.fs",
			"storage.sqlite",
			"storage.s3",
			"payment.stripe",
			"workers",
			"entropy.read",
		},
		Config: map[string]any{
			"internal_secret":      "",
			"frontend_url":         "https://sessions.gg",
			"max_servers":          12,
			"poll_interval":        "15s",
			"server_lifetime":      "336h",
			"refund_threshold":     "10m",
			"db_dialect":           "",
			"r2_account_id":        "stub-account",
			"r2_access_key_id":     "stub-key",
			"r2_secret_access_key": "stub-secret",
			"r2_bucket":            "stub-bucket",
			// Stub-served sidecar — the http.outbound stub answers /capabilities
			// + the proxied endpoints regardless of host, so any URL works.
			"minecraft_sidecar_url": "http://mc-sidecar.stub",
		},
		CapabilityOverrides: evolutionStubOverrides(),
	})
}

// TestEvolution_GenericProxyServes is the Phase 2 gate (after the hardcoded MC
// handlers were deleted): each generic /api/:game/... route must register from
// the sidecar's /capabilities declaration and proxy through the (stub-served)
// sidecar body unchanged. The legacy /api/mc-versions etc. no longer exist.
func TestEvolution_GenericProxyServes(t *testing.T) {
	h := startEvolutionWithSidecar(t)
	warmEvolution(t, h)

	cases := []struct {
		name          string
		method        string
		path          string
		body          []byte
		resolverPath  string
		resolverQuery map[string]string
	}{
		{"versions", "GET", "/api/minecraft/versions", nil, "/versions", nil},
		{"mods", "GET", "/api/minecraft/mods?loader=fabric", nil, "/mods", map[string]string{"loader": "fabric"}},
		{"client-mods", "POST", "/api/minecraft/client-mods", []byte(`{"mods":["fabric-api"]}`), "/client-mods", nil},
		{"preflight-jre", "POST", "/api/minecraft/preflight/jre", []byte(`{"mods":["fabric-api"]}`), "/preflight/jre", nil},
	}

	resolver := h.cellsByName["minecraft-resolver"]
	if resolver == nil {
		t.Fatal("Evolution application did not load the minecraft-resolver package")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, b := h.Do(tc.method, tc.path, nil, tc.body)
			if s != 200 {
				t.Fatalf("generic %s %s: want 200, got %d (%s)", tc.method, tc.path, s, b)
			}
			request := geneHTTPRequest{
				Method: tc.method, Path: tc.resolverPath, Query: tc.resolverQuery, Body: tc.body,
			}
			wire, err := msgpack.Marshal(request)
			if err != nil {
				t.Fatalf("encode resolver request: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			raw, err := resolver.Call(ctx, "minecraft-resolver.route.v1", wire)
			if err != nil {
				t.Fatalf("call resolver package: %v", err)
			}
			var expected geneHTTPResponse
			if err := msgpack.Unmarshal(raw, &expected); err != nil {
				t.Fatalf("decode resolver response: %v", err)
			}
			if uint32(s) != expected.Status || string(b) != string(expected.Body) {
				t.Fatalf("%s did not preserve the resolver package response\n want=%d %s\n got =%d %s",
					tc.name, expected.Status, expected.Body, s, b)
			}
		})
	}
}

// TestEvolution_LegacyMCRoutesGone confirms the cutover removed the hardcoded
// routes — they must 404 now (only the generic /api/:game/... paths remain).
func TestEvolution_LegacyMCRoutesGone(t *testing.T) {
	h := startEvolutionWithSidecar(t)
	warmEvolution(t, h)
	for _, p := range []string{"/api/mc-versions", "/api/mods", "/api/client-mods", "/api/preflight/jre"} {
		if s, _ := h.Do("GET", p, nil, nil); s != 404 {
			t.Errorf("legacy route %s: want 404 after cutover, got %d", p, s)
		}
	}
}
