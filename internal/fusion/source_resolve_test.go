package fusion

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/BananaLabs-OSS/Pulp/registry"
)

type sourceCatalog map[registry.ReleaseID]Source

func (c sourceCatalog) FusionSource(_ context.Context, release registry.ReleaseID) (Source, error) {
	source, ok := c[release]
	if !ok {
		return Source{}, fmt.Errorf("source not found")
	}
	return source, nil
}

func TestRecipeFromLockBindsLogicalMembersToExactReleases(t *testing.T) {
	alphaVersion, _ := registry.ParseVersion("1.2.3")
	zetaVersion, _ := registry.ParseVersion("2.0.0")
	alphaRelease := registry.ReleaseID{ID: "example/alpha", Version: alphaVersion}
	zetaRelease := registry.ReleaseID{ID: "example/zeta", Version: zetaVersion}
	lock := &registry.Lock{SchemaVersion: 1, Target: "source", Modules: []registry.LockedModule{{ID: alphaRelease.ID, Version: alphaVersion.String()}, {ID: zetaRelease.ID, Version: zetaVersion.String()}}}
	group := Group{Name: "state", ABI: "pulp-linear-v1", Members: []*manifest.CellSpec{
		{Name: "logical-zeta", Version: "2", Provides: []string{"z.v1"}},
		{Name: "logical-alpha", Version: "1", Provides: []string{"a.v1"}},
	}}
	catalog := sourceCatalog{
		alphaRelease: {ModulePath: "example/alpha", ImportPath: "example/alpha/core", Version: "v1.2.3", SourceSHA256: testDigest, Entrypoint: "Register"},
		zetaRelease:  {ModulePath: "example/zeta", ImportPath: "example/zeta/core", Version: "v2.0.0", SourceSHA256: testDigest, Entrypoint: "Register"},
	}
	recipe, err := RecipeFromLock(context.Background(), group, "go1.25.6", lock, map[string]registry.ModuleID{"logical-alpha": alphaRelease.ID, "logical-zeta": zetaRelease.ID}, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if recipe.Members[0].Name != "logical-alpha" || recipe.Members[0].Source.Version != "v1.2.3" {
		t.Fatalf("recipe = %#v", recipe)
	}
}

func TestRecipeFromLockFailsClosedForUnboundOrUnlockedSource(t *testing.T) {
	version, _ := registry.ParseVersion("1.0.0")
	lock := &registry.Lock{SchemaVersion: 1, Target: "source", Modules: []registry.LockedModule{{ID: "example/other", Version: version.String()}}}
	group := Group{Name: "state", ABI: "pulp-linear-v1", Members: []*manifest.CellSpec{{Name: "logical", Version: "1"}}}
	catalog := sourceCatalog{}
	if _, err := RecipeFromLock(context.Background(), group, "go1.25.6", lock, nil, catalog); err == nil || !strings.Contains(err.Error(), "no registry module binding") {
		t.Fatalf("unbound error = %v", err)
	}
	if _, err := RecipeFromLock(context.Background(), group, "go1.25.6", lock, map[string]registry.ModuleID{"logical": "example/missing"}, catalog); err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("unlocked error = %v", err)
	}
}
