package run

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/vmihailenco/msgpack/v5"
)

// TestSessionsBananauthHumanAuthOwnerParity starts the real Bananauth identity
// and session owner WASI cells, then reaches them only through the same
// cross-application registry used by a hosted composition. The gateway is a
// test-only stand-in for the typed Sessions adapter: it never has an Evolution
// target, so an owner-declared failure cannot silently fall back to legacy
// email-code transport.
func TestSessionsBananauthHumanAuthOwnerParity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real Bananauth owner WASI parity test in short mode")
	}
	workspace, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	cross := newCrossApplicationRegistry()
	bananauth := sessionsBananauthStartOwners(t, ctx, workspace, cross, "parity")
	t.Cleanup(func() { bananauth.close(context.Background()) })

	caller := HostedApplication{
		Identity:  ApplicationIdentity{ApplicationID: "sessions-auth-parity", InstanceID: "parity"},
		DependsOn: []string{"bananauth"},
	}
	gateway := sessionsBananauthGateway{registry: cross, caller: crossApplicationCaller{
		application: caller,
		cellAddress: "sessions-human-auth-parity",
		hostConsumes: []string{
			"auth.identity.v1.native.authenticate",
			"auth.identity.v1.oauth.resolve",
			"auth.identity.v1.email-verification.issue",
			"auth.identity.v1.email-verification.consume",
			"auth.identity.v1.retention-lease.create",
			"auth.identity.v1.retention-lease.release",
			"auth.identity.v1.retention-eligibility.get",
			"auth.session.v1.create",
			"auth.session.v1.get",
			"auth.session.v1.revoke",
		},
	}}

	// Seed owner state directly as a fixture. The Sessions gateway itself is
	// restricted to the five declared read/auth/session operations above.
	sessionsBananauthSeedIdentity(t, ctx, bananauth.runtime)

	native := gateway.call(t, ctx, "auth-identity", "auth.identity.v1.native.authenticate", map[string]any{
		"email": "user@example.com", "password": "correct-horse-battery-staple",
	})
	sessionsBananauthRequireOK(t, native, "auth-identity.v1", "account_id", "account-1")

	oauth := gateway.call(t, ctx, "auth-identity", "auth.identity.v1.oauth.resolve", map[string]any{
		"provider": "discord", "provider_id": "discord-user-1",
	})
	sessionsBananauthRequireOK(t, oauth, "auth-identity.v1", "account_id", "account-oauth-1")

	issuedVerification := gateway.call(t, ctx, "auth-identity", "auth.identity.v1.email-verification.issue", map[string]any{
		"request_id": "sessions-email-issue-1", "verification_id": "verification-1", "effect_id": "verification-effect-1",
		"email": "user@example.com", "code": "123456", "now": int64(1_700_000_000_000), "expires_at": int64(1_700_000_001_800),
	})
	sessionsBananauthRequireOK(t, issuedVerification, "auth-identity.v1", "accepted", true)
	verifiedEmail := gateway.call(t, ctx, "auth-identity", "auth.identity.v1.email-verification.consume", map[string]any{
		"request_id": "sessions-email-consume-1", "email": "user@example.com", "code": "123456", "now": int64(1_700_000_000_100),
	})
	sessionsBananauthRequireOK(t, verifiedEmail, "auth-identity.v1", "verified", true)

	created := gateway.call(t, ctx, "auth-session", "auth.session.v1.create", map[string]any{
		"request_id": "sessions-parity-create-1", "session_id": "session-1", "account_id": "account-1",
		"created_at": int64(1_700_000_000_000), "expires_at": int64(1_700_000_600_000),
	})
	sessionsBananauthRequireOK(t, created, "auth-session.v1", "active", true)

	active := gateway.call(t, ctx, "auth-session", "auth.session.v1.get", map[string]any{
		"session_id": "session-1", "now": int64(1_700_000_000_100),
	})
	sessionsBananauthRequireOK(t, active, "auth-session.v1", "active", true)

	revoked := gateway.call(t, ctx, "auth-session", "auth.session.v1.revoke", map[string]any{
		"request_id": "sessions-parity-revoke-1", "session_id": "session-1", "revoked_at": int64(1_700_000_000_200),
	})
	sessionsBananauthRequireOK(t, revoked, "auth-session.v1", "active", false)

	failed := gateway.call(t, ctx, "auth-identity", "auth.identity.v1.native.authenticate", map[string]any{
		"email": "user@example.com", "password": "wrong-password",
	})
	if failed.Version != "auth-identity.v1" || failed.OK || failed.Error == nil || failed.Error.Code != "invalid_credentials" {
		t.Fatalf("owner failure = %#v, want Bananauth invalid_credentials response", failed)
	}
	if gateway.legacyFallbackCalls != 0 {
		t.Fatalf("owner failure invoked Evolution compatibility fallback %d times", gateway.legacyFallbackCalls)
	}

	retained := gateway.call(t, ctx, "auth-identity", "auth.identity.v1.retention-lease.create", map[string]any{
		"request_id": "sessions-retain-1", "now": int64(1_700_000_000_000),
		"lease": map[string]any{"account_id": "account-1", "lease_id": "lease-1", "reason_id": "sessions-retention:opaque-1", "expires_at": int64(1_700_000_600_000)},
	})
	sessionsBananauthRequireOK(t, retained, "auth-identity.v1", "lease_id", "lease-1")
	blockedDeletion := gateway.call(t, ctx, "auth-identity", "auth.identity.v1.retention-eligibility.get", map[string]any{
		"account_id": "account-1", "now": int64(1_700_000_000_100),
	})
	sessionsBananauthRequireOK(t, blockedDeletion, "auth-identity.v1", "deletable", false)
	releasedRetention := gateway.call(t, ctx, "auth-identity", "auth.identity.v1.retention-lease.release", map[string]any{
		"request_id": "sessions-release-1", "account_id": "account-1", "lease_id": "lease-1", "reason_id": "sessions-retention:opaque-1", "now": int64(1_700_000_000_200),
	})
	sessionsBananauthRequireOK(t, releasedRetention, "auth-identity.v1", "deletable", true)
}

// TestSessionsBananauthHumanAuthLuaAdapterParity loads the exact Sessions Lua
// adapter source in a hosted application. The two event bindings below exist
// only in this harness: the source module itself remains deliberately inert
// until a production host explicitly opts in.
func TestSessionsBananauthHumanAuthLuaAdapterParity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping hosted Sessions Lua to Bananauth adapter parity test in short mode")
	}
	workspace, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := os.ReadFile(filepath.Join(workspace, "Sessions-Gene", "application", "bananauth_human_auth_adapter_v1.lua"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	cross := newCrossApplicationRegistry()
	bananauth := sessionsBananauthStartOwners(t, ctx, workspace, cross, "primary")
	t.Cleanup(func() { bananauth.close(context.Background()) })
	sessionsBananauthSeedIdentity(t, ctx, bananauth.runtime)

	application := HostedApplication{
		Identity:  ApplicationIdentity{ApplicationID: "sessions-auth-adapter-parity", InstanceID: "parity"},
		DependsOn: []string{"bananauth"},
	}
	cache := t.TempDir()
	luaWASM := buildLuaHarnessCell(t, filepath.Join(workspace, "Pulp-Lua", "pulp-cell"), "sessions-auth-adapter", cache)
	script := string(adapter) + `
pulp.on("sessions.auth.email.issue.v1", function(payload)
  return __bananauth_human_auth_adapter_v1.issue(payload)
end)
pulp.on("sessions.auth.email.consume.v1", function(payload)
  return __bananauth_human_auth_adapter_v1.consume(payload)
end)
pulp.on("sessions.identity.server.active.v1", function(payload)
  return __bananauth_human_auth_adapter_v1.server_retain(payload)
end)
pulp.on("sessions.identity.server.terminal.v1", function(payload)
  return __bananauth_human_auth_adapter_v1.server_release(payload)
end)
pulp.on("sessions.identity.world.active.v1", function(payload)
  return __bananauth_human_auth_adapter_v1.world_retain(payload)
end)
pulp.on("sessions.identity.world.terminal.v1", function(payload)
  return __bananauth_human_auth_adapter_v1.world_release(payload)
end)
pulp.on("sessions.identity.retention.eligibility.v1", function(payload)
  return __bananauth_human_auth_adapter_v1.lifecycle_eligibility(payload)
end)
`
	spec := &manifest.CellSpec{
		Name:      "lua-orchestrator",
		Version:   "0.0.0-test",
		WASMPath:  luaWASM,
		Provides:  []string{"orchestrator.dispatch"},
		DependsOn: []string{},
		HostConsumes: []string{
			"auth.identity.v1.email-verification.issue",
			"auth.identity.v1.email-verification.consume",
			"auth.identity.v1.retention-lease.create",
			"auth.identity.v1.retention-lease.renew",
			"auth.identity.v1.retention-lease.release",
			"auth.identity.v1.retention-eligibility.get",
		},
		Config: map[string]any{"script": script, "timeout_ms": int64(5000)},
	}
	runtimes := map[string]*cellRuntime{"lua-orchestrator": {spec: spec}}
	registry := host.NewRegistry()
	registry.Always(siblingCapabilityWithCrossApplication(newSiblingRegistry(runtimes), cross, application))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scope, err := application.NewCellScope(spec.Name, "primary")
	if err != nil {
		t.Fatal(err)
	}
	cell, err := host.LoadScoped(ctx, spec, registry, nil, logger, scope)
	if err != nil {
		t.Fatalf("load Sessions adapter Lua: %v", err)
	}
	runtimes[spec.Name].cell = cell
	t.Cleanup(func() {
		cross.markUnavailable(application.Identity)
		_ = cell.Shutdown(context.Background())
		_ = cell.Close(context.Background())
	})
	config, err := manifest.EncodeConfig(spec.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := cell.Init(ctx, config); err != nil {
		t.Fatalf("init Sessions adapter Lua: %v", err)
	}
	if err := cross.markReady(application, &applicationRuntime{application: application, runtimes: runtimes}); err != nil {
		t.Fatal(err)
	}

	issue := sessionsBananauthLuaAdapterDispatch(t, ctx, cell, "sessions.auth.email.issue.v1", map[string]any{
		"request_msgpack": sessionsBananauthAdapterRequest(t, map[string]any{
			"request_id": "sessions-adapter-issue-1", "id": "verification-adapter-1",
			"email": "adapter@example.com", "code": "123456",
			"now": int64(1_700_000_000_000), "expires_at": int64(1_700_000_001_800),
		}),
	})
	if issue.Status != 200 || issue.Body["sent"] != true {
		t.Fatalf("issue projection = %#v, want sent", issue)
	}
	consume := sessionsBananauthLuaAdapterDispatch(t, ctx, cell, "sessions.auth.email.consume.v1", map[string]any{
		"request_msgpack": sessionsBananauthAdapterRequest(t, map[string]any{
			"request_id": "sessions-adapter-consume-1", "id": "verification-adapter-1",
			"account_id": "8b0d821e-baba-4b18-8f5e-6035fb8864d0",
			"email": "adapter@example.com", "code": "123456", "now": int64(1_700_000_000_100),
		}),
	})
	if consume.Status != 200 || consume.Body["verified"] != true || consume.Body["account_id"] != "8b0d821e-baba-4b18-8f5e-6035fb8864d0" {
		t.Fatalf("consume projection = %#v, want verified", consume)
	}
	// The account was registered by the owner-only parity test above. Lifecycle
	// policy is explicit: server/world activity holds opaque leases, terminal
	// transitions replace only their active lease with grace, and no handler
	// deletes the identity.
	serverActive := sessionsBananauthLuaAdapterDispatch(t, ctx, cell, "sessions.identity.server.active.v1", map[string]any{
		"request_msgpack": sessionsBananauthAdapterRequest(t, map[string]any{
			"request_id": "sessions-lifecycle-server-active-1", "account_id": "account-1", "subject_id": "server-1",
			"now": int64(1_700_000_000_000), "retain_until": int64(1_700_000_600_000),
		}),
	})
	if serverActive.Status != 200 || serverActive.Body["retained"] != true {
		t.Fatalf("server active projection = %#v, want retained", serverActive)
	}
	worldActive := sessionsBananauthLuaAdapterDispatch(t, ctx, cell, "sessions.identity.world.active.v1", map[string]any{
		"request_msgpack": sessionsBananauthAdapterRequest(t, map[string]any{
			"request_id": "sessions-lifecycle-world-active-1", "account_id": "account-1", "subject_id": "world-1",
			"now": int64(1_700_000_000_100), "retain_until": int64(1_700_000_600_000),
		}),
	})
	if worldActive.Status != 200 || worldActive.Body["retained"] != true {
		t.Fatalf("world active projection = %#v, want retained", worldActive)
	}
	serverTerminal := sessionsBananauthLuaAdapterDispatch(t, ctx, cell, "sessions.identity.server.terminal.v1", map[string]any{
		"request_msgpack": sessionsBananauthAdapterRequest(t, map[string]any{
			"request_id": "sessions-lifecycle-server-terminal-1", "account_id": "account-1", "subject_id": "server-1", "now": int64(1_700_000_000_200),
		}),
	})
	if serverTerminal.Status != 200 || serverTerminal.Body["released"] != true || serverTerminal.Body["deletable"] != false {
		t.Fatalf("server terminal projection = %#v, want active grace", serverTerminal)
	}
	beforeFinal := sessionsBananauthLuaAdapterDispatch(t, ctx, cell, "sessions.identity.retention.eligibility.v1", map[string]any{
		"request_msgpack": sessionsBananauthAdapterRequest(t, map[string]any{
			"request_id": "sessions-lifecycle-eligibility-1", "account_id": "account-1", "subject_id": "eligibility", "now": int64(1_700_000_000_300),
		}),
	})
	if beforeFinal.Status != 200 || beforeFinal.Body["deletable"] != false {
		t.Fatalf("eligibility with world lease = %#v, want blocked", beforeFinal)
	}
	worldTerminal := sessionsBananauthLuaAdapterDispatch(t, ctx, cell, "sessions.identity.world.terminal.v1", map[string]any{
		"request_msgpack": sessionsBananauthAdapterRequest(t, map[string]any{
			"request_id": "sessions-lifecycle-world-terminal-1", "account_id": "account-1", "subject_id": "world-1", "now": int64(1_700_000_000_400),
		}),
	})
	if worldTerminal.Status != 200 || worldTerminal.Body["released"] != true || worldTerminal.Body["deletable"] != false {
		t.Fatalf("world terminal projection = %#v, want active grace", worldTerminal)
	}
	afterGrace := sessionsBananauthLuaAdapterDispatch(t, ctx, cell, "sessions.identity.retention.eligibility.v1", map[string]any{
		"request_msgpack": sessionsBananauthAdapterRequest(t, map[string]any{
			"request_id": "sessions-lifecycle-eligibility-2", "account_id": "account-1", "subject_id": "eligibility", "now": int64(1_700_000_605_201),
		}),
	})
	if afterGrace.Status != 200 || afterGrace.Body["deletable"] != true {
		t.Fatalf("eligibility after final grace = %#v, want deletable", afterGrace)
	}
}

type sessionsBananauthAdapterProjection struct {
	Status int            `msgpack:"status"`
	Body   map[string]any `msgpack:"body"`
}

func sessionsBananauthAdapterRequest(t *testing.T, request map[string]any) []byte {
	t.Helper()
	wire, err := msgpack.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func sessionsBananauthLuaAdapterDispatch(t *testing.T, ctx context.Context, cell *host.Cell, event string, payload map[string]any) sessionsBananauthAdapterProjection {
	t.Helper()
	request, err := msgpack.Marshal(luaDispatchRequest{Event: event, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	response, err := cell.Call(ctx, "orchestrator.dispatch", request)
	if err != nil {
		t.Fatalf("dispatch %s: %v", event, err)
	}
	var dispatched luaDispatchResult
	if err := msgpack.Unmarshal(response, &dispatched); err != nil {
		t.Fatal(err)
	}
	projectionWire, err := msgpack.Marshal(dispatched.Value)
	if err != nil {
		t.Fatal(err)
	}
	var projection sessionsBananauthAdapterProjection
	if err := msgpack.Unmarshal(projectionWire, &projection); err != nil {
		t.Fatal(err)
	}
	return projection
}

type sessionsBananauthGateway struct {
	registry            *crossApplicationRegistry
	caller              crossApplicationCaller
	legacyFallbackCalls int
}

func (g *sessionsBananauthGateway) call(t *testing.T, ctx context.Context, cell, provider string, request any) sessionsBananauthResult {
	t.Helper()
	raw, err := msgpack.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	response, err := g.registry.call(ctx, g.caller,
		ApplicationIdentity{ApplicationID: "bananauth", InstanceID: "parity"}, cell, provider, raw)
	if err != nil {
		t.Fatalf("Bananauth %s/%s: %v", cell, provider, err)
	}
	var result sessionsBananauthResult
	if err := msgpack.Unmarshal(response, &result); err != nil {
		t.Fatalf("decode Bananauth %s response: %v", provider, err)
	}
	return result
}

type sessionsBananauthResult struct {
	Version string                  `msgpack:"version"`
	OK      bool                    `msgpack:"ok"`
	Value   map[string]any          `msgpack:"value"`
	Error   *sessionsBananauthError `msgpack:"error"`
}

type sessionsBananauthError struct {
	Code    string `msgpack:"code"`
	Message string `msgpack:"message"`
}

func sessionsBananauthRequireOK(t *testing.T, result sessionsBananauthResult, version, field string, want any) {
	t.Helper()
	if result.Version != version || !result.OK || result.Error != nil || result.Value[field] != want {
		t.Fatalf("result = %#v, want %s ok with %s=%#v", result, version, field, want)
	}
}

type sessionsBananauthOwners struct {
	application HostedApplication
	runtime     *applicationRuntime
	cells       []*host.Cell
	caps        []ext.Capability
	scope       ext.Scope
	cross       *crossApplicationRegistry
}

func sessionsBananauthStartOwners(t *testing.T, ctx context.Context, workspace string, cross *crossApplicationRegistry, instanceID string) *sessionsBananauthOwners {
	t.Helper()
	cache, storageRoot := t.TempDir(), t.TempDir()
	identityWASM := buildLuaHarnessCell(t, filepath.Join(workspace, "Bananauth", "identity-owner"), "auth-identity", cache)
	sessionWASM := buildLuaHarnessCell(t, filepath.Join(workspace, "Bananauth", "session-owner"), "auth-session", cache)
	application := HostedApplication{
		Identity:         ApplicationIdentity{ApplicationID: "bananauth", InstanceID: instanceID},
		StorageNamespace: "bananauth-parity",
		EventNamespace:   "bananauth-parity",
	}
	specs := []*manifest.CellSpec{
		{Name: "auth-identity", Version: "0.1.0", WASMPath: identityWASM,
			Provides: []string{
				"auth.identity.v1.native.register", "auth.identity.v1.native.authenticate",
				"auth.identity.v1.oauth.resolve", "auth.identity.v1.oauth.upsert",
				"auth.identity.v1.email-verification.issue", "auth.identity.v1.email-verification.consume",
				"auth.identity.v1.retention-lease.create", "auth.identity.v1.retention-lease.renew", "auth.identity.v1.retention-lease.release",
				"auth.identity.v1.retention-eligibility.get",
			},
			Capabilities: []string{"storage.sqlite", "workers"}},
		{Name: "auth-session", Version: "0.1.0", WASMPath: sessionWASM,
			Provides:     []string{"auth.session.v1.create", "auth.session.v1.get", "auth.session.v1.revoke"},
			Capabilities: []string{"storage.sqlite"}},
	}
	capabilities := evolutionAppCapabilities()
	declared := map[string]bool{"storage.sqlite": true, "workers": true}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scope, err := ext.NewScope(application.Identity.ApplicationID, application.Identity.InstanceID, "host", "primary")
	if err != nil {
		t.Fatal(err)
	}
	activeCaps := make([]ext.Capability, 0, len(declared))
	for name := range declared {
		capability, ok := capabilities[name]
		if !ok {
			t.Fatalf("Bananauth parity capability %q is unavailable", name)
		}
		if capability.Setup != nil {
			if err := capability.Setup(ext.SetupEnv{Scope: scope, StorageRoot: storageRoot, Logger: logger}); err != nil {
				t.Fatalf("setup Bananauth parity capability %q: %v", name, err)
			}
		}
		activeCaps = append(activeCaps, capability)
	}

	runtimes := map[string]*cellRuntime{}
	for _, spec := range specs {
		runtimes[spec.Name] = &cellRuntime{spec: spec}
	}
	registry := host.NewRegistry()
	for _, capability := range capabilities {
		registry.Gated(capability)
	}
	registry.Always(siblingCapabilityWithCrossApplication(newSiblingRegistry(runtimes), cross, application))
	owners := &sessionsBananauthOwners{
		application: application,
		runtime:     &applicationRuntime{application: application, runtimes: runtimes},
		caps:        activeCaps, scope: scope, cross: cross,
	}
	for _, spec := range specs {
		cellScope, err := application.NewCellScope(spec.Name, "primary")
		if err != nil {
			owners.close(context.Background())
			t.Fatal(err)
		}
		cell, err := host.LoadScoped(ctx, spec, registry, nil, logger, cellScope)
		if err != nil {
			owners.close(context.Background())
			t.Fatalf("load Bananauth %s: %v", spec.Name, err)
		}
		runtimes[spec.Name].cell = cell
		owners.cells = append(owners.cells, cell)
		if err := cell.Init(ctx, nil); err != nil {
			owners.close(context.Background())
			t.Fatalf("init Bananauth %s: %v", spec.Name, err)
		}
	}
	if err := cross.markReady(application, owners.runtime); err != nil {
		owners.close(context.Background())
		t.Fatal(err)
	}
	return owners
}

func (o *sessionsBananauthOwners) close(ctx context.Context) {
	if o == nil {
		return
	}
	if o.cross != nil {
		o.cross.markUnavailable(o.application.Identity)
	}
	for index := len(o.cells) - 1; index >= 0; index-- {
		_ = o.cells[index].Shutdown(ctx)
		_ = o.cells[index].Close(context.Background())
	}
	for _, capability := range o.caps {
		if capability.TeardownScope != nil {
			_ = capability.TeardownScope(ctx, o.scope)
		}
	}
}

func sessionsBananauthSeedIdentity(t *testing.T, ctx context.Context, runtime *applicationRuntime) {
	t.Helper()
	seed := func(provider string, request any) {
		raw, err := msgpack.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		response, err := callDeclaredProvider(runtime, ctx, "auth-identity", provider, raw)
		if err != nil {
			t.Fatalf("seed %s: %v", provider, err)
		}
		var result sessionsBananauthResult
		if err := msgpack.Unmarshal(response, &result); err != nil || !result.OK {
			t.Fatalf("seed %s result = %#v, err = %v", provider, result, err)
		}
	}
	seed("auth.identity.v1.native.register", map[string]any{
		"request_id": "seed-native-1", "account_id": "account-1", "credential_id": "credential-1",
		"email": "user@example.com", "username": "user", "password": "correct-horse-battery-staple", "now": int64(1_700_000_000_000),
	})
	seed("auth.identity.v1.oauth.upsert", map[string]any{
		"request_id": "seed-oauth-1", "account_id": "account-oauth-1", "link_id": "link-1",
		"provider": "discord", "provider_id": "discord-user-1", "provider_email": "oauth@example.com", "now": int64(1_700_000_000_001),
	})
}
