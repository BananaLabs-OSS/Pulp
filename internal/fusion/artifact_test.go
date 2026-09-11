package fusion

import (
	"strings"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

const testDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestRecipeAndAggregatorAreDeterministic(t *testing.T) {
	group := Group{Name: "state", ABI: "pulp-linear-v1", Members: []*manifest.CellSpec{
		{Name: "zeta", Version: "1", Provides: []string{"z.v1", "a.v1"}, Capabilities: []string{"storage.sqlite"}},
		{Name: "alpha", Version: "2", Provides: []string{"b.v1"}, Capabilities: []string{"storage.sqlite"}},
	}}
	sources := map[string]Source{
		"zeta":  {ModulePath: "example/zeta", ImportPath: "example/zeta/core", Version: "v1.0.0", SourceSHA256: testDigest, Entrypoint: "Register"},
		"alpha": {ModulePath: "example/alpha", ImportPath: "example/alpha/core", Version: "v2.0.0", SourceSHA256: testDigest, Entrypoint: "Register"},
	}
	recipe, err := NewRecipe(group, "go1.25.6", sources)
	if err != nil {
		t.Fatal(err)
	}
	if recipe.Members[0].Name != "alpha" || recipe.Members[1].Providers[0] != "a.v1" {
		t.Fatalf("recipe is not canonical: %#v", recipe)
	}
	first, err := GenerateGoAggregator(recipe)
	if err != nil {
		t.Fatal(err)
	}
	recipe.Members[0], recipe.Members[1] = recipe.Members[1], recipe.Members[0]
	second, err := GenerateGoAggregator(recipe)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("generated source changed with input order\n%s\n%s", first, second)
	}
	if !strings.Contains(string(first), `m000 "example/alpha/core"`) || !strings.Contains(string(first), "m001.Register(config)") {
		t.Fatalf("generated source lacks stable imports/calls:\n%s", first)
	}
}

func TestValidateRecipeRequiresExactContracts(t *testing.T) {
	group := Group{Name: "state", ABI: "pulp-linear-v1", Members: []*manifest.CellSpec{{Name: "one", Version: "1", Provides: []string{"one.v1"}, Capabilities: []string{"storage.sqlite"}}}}
	recipe, err := NewRecipe(group, "go1.25.6", map[string]Source{"one": {ModulePath: "example/one", ImportPath: "example/one/core", Version: "v1", SourceSHA256: testDigest, Entrypoint: "Register"}})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		mutate   func(*Recipe)
		contains string
	}{
		{"abi", func(r *Recipe) { r.ABI = "other" }, "recipe ABI"},
		{"provider", func(r *Recipe) { r.Members[0].Providers = append(r.Members[0].Providers, "extra.v1") }, "providers do not exactly match"},
		{"capability", func(r *Recipe) { r.Members[0].Capabilities = nil }, "capabilities do not exactly match"},
		{"member", func(r *Recipe) { r.Members[0].Name = "other" }, "unexpected member"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := recipe
			changed.Members = append([]RecipeMember(nil), recipe.Members...)
			tt.mutate(&changed)
			if err := ValidateRecipe(changed, group); err == nil || !strings.Contains(err.Error(), tt.contains) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestValidateRecipeRejectsDuplicateProviderOwnership(t *testing.T) {
	group := Group{Name: "state", ABI: "pulp-linear-v1", Members: []*manifest.CellSpec{
		{Name: "one", Version: "1", Provides: []string{"shared.v1"}},
		{Name: "two", Version: "1", Provides: []string{"shared.v1"}},
	}}
	recipe := Recipe{SchemaVersion: RecipeSchemaV1, Group: "state", ABI: "pulp-linear-v1", GoVersion: "go1.25.6", Members: []RecipeMember{
		{Name: "one", Version: "1", Providers: []string{"shared.v1"}, Source: Source{ModulePath: "example/one", ImportPath: "example/one/core", Version: "v1", SourceSHA256: testDigest, Entrypoint: "Register"}},
		{Name: "two", Version: "1", Providers: []string{"shared.v1"}, Source: Source{ModulePath: "example/two", ImportPath: "example/two/core", Version: "v1", SourceSHA256: testDigest, Entrypoint: "Register"}},
	}}
	if err := ValidateRecipe(recipe, group); err == nil || !strings.Contains(err.Error(), "owned by both") {
		t.Fatalf("error = %v", err)
	}
}

func TestArtifactMetadataBindsExactRecipeAndArtifact(t *testing.T) {
	group := Group{Name: "state", ABI: "pulp-linear-v1", Members: []*manifest.CellSpec{{Name: "one", Version: "1", Provides: []string{"one.v1"}}}}
	recipe, err := NewRecipe(group, "go1.25.6", map[string]Source{"one": {ModulePath: "example/one", ImportPath: "example/one/core", Version: "v1", SourceSHA256: testDigest, Entrypoint: "Register"}})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := NewArtifactMetadata(recipe, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateArtifact(metadata, recipe); err != nil {
		t.Fatal(err)
	}
	metadata.Providers = append(metadata.Providers, "leaked.v1")
	if err := ValidateArtifact(metadata, recipe); err == nil || !strings.Contains(err.Error(), "providers") {
		t.Fatalf("error = %v", err)
	}
	attestation := Attestation{Metadata: metadata, Builder: "builder.example/v1"}
	if !strings.Contains(attestation.RegistryKey(), "/") {
		t.Fatalf("registry key = %q", attestation.RegistryKey())
	}
}

func TestRecipeDigestBindsTransitiveDependencyProvenance(t *testing.T) {
	recipe := Recipe{SchemaVersion: RecipeSchemaV1, Group: "state", ABI: "pulp-linear-v1", GoVersion: "go1.25.6", Members: []RecipeMember{
		{Name: "one", Version: "1", Source: Source{ModulePath: "example/one", ImportPath: "example/one/core", Version: "v1", SourceSHA256: testDigest, Entrypoint: "Register"}},
		{Name: "two", Version: "1", Source: Source{ModulePath: "example/two", ImportPath: "example/two/core", Version: "v1", SourceSHA256: testDigest, Entrypoint: "Register"}},
	}, Dependencies: []Dependency{{ModulePath: "example/upstream", Version: "v1.2.3", SourceSHA256: testDigest, Origin: "https://proxy.golang.org", GoSum: "h1:source", GoModSum: "h1:mod"}}}
	first, err := RecipeDigest(recipe)
	if err != nil {
		t.Fatal(err)
	}
	recipe.Dependencies[0].GoSum = "h1:different"
	second, err := RecipeDigest(recipe)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("dependency provenance did not affect recipe digest")
	}
}

func TestV2RecipeBindsMemberScopesAndV1GeneratorFailsClosed(t *testing.T) {
	a := &manifest.CellSpec{Name: "a", Version: "1", Provides: []string{"a.v1"}, Snapshotable: true, Config: map[string]any{"mode": "a"}, Execution: manifest.ExecutionSpec{ABI: MemberABIV2}}
	b := &manifest.CellSpec{Name: "b", Version: "1", Provides: []string{"b.v1"}, Config: map[string]any{"mode": "b"}, Execution: manifest.ExecutionSpec{ABI: MemberABIV2}}
	group := Group{Name: "state", ABI: MemberABIV2, Members: []*manifest.CellSpec{a, b}}
	sources := map[string]Source{
		"a": {ModulePath: "example/a", ImportPath: "example/a/core", Version: "v1", SourceSHA256: testDigest, Entrypoint: "RegisterV2"},
		"b": {ModulePath: "example/b", ImportPath: "example/b/core", Version: "v1", SourceSHA256: testDigest, Entrypoint: "RegisterV2"},
	}
	recipe, err := NewRecipe(group, "go1.25.6", sources)
	if err != nil {
		t.Fatal(err)
	}
	if recipe.SchemaVersion != RecipeSchemaV2 || !recipe.Members[0].Snapshotable || len(recipe.Members[0].Config) == 0 {
		t.Fatalf("recipe scopes = %+v", recipe)
	}
	generated, err := GenerateGoAggregator(recipe)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(generated), "RegisterV2") || !strings.Contains(string(generated), "pulp.OnShutdown") || !strings.Contains(string(generated), "fusion member") {
		t.Fatalf("v2 wrapper missing scoped hooks:\n%s", generated)
	}
	changed := recipe
	changed.Members = append([]RecipeMember(nil), recipe.Members...)
	changed.Members[0].Config = []byte("changed")
	if err := ValidateRecipe(changed, group); err == nil {
		t.Fatal("changed member config accepted")
	}
}
