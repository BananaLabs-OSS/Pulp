package dependency

import "fmt"

// Collapse maps a logical plan onto deployment-selected physical nodes. Every
// logical node must have a non-empty physical target. Logical edges whose ends
// map to the same physical node disappear; all other edges are deduplicated.
// Physical node order follows first appearance in the logical start order.
func Collapse(logical *Plan, physicalFor map[string]string) (*Plan, error) {
	if logical == nil {
		return nil, fmt.Errorf("collapse requires a logical dependency plan")
	}
	physicalOrder := make([]string, 0)
	seenPhysical := make(map[string]bool)
	physicalDependencies := make(map[string][]string)
	seenEdges := make(map[string]map[string]bool)
	for _, logicalID := range logical.startOrder {
		physicalID := physicalFor[logicalID]
		if physicalID == "" {
			return nil, fmt.Errorf("logical dependency node %q has no physical target", logicalID)
		}
		if !seenPhysical[physicalID] {
			seenPhysical[physicalID] = true
			physicalOrder = append(physicalOrder, physicalID)
		}
		node := logical.nodes[logicalID]
		for _, logicalDependency := range node.dependsOn {
			physicalDependency := physicalFor[logicalDependency]
			if physicalDependency == "" {
				return nil, fmt.Errorf("logical dependency node %q has no physical target", logicalDependency)
			}
			if physicalDependency == physicalID {
				continue
			}
			if seenEdges[physicalID] == nil {
				seenEdges[physicalID] = make(map[string]bool)
			}
			if !seenEdges[physicalID][physicalDependency] {
				seenEdges[physicalID][physicalDependency] = true
				physicalDependencies[physicalID] = append(physicalDependencies[physicalID], physicalDependency)
			}
		}
	}
	items := make([]Item, 0, len(physicalOrder))
	for _, physicalID := range physicalOrder {
		items = append(items, Item{ID: physicalID, DependsOn: physicalDependencies[physicalID]})
	}
	return Build(items)
}
