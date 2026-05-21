package manifest

import (
	"fmt"
	"strings"
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

	// Duplicate cell names across the set.
	byName := make(map[string]*CellSpec, len(specs))
	for _, s := range specs {
		if prev, ok := byName[s.Name]; ok {
			return nil, fmt.Errorf("duplicate cell name %q (in %s and %s)", s.Name, prev.ManifestPath, s.ManifestPath)
		}
		byName[s.Name] = s
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

	// Topological sort (Kahn). Detects cycles.
	order, err := topoSort(specs, byName)
	if err != nil {
		return nil, err
	}

	return &Set{Cells: specs, Order: order}, nil
}

// topoSort runs Kahn's algorithm on the dep graph. Returns the ordering
// or an error listing the cycle members.
func topoSort(specs []*CellSpec, byName map[string]*CellSpec) ([]*CellSpec, error) {
	// in-degree = number of dependencies this cell has
	indeg := make(map[string]int, len(specs))
	// adjacency: dep -> cells that depend on dep
	adj := make(map[string][]string, len(specs))
	for _, s := range specs {
		indeg[s.Name] = len(s.DependsOn)
		for _, dep := range s.DependsOn {
			adj[dep] = append(adj[dep], s.Name)
		}
	}

	// Seed queue with all cells that have no deps. Preserve declaration
	// order within a degree level for deterministic boot sequences.
	var queue []string
	for _, s := range specs {
		if indeg[s.Name] == 0 {
			queue = append(queue, s.Name)
		}
	}

	order := make([]*CellSpec, 0, len(specs))
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		order = append(order, byName[n])
		for _, m := range adj[n] {
			indeg[m]--
			if indeg[m] == 0 {
				queue = append(queue, m)
			}
		}
	}

	if len(order) != len(specs) {
		var stuck []string
		for name, d := range indeg {
			if d > 0 {
				stuck = append(stuck, name)
			}
		}
		return nil, fmt.Errorf("dependency cycle involving: %s", strings.Join(stuck, ", "))
	}

	return order, nil
}
