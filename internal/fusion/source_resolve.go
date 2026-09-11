package fusion

import (
	"context"
	"fmt"

	"github.com/BananaLabs-OSS/Pulp/registry"
)

// SourceCatalog is the narrow bridge from registry releases to buildable
// source metadata. It can be backed by a future registry source artifact,
// without teaching the fusion planner about storage or transport.
type SourceCatalog interface {
	FusionSource(context.Context, registry.ReleaseID) (Source, error)
}

// RecipeFromLock binds every logical member in an accepted fusion group to an
// exact release already selected by the module lock, then resolves its source
// descriptor. bindings is explicit because logical cell names and registry
// module IDs intentionally occupy different identity namespaces.
func RecipeFromLock(ctx context.Context, group Group, goVersion string, lock *registry.Lock, bindings map[string]registry.ModuleID, catalog SourceCatalog) (Recipe, error) {
	if lock == nil {
		return Recipe{}, fmt.Errorf("registry lock is required")
	}
	if catalog == nil {
		return Recipe{}, fmt.Errorf("fusion source catalog is required")
	}
	locked := make(map[registry.ModuleID]registry.ReleaseID, len(lock.Modules))
	for _, module := range lock.Modules {
		version, err := registry.ParseVersion(module.Version)
		if err != nil {
			return Recipe{}, fmt.Errorf("locked module %s: %w", module.ID, err)
		}
		locked[module.ID] = registry.ReleaseID{ID: module.ID, Version: version}
	}
	sources := make(map[string]Source, len(group.Members))
	for _, member := range group.Members {
		if err := ctx.Err(); err != nil {
			return Recipe{}, err
		}
		if member == nil {
			return Recipe{}, fmt.Errorf("fusion group %q contains a nil member", group.Name)
		}
		moduleID := bindings[member.Name]
		if moduleID == "" {
			return Recipe{}, fmt.Errorf("fusion member %q has no registry module binding", member.Name)
		}
		release, ok := locked[moduleID]
		if !ok {
			return Recipe{}, fmt.Errorf("fusion member %q module %q is not present in registry lock", member.Name, moduleID)
		}
		source, err := catalog.FusionSource(ctx, release)
		if err != nil {
			return Recipe{}, fmt.Errorf("fusion member %q source from %s: %w", member.Name, release, err)
		}
		sources[member.Name] = source
	}
	return NewRecipe(group, goVersion, sources)
}
