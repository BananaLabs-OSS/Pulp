package run

import (
	"fmt"

	"github.com/BananaLabs-OSS/Pulp/internal/dependency"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

// physicalDependencyPlan expands logical dependencies across repeated
// placements and collapses execution-unit members onto their selected artifact.
// A placement depends on every placement of each logical dependency.
func physicalDependencyPlan(app *manifest.Application, placements []manifest.CellPlacement, fusedTargets map[string]string) (*dependency.Plan, error) {
	physicalByLogical := make(map[string][]string, len(app.Cells.Cells))
	logicalByPhysical := make(map[string][]string, len(placements))
	logicalNames := make(map[string]bool, len(app.Cells.Cells))
	for _, spec := range app.Cells.Cells {
		logicalNames[spec.Name] = true
	}
	for _, placement := range placements {
		if logicalNames[placement.Spec.Name] {
			physicalByLogical[placement.Spec.Name] = append(physicalByLogical[placement.Spec.Name], placement.Address)
			logicalByPhysical[placement.Address] = append(logicalByPhysical[placement.Address], placement.Spec.Name)
		}
	}
	for logical, physical := range fusedTargets {
		physicalByLogical[logical] = []string{physical}
		logicalByPhysical[physical] = append(logicalByPhysical[physical], logical)
	}
	items := make([]dependency.Item, 0, len(placements))
	for _, placement := range placements {
		seen := make(map[string]bool)
		var dependencies []string
		for _, logical := range logicalByPhysical[placement.Address] {
			node, ok := app.Cells.Plan.Node(logical)
			if !ok {
				return nil, fmt.Errorf("physical placement %q references unknown logical cell %q", placement.Address, logical)
			}
			for _, logicalDependency := range node.Dependencies() {
				for _, physicalDependency := range physicalByLogical[logicalDependency] {
					if physicalDependency != placement.Address && !seen[physicalDependency] {
						seen[physicalDependency] = true
						dependencies = append(dependencies, physicalDependency)
					}
				}
			}
		}
		items = append(items, dependency.Item{ID: placement.Address, DependsOn: dependencies})
	}
	return dependency.Build(items)
}
