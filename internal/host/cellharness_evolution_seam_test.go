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

// TestEvolution_GenericProxyServes is the Phase 2 gate (after the hardcoded MC
// handlers were deleted): each generic /api/:game/... route must register from
// the sidecar's /capabilities declaration and proxy through the (stub-served)
// sidecar body unchanged. The legacy /api/mc-versions etc. no longer exist.
func TestEvolution_GenericProxyServes(t *testing.T) {
	h := startEvolutionWithSidecar(t)
	warmEvolution(t, h)

	cases := []struct {
		name   string
		method string
		path   string
		body   []byte
		want   string // exact body the stub sidecar serves for this path
	}{
		{"versions", "GET", "/api/minecraft/versions", nil, `{"versions":["1.21.4","1.21.3"],"latest":"1.21.4","crossplay":true}`},
		{"mods", "GET", "/api/minecraft/mods?loader=fabric", nil, `{"mods":[{"id":"fabric-api","name":"Fabric API"}]}`},
		{"client-mods", "POST", "/api/minecraft/client-mods", []byte(`{"mods":["fabric-api"]}`), `{"client_mods":[{"id":"sodium","url":"https://stub/sodium.jar"}]}`},
		{"preflight-jre", "POST", "/api/minecraft/preflight/jre", []byte(`{"mods":["fabric-api"]}`), `{"jre":"17","ok":true}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, b := h.Do(tc.method, tc.path, nil, tc.body)
			if s != 200 {
				t.Fatalf("generic %s %s: want 200, got %d (%s)", tc.method, tc.path, s, b)
			}
			if string(b) != tc.want {
				t.Fatalf("%s: body mismatch\n want=%s\n got =%s", tc.name, tc.want, b)
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
