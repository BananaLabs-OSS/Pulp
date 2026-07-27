package host

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"testing"
	"time"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

const evolutionLegacyOwnerRefreshProvider = "sessions.legacy-owner.refresh.v1"

type composedHarnessRuntime struct {
	spec *manifest.CellSpec
	cell *Cell
}

// StartEvolutionApplicationHTTP boots the checked-in monolithic compatibility
// application, not an isolated Evolution cell. Requests therefore enter the
// real Evolution HTTP adapter and can traverse Sessions Lua plus the owner and
// resolver WASM packages declared by pulp.app.toml.
func StartEvolutionApplicationHTTP(t *testing.T, cfg CellHarnessConfig) *CellHarness {
	t.Helper()

	workspace, err := filepath.Abs(filepath.Join(evolutionSourceDir(), "..", ".."))
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	appPath := filepath.Join(workspace, "Evolution", "pulp-cell", "pulp.app.toml")
	application, err := manifest.LoadApp(appPath)
	if err != nil {
		t.Fatalf("load Evolution application: %v", err)
	}
	sources := evolutionApplicationHarnessSources(workspace)
	runtimes := make(map[string]*composedHarnessRuntime, len(application.Cells.Order))
	for _, spec := range application.Cells.Order {
		source, ok := sources[spec.Name]
		if !ok {
			t.Fatalf("Evolution application contains unmapped cell %q", spec.Name)
		}
		spec.WASMPath = BuildCell(t, source)
		if spec.Name == "evolution" {
			if spec.Config == nil {
				spec.Config = map[string]any{}
			}
			for key, value := range cfg.Config {
				spec.Config[key] = value
			}
			spec.Config["legacy_owner_imports_single_app"] = true
		}
		runtimes[spec.Name] = &composedHarnessRuntime{spec: spec}
	}

	port := freePort(t)
	t.Setenv("HTTP_PORT", fmt.Sprintf("%d", port))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	capabilities := evolutionApplicationHarnessCapabilities(cfg.CapabilityOverrides)
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
		if name == "pulp.sibling" {
			continue
		}
		registry.Gated(capability)
	}
	registry.Always(composedHarnessSiblingCapability(runtimes))

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

	evolution := runtimes["evolution"].cell
	if evolution == nil {
		t.Fatal("Evolution application did not load its HTTP adapter")
	}
	harness.cell = evolution
	harness.beforeRequest = func() error {
		refreshCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := evolution.Call(refreshCtx, evolutionLegacyOwnerRefreshProvider, nil)
		return err
	}
	harness.pumpWG.Add(1)
	go harness.pump(ctx)
	return harness
}

// evolutionApplicationHarnessCapabilities supplies the one hermetic host
// effect that every composed Evolution application needs to instantiate. It
// is an Evolution-harness default, not a production capability registration;
// caller-provided overrides are applied last and therefore retain their normal
// replacement semantics.
func evolutionApplicationHarnessCapabilities(overrides []ext.Capability) map[string]ext.Capability {
	capabilities := map[string]ext.Capability{}
	for _, capability := range ext.All() {
		capabilities[capability.Name] = capability
	}
	for _, capability := range []ext.Capability{
		fleetRuntimeEffectStubCapability(),
		newServerReadsV2ObservationStub().capability(),
		statusSignalEffectStubCapability(),
		stripeRuntimeStubCapability(),
		evoServerMutationEffectStub(),
		evolutionApplicationUnavailableCapability("effect.service.observation", "service_observation_execute"),
		evolutionApplicationUnavailableCapability("effect.capacity.observation", "capacity_observation_execute"),
		evolutionApplicationUnavailableCapability("effect.http.probe.v1", "http_probe_execute"),
		evolutionApplicationUnavailableCapability("storage.s3.public-upload.v1", "s3_exact_object_presign_put", "s3_exact_object_validate_put", "s3_exact_object_delete"),
		evolutionApplicationUnavailableCapability("storage.s3.exact-object-download-reference.v1", "s3_exact_object_download_reference"),
		evolutionApplicationUnavailableCapability("storage.s3.artifact-validation.v1", "s3_exact_object_validate_artifact_zip"),
	} {
		capabilities[capability.Name] = capability
	}
	for _, capability := range overrides {
		capabilities[capability.Name] = capability
	}
	return capabilities
}

// evolutionApplicationUnavailableCapability exposes only the declared import
// ABI for owner cells not exercised by the legacy HTTP harness. It deliberately
// returns the normal unavailable code rather than borrowing a broad storage or
// Stripe capability, so declarations remain required and call paths fail
// closed if a legacy test unexpectedly reaches one.
func evolutionApplicationUnavailableCapability(name string, exports ...string) ext.Capability {
	bind := func(builder wazero.HostModuleBuilder, _ ext.Cell) error {
		unavailable := func(context.Context, api.Module, uint32, uint32, uint32, uint32) uint32 { return 4 }
		for _, export := range exports {
			builder.NewFunctionBuilder().WithFunc(unavailable).Export(export)
		}
		return nil
	}
	return ext.Capability{Name: name, Register: bind, Stub: bind}
}

func TestEvolutionApplicationHarnessCapabilitiesSupportDowntimeOverrides(t *testing.T) {
	overrides := evoDowntimeOverrides()
	capabilities := evolutionApplicationHarnessCapabilities(overrides)
	if capabilities[fleetRuntimeEffectCapability].Name != fleetRuntimeEffectCapability {
		t.Fatalf("composed downtime harness is missing %q", fleetRuntimeEffectCapability)
	}
	if capabilities[statusSignalEffectCapability].Name != statusSignalEffectCapability {
		t.Fatalf("composed downtime harness is missing %q", statusSignalEffectCapability)
	}
	observation := capabilities[serverReadsV2FleetObservationCapability]
	if observation.Name != serverReadsV2FleetObservationCapability {
		t.Fatalf("composed downtime harness is missing %q", serverReadsV2FleetObservationCapability)
	}
	if observation.Provider != "evolution-deployment" {
		t.Fatalf("Fleet observation provider = %q, want production identity", observation.Provider)
	}
	for _, override := range overrides {
		if capabilities[override.Name].Name != override.Name {
			t.Fatalf("composed downtime harness lost custom override %q", override.Name)
		}
	}

	want := fmt.Errorf("custom Fleet runtime override")
	custom := ext.Capability{
		Name: fleetRuntimeEffectCapability,
		Setup: func(ext.SetupEnv) error {
			return want
		},
	}
	got := evolutionApplicationHarnessCapabilities([]ext.Capability{custom})[fleetRuntimeEffectCapability]
	if got.Setup == nil || got.Setup(ext.SetupEnv{}) != want {
		t.Fatal("caller Fleet runtime override did not replace the harness default")
	}

	wantStatus := fmt.Errorf("custom status signal override")
	customStatus := ext.Capability{
		Name: statusSignalEffectCapability,
		Setup: func(ext.SetupEnv) error {
			return wantStatus
		},
	}
	got = evolutionApplicationHarnessCapabilities([]ext.Capability{customStatus})[statusSignalEffectCapability]
	if got.Setup == nil || got.Setup(ext.SetupEnv{}) != wantStatus {
		t.Fatal("caller status signal override did not replace the harness default")
	}

	wantObservation := fmt.Errorf("custom Fleet observation override")
	customObservation := ext.Capability{
		Name: serverReadsV2FleetObservationCapability,
		Setup: func(ext.SetupEnv) error {
			return wantObservation
		},
	}
	got = evolutionApplicationHarnessCapabilities([]ext.Capability{customObservation})[serverReadsV2FleetObservationCapability]
	if got.Setup == nil || got.Setup(ext.SetupEnv{}) != wantObservation {
		t.Fatal("caller Fleet observation override did not replace the harness default")
	}
}

func evolutionApplicationHarnessSources(workspace string) map[string]string {
	return map[string]string{
		"sessions":                   filepath.Join(workspace, "Sessions-Gene", "composition-cell"),
		"commerce":                   filepath.Join(workspace, "Evolution", "commerce"),
		"fleet":                      filepath.Join(workspace, "Evolution", "fleet"),
		"funding":                    filepath.Join(workspace, "Evolution", "funding"),
		"identity":                   filepath.Join(workspace, "Evolution", "identity"),
		"control":                    filepath.Join(workspace, "Evolution", "control"),
		"effects":                    filepath.Join(workspace, "Evolution", "effects"),
		"public-upload":              filepath.Join(workspace, "Evolution", "public-upload"),
		"exact-object-upload":        filepath.Join(workspace, "Evolution", "exact-object-upload"),
		"artifact-validator":         filepath.Join(workspace, "Evolution", "artifact-validator"),
		"configuration-registry":     filepath.Join(workspace, "Evolution", "configuration-registry"),
		"fixed-window-counter":       filepath.Join(workspace, "Evolution", "fixed-window-counter"),
		"workload-inventory":         filepath.Join(workspace, "Evolution", "workload-inventory"),
		"capacity-scheduler":         filepath.Join(workspace, "Evolution", "capacity-scheduler"),
		"workload-provisioning":      filepath.Join(workspace, "Evolution", "workload-provisioning"),
		"runtime-control":            filepath.Join(workspace, "Evolution", "runtime-control"),
		"artifact-lifecycle":         filepath.Join(workspace, "Evolution", "artifact-lifecycle"),
		"archive-lifecycle":          filepath.Join(workspace, "Evolution", "archive-lifecycle"),
		"observation-registry":       filepath.Join(workspace, "Evolution", "observation-registry"),
		"notification-outbox":        filepath.Join(workspace, "Evolution", "notification-outbox"),
		"minecraft-profile-resolver": filepath.Join(workspace, "Evolution", "minecraft-profile-resolver"),
		"lua-orchestrator":           filepath.Join(workspace, "Pulp-Lua", "pulp-cell"),
		"jvm-jre-detect":             filepath.Join(workspace, "minecraft-resolver", "jvm-jre-detect"),
		"minecraft-resolver":         filepath.Join(workspace, "minecraft-resolver", "pulp-cell"),
		"evolution":                  filepath.Join(workspace, "Evolution", "pulp-cell"),
	}
}

func composedHarnessSiblingCapability(runtimes map[string]*composedHarnessRuntime) ext.Capability {
	bind := func(builder wazero.HostModuleBuilder, callerCell ext.Cell) error {
		caller := callerCell.Name()
		builder.NewFunctionBuilder().
			WithFunc(func(ctx context.Context, module api.Module,
				targetPtr, targetLen,
				functionPtr, functionLen,
				argsPtr, argsLen,
				responsePtrOut, responseLenOut uint32,
			) (result uint32) {
				defer func() {
					if recovered := recover(); recovered != nil {
						slog.Default().Error(
							"composed harness sibling call panic",
							"caller", caller,
							"panic", recovered,
							"stack", string(debug.Stack()),
						)
						result = 99
					}
				}()
				targetBytes, ok := module.Memory().Read(targetPtr, targetLen)
				if !ok {
					return 2
				}
				functionBytes, ok := module.Memory().Read(functionPtr, functionLen)
				if !ok {
					return 2
				}
				target := string(targetBytes)
				function := string(functionBytes)
				if !composedHarnessCallAllowed(runtimes, caller, target, function) {
					return 11
				}
				var args []byte
				if argsLen != 0 {
					value, ok := module.Memory().Read(argsPtr, argsLen)
					if !ok {
						return 2
					}
					args = append([]byte(nil), value...)
				}
				targetRuntime := runtimes[target]
				if targetRuntime == nil || targetRuntime.cell == nil {
					return 4
				}
				response, err := targetRuntime.cell.Call(ctx, function, args)
				if err != nil {
					return 4
				}
				return writeComposedHarnessSiblingResponse(ctx, module, response, responsePtrOut, responseLenOut)
			}).
			Export("pulp_call")
		return nil
	}
	return ext.Capability{Name: "pulp.sibling", Register: bind, Stub: bind}
}

func composedHarnessCallAllowed(runtimes map[string]*composedHarnessRuntime, caller, target, function string) bool {
	callerRuntime := runtimes[caller]
	targetRuntime := runtimes[target]
	if callerRuntime == nil || targetRuntime == nil {
		return false
	}
	return composedHarnessContains(callerRuntime.spec.Consumes, function) &&
		composedHarnessContains(targetRuntime.spec.Provides, function)
}

func composedHarnessContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func writeComposedHarnessSiblingResponse(
	ctx context.Context,
	module api.Module,
	response []byte,
	responsePtrOut, responseLenOut uint32,
) uint32 {
	var pointer uint32
	if len(response) != 0 {
		allocate := module.ExportedFunction("pulp_alloc")
		if allocate == nil {
			return 7
		}
		results, err := allocate.Call(ctx, uint64(len(response)))
		if err != nil || len(results) == 0 {
			return 7
		}
		pointer = uint32(results[0])
		if pointer == 0 || !module.Memory().Write(pointer, response) {
			return 8
		}
	}
	if !module.Memory().WriteUint32Le(responsePtrOut, pointer) ||
		!module.Memory().WriteUint32Le(responseLenOut, uint32(len(response))) {
		return 8
	}
	return 0
}
