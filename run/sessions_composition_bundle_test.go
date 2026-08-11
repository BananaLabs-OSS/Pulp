package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/vmihailenco/msgpack/v5"
)

// TestSessionsCompositionStagesSelfContainedBundle makes the application
// descriptor executable as a bundle, rather than only valid while its source
// repositories happen to be adjacent on disk. The integration test exercises
// the loaded cells; this test owns the complementary build/staging contract.
func TestSessionsCompositionStagesSelfContainedBundle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Sessions composition WASM build in short mode")
	}

	workspace, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	appPath := filepath.Join(workspace, "Evolution", "pulp-cell", "pulp.app.toml")
	set, app, err := loadManifestInputs(appPath, nil)
	if err != nil {
		t.Fatalf("load Evolution application: %v", err)
	}
	if app == nil {
		t.Fatal("Evolution pulp.app.toml did not load as an application")
	}

	assertSessionsCompositionGraph(t, set)
	assertSessionsCompositionCapabilities(t, set)

	stage := t.TempDir()
	cache := filepath.Join(stage, "go-build-cache")
	manifestPaths := make([]string, 0, len(set.Cells))
	wasmDigests := make(map[string]string, len(set.Cells))
	for _, spec := range set.Cells {
		manifestName := filepath.Base(spec.ManifestPath)
		cellStage := filepath.Join(stage, "cells", spec.Name)
		if err := os.MkdirAll(cellStage, 0o755); err != nil {
			t.Fatalf("create stage for %q: %v", spec.Name, err)
		}
		stagedManifest := filepath.Join(cellStage, manifestName)
		copySessionsCompositionFile(t, spec.ManifestPath, stagedManifest)
		// Build Go/WASI cells from their manifest directory. A future owner may
		// instead be compiled by another WASM toolchain; in that case its manifest
		// artifact is staged and still receives the same checksum and containment
		// verification below.
		builtWASM := stageSessionsCompositionWASM(t, spec, cache)
		stagedWASM := filepath.Join(cellStage, filepath.Base(spec.WASMPath))
		copySessionsCompositionFile(t, builtWASM, stagedWASM)
		wasmDigests[spec.Name] = sessionsCompositionSHA256(t, stagedWASM)
		rewriteSessionsCompositionWASMPath(t, stagedManifest, filepath.Base(spec.WASMPath), wasmDigests[spec.Name])
		manifestPaths = append(manifestPaths, filepath.ToSlash(filepath.Join("cells", spec.Name, manifestName)))
	}

	stagedScript := filepath.Join(stage, filepath.Base(app.OrchestrationScript))
	copySessionsCompositionFile(t, app.OrchestrationScript, stagedScript)
	stagedApp := filepath.Join(stage, "pulp.app.toml")
	writeSessionsCompositionApp(t, stagedApp, app, manifestPaths, filepath.ToSlash(filepath.Join(
		"cells", app.OrchestratorCell, filepath.Base(app.OrchestratorManifest))), filepath.Base(stagedScript))

	stagedSet, stagedApplication, err := loadManifestInputs(stagedApp, nil)
	if err != nil {
		t.Fatalf("load staged Sessions application: %v", err)
	}
	if stagedApplication == nil || stagedApplication.Name != app.Name || len(stagedSet.Cells) != len(set.Cells) {
		t.Fatalf("staged application = %#v, cells = %d; want %q and %d cells", stagedApplication, len(stagedSet.Cells), app.Name, len(set.Cells))
	}
	for _, spec := range stagedSet.Cells {
		if !pathWithin(stage, spec.ManifestPath) || !pathWithin(stage, spec.WASMPath) {
			t.Fatalf("staged cell %q resolves outside bundle: manifest=%q wasm=%q", spec.Name, spec.ManifestPath, spec.WASMPath)
		}
		if got := sessionsCompositionSHA256(t, spec.WASMPath); got != wasmDigests[spec.Name] {
			t.Fatalf("staged %q WASM checksum = %s, want %s", spec.Name, got, wasmDigests[spec.Name])
		}
	}
	if !pathWithin(stage, stagedApplication.OrchestrationScript) {
		t.Fatalf("staged orchestration script resolves outside bundle: %q", stagedApplication.OrchestrationScript)
	}
	exerciseStagedSessionsComposition(t, stagedSet)
}

func TestRewriteSessionsCompositionWASMPathPinsCopiedArtifact(t *testing.T) {
	root := t.TempDir()
	wasm := []byte("copied package bytes")
	wasmPath := filepath.Join(root, "cell.wasm")
	if err := os.WriteFile(wasmPath, wasm, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, "cell.toml")
	if err := os.WriteFile(manifestPath, []byte("name = \"lua-orchestrator\"\nversion = \"1\"\nwasm = \"source.wasm\"\n[config]\nmode = \"staged\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sessionsCompositionSHA256(t, wasmPath)
	rewriteSessionsCompositionWASMPath(t, manifestPath, "cell.wasm", digest)

	script := []byte("return true")
	if err := os.WriteFile(filepath.Join(root, "app.lua"), script, 0o600); err != nil {
		t.Fatal(err)
	}
	scriptDigest := sha256.Sum256(script)
	appPath := filepath.Join(root, "pulp.app.toml")
	app := fmt.Sprintf("name = \"staged\"\nversion = \"1\"\nrequire_wasm_sha256 = true\ncells = [\"cell.toml\"]\n[orchestrator]\nmanifest = \"cell.toml\"\nscript = \"app.lua\"\nsha256 = \"%x\"\n", scriptDigest)
	if err := os.WriteFile(appPath, []byte(app), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.LoadApp(appPath)
	if err != nil {
		t.Fatalf("LoadApp staged bundle: %v", err)
	}
	cell := loaded.Cells.Lookup("lua-orchestrator")
	if cell == nil || cell.WASMSHA256 != digest || cell.Config["wasm_sha256"] != nil {
		t.Fatalf("staged cell pin/config = %#v", cell)
	}
}

// exerciseStagedSessionsComposition proves the staged, self-contained files
// can be loaded by Pulp and driven through the actual Lua -> Sessions WASM
// call path. The real HTTP adapter is separately covered by Evolution's host
// harness; this check deliberately has no live network or privileged effects.
func exerciseStagedSessionsComposition(t *testing.T, set *manifest.Set) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	capabilities := map[string]ext.Capability{}
	for _, capability := range ext.All() {
		capabilities[capability.Name] = capability
	}
	for _, capability := range evolutionAppBackendStubs() {
		capabilities[capability.Name] = capability
	}

	declared := map[string]bool{}
	for _, spec := range set.Cells {
		for _, capability := range spec.Capabilities {
			declared[capability] = true
		}
	}
	t.Setenv("HTTP_PORT", "0")
	storageRoot := t.TempDir()
	setupCapabilities := make([]ext.Capability, 0, len(capabilities))
	for name, capability := range capabilities {
		if !declared[name] || capability.Setup == nil {
			continue
		}
		if err := capability.Setup(ext.SetupEnv{StorageRoot: storageRoot, Logger: logger}); err != nil {
			t.Fatalf("setup staged capability %q: %v", name, err)
		}
		setupCapabilities = append(setupCapabilities, capability)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	runtimes := map[string]*cellRuntime{}
	for _, spec := range set.Order {
		runtimes[spec.Name] = &cellRuntime{spec: spec}
	}
	registry := host.NewRegistry()
	for _, capability := range capabilities {
		registry.Gated(capability)
	}
	registry.Always(siblingCapability(newSiblingRegistry(runtimes)))

	loaded := make([]*host.Cell, 0, len(set.Order))
	t.Cleanup(func() {
		for i := len(loaded) - 1; i >= 0; i-- {
			_ = loaded[i].Shutdown(context.Background())
			_ = loaded[i].Close(context.Background())
		}
		cancel()
		for i := len(setupCapabilities) - 1; i >= 0; i-- {
			if setupCapabilities[i].Teardown != nil {
				_ = setupCapabilities[i].Teardown(context.Background())
			}
		}
	})

	for _, spec := range set.Order {
		cell, err := host.Load(ctx, spec, registry, nil, logger)
		if err != nil {
			t.Fatalf("load staged cell %q: %v", spec.Name, err)
		}
		runtimes[spec.Name].cell = cell
		loaded = append(loaded, cell)
		config, err := manifest.EncodeConfig(spec.Config)
		if err != nil {
			t.Fatalf("encode staged config for %q: %v", spec.Name, err)
		}
		if err := cell.Init(ctx, config); err != nil {
			t.Fatalf("init staged cell %q: %v", spec.Name, err)
		}
	}
	seedEvolutionControlProjection(t, ctx, runtimes["control"].cell)

	routeRequest, err := msgpack.Marshal(evolutionAppGeneRequest{Method: "GET", Path: "/api/tiers"})
	if err != nil {
		t.Fatalf("marshal staged gene request: %v", err)
	}
	dispatchRequest, err := msgpack.Marshal(luaDispatchRequest{
		Event: "evolution.sessions.tiers.get.v1",
		Payload: map[string]any{
			"request_msgpack": string(routeRequest),
		},
	})
	if err != nil {
		t.Fatalf("marshal staged Lua dispatch: %v", err)
	}
	response, err := runtimes["lua-orchestrator"].cell.Call(ctx, "orchestrator.dispatch", dispatchRequest)
	if err != nil {
		t.Fatalf("dispatch staged Lua route: %v", err)
	}
	var dispatchResult luaDispatchResult
	if err := msgpack.Unmarshal(response, &dispatchResult); err != nil {
		t.Fatalf("decode staged Lua dispatch: %v", err)
	}
	rawResponse, ok := dispatchResult.Value.(string)
	if !ok {
		t.Fatalf("staged Lua route returned %T, want raw msgpack string", dispatchResult.Value)
	}
	var geneResponse evolutionAppGeneResponse
	if err := msgpack.Unmarshal([]byte(rawResponse), &geneResponse); err != nil {
		t.Fatalf("decode staged Sessions response: %v", err)
	}
	if geneResponse.Status != 200 || !strings.Contains(string(geneResponse.Body), "tier-compose-smoke") {
		t.Fatalf("staged GET /api/tiers = status %d body %s", geneResponse.Status, geneResponse.Body)
	}
}

func stageSessionsCompositionWASM(t *testing.T, spec *manifest.CellSpec, cache string) string {
	t.Helper()
	sourceDir := filepath.Dir(spec.ManifestPath)
	// The application owns the Lua *manifest* beside evolution.lua, while the
	// reusable Lua cell's Go source lives in its own package. Likewise, the
	// compatibility manifest for minecraft-resolver lives beside Evolution but
	// deliberately pins the production resolver artifact. Build the owner
	// sources in both cases so staging cannot substitute Evolution's WASM under
	// the resolver's narrower manifest and capability declaration.
	switch spec.Name {
	case "lua-orchestrator":
		sourceDir = filepath.Join(sourceDir, "..", "..", "Pulp-Lua", "pulp-cell")
	case "minecraft-resolver":
		sourceDir = filepath.Join(sourceDir, "..", "..", "minecraft-resolver", "pulp-cell")
	case "notification-outbox":
		// The portable owner is a reusable library module; its WASI entrypoint
		// intentionally lives under cmd rather than at module root.
		sourceDir = filepath.Join(sourceDir, "cmd", "notification-outbox")
	}
	goSources, err := filepath.Glob(filepath.Join(sourceDir, "*.go"))
	if err != nil {
		t.Fatalf("find Go sources for %q: %v", spec.Name, err)
	}
	if len(goSources) != 0 {
		return buildLuaHarnessCell(t, sourceDir, spec.Name, cache)
	}
	if _, err := os.Stat(spec.WASMPath); err == nil {
		return spec.WASMPath
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat WASM artifact for %q: %v", spec.Name, err)
	}
	t.Fatalf("cell %q has neither Go sources nor a built WASM artifact at %q", spec.Name, spec.WASMPath)
	return ""
}

func assertSessionsCompositionGraph(t *testing.T, set *manifest.Set) {
	t.Helper()
	for _, required := range []string{"sessions", "lua-orchestrator", "evolution"} {
		if set.Lookup(required) == nil {
			t.Fatalf("Sessions composition is missing required cell %q", required)
		}
	}

	providers := make(map[string][]string)
	for _, spec := range set.Cells {
		for _, capability := range spec.Provides {
			providers[capability] = append(providers[capability], spec.Name)
		}
	}
	for capability := range providers {
		sort.Strings(providers[capability])
	}

	edges := make(map[string][]string, len(set.Cells))
	for _, spec := range set.Cells {
		for _, capability := range spec.Consumes {
			candidates := providers[capability]
			if len(candidates) != 1 {
				t.Fatalf("cell %q consumes %q with providers %v; Sessions composition requires one explicit provider", spec.Name, capability, candidates)
			}
			provider := candidates[0]
			if !containsSessionsCompositionString(spec.DependsOn, provider) {
				t.Fatalf("cell %q consumes %q from %q without depends_on edge", spec.Name, capability, provider)
			}
			edges[spec.Name] = append(edges[spec.Name], provider)
		}
	}
	assertSessionsCompositionAcyclic(t, edges)

	// Owner manifests expose exact callable providers. Aggregate family labels
	// are ambient authority and no longer satisfy composition validation.
	ownerCapabilities := map[string]string{
		"commerce": "commerce.order.create.v1",
		"fleet":    "fleet.v1.command.server.upsert",
		"funding":  "funding.v1.pool.create",
		"identity": "identity.checkout.compliance.v1",
		"control":  "control.v1.sessions.tiers.get",
	}
	for owner, capability := range ownerCapabilities {
		spec := set.Lookup(owner)
		if spec != nil && !containsSessionsCompositionString(spec.Provides, capability) {
			t.Fatalf("state-owner cell %q must provide %q; provides %v", owner, capability, spec.Provides)
		}
	}
}

func assertSessionsCompositionCapabilities(t *testing.T, set *manifest.Set) {
	t.Helper()
	available := map[string]bool{}
	for _, capability := range ext.All() {
		available[capability.Name] = true
	}
	for _, capability := range evolutionAppBackendStubs() {
		available[capability.Name] = true
	}
	for _, spec := range set.Cells {
		for _, capability := range spec.Capabilities {
			if !available[capability] {
				t.Fatalf("cell %q declares unavailable host capability %q", spec.Name, capability)
			}
		}
	}
}

func assertSessionsCompositionAcyclic(t *testing.T, edges map[string][]string) {
	t.Helper()
	state := map[string]uint8{}
	var visit func(string)
	visit = func(node string) {
		switch state[node] {
		case 1:
			t.Fatalf("capability dependency cycle includes %q", node)
		case 2:
			return
		}
		state[node] = 1
		for _, dependency := range edges[node] {
			visit(dependency)
		}
		state[node] = 2
	}
	for node := range edges {
		visit(node)
	}
}

func writeSessionsCompositionApp(t *testing.T, path string, app *manifest.Application, cells []string, orchestratorManifest, script string) {
	t.Helper()
	var builder strings.Builder
	fmt.Fprintf(&builder, "schema_version = %d\nname = %q\nversion = %q\nrequire_wasm_sha256 = true\n\ncells = [\n", app.SchemaVersion, app.Name, app.Version)
	for _, cell := range cells {
		fmt.Fprintf(&builder, "    %q,\n", cell)
	}
	fmt.Fprintf(&builder, "]\n\n[orchestrator]\nmanifest = %q\nscript = %q\nsha256 = %q\n", orchestratorManifest, script, app.OrchestrationSHA256)
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		t.Fatalf("write staged app manifest: %v", err)
	}
}

func copySessionsCompositionFile(t *testing.T, source, destination string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %q: %v", source, err)
	}
	if err := os.WriteFile(destination, data, 0o600); err != nil {
		t.Fatalf("write %q: %v", destination, err)
	}
}

func rewriteSessionsCompositionWASMPath(t *testing.T, manifestPath, wasmBase, wasmSHA256 string) {
	t.Helper()
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read staged manifest %q: %v", manifestPath, err)
	}
	lines := strings.Split(string(data), "\n")
	wasmReplaced := false
	digestReplaced := false
	firstTable := len(lines)
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && firstTable == len(lines) {
			firstTable = index
		}
		if strings.HasPrefix(strings.TrimSpace(line), "wasm = ") {
			lines[index] = fmt.Sprintf("wasm = %q", wasmBase)
			wasmReplaced = true
		}
		if index < firstTable && strings.HasPrefix(trimmed, "wasm_sha256 = ") {
			lines[index] = fmt.Sprintf("wasm_sha256 = %q", wasmSHA256)
			digestReplaced = true
		}
	}
	if !wasmReplaced {
		t.Fatalf("staged manifest %q has no wasm field", manifestPath)
	}
	if !digestReplaced {
		lines = append(lines, "")
		copy(lines[firstTable+1:], lines[firstTable:])
		lines[firstTable] = fmt.Sprintf("wasm_sha256 = %q", wasmSHA256)
	}
	if err := os.WriteFile(manifestPath, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatalf("rewrite staged manifest %q: %v", manifestPath, err)
	}
}

func sessionsCompositionSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q for checksum: %v", path, err)
	}
	if len(data) == 0 {
		t.Fatalf("bundle artifact %q is empty", path)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func containsSessionsCompositionString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
