package fusion

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/BananaLabs-OSS/Pulp/registry"
)

func coordinatorFixture(t *testing.T) (Coordinator, PrepareRequest, *fakeRunner) {
	t.Helper()
	sourceVersion, _ := registry.ParseVersion("1.0.0")
	alphaRelease := registry.ReleaseID{ID: "example/alpha-source", Version: sourceVersion}
	zetaRelease := registry.ReleaseID{ID: "example/zeta-source", Version: sourceVersion}
	sourceLock := &registry.Lock{SchemaVersion: 1, Target: "source", Modules: []registry.LockedModule{{ID: alphaRelease.ID, Version: sourceVersion.String()}, {ID: zetaRelease.ID, Version: sourceVersion.String()}}}
	group := Group{Name: "state", ABI: "pulp-linear-v1", Members: []*manifest.CellSpec{
		{Name: "alpha", Version: "1", Provides: []string{"a.v1"}},
		{Name: "zeta", Version: "1", Provides: []string{"z.v1"}},
	}}
	catalog := sourceCatalog{
		alphaRelease: {ModulePath: "example/alpha-source", ImportPath: "example/alpha-source/core", Version: "v1.0.0", SourceSHA256: testDigest, Entrypoint: "Register"},
		zetaRelease:  {ModulePath: "example/zeta-source", ImportPath: "example/zeta-source/core", Version: "v1.0.0", SourceSHA256: testDigest, Entrypoint: "Register"},
	}
	runner := &fakeRunner{wasm: []byte("coordinator-wasm")}
	store, err := registry.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	recipe, builder := hermeticFixture(t, Recipe{SchemaVersion: RecipeSchemaV1, Group: "state", ABI: "pulp-linear-v1", GoVersion: "go1.25.6", Members: []RecipeMember{
		{Name: "alpha", Version: "1", Providers: []string{"a.v1"}, Source: catalog[alphaRelease]},
		{Name: "zeta", Version: "1", Providers: []string{"z.v1"}, Source: catalog[zetaRelease]},
	}}, runner, Toolchain{FiberVersion: "v0.4.0", BuilderID: "coordinator/v1"})
	for _, member := range recipe.Members {
		if member.Name == "alpha" {
			catalog[alphaRelease] = member.Source
		} else {
			catalog[zetaRelease] = member.Source
		}
	}
	coordinator := Coordinator{Store: store, Publisher: store, Sources: catalog, Builder: builder, Materializer: DirectoryMaterializer{Root: t.TempDir()}}
	request := PrepareRequest{Group: group, SourceLock: sourceLock, SourceBindings: map[string]registry.ModuleID{"alpha": alphaRelease.ID, "zeta": zetaRelease.ID}, GoVersion: "go1.25.6", ModuleID: "example/fused", ModuleVersion: "1.0.0", Target: "wasip1/wasm", PhysicalName: "fused-state"}
	return coordinator, request, runner
}

func TestCoordinatorBuildsPublishesSelectsAndMaterializes(t *testing.T) {
	coordinator, request, runner := coordinatorFixture(t)
	activation, err := coordinator.Prepare(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !activation.Fused || activation.Spec.Name != "fused-state" || activation.Spec.WASMPath == "" {
		t.Fatalf("activation = %#v", activation)
	}
	if bytes, err := os.ReadFile(activation.Spec.WASMPath); err != nil || string(bytes) != "coordinator-wasm" {
		t.Fatalf("materialized bytes = %q, %v", bytes, err)
	}
	if runner.command == "" {
		t.Fatal("cache miss did not invoke builder")
	}

	// The immutable cached release must be selected without a second build.
	runner.command = ""
	second, err := coordinator.Prepare(context.Background(), request)
	if err != nil || !second.Fused {
		t.Fatalf("cached activation = %#v, %v", second, err)
	}
	if runner.command != "" {
		t.Fatal("cache hit unexpectedly invoked builder")
	}
}

func TestCoordinatorFallbackIsExplicit(t *testing.T) {
	coordinator, request, _ := coordinatorFixture(t)
	coordinator.Publisher = nil
	request.Fallback = AllowIsolatedFallback
	activation, err := coordinator.Prepare(context.Background(), request)
	if err != nil || activation.Fused || activation.Fallback == nil {
		t.Fatalf("fallback activation = %#v, %v", activation, err)
	}
	request.Fallback = FusionRequired
	if _, err := coordinator.Prepare(context.Background(), request); err == nil {
		t.Fatal("required fusion silently fell back")
	}
}

type failingMaterializer struct{}

func (failingMaterializer) Materialize(context.Context, registry.Digest, []byte) (string, error) {
	return "", errors.New("disk unavailable")
}

func TestCoordinatorDoesNotTreatInvalidCachedReleaseAsMiss(t *testing.T) {
	coordinator, request, runner := coordinatorFixture(t)
	if _, err := coordinator.Prepare(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	runner.command = ""
	coordinator.Materializer = failingMaterializer{}
	if _, err := coordinator.Prepare(context.Background(), request); err == nil {
		t.Fatal("materialization failure accepted")
	}
	if runner.command != "" {
		t.Fatal("existing release failure triggered an unauthorized rebuild")
	}
}
