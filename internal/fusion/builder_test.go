package fusion

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/registry"
)

type archiveMap map[string][]byte

func (a archiveMap) Archive(_ context.Context, source Source) ([]byte, error) {
	return a[source.ModulePath], nil
}

func sourceZip(t *testing.T, module string) []byte {
	t.Helper()
	var output bytes.Buffer
	w := zip.NewWriter(&output)
	f, err := w.Create("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("module " + module + "\n\ngo 1.25\n"))
	f, err = w.Create("core/core.go")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("package core\nfunc Register([]byte) error { return nil }\n"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func hermeticFixture(t *testing.T, recipe Recipe, runner CommandRunner, toolchain Toolchain) (Recipe, Builder) {
	t.Helper()
	archives := archiveMap{}
	for index := range recipe.Members {
		blob := sourceZip(t, recipe.Members[index].Source.ModulePath)
		recipe.Members[index].Source.SourceSHA256 = strings.TrimPrefix(registry.Sum(blob).String(), "sha256:")
		archives[recipe.Members[index].Source.ModulePath] = blob
	}
	fiber := sourceZip(t, "github.com/BananaLabs-OSS/Fiber")
	toolchain.FiberSource = Source{ModulePath: "github.com/BananaLabs-OSS/Fiber", ImportPath: "github.com/BananaLabs-OSS/Fiber/pulp", Version: toolchain.FiberVersion, SourceSHA256: strings.TrimPrefix(registry.Sum(fiber).String(), "sha256:"), Entrypoint: "Register"}
	archives[toolchain.FiberSource.ModulePath] = fiber
	return recipe, Builder{Runner: runner, TempRoot: t.TempDir(), Toolchain: toolchain, Archives: archives}
}

type fakeRunner struct {
	directory   string
	environment []string
	command     string
	arguments   []string
	main        []byte
	module      []byte
	wasm        []byte
}

func (r *fakeRunner) Run(_ context.Context, directory string, environment []string, command string, arguments ...string) error {
	r.directory, r.environment, r.command, r.arguments = directory, append([]string(nil), environment...), command, append([]string(nil), arguments...)
	r.main, _ = os.ReadFile(filepath.Join(directory, "main.go"))
	r.module, _ = os.ReadFile(filepath.Join(directory, "go.mod"))
	return os.WriteFile(filepath.Join(directory, "fusion.wasm"), r.wasm, 0o600)
}

func builderRecipe() Recipe {
	return Recipe{SchemaVersion: RecipeSchemaV1, Group: "state", ABI: "pulp-linear-v1", GoVersion: "go1.25.6", Members: []RecipeMember{
		{Name: "zeta", Version: "1", Providers: []string{"z.v1"}, Capabilities: []string{"storage.sqlite"}, Source: Source{ModulePath: "example/zeta", ImportPath: "example/zeta/core", Version: "v1.2.3", SourceSHA256: testDigest, Entrypoint: "Register"}},
		{Name: "alpha", Version: "1", Providers: []string{"a.v1"}, Capabilities: []string{"storage.sqlite"}, Source: Source{ModulePath: "example/alpha", ImportPath: "example/alpha/core", Version: "v2.0.0", SourceSHA256: testDigest, Entrypoint: "Register"}},
	}}
}

func TestGenerateGoModuleIsCanonical(t *testing.T) {
	recipe := builderRecipe()
	first, err := GenerateGoModule(recipe, "v0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	recipe.Members[0], recipe.Members[1] = recipe.Members[1], recipe.Members[0]
	second, err := GenerateGoModule(recipe, "v0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("go.mod depends on member order:\n%s\n%s", first, second)
	}
	wantOrder := "example/alpha v2.0.0\n\texample/zeta v1.2.3\n\tgithub.com/BananaLabs-OSS/Fiber v0.4.0"
	if !strings.Contains(string(first), wantOrder) {
		t.Fatalf("go.mod ordering:\n%s", first)
	}
}

func TestBuilderUsesInjectedToolchainAndHashesWasm(t *testing.T) {
	runner := &fakeRunner{wasm: []byte("fake-wasm")}
	recipe, builder := hermeticFixture(t, builderRecipe(), runner, Toolchain{GoCommand: "custom-go", FiberVersion: "v0.4.0", BuilderID: "test-builder/v1"})
	result, err := builder.Build(context.Background(), recipe)
	if err != nil {
		t.Fatal(err)
	}
	if runner.command != "custom-go" || !reflect.DeepEqual(runner.arguments, []string{"build", "-trimpath", "-buildvcs=false", "-buildmode=c-shared", "-ldflags=-buildid=", "-o", filepath.Join(runner.directory, "fusion.wasm"), "."}) {
		t.Fatalf("command = %q %#v", runner.command, runner.arguments)
	}
	if !containsEnvironment(runner.environment, "GOPROXY=off") || !containsEnvironment(runner.environment, "GOWORK=off") {
		t.Fatalf("environment = %#v", runner.environment)
	}
	wantDigest := strings.TrimPrefix(registry.Sum(runner.wasm).String(), "sha256:")
	if result.Attestation.Metadata.ArtifactSHA256 != wantDigest || result.Attestation.Builder != "test-builder/v1" {
		t.Fatalf("attestation = %#v", result.Attestation)
	}
	if len(runner.main) == 0 || len(runner.module) == 0 {
		t.Fatal("builder did not materialize deterministic inputs")
	}
	if _, err := os.Stat(runner.directory); !os.IsNotExist(err) {
		t.Fatalf("temporary workspace was not removed: %v", err)
	}
}

func TestBuilderCarriesV2WrapperIntoAttestedArtifact(t *testing.T) {
	recipe := builderRecipe()
	recipe.SchemaVersion, recipe.ABI = RecipeSchemaV2, MemberABIV2
	for i := range recipe.Members {
		recipe.Members[i].Source.Entrypoint = "RegisterV2"
		recipe.Members[i].Config = []byte{byte(i + 1)}
		recipe.Members[i].Snapshotable = true
	}
	runner := &fakeRunner{wasm: []byte("v2-wasm")}
	recipe, builder := hermeticFixture(t, recipe, runner, Toolchain{FiberVersion: "v0.4.0", BuilderID: "test-builder/v2"})
	result, err := builder.Build(context.Background(), recipe)
	if err != nil {
		t.Fatal(err)
	}
	if result.Attestation.Metadata.SchemaVersion != RecipeSchemaV2 || result.Attestation.Metadata.ABI != MemberABIV2 {
		t.Fatalf("metadata = %+v", result.Attestation.Metadata)
	}
	if !strings.Contains(string(result.MainSource), "RegisterV2") || !strings.Contains(string(result.MainSource), "pulp.Provide") {
		t.Fatalf("wrapper =\n%s", result.MainSource)
	}
}

func TestBuildResultProducesPublishableRegistryPackage(t *testing.T) {
	runner := &fakeRunner{wasm: []byte("fake-wasm")}
	recipe, builder := hermeticFixture(t, builderRecipe(), runner, Toolchain{FiberVersion: "v0.4.0", BuilderID: "test-builder/v1"})
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
	versions, err := store.Versions(context.Background(), "example/fused-state")
	if err != nil || len(versions) != 1 || versions[0].String() != "1.0.0" {
		t.Fatalf("versions = %#v, %v", versions, err)
	}
	manifest, err := store.Manifest(context.Background(), registry.ReleaseID{ID: "example/fused-state", Version: versions[0]})
	if err != nil || len(manifest.Artifacts) != 2 || manifest.FusionABI != "pulp-linear-v1" {
		t.Fatalf("manifest = %#v, %v", manifest, err)
	}
}

func containsEnvironment(environment []string, want string) bool {
	for _, value := range environment {
		if value == want {
			return true
		}
	}
	return false
}

func TestGenerateGoModuleRejectsConflictingModuleVersions(t *testing.T) {
	recipe := builderRecipe()
	recipe.Members[1].Source.ModulePath = recipe.Members[0].Source.ModulePath
	if _, err := GenerateGoModule(recipe, "v0.4.0"); err == nil || !strings.Contains(err.Error(), "conflicting versions") {
		t.Fatalf("error = %v", err)
	}
}
