package host

// Supervisor harness. Proves the first-increment supervision primitives:
//
//   - RUNAWAY BOUNDED: a supervised Interruptible cell that infinite-loops
//     inside a step is bounded by the per-call budget — GuardStep returns
//     ErrRunaway promptly, the state goes Runaway, the reporter fires, and the
//     runaway count is 1 (testdata/runaway).
//
//   - HEALTHY UNAFFECTED: a normal supervised cell runs many guarded steps with
//     zero interference — all succeed, state stays Ready, Healthy() holds,
//     lastOK advances, and a liveness monitor stays green (testdata/heartbeat).
//
//   - LIFECYCLE: New -> Ready -> Stopped transitions and the ready signal.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/BananaLabs-OSS/Pulp/abi"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

// loadUninitedCell builds + Loads a testdata cell with the given limits but
// does NOT Init it — the Supervisor owns the Init via Start.
func loadUninitedCell(t *testing.T, sourceDir, name string, lim *Limits) *Cell {
	t.Helper()
	wasmPath := BuildCell(t, sourceDir)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec := &manifest.CellSpec{
		SchemaVersion: manifest.CurrentSchemaVersion,
		Name:          name,
		Version:       "0.0.0-test",
		WASMPath:      wasmPath,
	}
	cell, err := Load(context.Background(), spec, nil, lim, logger)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	t.Cleanup(func() { _ = cell.Close(context.Background()) })
	return cell
}

func TestSupervisor_RunawayBounded(t *testing.T) {
	cell := loadUninitedCell(t, "../../testdata/runaway", "runaway",
		&Limits{Interruptible: true, CallTimeout: 30 * time.Second})

	var (
		mu       sync.Mutex
		reported []RunawayReport
	)
	sup := NewSupervisor(cell, "runaway",
		WithBudget(120*time.Millisecond),
		WithRunawayReporter(func(r RunawayReport) {
			mu.Lock()
			reported = append(reported, r)
			mu.Unlock()
		}),
	)

	ctx := context.Background()
	if err := sup.Start(ctx, nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	if sup.State() != CellReady {
		t.Fatalf("state after start = %v, want ready", sup.State())
	}

	// A payload event makes the cell run away. The guard must bound it.
	env := abi.StepEnvelope{CallNumber: 1, WallTime: uint64(time.Now().UnixNano()), Payload: []byte{1, 2, 3, 4}}
	start := time.Now()
	_, err := sup.GuardStep(ctx, env)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrRunaway) {
		t.Fatalf("GuardStep err = %v, want ErrRunaway", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("runaway not bounded promptly: took %v (budget 120ms)", elapsed)
	}
	if sup.State() != CellRunaway {
		t.Errorf("state = %v, want runaway", sup.State())
	}
	if sup.Healthy() {
		t.Error("Healthy() = true after runaway, want false")
	}
	if sup.RunawayCount() != 1 {
		t.Errorf("RunawayCount = %d, want 1", sup.RunawayCount())
	}
	mu.Lock()
	n := len(reported)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("onRunaway fired %d times, want 1", n)
	}
}

func TestSupervisor_HealthyUnaffected(t *testing.T) {
	cell := loadUninitedCell(t, "../../testdata/heartbeat", "heartbeat",
		&Limits{Interruptible: true, CallTimeout: 30 * time.Second})

	sup := NewSupervisor(cell, "heartbeat", WithBudget(500*time.Millisecond))

	ctx := context.Background()
	if err := sup.Start(ctx, nil); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Many guarded steps — all must succeed with zero interference.
	for i := 0; i < 200; i++ {
		env := abi.StepEnvelope{CallNumber: uint64(i), WallTime: uint64(time.Now().UnixNano())}
		if _, err := sup.GuardStep(ctx, env); err != nil {
			t.Fatalf("healthy GuardStep %d: %v", i, err)
		}
	}
	if sup.State() != CellReady {
		t.Errorf("state = %v, want ready", sup.State())
	}
	if !sup.Healthy() {
		t.Error("Healthy() = false, want true")
	}
	if sup.RunawayCount() != 0 {
		t.Errorf("RunawayCount = %d, want 0", sup.RunawayCount())
	}
	if sup.LastOK().IsZero() {
		t.Error("LastOK is zero after successful calls")
	}

	// Liveness monitor stays green over its run.
	var (
		mu    sync.Mutex
		snaps []HealthSnapshot
	)
	mctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	sup.MonitorLiveness(mctx, 20*time.Millisecond, nil, func(s HealthSnapshot) {
		mu.Lock()
		snaps = append(snaps, s)
		mu.Unlock()
	})
	mu.Lock()
	defer mu.Unlock()
	if len(snaps) == 0 {
		t.Fatal("liveness monitor emitted no snapshots")
	}
	for i, s := range snaps {
		if !s.Healthy || s.State != CellReady {
			t.Fatalf("snapshot %d unhealthy: %+v", i, s)
		}
	}
}

func TestSupervisor_Lifecycle(t *testing.T) {
	cell := loadUninitedCell(t, "../../testdata/heartbeat", "heartbeat",
		&Limits{Interruptible: true})

	sup := NewSupervisor(cell, "heartbeat")
	if sup.State() != CellNew {
		t.Fatalf("initial state = %v, want new", sup.State())
	}

	select {
	case <-sup.Ready():
		t.Fatal("ready channel closed before Start")
	default:
	}

	ctx := context.Background()
	if err := sup.Start(ctx, nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-sup.Ready():
	case <-time.After(time.Second):
		t.Fatal("ready channel not closed after Start")
	}
	if sup.State() != CellReady {
		t.Fatalf("state = %v, want ready", sup.State())
	}

	if err := sup.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if sup.State() != CellStopped {
		t.Fatalf("state = %v, want stopped", sup.State())
	}
}
