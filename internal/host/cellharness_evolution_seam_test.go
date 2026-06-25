package host

// P1-7 game-capability seam harness (DESIGN-GAME-CAPABILITY-SEAM).
//
// Proves the generic declaration-driven proxy (/api/:game/...) returns
// byte-identical responses to the hardcoded MC routes (/api/mc-versions etc.)
// before Phase 2 deletes the hardcoded handlers. The http.outbound stub
// (cellharness_evostubs_test.go) serves /capabilities so the cell registers the
// generic routes at boot, then serves stable per-path bodies so both the legacy
// and generic routes proxying the SAME sidecar path get identical bytes.

import "testing"

// startEvolutionWithSidecar boots the cell with a (stub-served) minecraft
// sidecar configured, so GameSidecarURLs["minecraft"] is set and both the
// hardcoded MC routes and the generic /capabilities-driven routes register.
func startEvolutionWithSidecar(t *testing.T) *CellHarness {
	return StartCellHTTP(t, CellHarnessConfig{
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

// TestEvolution_GenericProxyMatchesLegacy is the Phase 1 red→green gate: each
// generic /api/:game/... route must match its hardcoded /api/... twin in both
// status and body. If this passes, Phase 2 can delete the hardcoded handlers.
func TestEvolution_GenericProxyMatchesLegacy(t *testing.T) {
	h := startEvolutionWithSidecar(t)
	warmEvolution(t, h)

	cases := []struct {
		name    string
		method  string
		legacy  string
		generic string
		body    []byte
	}{
		{"versions", "GET", "/api/mc-versions", "/api/minecraft/versions", nil},
		{"mods", "GET", "/api/mods?loader=fabric", "/api/minecraft/mods?loader=fabric", nil},
		{"client-mods", "POST", "/api/client-mods", "/api/minecraft/client-mods", []byte(`{"mods":["fabric-api"]}`)},
		{"preflight-jre", "POST", "/api/preflight/jre", "/api/minecraft/preflight/jre", []byte(`{"mods":["fabric-api"]}`)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sL, bL := h.Do(tc.method, tc.legacy, nil, tc.body)
			sG, bG := h.Do(tc.method, tc.generic, nil, tc.body)
			if sG != 200 {
				t.Fatalf("generic %s %s: want 200, got %d (%s)", tc.method, tc.generic, sG, bG)
			}
			if sL != sG {
				t.Fatalf("%s: status mismatch legacy=%d generic=%d", tc.name, sL, sG)
			}
			if string(bL) != string(bG) {
				t.Fatalf("%s: body mismatch\n legacy=%s\ngeneric=%s", tc.name, bL, bG)
			}
		})
	}
}
