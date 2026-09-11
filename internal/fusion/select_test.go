package fusion

import (
	"context"
	"strings"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/BananaLabs-OSS/Pulp/registry"
)

func publishFusionFixture(t *testing.T, recipe Recipe) (*registry.LocalStore, *registry.Lock, *BuildResult) {
	t.Helper()
	runner := &fakeRunner{wasm: []byte("fused-wasm")}
	recipe, builder := hermeticFixture(t, recipe, runner, Toolchain{FiberVersion: "v0.4.0", BuilderID: "fixture-builder/v1"})
	result, err := builder.Build(context.Background(), recipe)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := result.RegistryPublication("example/fused-state", "1.0.0", "wasip1/wasm")
	if err != nil {
		t.Fatal(err)
	}
	store, err := registry.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(context.Background(), publication.Manifest, publication.Blobs); err != nil {
		t.Fatal(err)
	}
	lock, err := registry.Resolve(context.Background(), store, registry.Request{Target: "wasip1/wasm", Roots: []registry.Dependency{{ID: "example/fused-state", Requirement: "1.0.0"}}})
	if err != nil {
		t.Fatal(err)
	}
	return store, lock, result
}

func TestRegistryResolveSelectAndCellValidation(t *testing.T) {
	recipe := builderRecipe()
	store, lock, result := publishFusionFixture(t, recipe)
	selected, err := SelectFromRegistry(context.Background(), store, lock, "example/fused-state", recipe)
	if err != nil {
		t.Fatal(err)
	}
	if string(selected.Wasm) != "fused-wasm" || selected.Attestation.Builder != "fixture-builder/v1" {
		t.Fatalf("selection = %#v", selected)
	}
	spec := &manifest.CellSpec{Name: "fused-state", Provides: []string{"z.v1", "a.v1"}, Capabilities: []string{"storage.sqlite"}, WASMSHA256: result.Attestation.Metadata.ArtifactSHA256, Execution: manifest.ExecutionSpec{ABI: "pulp-linear-v1"}}
	if err := ValidateCellArtifact(spec, selected); err != nil {
		t.Fatal(err)
	}
	spec.Provides = append(spec.Provides, "leaked.v1")
	if err := ValidateCellArtifact(spec, selected); err == nil || !strings.Contains(err.Error(), "providers") {
		t.Fatalf("error = %v", err)
	}
}

func TestSelectionRejectsRecipeMismatch(t *testing.T) {
	recipe := builderRecipe()
	store, lock, _ := publishFusionFixture(t, recipe)
	recipe.Members[0].Providers = []string{"changed.v1"}
	if _, err := SelectFromRegistry(context.Background(), store, lock, "example/fused-state", recipe); err == nil || !strings.Contains(err.Error(), "recipe digest") {
		t.Fatalf("error = %v", err)
	}
}

func TestSelectionRequiresLockedModuleAndAttestation(t *testing.T) {
	recipe := builderRecipe()
	store, lock, _ := publishFusionFixture(t, recipe)
	if _, err := SelectFromRegistry(context.Background(), store, lock, "example/not-locked", recipe); err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("error = %v", err)
	}

	// Publishing a normal module proves selection fails closed when the
	// companion attestation target is absent.
	plainStore, err := registry.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blob := []byte("plain")
	manifestText := moduleTextForSelection("example/plain", blob)
	digest := registry.Sum(blob)
	if err := plainStore.Publish(context.Background(), manifestText, map[registry.Digest][]byte{digest: blob}); err != nil {
		t.Fatal(err)
	}
	plainLock, err := registry.Resolve(context.Background(), plainStore, registry.Request{Target: "wasip1/wasm", Roots: []registry.Dependency{{ID: "example/plain", Requirement: "1.0.0"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SelectFromRegistry(context.Background(), plainStore, plainLock, "example/plain", recipe); err == nil || !strings.Contains(err.Error(), "fusion-attestation") {
		t.Fatalf("error = %v", err)
	}
}

func moduleTextForSelection(id string, blob []byte) []byte {
	return []byte("schema_version = 1\nprovides = []\nconsumes = []\ncapabilities = []\n[module]\nid = \"" + id + "\"\nversion = \"1.0.0\"\n[[artifacts]]\ntarget = \"wasip1/wasm\"\ndigest = \"" + registry.Sum(blob).String() + "\"\nsize = 5\n")
}
