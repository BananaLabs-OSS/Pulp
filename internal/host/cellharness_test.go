package host

// Cell test harness — instantiate a real WASM cell in the Pulp host with
// real ext host-functions wired, drive an HTTP request into it, and assert
// the response. This is the reusable surface that lets the deployed cells
// (Bananasplit, Hytale-Auth, Hand, …) be tested end-to-end instead of only
// as native dead-tree unit tests.
//
// Why this lives in internal/host as a _test.go file: the manifest, abi, and
// host packages are internal to the Pulp module, and Pulp already depends on
// Pulp-ext-http (see go.mod replace). A test file here can therefore wire the
// real HTTP transport capability with zero new module plumbing. The blank
// import below registers transport.http.{inbound,outbound} into ext.All(),
// which NewRegistry() folds into the gated capability set automatically.
//
// HOW TO REUSE for another cell (recipe):
//   1. Have a cell whose pulp.cell.toml declares transport.http.inbound
//      (and optionally .outbound). Build it once to wasm — BuildCell does
//      this for you (GOOS=wasip1 GOARCH=wasm go build).
//   2. Call StartCellHTTP(t, CellHarnessConfig{...}) with the cell source
//      dir, manifest name, declared capabilities, and a [config] map (this
//      is where you inject things like service_token to exercise an
//      audit-fixed auth path).
//   3. Use the returned *CellHarness.Do / .URL to fire real HTTP requests at
//      the cell and assert status/body. The harness owns a real net listener
//      on an ephemeral port, runs a pump goroutine (Poll→Step→Finalize), and
//      tears everything down on t.Cleanup.
//
// For cells needing storage.sqlite / storage.fs / entropy.read, add the
// corresponding blank import (Pulp-ext-sqlite, Pulp-ext-fs, Pulp-ext-entropy)
// and list the capability in Capabilities. Those exts back onto a temp dir /
// in-memory store, so no external backend is required. ext-http is proven
// here; the others follow the identical pattern.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/BananaLabs-OSS/Pulp/abi"
	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"

	// Registers transport.http.{inbound,outbound,ws,sse} into ext.All().
	// The harness wires the inbound capability's real Setup (starts an HTTP
	// listener) + Poll/Finalize (feeds requests into the cell's step loop).
	_ "github.com/BananaLabs-OSS/Pulp-ext-http"
)

// CellHarnessConfig describes the cell to build, load, and drive.
type CellHarnessConfig struct {
	// SourceDir is the directory containing the cell's main package
	// (the one with pulp.cell.toml + main.go). Built with GOOS=wasip1.
	SourceDir string
	// Name is the cell's manifest name (used as the wazero module name and
	// the cell identity ext-http keys routes by). Any non-empty string.
	Name string
	// Capabilities the cell declares (e.g. transport.http.inbound). The
	// harness binds exactly these via the Registry; anything the cell
	// imports but does not declare fails Load loudly, same as production.
	Capabilities []string
	// Config is the cell's [config] table. Injected as MessagePack into
	// pulp_init exactly as run.Main does via manifest.EncodeConfig. This is
	// where you set service_token, urls, etc. to exercise a specific path.
	Config map[string]any
}

// CellHarness is a running cell with a real HTTP listener in front of it.
type CellHarness struct {
	URL    string // http://127.0.0.1:<port>
	cell   *Cell
	client *http.Client

	cancel  context.CancelFunc
	pumpWG  sync.WaitGroup
	t       *testing.T
	httpCap ext.Capability
}

// BuildCell compiles the cell at sourceDir to a wasip1/wasm module and
// returns the output path. The binary is written under t.TempDir so it is
// cleaned up automatically. Build failures fail the test immediately — a
// cell that does not compile to wasm cannot be harnessed.
func BuildCell(t *testing.T, sourceDir string) string {
	t.Helper()
	abs, err := filepath.Abs(sourceDir)
	if err != nil {
		t.Fatalf("resolve source dir: %v", err)
	}
	out := filepath.Join(t.TempDir(), "cell.wasm")
	// -buildmode=c-shared produces a wasip1 REACTOR (exports _initialize +
	// keeps the module alive) rather than a command (_start → main → exit).
	// Pulp cells are reactors: pulp_init/step/on_call are called repeatedly
	// after instantiation, so the module must not exit. This matches the
	// canonical build in cmd/pulp/http_integration_test.go (buildEchoWASM).
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, ".")
	cmd.Dir = abs
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build cell %s to wasm: %v\n%s", sourceDir, err, stderr.String())
	}
	return out
}

// freePort grabs an ephemeral TCP port by binding and immediately closing.
// There is an inherent race (another process could grab it before the cell
// server binds) but it is vanishingly small for a serial test run and is the
// standard Go test idiom.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// httpInboundCapability returns the registered transport.http.inbound
// capability from ext.All() (populated by the blank import above).
func httpInboundCapability(t *testing.T) ext.Capability {
	t.Helper()
	for _, c := range ext.All() {
		if c.Name == "transport.http.inbound" {
			return c
		}
	}
	t.Fatal("transport.http.inbound capability not registered (missing ext-http import?)")
	return ext.Capability{}
}

// StartCellHTTP builds the cell, starts the real HTTP transport on an
// ephemeral port, loads + Inits the cell in the Pulp host, and starts a pump
// goroutine that drives inbound HTTP requests through the cell's step loop.
// Everything is torn down via t.Cleanup. The cell is reachable at h.URL.
func StartCellHTTP(t *testing.T, cfg CellHarnessConfig) *CellHarness {
	t.Helper()

	wasmPath := BuildCell(t, cfg.SourceDir)

	// ext-http reads HTTP_PORT during Setup. Tests run serially within a
	// package by default; we set the env var around Setup. (If a project
	// runs host tests with -parallel, gate them behind a mutex — ext-http's
	// `server` is a package global.)
	port := freePort(t)
	t.Setenv("HTTP_PORT", fmt.Sprintf("%d", port))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	httpCap := httpInboundCapability(t)
	if httpCap.Setup != nil {
		if err := httpCap.Setup(ext.SetupEnv{
			StorageRoot: t.TempDir(),
			Logger:      logger,
		}); err != nil {
			t.Fatalf("http capability setup: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	spec := &manifest.CellSpec{
		SchemaVersion: manifest.CurrentSchemaVersion,
		Name:          cfg.Name,
		Version:       "0.0.0-test",
		Capabilities:  cfg.Capabilities,
		Config:        cfg.Config,
		WASMPath:      wasmPath,
	}

	registry := NewRegistry()
	for _, c := range ext.All() {
		registry.Gated(c)
	}

	configBytes, err := manifest.EncodeConfig(cfg.Config)
	if err != nil {
		cancel()
		t.Fatalf("encode config: %v", err)
	}

	cell, err := Load(ctx, spec, registry, nil, logger)
	if err != nil {
		cancel()
		t.Fatalf("load cell: %v", err)
	}
	if err := cell.Init(ctx, configBytes); err != nil {
		cell.Close(context.Background())
		cancel()
		t.Fatalf("init cell: %v", err)
	}

	h := &CellHarness{
		URL:     fmt.Sprintf("http://127.0.0.1:%d", port),
		cell:    cell,
		client:  &http.Client{Timeout: 5 * time.Second},
		cancel:  cancel,
		t:       t,
		httpCap: httpCap,
	}

	// Pump: poll the http capability for inbound requests, step them into
	// the cell, then Finalize. This is exactly run.Main's step loop, minus
	// the multi-cell fanout (single cell here). An idle step keeps the
	// cell's own wall-time/tick logic advancing.
	h.pumpWG.Add(1)
	go h.pump(ctx)

	t.Cleanup(h.stop)
	return h
}

func (h *CellHarness) pump(ctx context.Context) {
	defer h.pumpWG.Done()
	var callNum uint64
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		ev, ok := ext.Capability(h.httpCap).Poll()
		if !ok {
			// No pending request: submit an idle step so cell tickers
			// advance, then back off briefly.
			h.step(ctx, callNum, "", nil)
			callNum++
			time.Sleep(200 * time.Microsecond)
			continue
		}
		h.step(ctx, callNum, ev.Kind, ev.Payload)
		callNum++
		if h.httpCap.Finalize != nil {
			h.httpCap.Finalize(ev.ID)
		}
	}
}

func (h *CellHarness) step(ctx context.Context, callNum uint64, kind string, payload []byte) {
	var stepPayload []byte
	if kind != "" {
		enc, err := abi.EncodeStepEvent(kind, payload)
		if err != nil {
			return
		}
		stepPayload = enc
	}
	env := abi.StepEnvelope{
		CallNumber: callNum,
		WallTime:   uint64(time.Now().UnixNano()),
		Payload:    stepPayload,
	}
	_, _ = h.cell.Step(ctx, env)
}

func (h *CellHarness) stop() {
	h.cancel()
	h.pumpWG.Wait()
	if h.httpCap.Teardown != nil {
		_ = h.httpCap.Teardown(context.Background())
	}
	if h.cell != nil {
		_ = h.cell.Shutdown(context.Background())
		_ = h.cell.Close(context.Background())
	}
}

// Do fires an HTTP request at the cell and returns status + body. Headers can
// be supplied via the headers map. Body may be nil.
func (h *CellHarness) Do(method, path string, headers map[string]string, body []byte) (status int, respBody []byte) {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, h.URL+path, rdr)
	if err != nil {
		h.t.Fatalf("build request %s %s: %v", method, path, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("do request %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}
