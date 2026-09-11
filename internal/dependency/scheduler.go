package dependency

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Execute starts a node as soon as every dependency has completed
// successfully. maxParallel <= 0 means unbounded concurrency. On the first
// failure it cancels in-flight work, starts no more nodes, and waits for every
// worker before returning. Ready is always in deterministic plan order.
func Execute(ctx context.Context, plan *Plan, maxParallel int, run func(context.Context, string) error) (ready []string, err error) {
	if plan == nil {
		return nil, errors.New("dependency scheduler requires a plan")
	}
	if run == nil {
		return nil, errors.New("dependency scheduler requires a runner")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	limit := maxParallel
	if limit <= 0 || limit > len(plan.startOrder) {
		limit = len(plan.startOrder)
	}
	type completion struct {
		id  string
		err error
	}
	done := make(chan completion, limit)
	remaining := make(map[string]int, len(plan.nodes))
	queue := make([]string, 0)
	for _, id := range plan.startOrder {
		node := plan.nodes[id]
		remaining[id] = len(node.dependsOn)
		if len(node.dependsOn) == 0 {
			queue = append(queue, id)
		}
	}
	running := 0
	completed := make(map[string]bool, len(plan.nodes))
	failures := make(map[string]error)
	failed := false
	launch := func(id string) {
		running++
		go func() { done <- completion{id: id, err: run(workerCtx, id)} }()
	}
	for running > 0 || (!failed && len(queue) > 0) {
		for !failed && running < limit && len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			launch(id)
		}
		result := <-done
		running--
		if result.err != nil {
			failures[result.id] = result.err
			if !failed {
				failed = true
				cancel()
			}
			continue
		}
		completed[result.id] = true
		if failed {
			continue
		}
		for _, dependent := range plan.nodes[result.id].dependents {
			remaining[dependent]--
			if remaining[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
		sort.SliceStable(queue, func(i, j int) bool { return plan.nodes[queue[i]].Ordinal < plan.nodes[queue[j]].Ordinal })
	}
	for _, id := range plan.startOrder {
		if completed[id] {
			ready = append(ready, id)
		}
	}
	if len(failures) != 0 {
		errs := make([]error, 0, len(failures))
		for _, id := range plan.startOrder {
			if failure := failures[id]; failure != nil {
				errs = append(errs, fmt.Errorf("dependency node %s: %w", id, failure))
			}
		}
		return ready, errors.Join(errs...)
	}
	if ctx.Err() != nil {
		return ready, ctx.Err()
	}
	return ready, nil
}
