package run

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// applicationSnapshot is deliberately host-owned data. No pointer into guest
// linear memory survives Cell.Snapshot, which makes the value safe to retain
// while a replacement runtime is built and health checked.
type applicationSnapshot struct {
	Cells map[string][]byte
}

// SnapshotLive snapshots every stateful physical cell in stable address
// order. A snapshotable cell without the guest ABI is rejected rather than
// silently losing state during a live deployment.
func (r *applicationRuntime) SnapshotLive(ctx context.Context, _ []string) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return nil, errors.New("application runtime is not started")
	}
	addresses := make([]string, 0, len(r.runtimes))
	for address, runtime := range r.runtimes {
		if runtime.spec.Snapshotable {
			addresses = append(addresses, address)
		}
	}
	sort.Strings(addresses)
	snapshot := applicationSnapshot{Cells: make(map[string][]byte, len(addresses))}
	for _, address := range addresses {
		runtime := r.runtimes[address]
		runtime.execution.Lock()
		if runtime.cell == nil || runtime.failed.Load() {
			runtime.execution.Unlock()
			return nil, fmt.Errorf("snapshot cell %s: cell is unavailable", address)
		}
		state, err := runtime.cell.Snapshot(ctx)
		runtime.execution.Unlock()
		if err != nil {
			return nil, fmt.Errorf("snapshot cell %s: %w", address, err)
		}
		snapshot.Cells[address] = state
	}
	return snapshot, nil
}

// RestoreLive restores the matching stateful cells of a prepared runtime.
// Extra, missing, or newly-stateful cells fail closed: cross-version schema
// conversion belongs in an explicit migration layer, not an implicit host
// guess.
func (r *applicationRuntime) RestoreLive(ctx context.Context, value any, _ []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return errors.New("candidate application runtime is not started")
	}
	snapshot, ok := value.(applicationSnapshot)
	if !ok {
		if value == nil && !r.LiveStatefulUnlocked() {
			return nil
		}
		return fmt.Errorf("unsupported application snapshot type %T", value)
	}
	want := make(map[string]bool)
	for address, runtime := range r.runtimes {
		if runtime.spec.Snapshotable {
			want[address] = true
		}
	}
	for address := range snapshot.Cells {
		if !want[address] {
			return fmt.Errorf("snapshot contains state for unknown or stateless cell %q", address)
		}
	}
	addresses := make([]string, 0, len(want))
	for address := range want {
		if _, ok := snapshot.Cells[address]; !ok {
			return fmt.Errorf("snapshot is missing stateful cell %q", address)
		}
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	for _, address := range addresses {
		runtime := r.runtimes[address]
		runtime.execution.Lock()
		if runtime.cell == nil || runtime.failed.Load() {
			runtime.execution.Unlock()
			return fmt.Errorf("restore cell %s: cell is unavailable", address)
		}
		err := runtime.cell.Restore(ctx, snapshot.Cells[address])
		runtime.execution.Unlock()
		if err != nil {
			return fmt.Errorf("restore cell %s: %w", address, err)
		}
	}
	return nil
}

func (r *applicationRuntime) LiveStatefulUnlocked() bool {
	for _, runtime := range r.runtimes {
		if runtime.spec.Snapshotable {
			return true
		}
	}
	return false
}
