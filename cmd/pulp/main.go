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
	"path/filepath"
	"runtime"
	"time"

	"github.com/BananaLabs-OSS/Pulp/internal/abi"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/BananaLabs-OSS/Pulp/internal/storage"
	"github.com/BananaLabs-OSS/Pulp/internal/transport"
)

func main() {
	var manifestPath string
	var httpPort int
	var httpCert, httpKey string
	var storageRoot string
	flag.StringVar(&manifestPath, "manifest", "", "path to pulp.plugin.toml")
	flag.IntVar(&httpPort, "http-port", 8080, "HTTP inbound listener port")
	flag.StringVar(&httpCert, "http-cert", "", "TLS certificate file (PEM); requires -http-key")
	flag.StringVar(&httpKey, "http-key", "", "TLS key file (PEM); requires -http-cert")
	flag.StringVar(&storageRoot, "storage-root", "./data", "root directory for plugin-scoped storage")
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

	needsPort := hasCapability(spec, "transport.http.inbound") ||
		hasCapability(spec, "transport.ws.inbound") ||
		hasCapability(spec, "transport.sse")

	var httpServer *transport.HTTPServer
	var wsServer *transport.WSServer
	var sseServer *transport.SSEServer
	if needsPort {
		httpServer = transport.NewHTTPServer(fmt.Sprintf(":%d", httpPort), logger)
		if httpCert != "" || httpKey != "" {
			if err := httpServer.EnableTLS(httpCert, httpKey); err != nil {
				logger.Error("tls config failed", "err", err)
				os.Exit(1)
			}
		}
	}

	// Register every known gated capability with the registry. When the
	// plugin declares the capability, the real host imports bind; when
	// it does not, the capability's Stub binds no-ops returning error
	// code 99. This keeps the plugin's WASM binary loadable even when
	// Go's DCE leaves references to unused host imports (common when
	// plugins depend on pulpgin, which references ws.* unconditionally).
	if hasCapability(spec, "transport.ws.inbound") {
		wsServer = transport.NewWSServer(logger)
		if httpServer != nil {
			httpServer.AttachWebSocket(wsServer)
		}
	}
	if hasCapability(spec, "transport.sse") {
		sseServer = transport.NewSSEServer(logger)
		if httpServer != nil {
			httpServer.AttachSSE(sseServer)
		}
	}
	registry.Gated(transport.HTTPInboundCapability(httpServer))
	registry.Gated(transport.WSInboundCapability(wsServer))
	registry.Gated(transport.SSECapability(sseServer))
	registry.Gated(transport.HTTPOutboundCapability(transport.NewFetcher(logger)))

	if hasCapability(spec, "storage.fs") {
		pluginRoot := filepath.Join(storageRoot, spec.Name)
		fs, err := storage.NewFS(pluginRoot, logger)
		if err != nil {
			logger.Error("storage.fs init failed", "err", err)
			os.Exit(1)
		}
		logger.Info("storage.fs ready", "root", fs.Root())
		registry.Gated(storage.FSCapability(fs))
	} else {
		registry.Gated(storage.FSCapability(nil))
	}

	var sqliteDB *storage.SQLite
	if hasCapability(spec, "storage.sqlite") {
		dbPath := filepath.Join(storageRoot, spec.Name, "data.db")
		db, err := storage.NewSQLite(dbPath, logger)
		if err != nil {
			logger.Error("storage.sqlite init failed", "err", err)
			os.Exit(1)
		}
		sqliteDB = db
		logger.Info("storage.sqlite ready", "path", db.Path())
		registry.Gated(storage.SQLiteCapability(db))
	} else {
		registry.Gated(storage.SQLiteCapability(nil))
	}

	plugin, err := host.Load(ctx, spec, registry, logger)
	if err != nil {
		logger.Error("load failed", "err", err)
		os.Exit(1)
	}
	defer plugin.Close(context.Background())
	if sqliteDB != nil {
		defer sqliteDB.Close()
	}

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
			if wsServer != nil {
				wsServer.Stop()
			}
			if sseServer != nil {
				sseServer.Stop()
			}
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, shutdownSignals()...)

	stepLoop(ctx, plugin, httpServer, wsServer, sigCh, logger)

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
func stepLoop(ctx context.Context, plugin *host.Plugin, httpServer *transport.HTTPServer, wsServer *transport.WSServer, sigCh <-chan os.Signal, logger *slog.Logger) {
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
				reqBytes, err := abi.EncodeHTTPRequest(req)
				if err != nil {
					logger.Error("encode http request", "id", req.ID, "err", err)
					httpServer.Finalize(req.ID)
				} else if ev, err := abi.EncodeStepEvent(abi.EventHTTPRequest, reqBytes); err != nil {
					logger.Error("encode step event", "id", req.ID, "err", err)
					httpServer.Finalize(req.ID)
				} else {
					payload = ev
					activeReqID = req.ID
					hasRequest = true
				}
			}
		}
		if payload == nil && wsServer != nil {
			if ev, ok := wsServer.PopEvent(); ok {
				payload = ev
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
		if payload == nil {
			// No event this step — idle. Sleep briefly so the loop does
			// not burn CPU (and, more urgently, so per-step allocations
			// in the plugin do not outrun Go-WASM's GC). 200µs caps the
			// idle rate at ~5k steps/sec while still giving sub-ms
			// dispatch latency on real events.
			time.Sleep(200 * time.Microsecond)
		} else {
			runtime.Gosched()
		}
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
