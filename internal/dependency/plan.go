// Package dependency builds and executes deterministic dependency plans.
package dependency

import (
	"fmt"
	"sort"
	"strings"
)

// Item declares one uniquely named node and its direct dependencies. Items are
// ordered: that order is retained as the deterministic tie-breaker throughout
// the resulting plan.
type Item struct {
	ID        string
	DependsOn []string
}

// Node is an immutable snapshot of one node in a Plan. Slice-returning Plan
// methods return copies, so callers cannot mutate the plan through a Node.
type Node struct {
	ID         string
	Ordinal    int
	Level      int
	dependsOn  []string
	dependents []string
}

func (n Node) Dependencies() []string { return append([]string(nil), n.dependsOn...) }
func (n Node) Dependents() []string   { return append([]string(nil), n.dependents...) }

// Plan is a validated, deterministic DAG. Levels may execute from first to
// last; nodes within one level never depend on each other.
type Plan struct {
	nodes      map[string]Node
	levels     [][]string
	startOrder []string
	stopOrder  []string
}

// Build validates items and constructs a dependency plan.
func Build(items []Item) (*Plan, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("dependency plan requires at least one node")
	}
	ordinals := make(map[string]int, len(items))
	dependencies := make(map[string][]string, len(items))
	dependents := make(map[string][]string, len(items))
	for ordinal, item := range items {
		if strings.TrimSpace(item.ID) == "" {
			return nil, fmt.Errorf("dependency node at index %d has an empty ID", ordinal)
		}
		if _, exists := ordinals[item.ID]; exists {
			return nil, fmt.Errorf("duplicate dependency node %q", item.ID)
		}
		ordinals[item.ID] = ordinal
	}
	for _, item := range items {
		seen := make(map[string]struct{}, len(item.DependsOn))
		for _, dependency := range item.DependsOn {
			if dependency == item.ID {
				return nil, fmt.Errorf("dependency node %q depends on itself", item.ID)
			}
			if _, exists := ordinals[dependency]; !exists {
				return nil, fmt.Errorf("dependency node %q depends on unknown node %q", item.ID, dependency)
			}
			if _, duplicate := seen[dependency]; duplicate {
				return nil, fmt.Errorf("dependency node %q lists dependency %q more than once", item.ID, dependency)
			}
			seen[dependency] = struct{}{}
			dependencies[item.ID] = append(dependencies[item.ID], dependency)
			dependents[dependency] = append(dependents[dependency], item.ID)
		}
	}
	less := func(left, right string) bool { return ordinals[left] < ordinals[right] }
	for id := range dependents {
		sort.SliceStable(dependents[id], func(i, j int) bool { return less(dependents[id][i], dependents[id][j]) })
	}

	remaining := make(map[string]int, len(items))
	ready := make([]string, 0, len(items))
	for _, item := range items {
		remaining[item.ID] = len(dependencies[item.ID])
		if remaining[item.ID] == 0 {
			ready = append(ready, item.ID)
		}
	}
	levels := make([][]string, 0)
	startOrder := make([]string, 0, len(items))
	levelByID := make(map[string]int, len(items))
	for len(ready) != 0 {
		level := append([]string(nil), ready...)
		levels = append(levels, level)
		startOrder = append(startOrder, level...)
		next := make([]string, 0)
		for _, id := range level {
			levelByID[id] = len(levels) - 1
			for _, dependent := range dependents[id] {
				remaining[dependent]--
				if remaining[dependent] == 0 {
					next = append(next, dependent)
				}
			}
		}
		sort.SliceStable(next, func(i, j int) bool { return less(next[i], next[j]) })
		ready = next
	}
	if len(startOrder) != len(items) {
		stuck := make([]string, 0)
		for _, item := range items {
			if remaining[item.ID] != 0 {
				stuck = append(stuck, item.ID)
			}
		}
		return nil, fmt.Errorf("dependency cycle involving: %s", strings.Join(stuck, ", "))
	}

	nodes := make(map[string]Node, len(items))
	for _, item := range items {
		nodes[item.ID] = Node{ID: item.ID, Ordinal: ordinals[item.ID], Level: levelByID[item.ID], dependsOn: append([]string(nil), dependencies[item.ID]...), dependents: append([]string(nil), dependents[item.ID]...)}
	}
	stopOrder := make([]string, len(startOrder))
	for index := range startOrder {
		stopOrder[index] = startOrder[len(startOrder)-1-index]
	}
	return &Plan{nodes: nodes, levels: levels, startOrder: startOrder, stopOrder: stopOrder}, nil
}

func (p *Plan) Node(id string) (Node, bool) {
	node, ok := p.nodes[id]
	return node, ok
}
func (p *Plan) Levels() [][]string {
	levels := make([][]string, len(p.levels))
	for index := range p.levels {
		levels[index] = append([]string(nil), p.levels[index]...)
	}
	return levels
}
func (p *Plan) StartOrder() []string { return append([]string(nil), p.startOrder...) }
func (p *Plan) StopOrder() []string  { return append([]string(nil), p.stopOrder...) }
