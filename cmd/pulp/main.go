// Pulp v0.1 — Application OS runtime.
//
// Loads a single WASM file, calls pulp_init, calls pulp_step in a loop with
// the step envelope, calls pulp_shutdown on SIGTERM/SIGINT, exits cleanly.
// No plugins included. No transport. Runtime only.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"time"

	"github.com/BananaLabs-OSS/Pulp/internal/abi"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
)

func main() {
	var wasmPath string
	flag.StringVar(&wasmPath, "plugin", "", "path to the WASM plugin file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if wasmPath == "" {
		logger.Error("missing required flag -plugin <path-to-wasm>")
		os.Exit(2)
	}

	logger.Info("pulp boot", "version", "0.1.0", "plugin", wasmPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	plugin, err := host.Load(ctx, wasmPath, logger)
	if err != nil {
		logger.Error("load failed", "err", err)
		os.Exit(1)
	}
	defer plugin.Close(context.Background())

	if err := plugin.Init(ctx, nil); err != nil {
		logger.Error("init failed", "err", err)
		os.Exit(1)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, shutdownSignals()...)

	stepLoop(ctx, plugin, sigCh, logger)

	// Optional probe — if the plugin exposes envelope-inspection exports, read
	// them so the operator can confirm that step envelopes round-tripped
	// correctly. Missing exports are not an error; production plugins will not
	// expose these.
	if last, ok := plugin.ProbeLastCall(context.Background()); ok {
		logger.Info("probe last envelope", "last_call", last)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := plugin.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown failed", "err", err)
		os.Exit(1)
	}

	logger.Info("pulp exit clean")
}

// stepLoop calls pulp_step repeatedly until a signal arrives on sigCh.
// Plugin owns its own cadence — it reads wall_time from the envelope and
// decides whether to process or skip. Pulp yields between calls to avoid
// busy-spinning the CPU; Go's goroutine scheduler handles fairness across
// future plugins.
func stepLoop(ctx context.Context, plugin *host.Plugin, sigCh <-chan os.Signal, logger *slog.Logger) {
	var callNumber uint64
	for {
		select {
		case sig := <-sigCh:
			logger.Info("signal received", "signal", sig.String())
			return
		case <-ctx.Done():
			return
		default:
		}

		env := abi.StepEnvelope{
			CallNumber: callNumber,
			WallTime:   uint64(time.Now().UnixNano()),
			Payload:    nil,
		}

		handle, err := plugin.Step(ctx, env)
		if err != nil {
			logger.Error("step failed", "call_number", callNumber, "err", err)
			// Non-fatal: log and continue per the design doc.
			// If the plugin trapped, wazero will have marked the module failed
			// and subsequent calls will error out the same way until shutdown.
		}

		if handle != 0 {
			logger.Debug("step output", "call_number", callNumber, "handle", handle)
		}

		if callNumber%10000 == 0 {
			logger.Info("step heartbeat", "call_number", callNumber)
		}

		callNumber++
		runtime.Gosched()
	}
}
