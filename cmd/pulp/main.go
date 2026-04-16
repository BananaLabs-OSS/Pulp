// Pulp v0.2 — Application OS runtime.
//
// Loads a plugin from a pulp.plugin.toml manifest, serializes config to
// MessagePack, calls pulp_init with the encoded config, calls pulp_step in
// a loop with the step envelope, calls pulp_shutdown on interrupt, exits.
// Still no transport; capabilities declared in the manifest are parsed but
// not yet bound.
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
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

func main() {
	var manifestPath string
	flag.StringVar(&manifestPath, "manifest", "", "path to pulp.plugin.toml")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if manifestPath == "" {
		logger.Error("missing required flag -manifest <path-to-pulp.plugin.toml>")
		os.Exit(2)
	}

	spec, err := manifest.Load(manifestPath)
	if err != nil {
		logger.Error("manifest load failed", "err", err)
		os.Exit(1)
	}

	logger.Info("pulp boot",
		"plugin", spec.Name,
		"version", spec.Version,
		"manifest", spec.ManifestPath,
		"wasm", spec.WASMPath,
		"capabilities", spec.Capabilities,
		"provides", spec.Provides,
		"consumes", spec.Consumes,
	)

	configBytes, err := manifest.EncodeConfig(spec.Config)
	if err != nil {
		logger.Error("config encode failed", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	plugin, err := host.Load(ctx, spec.WASMPath, logger)
	if err != nil {
		logger.Error("load failed", "err", err)
		os.Exit(1)
	}
	defer plugin.Close(context.Background())

	if err := plugin.Init(ctx, configBytes); err != nil {
		logger.Error("init failed", "err", err)
		os.Exit(1)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, shutdownSignals()...)

	stepLoop(ctx, plugin, sigCh, logger)

	if last, ok := plugin.ProbeLastCall(context.Background()); ok {
		logger.Info("probe last envelope", "last_call", last)
	}
	if marker, ok := plugin.ProbeConfigMarker(context.Background()); ok {
		logger.Info("probe config marker", "marker", marker)
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
// decides whether to process or skip. runtime.Gosched yields to avoid
// busy-spinning the CPU.
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
