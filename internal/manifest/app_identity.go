package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

const applicationIdentitySchema = "pulp.application-composition/v1"

type canonicalCellIdentity struct {
	Name, Version, WasmSHA256                                                     string
	SchemaVersion                                                                 int
	Provides, Consumes, HostConsumes, DependsOn, Capabilities, SharedMemoryGroups []string
	Execution                                                                     ExecutionSpec
	DedicatedThread, Snapshotable                                                 bool
	Restart                                                                       string
	MaxMemoryPages, CallTimeoutMS                                                 uint32
	Config                                                                        map[string]any
}

type canonicalPlacementIdentity struct {
	Address, InstanceID string
	Cell                canonicalCellIdentity
}

type canonicalExecutionUnitIdentity struct {
	Name     string
	Members  []string
	Artifact canonicalCellIdentity
}

type canonicalApplicationIdentity struct {
	Schema              string
	SchemaVersion       int
	Name, Version       string
	RequireWASMSHA256   bool
	OrchestratorCell    string
	OrchestrationSHA256 string
	Orchestration       []byte
	Cells               []canonicalCellIdentity
	Placements          []canonicalPlacementIdentity
	ExecutionUnits      []canonicalExecutionUnitIdentity
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func canonicalCell(spec *CellSpec) (canonicalCellIdentity, error) {
	if spec == nil {
		return canonicalCellIdentity{}, fmt.Errorf("nil cell")
	}
	body, err := os.ReadFile(spec.WASMPath)
	if err != nil {
		// Legacy, non-release manifests may intentionally omit an artifact until
		// runtime loading. Preserve LoadApp compatibility; release applications
		// with require_wasm_sha256 already fail before identity construction.
		if os.IsNotExist(err) && spec.WASMSHA256 == "" {
			body = nil
		} else {
			return canonicalCellIdentity{}, fmt.Errorf("read cell %q Wasm: %w", spec.Name, err)
		}
	}
	wasmDigest := ""
	if body != nil {
		digest := sha256.Sum256(body)
		wasmDigest = hex.EncodeToString(digest[:])
	}
	return canonicalCellIdentity{
		Name: spec.Name, Version: spec.Version, WasmSHA256: wasmDigest, SchemaVersion: spec.SchemaVersion,
		Provides: sortedCopy(spec.Provides), Consumes: sortedCopy(spec.Consumes), HostConsumes: sortedCopy(spec.HostConsumes),
		DependsOn: sortedCopy(spec.DependsOn), Capabilities: sortedCopy(spec.Capabilities), SharedMemoryGroups: sortedCopy(spec.SharedMemoryGroups),
		Execution: spec.Execution, DedicatedThread: spec.DedicatedThread, Snapshotable: spec.Snapshotable,
		Restart: spec.Restart, MaxMemoryPages: spec.MaxMemoryPages, CallTimeoutMS: spec.CallTimeoutMS,
		Config: spec.Config,
	}, nil
}

func canonicalApplicationDigest(app *Application, orchestration []byte) (string, error) {
	identity := canonicalApplicationIdentity{
		Schema: applicationIdentitySchema, SchemaVersion: app.SchemaVersion, Name: app.Name, Version: app.Version,
		RequireWASMSHA256: app.RequireWASMSHA256, OrchestratorCell: app.OrchestratorCell,
		OrchestrationSHA256: app.OrchestrationSHA256, Orchestration: append([]byte(nil), orchestration...),
	}
	for _, spec := range app.Cells.Cells {
		cell, err := canonicalCell(spec)
		if err != nil {
			return "", err
		}
		identity.Cells = append(identity.Cells, cell)
	}
	for _, placement := range app.Placements {
		cell, err := canonicalCell(placement.Spec)
		if err != nil {
			return "", err
		}
		identity.Placements = append(identity.Placements, canonicalPlacementIdentity{Address: placement.Address, InstanceID: placement.InstanceID, Cell: cell})
	}
	for _, unit := range app.ExecutionUnits {
		artifact, err := canonicalCell(unit.Artifact)
		if err != nil {
			return "", err
		}
		identity.ExecutionUnits = append(identity.ExecutionUnits, canonicalExecutionUnitIdentity{Name: unit.Name, Members: append([]string(nil), unit.Members...), Artifact: artifact})
	}
	wire, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("encode canonical composition: %w", err)
	}
	digest := sha256.Sum256(wire)
	return hex.EncodeToString(digest[:]), nil
}
