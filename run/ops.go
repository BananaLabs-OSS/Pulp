package run

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/safe"
)

// runtimeOps implements the controlOps interface for the control
// socket. It owns per-cell shutdown: cancels the cell's context,
// waits for its step loop to exit, calls cell.Shutdown, and invokes
// each extension's TeardownCell hook (if set) to drop cell-scoped
// state while leaving other cells' state intact.
type runtimeOps struct {
	runtimes      map[string]*cellRuntime
	allCaps       []ext.Capability
	declaredUnion map[string]bool
	logger        *slog.Logger

	mu      sync.Mutex
	stopped map[string]bool // cell names that have already been shut down

	// watchdogStop cancels the allShutdown poller goroutine on process
	// exit so it doesn't leak when the signal path is taken instead of
	// the control-socket path.
	watchdogStop     chan struct{}
	watchdogStopOnce sync.Once
}

func (o *runtimeOps) status() []ctlStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]ctlStatus, 0, len(o.runtimes))
	for name, rt := range o.runtimes {
		state := "ready"
		switch {
		case rt.failed:
			state = "failed"
		case o.stopped[name]:
			state = "stopped"
		}
		out = append(out, ctlStatus{
			Name:  name,
			State: state,
			Steps: rt.callNumber,
		})
	}
	return out
}

func (o *runtimeOps) shutdownCell(name string) error {
	o.mu.Lock()
	rt, ok := o.runtimes[name]
	if !ok {
		o.mu.Unlock()
		return fmt.Errorf("unknown cell: %q", name)
	}
	if o.stopped == nil {
		o.stopped = map[string]bool{}
	}
	if o.stopped[name] {
		o.mu.Unlock()
		return fmt.Errorf("cell %q already stopped", name)
	}
	o.stopped[name] = true
	o.mu.Unlock()

	o.logger.Info("control: shutting down cell", "cell", name)

	// 1. Cancel the cell's context — its step goroutine exits.
	rt.cancel()

	// 2. Shutdown the WASM module. Uses a fresh context because rt.ctx
	// is already cancelled.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if rt.cell != nil {
		if err := rt.cell.Shutdown(shutdownCtx); err != nil {
			o.logger.Error("control: cell shutdown failed", "cell", name, "err", err)
		}
		rt.cell.Close(context.Background())
		rt.cell = nil
	}

	// 3. Call TeardownCell on each capability the cell declared —
	// extensions that maintain per-cell state (routes, sockets) drop
	// just this cell's entries.
	for _, c := range o.allCaps {
		if !rt.declared[c.Name] || c.TeardownCell == nil {
			continue
		}
		if err := c.TeardownCell(shutdownCtx, name); err != nil {
			o.logger.Warn("control: capability teardown_cell failed",
				"cell", name, "capability", c.Name, "err", err)
		}
	}

	// 4. Drain any pending events in the cell's channel under panic
	// recovery — Finalize is best-effort here.
	drain := func() {
		for {
			select {
			case re := <-rt.events:
				for _, c := range re.caps {
					safe.CallFinalize(c, re.ev.ID, o.logger)
				}
			default:
				return
			}
		}
	}
	drain()

	o.logger.Info("control: cell stopped", "cell", name)
	return nil
}

func (o *runtimeOps) shutdownAll() error {
	o.mu.Lock()
	names := make([]string, 0, len(o.runtimes))
	for n := range o.runtimes {
		names = append(names, n)
	}
	o.mu.Unlock()

	for _, n := range names {
		o.mu.Lock()
		already := o.stopped[n]
		o.mu.Unlock()
		if already {
			continue
		}
		_ = o.shutdownCell(n)
	}
	return nil
}

// allShutdown returns a channel that closes when every non-failed
// cell has been stopped (either via the control socket or naturally).
// run.Main selects on this channel alongside the signal channel so a
// control-socket shutdown_all can exit the host cleanly without a
// signal.
//
// The poller goroutine is cancelled via stopWatchdog when run.Main
// takes the signal path — without that, a signal-driven exit would
// leak the goroutine until process teardown.
func (o *runtimeOps) allShutdown() <-chan struct{} {
	ch := make(chan struct{})
	o.mu.Lock()
	if o.watchdogStop == nil {
		o.watchdogStop = make(chan struct{})
	}
	stop := o.watchdogStop
	o.mu.Unlock()

	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			o.mu.Lock()
			total := 0
			stoppedCount := 0
			for name, rt := range o.runtimes {
				if rt.failed {
					continue
				}
				total++
				if o.stopped[name] {
					stoppedCount++
				}
			}
			o.mu.Unlock()
			if total > 0 && stoppedCount == total {
				close(ch)
				return
			}
		}
	}()
	return ch
}

// isStopped reports whether a cell has been shut down via the control
// socket. Used by run.Main's final shutdown loop to skip cells whose
// Shutdown + Close + TeardownCell have already run, avoiding a race on
// rt.cell being cleared under the runtimeOps lock.
func (o *runtimeOps) isStopped(name string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stopped[name]
}

// stopWatchdog cancels the allShutdown poller goroutine. Idempotent.
func (o *runtimeOps) stopWatchdog() {
	o.watchdogStopOnce.Do(func() {
		o.mu.Lock()
		ch := o.watchdogStop
		o.mu.Unlock()
		if ch != nil {
			close(ch)
		}
	})
}
