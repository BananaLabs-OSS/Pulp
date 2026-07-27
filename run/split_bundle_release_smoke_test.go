package run

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

// TestProductionSplitBundleStagesLoadsStartsAndRoutes proves the deployable
// form of Evolution/pulp.host.toml. It does not run source manifests with
// rewritten in-memory paths: every WASM package is release-built, copied into
// one self-contained stage, digest-pinned by its copied cell manifest, and
// then loaded and started from that stage as Resolver + Sessions + thin
// Evolution. The representative tiers request must cross Evolution Lua's
// exact AppCall grant into the independently hosted Sessions application.
func TestProductionSplitBundleStagesLoadsStartsAndRoutes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping release-style split-bundle WASM build in short mode")
	}

	workspace, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	sourceHostPath := filepath.Join(workspace, "Evolution", "pulp.host.toml")
	sourceHost, err := manifest.LoadHost(sourceHostPath)
	if err != nil {
		t.Fatalf("load exact production host before staging: %v", err)
	}
	assertPulpThreeAppResolverHostShape20260725(t, sourceHost)

	stage := t.TempDir()
	stagedHostPath := stageProductionSplitBundle(t, workspace, stage, sourceHost)
	stagedHost, err := manifest.LoadHost(stagedHostPath)
	if err != nil {
		t.Fatalf("load staged production host: %v", err)
	}
	assertPulpThreeAppResolverHostShape20260725(t, stagedHost)
	assertProductionSplitBundlePins(t, stage, stagedHost)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	endpoints := NewEndpointRegistry()
	crossApplications := newCrossApplicationRegistry()
	capabilities := evolutionAppCapabilities()
	storageRoot := t.TempDir()

	started := make(map[ApplicationIdentity]*evolutionHostedApplication)
	factory := ApplicationRuntimeFactoryFunc(func(_ context.Context, application HostedApplication) (ApplicationRuntime, error) {
		return &productionSplitBundleRuntime{
			identity: application.Identity,
			start: func(startCtx context.Context) *evolutionHostedApplication {
				harness := startStagedProductionSplitApplication(
					t, startCtx, storageRoot, endpoints, crossApplications,
					capabilities, logger, application,
				)
				started[application.Identity] = harness
				return harness
			},
		}, nil
	})
	supervisor, err := NewMultiHostSupervisor(ManifestHostLoader{}, factory)
	if err != nil {
		t.Fatalf("create staged host supervisor: %v", err)
	}
	if err := supervisor.Start(ctx, stagedHostPath); err != nil {
		t.Fatalf("start staged production host DAG: %v", err)
	}
	t.Cleanup(func() {
		if err := supervisor.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown staged production host DAG: %v", err)
		}
	})

	resolverID := ApplicationIdentity{ApplicationID: "minecraft-resolver", InstanceID: "primary"}
	sessionsID := ApplicationIdentity{ApplicationID: "sessions", InstanceID: "primary"}
	evolutionID := ApplicationIdentity{ApplicationID: "evolution", InstanceID: "primary"}
	if started[resolverID] == nil || started[sessionsID] == nil || started[evolutionID] == nil {
		t.Fatalf("staged host runtimes = %#v, want Resolver + Sessions + Evolution", started)
	}
	seedEvolutionControlProjection(t, ctx, started[sessionsID].cells["control"].cell)
	assertEvolutionLuaCrossApplicationRoute(t, ctx, started[evolutionID].cells["lua-orchestrator"].cell)
}

type productionSplitBundleRuntime struct {
	identity ApplicationIdentity
	start    func(context.Context) *evolutionHostedApplication
	harness  *evolutionHostedApplication
}

func (r *productionSplitBundleRuntime) Identity() ApplicationIdentity { return r.identity }

func (r *productionSplitBundleRuntime) Start(ctx context.Context) error {
	r.harness = r.start(ctx)
	return nil
}

func (r *productionSplitBundleRuntime) Shutdown(ctx context.Context) error {
	r.harness.close(ctx)
	r.harness = nil
	return nil
}

func stageProductionSplitBundle(t *testing.T, workspace, stage string, source *manifest.Host) string {
	t.Helper()
	packageRoot := filepath.Join(stage, "packages")
	if err := os.MkdirAll(packageRoot, 0o755); err != nil {
		t.Fatalf("create staged package root: %v", err)
	}

	cache := filepath.Join(stage, "go-build-cache")
	artifacts := make(map[string]string)
	for _, application := range source.ApplicationOrder {
		for _, spec := range application.Application.Cells.Order {
			if _, exists := artifacts[spec.Name]; exists {
				continue
			}
			sourceDir, ok := evolutionApplicationCellSources(workspace)[spec.Name]
			if !ok {
				t.Fatalf("release bundle has no source package for %s/%s", application.ID, spec.Name)
			}
			artifact := filepath.Join(packageRoot, spec.Name+".wasm")
			buildProductionSplitReleaseWASM(t, sourceDir, artifact, cache)
			artifacts[spec.Name] = artifact
		}
	}

	stagedApps := make(map[string]string, len(source.Applications))
	for _, application := range source.Applications {
		stagedApps[application.ID] = stageProductionSplitApplication(t, stage, application, artifacts)
	}
	return writeProductionSplitHost(t, stage, source, stagedApps)
}

func buildProductionSplitReleaseWASM(t *testing.T, sourceDir, output, cache string) {
	t.Helper()
	command := exec.Command(
		"go", "build",
		"-buildvcs=false",
		"-trimpath",
		"-buildmode=c-shared",
		"-ldflags=-buildid=",
		"-o", output,
		".",
	)
	command.Dir = sourceDir
	command.Env = append(os.Environ(),
		"GOOS=wasip1",
		"GOARCH=wasm",
		"GOCACHE="+cache,
	)
	if combined, err := command.CombinedOutput(); err != nil {
		t.Fatalf("release-build %s: %v\n%s", sourceDir, err, combined)
	}
}

func stageProductionSplitApplication(
	t *testing.T,
	stage string,
	source *manifest.HostedApplication,
	artifacts map[string]string,
) string {
	t.Helper()
	appDir := filepath.Join(stage, "apps", source.ID)
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatalf("create staged application %s: %v", source.ID, err)
	}
	scriptName := filepath.Base(source.Application.OrchestrationScript)
	copySessionsCompositionFile(t, source.Application.OrchestrationScript, filepath.Join(appDir, scriptName))

	cellManifests := make([]string, 0, len(source.Application.Cells.Cells))
	orchestratorManifest := ""
	for _, spec := range source.Application.Cells.Cells {
		artifact := artifacts[spec.Name]
		if artifact == "" {
			t.Fatalf("staged application %s has no built artifact for %s", source.ID, spec.Name)
		}
		cellDir := filepath.Join(appDir, "cells", spec.Name)
		if err := os.MkdirAll(cellDir, 0o755); err != nil {
			t.Fatalf("create staged %s/%s: %v", source.ID, spec.Name, err)
		}
		manifestName := filepath.Base(spec.ManifestPath)
		stagedManifest := filepath.Join(cellDir, manifestName)
		copySessionsCompositionFile(t, spec.ManifestPath, stagedManifest)
		relativeArtifact, err := filepath.Rel(cellDir, artifact)
		if err != nil {
			t.Fatalf("relocate staged %s/%s WASM: %v", source.ID, spec.Name, err)
		}
		rewriteSessionsCompositionWASMPath(
			t,
			stagedManifest,
			filepath.ToSlash(relativeArtifact),
			sessionsCompositionSHA256(t, artifact),
		)
		relativeManifest := filepath.ToSlash(filepath.Join("cells", spec.Name, manifestName))
		cellManifests = append(cellManifests, relativeManifest)
		if spec.Name == source.Application.OrchestratorCell {
			orchestratorManifest = relativeManifest
		}
	}
	if orchestratorManifest == "" {
		t.Fatalf("staged application %s has no orchestrator manifest", source.ID)
	}
	appPath := filepath.Join(appDir, "pulp.app.toml")
	writeSessionsCompositionApp(
		t,
		appPath,
		source.Application,
		cellManifests,
		orchestratorManifest,
		scriptName,
	)
	return appPath
}

func writeProductionSplitHost(
	t *testing.T,
	stage string,
	source *manifest.Host,
	stagedApps map[string]string,
) string {
	t.Helper()
	var content strings.Builder
	fmt.Fprintf(&content, "schema_version = %d\nname = %q\n", source.SchemaVersion, source.Name)
	for _, application := range source.Applications {
		relativeManifest, err := filepath.Rel(stage, stagedApps[application.ID])
		if err != nil {
			t.Fatalf("relocate staged application %s: %v", application.ID, err)
		}
		fmt.Fprintf(&content, "\n[[applications]]\nid = %q\nmanifest = %q\ninstances = %d\n",
			application.ID, filepath.ToSlash(relativeManifest), len(application.Instances))
		content.WriteString("aliases = [")
		for index, instance := range application.Instances {
			if index > 0 {
				content.WriteString(", ")
			}
			fmt.Fprintf(&content, "%q", instance.Alias)
		}
		fmt.Fprintf(&content, "]\nstorage_namespace = %q\nevent_namespace = %q\n",
			application.StorageNamespace, application.EventNamespace)
		if len(application.DependsOn) > 0 {
			content.WriteString("depends_on = [")
			for index, dependency := range application.DependsOn {
				if index > 0 {
					content.WriteString(", ")
				}
				fmt.Fprintf(&content, "%q", dependency)
			}
			content.WriteString("]\n")
		}
	}
	for _, route := range source.Routes {
		fmt.Fprintf(&content, "\n[[routes]]\npath = %q\napplication = %q\ninstance = %q\n",
			route.Path, route.Application, route.Instance)
	}
	hostPath := filepath.Join(stage, "pulp.host.toml")
	if err := os.WriteFile(hostPath, []byte(content.String()), 0o600); err != nil {
		t.Fatalf("write staged production host: %v", err)
	}
	return hostPath
}

func assertProductionSplitBundlePins(t *testing.T, stage string, hostManifest *manifest.Host) {
	t.Helper()
	for _, application := range hostManifest.Applications {
		if application.Application == nil || !application.Application.RequireWASMSHA256 {
			t.Fatalf("staged application %s does not require WASM SHA-256 pins", application.ID)
		}
		if !pathWithin(stage, application.ManifestPath) ||
			!pathWithin(stage, application.Application.OrchestrationScript) {
			t.Fatalf("staged application %s escapes bundle: manifest=%q script=%q",
				application.ID, application.ManifestPath, application.Application.OrchestrationScript)
		}
		for _, spec := range application.Application.Cells.Cells {
			if spec.WASMSHA256 == "" {
				t.Fatalf("staged application %s cell %s has no WASM SHA-256 pin", application.ID, spec.Name)
			}
			if !pathWithin(stage, spec.ManifestPath) || !pathWithin(stage, spec.WASMPath) {
				t.Fatalf("staged application %s cell %s escapes bundle: manifest=%q wasm=%q",
					application.ID, spec.Name, spec.ManifestPath, spec.WASMPath)
			}
			if got := sessionsCompositionSHA256(t, spec.WASMPath); got != spec.WASMSHA256 {
				t.Fatalf("staged application %s cell %s digest = %s, want %s",
					application.ID, spec.Name, got, spec.WASMSHA256)
			}
		}
	}
}

func startStagedProductionSplitApplication(
	t *testing.T,
	ctx context.Context,
	storageRoot string,
	endpoints *EndpointRegistry,
	cross *crossApplicationRegistry,
	capabilities map[string]ext.Capability,
	logger *slog.Logger,
	application HostedApplication,
) *evolutionHostedApplication {
	t.Helper()
	loaded, err := manifest.LoadApp(application.ManifestPath)
	if err != nil {
		t.Fatalf("load staged %s application: %v", application.Identity, err)
	}
	if loaded.Name != application.Identity.ApplicationID || !loaded.RequireWASMSHA256 {
		t.Fatalf("staged host application %s loads invalid app %#v", application.Identity, loaded)
	}

	declared := map[string]bool{}
	for _, spec := range loaded.Cells.Order {
		if spec.WASMSHA256 == "" {
			t.Fatalf("staged %s/%s is not digest-pinned", application.Identity, spec.Name)
		}
		for _, name := range spec.Capabilities {
			if _, ok := capabilities[name]; !ok {
				t.Fatalf("%s declares unavailable capability %q", application.Identity, name)
			}
			declared[name] = true
		}
	}
	capabilityScope, err := ext.NewScope(
		application.Identity.ApplicationID,
		application.Identity.InstanceID,
		"host",
		"primary",
	)
	if err != nil {
		t.Fatalf("create staged %s capability scope: %v", application.Identity, err)
	}
	activeCapabilities := make([]ext.Capability, 0, len(declared))
	for name := range declared {
		capability := capabilities[name]
		if capability.Setup != nil {
			if err := capability.Setup(ext.SetupEnv{
				Scope:       capabilityScope,
				StorageRoot: evolutionHostedStorageRoot(storageRoot, application),
				Endpoints:   endpoints,
				Logger:      logger,
			}); err != nil {
				t.Fatalf("setup staged %s capability %q: %v", application.Identity, name, err)
			}
		}
		activeCapabilities = append(activeCapabilities, capability)
	}

	cells := make(map[string]*cellRuntime, len(loaded.Cells.Order))
	for _, spec := range loaded.Cells.Order {
		cells[spec.Name] = &cellRuntime{spec: spec}
	}
	registry := host.NewRegistry()
	for _, capability := range capabilities {
		registry.Gated(capability)
	}
	registry.Always(siblingCapabilityWithCrossApplication(newSiblingRegistry(cells), cross, application))
	if missing := validateSiblingLinks(cells); len(missing) != 0 {
		t.Fatalf("%s staged local composition links: %v", application.Identity, missing)
	}

	harness := &evolutionHostedApplication{
		application:     application,
		cells:           cells,
		capabilities:    activeCapabilities,
		capabilityScope: capabilityScope,
		cross:           cross,
	}
	for _, spec := range loaded.Cells.Order {
		scope, err := application.NewCellScope(spec.Name, "primary")
		if err != nil {
			harness.close(context.Background())
			t.Fatalf("create staged %s/%s scope: %v", application.Identity, spec.Name, err)
		}
		cell, err := host.LoadScoped(ctx, spec, registry, nil, logger, scope)
		if err != nil {
			harness.close(context.Background())
			t.Fatalf("load staged %s/%s: %v", application.Identity, spec.Name, err)
		}
		cells[spec.Name].cell = cell
		harness.loaded = append(harness.loaded, cell)
		config, err := manifest.EncodeConfig(spec.Config)
		if err != nil {
			harness.close(context.Background())
			t.Fatalf("encode staged %s/%s config: %v", application.Identity, spec.Name, err)
		}
		if err := cell.Init(ctx, config); err != nil {
			harness.close(context.Background())
			t.Fatalf("init staged %s/%s: %v", application.Identity, spec.Name, err)
		}
	}
	if err := cross.markReady(
		application,
		&applicationRuntime{application: application, runtimes: cells},
	); err != nil {
		harness.close(context.Background())
		t.Fatalf("register staged %s providers: %v", application.Identity, err)
	}
	return harness
}
