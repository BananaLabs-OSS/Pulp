package manifest

import (
	"fmt"
	"strings"

	"github.com/BananaLabs-OSS/Pulp/internal/dependency"
)

// Set is a validated collection of cell specs ready for the host to boot.
// Built by LoadAll; guarantees unique cell names, no missing deps, and
// no dependency cycles.
type Set struct {
	// Cells in declaration order (the order manifests were supplied on
	// the command line). Use Order for dep-respecting boot order.
	Cells []*CellSpec

	// Order is a topological ordering: if A depends on B, B appears before
	// A. Used by the host to drive Setup + Init in an order that satisfies
	// every cell's depends_on.
	Order []*CellSpec

	// Plan retains the complete dependency graph, including stable levels and
	// reverse edges. Order is derived from this plan for compatibility.
	Plan *dependency.Plan
}

// Lookup finds a cell by name. Returns nil if not present.
func (s *Set) Lookup(name string) *CellSpec {
	for _, p := range s.Cells {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// LoadAll parses every manifest path, validates as a group, and returns
// the resulting Set. Any manifest-level parse error, duplicate cell
// name, missing dependency, or dependency cycle aborts the load.
//
// The host should reject a partial fleet — if any manifest is broken,
// nothing boots.
func LoadAll(paths []string) (*Set, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("no manifests supplied")
	}

	specs := make([]*CellSpec, 0, len(paths))
	for _, p := range paths {
		spec, err := Load(p)
		if err != nil {
			return nil, fmt.Errorf("manifest %s: %w", p, err)
		}
		specs = append(specs, spec)
	}
	return buildSet(specs)
}

// buildSet validates already-loaded cell specs as one deployment. App
// manifests use this after injecting their verified orchestration script;
// direct -manifest loading reaches the same validation through LoadAll.
func buildSet(specs []*CellSpec) (*Set, error) {
	if len(specs) == 0 {
		return nil, fmt.Errorf("no manifests supplied")
	}
	// Duplicate cell names across the set.
	byName := make(map[string]*CellSpec, len(specs))
	for _, s := range specs {
		if prev, ok := byName[s.Name]; ok {
			return nil, fmt.Errorf("duplicate cell name %q (in %s and %s)", s.Name, prev.ManifestPath, s.ManifestPath)
		}
		byName[s.Name] = s
	}

	// Every consumed value is an exact callable provider/function, not a cell
	// alias or broad permission. It must resolve to exactly one provider so a
	// caller never gains an implicit, order-dependent target. Runtime still
	// requires the explicit target cell to declare the same provider.
	providers := make(map[string][]string)
	for _, s := range specs {
		seen := make(map[string]struct{}, len(s.Provides))
		for _, provider := range s.Provides {
			if _, duplicate := seen[provider]; duplicate {
				continue
			}
			seen[provider] = struct{}{}
			providers[provider] = append(providers[provider], s.Name)
		}
	}
	for _, s := range specs {
		for _, consumed := range s.Consumes {
			owners := providers[consumed]
			switch len(owners) {
			case 0:
				return nil, fmt.Errorf("cell %q consumes %q but no cell provides that exact provider", s.Name, consumed)
			case 1:
				// Valid. Runtime still checks the explicitly named target cell.
			default:
				return nil, fmt.Errorf("cell %q consumes %q but it has ambiguous providers %s", s.Name, consumed, strings.Join(owners, ", "))
			}
		}
	}

	// Missing dependencies — every entry in depends_on must match a cell
	// name in the set.
	for _, s := range specs {
		for _, dep := range s.DependsOn {
			if _, ok := byName[dep]; !ok {
				return nil, fmt.Errorf("cell %q depends_on %q but no such cell is in the manifest set", s.Name, dep)
			}
			if dep == s.Name {
				return nil, fmt.Errorf("cell %q depends_on itself", s.Name)
			}
		}
	}

	items := make([]dependency.Item, 0, len(specs))
	for _, spec := range specs {
		items = append(items, dependency.Item{ID: spec.Name, DependsOn: append([]string(nil), spec.DependsOn...)})
	}
	plan, err := dependency.Build(items)
	if err != nil {
		return nil, err
	}
	order := make([]*CellSpec, 0, len(specs))
	for _, id := range plan.StartOrder() {
		order = append(order, byName[id])
	}
	return &Set{Cells: specs, Order: order, Plan: plan}, nil
}
