package host

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

// startPeelApplicationHTTP boots Peel's deployed four-cell application.
// The control API now belongs to peel-api, so a single peel-relay cell
// harness would truthfully return 404. This narrow harness preserves the
// real API -> Lua -> owner/relay sibling-call boundary used in production.
func startPeelApplicationHTTP(t *testing.T, serviceToken string) *CellHarness {
	t.Helper()

	workspace, err := filepath.Abs(filepath.Join(peelSourceDir(), "..", ".."))
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	application, err := manifest.LoadApp(filepath.Join(workspace, "Peel", "application", "pulp.app.toml"))
	if err != nil {
		t.Fatalf("load Peel application: %v", err)
	}
	sources := map[string]string{
		"peel-owner":       filepath.Join(workspace, "Peel", "owner-cell"),
		"peel-relay":       filepath.Join(workspace, "Peel", "pulp-cell"),
		"peel-api":         filepath.Join(workspace, "Peel", "api-cell"),
		"lua-orchestrator": filepath.Join(workspace, "Pulp-Lua", "pulp-cell"),
	}
	runtimes := make(map[string]*composedHarnessRuntime, len(application.Cells.Order))
	for _, spec := range application.Cells.Order {
		source, ok := sources[spec.Name]
		if !ok {
			t.Fatalf("Peel application contains unmapped cell %q", spec.Name)
		}
		spec.WASMPath = BuildCell(t, source)
		if spec.Config == nil {
			spec.Config = map[string]any{}
		}
		switch spec.Name {
		case "peel-relay":
			spec.Config["listen_addr"] = "127.0.0.1:0"
		case "peel-api":
			spec.Config["service_token"] = serviceToken
		}
		runtimes[spec.Name] = &composedHarnessRuntime{spec: spec}
	}

	port := freePort(t)
	t.Setenv("HTTP_PORT", fmt.Sprintf("%d", port))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	capabilities := map[string]ext.Capability{}
	for _, capability := range ext.All() {
		capabilities[capability.Name] = capability
	}
	httpCapability := capabilities["transport.http.inbound"]
	if httpCapability.Name == "" {
		t.Fatal("transport.http.inbound capability not registered")
	}

	declared := map[string]bool{}
	for _, runtime := range runtimes {
		for _, name := range runtime.spec.Capabilities {
			if _, ok := capabilities[name]; !ok {
				t.Fatalf("cell %q declares unavailable capability %q", runtime.spec.Name, name)
			}
			declared[name] = true
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	harness := &CellHarness{
		URL:         fmt.Sprintf("http://127.0.0.1:%d", port),
		client:      &http.Client{Timeout: 5 * time.Second},
		cellsByName: make(map[string]*Cell, len(runtimes)),
		cancel:      cancel,
		t:           t,
		httpCap:     httpCapability,
		StorageRoot: t.TempDir(),
	}
	t.Cleanup(harness.stop)
	for name, capability := range capabilities {
		if !declared[name] {
			continue
		}
		if capability.Teardown != nil {
			harness.teardownCaps = append(harness.teardownCaps, capability)
		}
		if capability.Setup != nil {
			if err := capability.Setup(ext.SetupEnv{StorageRoot: harness.StorageRoot, Logger: logger}); err != nil {
				t.Fatalf("capability %q setup: %v", name, err)
			}
		}
	}

	registry := NewRegistry()
	for name, capability := range capabilities {
		if name != "pulp.sibling" {
			registry.Gated(capability)
		}
	}
	registry.Always(composedHarnessSiblingCapability(runtimes))
	// Pulp-Lua imports the cross-application ABI unconditionally so one WASM
	// artifact can serve both split-host and monolith deployments. Peel has no
	// AppCall grants; bind the existing fail-closed harness implementation.
	registry.Always(crossApplicationHarnessStubCapability())

	for _, spec := range application.Cells.Order {
		configBytes, err := manifest.EncodeConfig(spec.Config)
		if err != nil {
			t.Fatalf("encode %s config: %v", spec.Name, err)
		}
		cell, err := Load(ctx, spec, registry, nil, logger)
		if err != nil {
			t.Fatalf("load %s cell: %v", spec.Name, err)
		}
		runtimes[spec.Name].cell = cell
		harness.cellsByName[spec.Name] = cell
		harness.cells = append(harness.cells, cell)
		if err := cell.Init(ctx, configBytes); err != nil {
			t.Fatalf("init %s cell: %v", spec.Name, err)
		}
	}

	harness.cell = runtimes["peel-api"].cell
	if harness.cell == nil {
		t.Fatal("Peel application did not load its HTTP API cell")
	}
	harness.pumpWG.Add(1)
	go harness.pump(ctx)
	return harness
}
