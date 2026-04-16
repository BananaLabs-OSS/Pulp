// Pulp v0.3 — Application OS runtime.
//
// Loads a plugin from a pulp.plugin.toml manifest, serializes config to
// MessagePack, calls pulp_init with the encoded config, calls pulp_step in
// a loop with the step envelope, calls pulp_shutdown on interrupt, exits.
// If the plugin declares transport.http.inbound, an HTTP server is started
// and incoming requests are delivered through the step envelope payload.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"time"

	"github.com/BananaLabs-OSS/Pulp/internal/abi"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/BananaLabs-OSS/Pulp/internal/transport"
)

func main() {
	var manifestPath string
	var httpPort int
	flag.StringVar(&manifestPath, "manifest", "", "path to pulp.plugin.toml")
	flag.IntVar(&httpPort, "http-port", 8080, "HTTP inbound listener port")
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

	registry := host.NewRegistry()

	var httpServer *transport.HTTPServer
	if hasCapability(spec, "transport.http.inbound") {
		httpServer = transport.NewHTTPServer(fmt.Sprintf(":%d", httpPort), logger)
		registry.Gated(transport.HTTPInboundCapability(httpServer))
	}

	if hasCapability(spec, "transport.http.outbound") {
		registry.Gated(transport.HTTPOutboundCapability(transport.NewFetcher(logger)))
	}

	plugin, err := host.Load(ctx, spec, registry, logger)
	if err != nil {
		logger.Error("load failed", "err", err)
		os.Exit(1)
	}
	defer plugin.Close(context.Background())

	if err := plugin.Init(ctx, configBytes); err != nil {
		logger.Error("init failed", "err", err)
		os.Exit(1)
	}

	if httpServer != nil {
		if err := httpServer.Start(ctx); err != nil {
			logger.Error("http start failed", "err", err)
			os.Exit(1)
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpServer.Stop(shutdownCtx)
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, shutdownSignals()...)

	stepLoop(ctx, plugin, httpServer, sigCh, logger)

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
// decides whether to process or skip. If an HTTP server is attached, each
// step drains one pending request from its queue and delivers it as the
// envelope payload; the plugin is expected to call http_respond during
// that step. runtime.Gosched yields to avoid busy-spinning the CPU.
func stepLoop(ctx context.Context, plugin *host.Plugin, httpServer *transport.HTTPServer, sigCh <-chan os.Signal, logger *slog.Logger) {
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

		var payload []byte
		var activeReqID uint64
		var hasRequest bool
		if httpServer != nil {
			if req, ok := httpServer.PopRequest(); ok {
				encoded, err := abi.EncodeHTTPRequest(req)
				if err != nil {
					logger.Error("encode http request", "id", req.ID, "err", err)
					httpServer.Finalize(req.ID)
				} else {
					payload = encoded
					activeReqID = req.ID
					hasRequest = true
				}
			}
		}

		env := abi.StepEnvelope{
			CallNumber: callNumber,
			WallTime:   uint64(time.Now().UnixNano()),
			Payload:    payload,
		}

		handle, err := plugin.Step(ctx, env)
		if err != nil {
			logger.Error("step failed", "call_number", callNumber, "err", err)
		}

		if hasRequest {
			httpServer.Finalize(activeReqID)
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

func hasCapability(spec *manifest.PluginSpec, name string) bool {
	for _, c := range spec.Capabilities {
		if c == name {
			return true
		}
	}
	return false
}
