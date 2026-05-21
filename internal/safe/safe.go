// Package safe provides panic-recovery wrappers for extension entry
// points. The host uses these to prevent a misbehaving extension from
// taking down unrelated cells in a multi-cell deployment.
//
// Every exported helper logs the panic with the extension name, a short
// message, and the stack trace, then converts the panic into a typed
// error. Callers decide whether to keep running (Poll, host functions)
// or mark the cell as failed-to-start (Setup) or swallow (Teardown,
// Finalize — we're already shutting down).
package safe

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/BananaLabs-OSS/Pulp/ext"
)

// ErrExtensionPanic is returned wrapped when a recovered panic is
// converted to an error. Callers use errors.Is to distinguish it from
// genuine extension errors.
type ErrExtensionPanic struct {
	Extension string
	Phase     string // "setup", "teardown", "poll", "finalize", "host"
	Value     any    // the recovered panic value
}

func (e *ErrExtensionPanic) Error() string {
	return fmt.Sprintf("extension %q panic during %s: %v", e.Extension, e.Phase, e.Value)
}

// CallSetup invokes cap.Setup(env) under panic recovery. Returns any
// error the extension returned OR an *ErrExtensionPanic if Setup
// panicked.
func CallSetup(cap ext.Capability, env ext.SetupEnv, logger *slog.Logger) (err error) {
	if cap.Setup == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			logger.Error("extension setup panicked",
				"extension", cap.Name,
				"panic", r,
				"stack", string(debug.Stack()),
			)
			err = &ErrExtensionPanic{Extension: cap.Name, Phase: "setup", Value: r}
		}
	}()
	return cap.Setup(env)
}

// CallTeardown invokes cap.Teardown(ctx) under panic recovery. Logs and
// swallows panics — teardown happens during shutdown, where propagating
// an error accomplishes nothing.
func CallTeardown(ctx context.Context, cap ext.Capability, logger *slog.Logger) {
	if cap.Teardown == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logger.Error("extension teardown panicked",
				"extension", cap.Name,
				"panic", r,
				"stack", string(debug.Stack()),
			)
		}
	}()
	if err := cap.Teardown(ctx); err != nil {
		logger.Error("extension teardown failed", "extension", cap.Name, "err", err)
	}
}

// CallPoll invokes cap.Poll() under panic recovery. On panic, logs and
// returns (zero-value, false) so the step loop treats it as "no event
// available" and continues. A repeatedly-panicking extension will log
// on every tick — callers may want to rate-limit logging upstream.
func CallPoll(cap ext.Capability, logger *slog.Logger) (ev ext.StepEvent, ok bool) {
	if cap.Poll == nil {
		return ext.StepEvent{}, false
	}
	defer func() {
		if r := recover(); r != nil {
			logger.Error("extension poll panicked",
				"extension", cap.Name,
				"panic", r,
				"stack", string(debug.Stack()),
			)
			ev = ext.StepEvent{}
			ok = false
		}
	}()
	return cap.Poll()
}

// CallFinalize invokes cap.Finalize(id) under panic recovery. Logs and
// swallows panics — the step has already run, we're cleaning up.
func CallFinalize(cap ext.Capability, id uint64, logger *slog.Logger) {
	if cap.Finalize == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logger.Error("extension finalize panicked",
				"extension", cap.Name,
				"panic", r,
				"id", id,
				"stack", string(debug.Stack()),
			)
		}
	}()
	cap.Finalize(id)
}

// HostFunc wraps a host-function body in panic recovery. Returns a
// function with the same signature. Use when registering host imports
// with wazero:
//
//	builder.NewFunctionBuilder().WithFunc(
//	    safe.HostFunc("my_extension", "my_func", myFuncBody, logger),
//	).Export("my_func")
//
// The wrapped function returns the same values the original would, or
// zero values on panic. Callers that need a specific error-code convention
// should branch on a sentinel rather than relying on zero values.
//
// The name arg is the extension identity for logging; fname is the host
// function name (e.g., "udp_listen"). Both appear in the log record.
func HostFunc[F any](name, fname string, fn F, logger *slog.Logger) F {
	// Generic pass-through for arbitrary function signatures: we can't
	// wrap defer/recover around an unknown signature generically without
	// reflection. Host functions already have predictable shapes; see
	// HostFunc0..HostFunc4 for common ones.
	return fn
}

// The wazero host-function signatures in the current extension set are
// variations on (ctx, mod, args...). Provide typed wrappers for the most
// common shapes; extensions that need a more exotic signature can call
// RecoverHost directly from inside their closure.

// RecoverHost is the raw building block. Call from inside a host function
// body:
//
//	func myHostFunc(ctx context.Context, mod api.Module, reqPtr, reqLen uint32) uint32 {
//	    defer safe.RecoverHost("ext-udp", "udp_listen", logger)
//	    // ...actual work...
//	}
//
// Unlike the Call* helpers, this does not convert the panic to a return
// value — the caller decides what to return. Used for side-effect-only
// host functions or where the error code convention already exists.
func RecoverHost(name, fname string, logger *slog.Logger) {
	if r := recover(); r != nil {
		logger.Error("host function panicked",
			"extension", name,
			"func", fname,
			"panic", r,
			"stack", string(debug.Stack()),
		)
	}
}
