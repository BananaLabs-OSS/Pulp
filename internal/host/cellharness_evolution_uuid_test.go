package host

// PORT of the resolveJavaUUID / resolveBedrockUUID success + not-found cases
// from Evolution/internal/router/uuid_resolve_test.go, driven THROUGH the real
// Evolution cell's POST /api/servers/:id/whitelist — the ONLY flow that reaches
// these helpers in the cell.
//
// The native tests call resolveJavaUUID / resolveBedrockUUID directly, swapping
// mojangBaseURL / geysermcBaseURL for an httptest server. In the cell those
// functions issue their Mojang / GeyserMC GET through pulp.HTTP.Fetch
// (transport.http.outbound), so this harness answers the lookup on the outbound
// stub (evoBananagineResponse: Mojang path -> Notch's id or 404 for "Nobody",
// Geyser path -> a Bedrock id) and drives the whitelist-add endpoint on a REAL
// active server provisioned through the cell. The endpoint dash-formats the id
// resolveJavaUUID/resolveBedrockUUID returned and echoes it back as "uuid", so
// the assertion lands on the resolver's output.

import (
	"encoding/json"
	"testing"
)

// whitelistSecret opens the internal-auth'd /api/servers/* route group so the
// whitelist-add flow (which is behind internalAuth) is reachable.
const whitelistSecret = "wl-internal-secret"

// postWhitelist adds a player to a server's whitelist and returns status + body.
// The route is internal-auth'd, so it carries the X-Internal-Secret header.
func postWhitelist(t *testing.T, h *CellHarness, serverID, name, platform string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name, "platform": platform})
	status, b := h.Do("POST", "/api/servers/"+serverID+"/whitelist",
		map[string]string{"Content-Type": "application/json", "X-Internal-Secret": whitelistSecret}, body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return status, out
}

// TestEvolution_WhitelistAdd_ResolvesJavaUUID ports TestResolveJavaUUID_Success:
// a Java player is resolved via Mojang and the endpoint echoes the dash-formatted
// UUID resolveJavaUUID produced.
func TestEvolution_WhitelistAdd_ResolvesJavaUUID(t *testing.T) {
	h, db := startEvolutionDowntimeCfg(t, whitelistSecret)
	srvID, _, _, _ := provisionActiveServer(t, h, db, "wljava@example.com")

	status, out := postWhitelist(t, h, srvID, "Notch", "java")
	if status != 200 {
		t.Fatalf("whitelist add java: want 200, got %d (%v)", status, out)
	}
	if out["uuid"] != "069a79f4-44e9-4726-a5be-fca90e38aaf5" {
		t.Fatalf("expected dash-formatted Java UUID from resolveJavaUUID, got %v", out["uuid"])
	}
}

// TestEvolution_WhitelistAdd_JavaPlayerNotFound ports
// TestResolveJavaUUID_PlayerNotFound_404: Mojang 404 -> resolveJavaUUID errors ->
// the whitelist-add handler surfaces 400 "Java player not found".
func TestEvolution_WhitelistAdd_JavaPlayerNotFound(t *testing.T) {
	h, db := startEvolutionDowntimeCfg(t, whitelistSecret)
	srvID, _, _, _ := provisionActiveServer(t, h, db, "wlghost@example.com")

	status, out := postWhitelist(t, h, srvID, "Nobody", "java")
	if status != 400 {
		t.Fatalf("whitelist add unknown java player: want 400, got %d (%v)", status, out)
	}
}

// TestEvolution_WhitelistAdd_ResolvesBedrockUUID ports
// TestResolveBedrockUUID_GeyserSuccess: a Bedrock gamertag is resolved via
// GeyserMC and the endpoint echoes the dash-formatted UUID resolveBedrockUUID
// produced (the added name is dot-prefixed for Bedrock).
func TestEvolution_WhitelistAdd_ResolvesBedrockUUID(t *testing.T) {
	h, db := startEvolutionDowntimeCfg(t, whitelistSecret)
	srvID, _, _, _ := provisionActiveServer(t, h, db, "wlbedrock@example.com")

	status, out := postWhitelist(t, h, srvID, "SirNiklas9369", "bedrock")
	if status != 200 {
		t.Fatalf("whitelist add bedrock: want 200, got %d (%v)", status, out)
	}
	if out["uuid"] != "00000000-0000-0000-0009-000005ccdde3" {
		t.Fatalf("expected dash-formatted Bedrock UUID from resolveBedrockUUID, got %v", out["uuid"])
	}
}
