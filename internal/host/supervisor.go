package host

// Supervisor — a first-increment cell supervisor for the Pulp host. It WRAPS a
// loaded *Cell (it does not modify Cell, and the existing Call/CallTyped/Step
// paths are untouched) and adds three concerns the roadmap's orchestration
// needs to stand on:
//
//   - LIFECYCLE: an explicit state machine New -> Starting -> Ready ->
//     {Runaway|Failed} -> Stopping -> Stopped, with a ready signal. This
//     formalizes the start/ready/stop that run.Main does ad hoc into one
//     addressable object.
//
//   - RUNAWAY GUARD: a per-call step/time budget. Guard runs a cell entry point
//     under a budget-deadline context; when the cell was loaded Interruptible
//     (Limits.Interruptible => wazero WithCloseOnContextDone), a guest that
//     hangs or infinite-loops is TERMINATED by wazero at the deadline instead
//     of wedging the host goroutine + cell mutex forever. Guard classifies that
//     termination, transitions to Runaway, reports it, and leaves the (now
//     wazero-closed) cell marked dead — bounded and reportable, not wedged.
//
//   - HEALTH: a periodic liveness signal. The supervisor tracks the time of the
//     last successful guarded call and exposes Healthy() + a HealthSnapshot;
//     MonitorLiveness runs a cheap guarded probe on a ticker and emits a
//     snapshot each tick, tripping the same Runaway path if the probe overruns.
//
// SCOPE: this is the supervision PRIMITIVE + runaway guard, deliberately not a
// full orchestrator. Wiring the guard into run/run.go's live step loop, a
// cell-authored health export, control-socket surfacing, and a Runaway restart
// policy are follow-ups tracked in adr/cell-supervisor.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BananaLabs-OSS/Pulp/abi"
)

// CellState is the supervised cell's lifecycle state.
type CellState int32

const (
	CellNew      CellState = iota // constructed, not started
	CellStarting                  // Init in flight
	CellReady                     // Init returned 0; serving
	CellRunaway                   // a guarded call exceeded its budget and was killed
	CellStopping                  // Stop in flight
	CellStopped                   // Shutdown + Close complete
	CellFailed                    // Start failed
)

func (s CellState) String() string {
	switch s {
	case CellNew:
		return "new"
	case CellStarting:
		return "starting"
	case CellReady:
		return "ready"
	case CellRunaway:
		return "runaway"
	case CellStopping:
		return "stopping"
	case CellStopped:
		return "stopped"
	case CellFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// ErrRunaway is returned by Guard when a guarded call exceeds its budget — the
// signature of a cell that hung or infinite-looped. For an Interruptible cell
// wazero has already terminated the guest and closed the module by this point.
var ErrRunaway = errors.New("cell runaway: guarded call exceeded its budget and was terminated")

// RunawayReport is handed to the onRunaway hook when a guarded call is bounded.
type RunawayReport struct {
	Cell    string
	Budget  time.Duration
	Elapsed time.Duration
	Err     error // the underlying error the terminated call returned, if any
}

// HealthSnapshot is a point-in-time liveness view of a supervised cell.
type HealthSnapshot struct {
	Cell         string
	State        CellState
	Healthy      bool
	LastOK       time.Time
	RunawayCount uint64
}

// Supervisor wraps a single loaded cell with lifecycle/health/runaway control.
type Supervisor struct {
	cell   *Cell
	name   string
	log    *slog.Logger
	budget time.Duration

	onRunaway func(RunawayReport)

	state        atomic.Int32
	readyCh      chan struct{}
	readyOnce    sync.Once
	lastOK       atomic.Int64 // unixnano of last successful guarded call
	runawayCount atomic.Uint64
}

// SupervisorOption configures a Supervisor at construction.
type SupervisorOption func(*Supervisor)

// WithBudget sets the per-call runaway budget. A call that runs longer than
// this is treated as a runaway. Zero or negative leaves the default (the
// cell's call timeout).
func WithBudget(d time.Duration) SupervisorOption {
	return func(s *Supervisor) {
		if d > 0 {
			s.budget = d
		}
	}
}

// WithRunawayReporter installs a hook invoked (synchronously) whenever a
// guarded call is bounded as a runaway.
func WithRunawayReporter(fn func(RunawayReport)) SupervisorOption {
	return func(s *Supervisor) { s.onRunaway = fn }
}

// WithSupervisorLogger overrides the logger (defaults to the cell's logger).
func WithSupervisorLogger(l *slog.Logger) SupervisorOption {
	return func(s *Supervisor) {
		if l != nil {
			s.log = l
		}
	}
}

// NewSupervisor wraps cell. The default budget is the cell's call timeout.
// Load the cell with Limits.Interruptible=true so the runaway guard can
// actually unwind a hung/looping guest (otherwise Guard still enforces and
// reports the deadline, but cannot terminate the guest — see Guard).
func NewSupervisor(cell *Cell, name string, opts ...SupervisorOption) *Supervisor {
	s := &Supervisor{
		cell:    cell,
		name:    name,
		log:     cell.log,
		budget:  cell.callTimeout,
		readyCh: make(chan struct{}),
	}
	if s.budget <= 0 {
		s.budget = DefaultCallTimeout
	}
	s.state.Store(int32(CellNew))
	for _, o := range opts {
		o(s)
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// State returns the current lifecycle state.
func (s *Supervisor) State() CellState { return CellState(s.state.Load()) }

// Ready returns a channel closed once the cell reaches Ready (or Failed —
// callers should check State after it closes).
func (s *Supervisor) Ready() <-chan struct{} { return s.readyCh }

// RunawayCount returns how many guarded calls have been bounded as runaways.
func (s *Supervisor) RunawayCount() uint64 { return s.runawayCount.Load() }

// LastOK returns the time of the most recent successful guarded call (zero if
// none yet).
func (s *Supervisor) LastOK() time.Time {
	ns := s.lastOK.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// Healthy reports whether the cell is Ready and has not gone Runaway/Failed.
func (s *Supervisor) Healthy() bool { return s.State() == CellReady }

// Snapshot returns a point-in-time health view.
func (s *Supervisor) Snapshot() HealthSnapshot {
	return HealthSnapshot{
		Cell:         s.name,
		State:        s.State(),
		Healthy:      s.Healthy(),
		LastOK:       s.LastOK(),
		RunawayCount: s.runawayCount.Load(),
	}
}

func (s *Supervisor) markReady() {
	s.readyOnce.Do(func() { close(s.readyCh) })
}

// Start drives the cell's Init and transitions New/Starting -> Ready. On Init
// failure the state becomes Failed. The ready channel is closed either way.
func (s *Supervisor) Start(ctx context.Context, config []byte) error {
	s.state.Store(int32(CellStarting))
	if err := s.cell.Init(ctx, config); err != nil {
		s.state.Store(int32(CellFailed))
		s.markReady()
		return err
	}
	s.state.Store(int32(CellReady))
	s.lastOK.Store(time.Now().UnixNano())
	s.markReady()
	s.log.Info("supervisor: cell ready", "cell", s.name, "budget", s.budget)
	return nil
}

// Stop transitions to Stopping, runs the cell's Shutdown (best-effort) and
// Close, then Stopped. Safe to call after a runaway (the cell is already
// closed; Shutdown/Close no-op or error harmlessly).
func (s *Supervisor) Stop(ctx context.Context) error {
	s.state.Store(int32(CellStopping))
	shutErr := s.cell.Shutdown(ctx)
	closeErr := s.cell.Close(ctx)
	s.state.Store(int32(CellStopped))
	s.markReady()
	return errors.Join(shutErr, closeErr)
}

// Guard runs fn under the per-call runaway budget. fn should invoke exactly one
// cell entry point with the context it is handed (so the budget deadline
// propagates into the wasm call). Classification:
//
//   - fn returns nil                       -> success; lastOK advances.
//   - fn returns an error, budget expired  -> RUNAWAY: state -> Runaway (from
//     Ready), runaway count++, onRunaway fired, ErrRunaway returned. For an
//     Interruptible cell wazero has already terminated the guest + closed the
//     module; for a non-interruptible cell the guest was NOT unwound (the host
//     goroutine may still be blocked in fn until the call returns on its own) —
//     Guard can only report, hence supervised cells SHOULD be Interruptible.
//   - fn returns an error, budget intact   -> a normal cell error; returned
//     unchanged (not a runaway).
//
// A parent-ctx cancellation (Stop, host shutdown) is never misclassified as a
// runaway: it is only a runaway when the budget deadline fired AND the parent
// ctx is not itself done.
func (s *Supervisor) Guard(ctx context.Context, fn func(ctx context.Context) error) error {
	budgetCtx, cancel := context.WithTimeout(ctx, s.budget)
	start := time.Now()
	err := fn(budgetCtx)
	elapsed := time.Since(start)
	// Cancel AFTER the call returns — this is the ordering that keeps an
	// Interruptible reactor alive across successful calls (proven: 500 calls).
	cancel()

	if err == nil {
		s.lastOK.Store(time.Now().UnixNano())
		return nil
	}
	// Runaway iff our budget deadline fired and the parent context did not.
	if budgetCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		s.runawayCount.Add(1)
		s.state.CompareAndSwap(int32(CellReady), int32(CellRunaway))
		s.log.Error("supervisor: RUNAWAY bounded",
			"cell", s.name, "budget", s.budget, "elapsed", elapsed, "err", err)
		if s.onRunaway != nil {
			s.onRunaway(RunawayReport{Cell: s.name, Budget: s.budget, Elapsed: elapsed, Err: err})
		}
		return ErrRunaway
	}
	return err
}

// GuardStep runs one cell.Step under the runaway budget.
func (s *Supervisor) GuardStep(ctx context.Context, env abi.StepEnvelope) (int32, error) {
	var out int32
	err := s.Guard(ctx, func(c context.Context) error {
		h, e := s.cell.Step(c, env)
		out = h
		return e
	})
	return out, err
}

// GuardCall runs one cell.Call (opaque msgpack sibling path) under the budget.
func (s *Supervisor) GuardCall(ctx context.Context, funcName string, args []byte) ([]byte, error) {
	var out []byte
	err := s.Guard(ctx, func(c context.Context) error {
		b, e := s.cell.Call(c, funcName, args)
		out = b
		return e
	})
	return out, err
}

// idleStepProbe is the default liveness probe: a nil-payload Step. A healthy
// reactor returns in microseconds; a wedged one trips the runaway budget.
func (s *Supervisor) idleStepProbe(ctx context.Context) error {
	env := abi.StepEnvelope{WallTime: uint64(time.Now().UnixNano())}
	_, err := s.cell.Step(ctx, env)
	return err
}

// MonitorLiveness runs a guarded liveness probe on an interval, emitting a
// HealthSnapshot to emit (if non-nil) after each probe. If probe is nil the
// default idle-Step probe is used. The loop exits when ctx is done or the cell
// leaves the Ready state (Runaway/Failed/Stopped). Run it in its own goroutine.
func (s *Supervisor) MonitorLiveness(ctx context.Context, interval time.Duration, probe func(context.Context) error, emit func(HealthSnapshot)) {
	if probe == nil {
		probe = s.idleStepProbe
	}
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.State() != CellReady {
				return
			}
			_ = s.Guard(ctx, probe)
			if emit != nil {
				emit(s.Snapshot())
			}
			if s.State() != CellReady {
				return
			}
		}
	}
}
