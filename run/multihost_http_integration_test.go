package run

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/BananaLabs-OSS/Pulp-ext-http"
	"github.com/BananaLabs-OSS/Pulp/abi"
	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

// TestMultiHostHTTPScopesSharedPackageAndRejectsAmbiguousBindings drives two
// separately scoped application cells made from the same WASM bytes through
// the real ext-http listener and Poll/Step loop. The host manifest gives each
// application an explicit external prefix; the cell packages deliberately
// retain the same internal route (/internal), proving package reuse does not
// merge state or event delivery.
func TestMultiHostHTTPScopesSharedPackageAndRejectsAmbiguousBindings(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping WASI multi-host HTTP integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	root := t.TempDir()
	wasmPath := multiHostHTTPBuildSharedCell(t, root)
	evolutionApp := multiHostHTTPWriteApp(t, root, "evolution", wasmPath)
	sessionsApp := multiHostHTTPWriteApp(t, root, "sessions", wasmPath)
	hostPath := multiHostHTTPWriteHost(t, root, evolutionApp, sessionsApp, false)

	loadedHost, err := manifest.LoadHost(hostPath)
	if err != nil {
		t.Fatalf("load explicit host routes: %v", err)
	}
	if len(loadedHost.Routes) != 2 || loadedHost.Routes[0].Path != "/evolution" || loadedHost.Routes[1].Path != "/sessions" {
		t.Fatalf("host routes = %#v, want distinct /evolution and /sessions bindings", loadedHost.Routes)
	}
	if _, err := manifest.LoadHost(multiHostHTTPWriteHost(t, root, evolutionApp, sessionsApp, true)); err == nil || !strings.Contains(err.Error(), "duplicate route path") {
		t.Fatalf("ambiguous external host binding error = %v, want duplicate route path", err)
	}

	portOne := multiHostHTTPReservePort(t)
	portTwo := multiHostHTTPReservePort(t)
	t.Setenv("HTTP_PORT", multiHostHTTPReservePort(t))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	httpCapability := multiHostHTTPCapability(t, "transport.http.inbound")
	if err := httpCapability.Setup(ext.SetupEnv{CellName: "host", Logger: logger}); err != nil {
		t.Fatalf("setup shared HTTP extension: %v", err)
	}
	t.Cleanup(func() { _ = httpCapability.Teardown(context.Background()) })

	registry := host.NewRegistry()
	for _, capability := range ext.All() {
		registry.Gated(capability)
	}

	evolution := HostedApplication{Identity: ApplicationIdentity{ApplicationID: "evolution", InstanceID: "primary"}}
	sessions := HostedApplication{Identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "primary"}}
	evolutionScope, err := evolution.NewCellScope("api", "default")
	if err != nil {
		t.Fatalf("evolution scope: %v", err)
	}
	sessionsScope, err := sessions.NewCellScope("api", "default")
	if err != nil {
		t.Fatalf("sessions scope: %v", err)
	}

	evolutionCell := multiHostHTTPLoadCell(t, ctx, registry, logger, wasmPath, evolutionScope, "evolution", "127.0.0.1:"+portOne)
	sessionsCell := multiHostHTTPLoadCell(t, ctx, registry, logger, wasmPath, sessionsScope, "sessions", "127.0.0.1:"+portTwo)
	t.Cleanup(func() {
		_ = sessionsCell.Shutdown(context.Background())
		_ = sessionsCell.Close(context.Background())
		_ = evolutionCell.Shutdown(context.Background())
		_ = evolutionCell.Close(context.Background())
	})

	byTarget := map[string]*host.Cell{
		ext.CellIDOf(evolutionCell): evolutionCell,
		ext.CellIDOf(sessionsCell):  sessionsCell,
	}
	if len(byTarget) != 2 {
		t.Fatalf("scoped event targets collided: evolution=%q sessions=%q", ext.CellIDOf(evolutionCell), ext.CellIDOf(sessionsCell))
	}
	multiHostHTTPPump(t, ctx, httpCapability, byTarget)

	multiHostHTTPAssertResponse(t, "http://127.0.0.1:"+portOne+"/internal", "evolution")
	multiHostHTTPAssertResponse(t, "http://127.0.0.1:"+portTwo+"/internal", "sessions")
}

func TestMultiHostGatewayUsesDistinctEphemeralEndpointsAndRebinds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping WASI multi-host gateway integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	root := t.TempDir()
	wasmPath := multiHostHTTPBuildSharedCell(t, root)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	endpoints := NewEndpointRegistry()
	httpCapability := multiHostHTTPCapability(t, "transport.http.inbound")
	if err := httpCapability.Setup(ext.SetupEnv{Endpoints: endpoints, Logger: logger}); err != nil {
		t.Fatalf("setup endpoint-enabled HTTP extension: %v", err)
	}
	t.Cleanup(func() { _ = httpCapability.Teardown(context.Background()) })

	registry := host.NewRegistry()
	for _, capability := range ext.All() {
		registry.Gated(capability)
	}
	evolutionApp := HostedApplication{Identity: ApplicationIdentity{ApplicationID: "evolution", InstanceID: "primary"}}
	sessionsApp := HostedApplication{Identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "primary"}}
	evolutionScope, err := evolutionApp.NewCellScope("api", "default")
	if err != nil {
		t.Fatal(err)
	}
	sessionsScope, err := sessionsApp.NewCellScope("api", "default")
	if err != nil {
		t.Fatal(err)
	}
	evolutionCell := multiHostHTTPLoadCell(t, ctx, registry, logger, wasmPath, evolutionScope, "evolution", "")
	sessionsCell := multiHostHTTPLoadCell(t, ctx, registry, logger, wasmPath, sessionsScope, "sessions", "")
	currentEvolution := evolutionCell
	t.Cleanup(func() {
		if currentEvolution != nil {
			_ = httpCapability.TeardownCell(context.Background(), ext.CellIDOf(currentEvolution))
			_ = currentEvolution.Shutdown(context.Background())
			_ = currentEvolution.Close(context.Background())
		}
		_ = httpCapability.TeardownCell(context.Background(), ext.CellIDOf(sessionsCell))
		_ = sessionsCell.Shutdown(context.Background())
		_ = sessionsCell.Close(context.Background())
	})

	evolutionRuntime := &multiHostHTTPEndpointRuntime{identity: evolutionApp.Identity, endpoints: endpoints}
	sessionsRuntime := &multiHostHTTPEndpointRuntime{identity: sessionsApp.Identity, endpoints: endpoints}
	evolutionAddress := evolutionRuntime.HTTPAddress()
	sessionsAddress := sessionsRuntime.HTTPAddress()
	if evolutionAddress == "" || sessionsAddress == "" || evolutionAddress == sessionsAddress {
		t.Fatalf("scoped endpoints evolution=%q sessions=%q", evolutionAddress, sessionsAddress)
	}

	gateway, err := NewHostGateway("127.0.0.1:0", []*manifest.RouteBinding{
		{Path: "/evolution", Application: "evolution", Instance: "primary"},
		{Path: "/sessions", Application: "sessions", Instance: "primary"},
	}, []ApplicationRuntime{evolutionRuntime, sessionsRuntime}, logger)
	if err != nil {
		t.Fatalf("NewHostGateway: %v", err)
	}
	if err := gateway.Start(ctx); err != nil {
		t.Fatalf("start gateway: %v", err)
	}
	t.Cleanup(func() { _ = gateway.Shutdown(context.Background()) })

	targets := &multiHostHTTPCellTargets{cells: map[string]*host.Cell{
		ext.CellIDOf(evolutionCell): evolutionCell,
		ext.CellIDOf(sessionsCell):  sessionsCell,
	}}
	multiHostHTTPPumpDynamic(t, ctx, httpCapability, targets)
	baseURL := "http://" + gateway.Addr()
	multiHostHTTPAssertResponse(t, baseURL+"/evolution/internal", "evolution")
	multiHostHTTPAssertResponse(t, baseURL+"/sessions/internal", "sessions")

	oldCellID := ext.CellIDOf(evolutionCell)
	if err := httpCapability.TeardownCell(ctx, oldCellID); err != nil {
		t.Fatalf("withdraw Evolution endpoint: %v", err)
	}
	targets.delete(oldCellID)
	_ = evolutionCell.Shutdown(context.Background())
	_ = evolutionCell.Close(context.Background())
	currentEvolution = nil
	response, err := http.Get(baseURL + "/evolution/internal")
	if err != nil {
		t.Fatalf("request withdrawn endpoint: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("withdrawn endpoint status = %d, want 503", response.StatusCode)
	}

	recovered := multiHostHTTPLoadCell(t, ctx, registry, logger, wasmPath, evolutionScope, "evolution-recovered", "")
	currentEvolution = recovered
	targets.set(ext.CellIDOf(recovered), recovered)
	if recoveredAddress := evolutionRuntime.HTTPAddress(); recoveredAddress == "" || recoveredAddress == evolutionAddress {
		t.Fatalf("recovered endpoint = %q, old = %q", recoveredAddress, evolutionAddress)
	}
	multiHostHTTPAssertResponse(t, baseURL+"/evolution/internal", "evolution-recovered")
}

type multiHostHTTPEndpointRuntime struct {
	identity  ApplicationIdentity
	endpoints *EndpointRegistry
}

func (r *multiHostHTTPEndpointRuntime) Identity() ApplicationIdentity  { return r.identity }
func (r *multiHostHTTPEndpointRuntime) Start(context.Context) error    { return nil }
func (r *multiHostHTTPEndpointRuntime) Shutdown(context.Context) error { return nil }
func (r *multiHostHTTPEndpointRuntime) HTTPAddress() string {
	address, _ := r.endpoints.ApplicationAddress(r.identity.ApplicationID, r.identity.InstanceID, "transport.http.inbound", "public")
	return address
}

type multiHostHTTPCellTargets struct {
	mu    sync.RWMutex
	cells map[string]*host.Cell
}

func (t *multiHostHTTPCellTargets) get(cellID string) *host.Cell {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.cells[cellID]
}

func (t *multiHostHTTPCellTargets) set(cellID string, cell *host.Cell) {
	t.mu.Lock()
	t.cells[cellID] = cell
	t.mu.Unlock()
}

func (t *multiHostHTTPCellTargets) delete(cellID string) {
	t.mu.Lock()
	delete(t.cells, cellID)
	t.mu.Unlock()
}

func multiHostHTTPCapability(t *testing.T, name string) ext.Capability {
	t.Helper()
	for _, capability := range ext.All() {
		if capability.Name == name {
			return capability
		}
	}
	t.Fatalf("capability %q is not registered", name)
	return ext.Capability{}
}

func multiHostHTTPReservePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split reserved port: %v", err)
	}
	return port
}

func multiHostHTTPLoadCell(t *testing.T, ctx context.Context, registry *host.Registry, logger *slog.Logger, wasmPath string, scope ext.Scope, label, listener string) *host.Cell {
	t.Helper()
	spec := &manifest.CellSpec{
		Name:         "api",
		Version:      "1",
		WASMPath:     wasmPath,
		Capabilities: []string{"transport.http.inbound"},
		Config:       map[string]any{"label": label, "listener": listener},
	}
	cell, err := host.LoadScoped(ctx, spec, registry, nil, logger, scope)
	if err != nil {
		t.Fatalf("load scoped %s API cell: %v", label, err)
	}
	config, err := manifest.EncodeConfig(spec.Config)
	if err != nil {
		t.Fatalf("encode %s config: %v", label, err)
	}
	if err := cell.Init(ctx, config); err != nil {
		_ = cell.Close(context.Background())
		t.Fatalf("init scoped %s API cell: %v", label, err)
	}
	return cell
}

func multiHostHTTPPump(t *testing.T, parent context.Context, capability ext.Capability, byTarget map[string]*host.Cell) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var callNumber uint64
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			event, ok := capability.Poll()
			if !ok {
				time.Sleep(time.Millisecond)
				continue
			}
			cell := byTarget[event.CellID]
			if cell == nil {
				t.Errorf("HTTP event targeted unknown scoped cell %q", event.CellID)
				continue
			}
			payload, err := abi.EncodeStepEvent(event.Kind, event.Payload)
			if err == nil {
				_, _ = cell.Step(ctx, abi.StepEnvelope{CallNumber: callNumber, WallTime: uint64(time.Now().UnixNano()), Payload: payload})
			}
			callNumber++
			if capability.Finalize != nil {
				capability.Finalize(event.ID)
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("multi-host HTTP pump did not stop")
		}
	})
}

func multiHostHTTPPumpDynamic(t *testing.T, parent context.Context, capability ext.Capability, targets *multiHostHTTPCellTargets) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var callNumber uint64
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			event, ok := capability.Poll()
			if !ok {
				time.Sleep(time.Millisecond)
				continue
			}
			cell := targets.get(event.CellID)
			if cell == nil {
				if capability.Finalize != nil {
					capability.Finalize(event.ID)
				}
				continue
			}
			payload, err := abi.EncodeStepEvent(event.Kind, event.Payload)
			if err == nil {
				_, _ = cell.Step(ctx, abi.StepEnvelope{CallNumber: callNumber, WallTime: uint64(time.Now().UnixNano()), Payload: payload})
			}
			callNumber++
			if capability.Finalize != nil {
				capability.Finalize(event.ID)
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("dynamic multi-host HTTP pump did not stop")
		}
	})
}

func multiHostHTTPAssertResponse(t *testing.T, rawURL, want string) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s: %v", rawURL, err)
	}
	if response.StatusCode != http.StatusOK || string(body) != want {
		t.Fatalf("GET %s = %d %q, want 200 %q", rawURL, response.StatusCode, body, want)
	}
}

func multiHostHTTPBuildSharedCell(t *testing.T, root string) string {
	t.Helper()
	fiberRoot, err := filepath.Abs(filepath.Join("..", "..", "Fiber"))
	if err != nil {
		t.Fatalf("resolve Fiber root: %v", err)
	}
	sourceDir := filepath.Join(root, "shared-api")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir shared API fixture: %v", err)
	}
	goMod := fmt.Sprintf("module example.com/multihost-http-fixture\n\ngo 1.25\n\nrequire github.com/BananaLabs-OSS/Fiber v0.0.0\n\nreplace github.com/BananaLabs-OSS/Fiber => %s\n", filepath.ToSlash(fiberRoot))
	source := `package main

import (
  "github.com/BananaLabs-OSS/Fiber/Pulp"
  "github.com/vmihailenco/msgpack/v5"
)

type config struct {
  Label string ` + "`msgpack:\"label\"`" + `
  Listener string ` + "`msgpack:\"listener\"`" + `
}

var active config

func init() {
  pulp.OnInit(func(raw []byte) error {
    if err := msgpack.Unmarshal(raw, &active); err != nil { return err }
    if active.Listener != "" {
      if err := pulp.HTTP.Listen(active.Listener); err != nil { return err }
    }
    return pulp.HTTP.Register("GET", "/internal")
  })
  pulp.OnStep(func(event pulp.StepEvent) error {
    if event.Kind != pulp.EventHTTPRequest { return nil }
    var request pulp.HTTPRequest
    if err := msgpack.Unmarshal(event.Payload, &request); err != nil { return err }
    return pulp.HTTP.Respond(pulp.HTTPResponse{ID: request.ID, Status: 200, Body: []byte(active.Label)})
  })
}

func main() {}
`
	if err := os.WriteFile(filepath.Join(sourceDir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatalf("write fixture go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "main.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("write fixture source: %v", err)
	}
	wasmPath := filepath.Join(root, "shared", "api.wasm")
	if err := os.MkdirAll(filepath.Dir(wasmPath), 0o755); err != nil {
		t.Fatalf("mkdir shared wasm path: %v", err)
	}
	command := exec.Command("go", "build", "-buildvcs=false", "-buildmode=c-shared", "-o", wasmPath, ".")
	command.Dir = sourceDir
	command.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "GOFLAGS=-mod=mod", "GOCACHE="+filepath.Join(root, "go-cache"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build shared API fixture: %v\n%s", err, output)
	}
	return wasmPath
}

func multiHostHTTPWriteApp(t *testing.T, root, name, wasmPath string) string {
	t.Helper()
	dir := filepath.Join(root, "apps", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s app: %v", name, err)
	}
	cell := "name = \"api\"\nversion = \"1\"\nwasm = \"../../shared/" + filepath.Base(wasmPath) + "\"\ncapabilities = [\"transport.http.inbound\"]\n"
	logic := "return {}\n"
	digest := sha256.Sum256([]byte(logic))
	app := fmt.Sprintf("name = %q\nversion = \"1\"\ncells = [\"api.cell.toml\"]\n[orchestrator]\nmanifest = \"api.cell.toml\"\nscript = \"logic.lua\"\nsha256 = %q\n", name, fmt.Sprintf("%x", digest))
	for filename, content := range map[string]string{"api.cell.toml": cell, "logic.lua": logic, "pulp.app.toml": app} {
		if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s %s: %v", name, filename, err)
		}
	}
	return filepath.Join(dir, "pulp.app.toml")
}

func multiHostHTTPWriteHost(t *testing.T, root, evolutionApp, sessionsApp string, duplicate bool) string {
	t.Helper()
	secondPath := "/sessions"
	if duplicate {
		secondPath = "/evolution"
	}
	content := fmt.Sprintf("name = \"http-multi-host\"\n[[applications]]\nid = \"evolution\"\nmanifest = %q\naliases = [\"primary\"]\nstorage_namespace = \"evolution-storage\"\nevent_namespace = \"evolution-events\"\n\n[[applications]]\nid = \"sessions\"\nmanifest = %q\naliases = [\"primary\"]\nstorage_namespace = \"sessions-storage\"\nevent_namespace = \"sessions-events\"\n\n[[routes]]\npath = \"/evolution\"\napplication = \"evolution\"\ninstance = \"primary\"\n\n[[routes]]\npath = %q\napplication = \"sessions\"\ninstance = \"primary\"\n", filepath.ToSlash(filepath.Join("apps", "evolution", filepath.Base(evolutionApp))), filepath.ToSlash(filepath.Join("apps", "sessions", filepath.Base(sessionsApp))), secondPath)
	name := "pulp.host.toml"
	if duplicate {
		name = "duplicate.host.toml"
	}
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write host manifest: %v", err)
	}
	return path
}
