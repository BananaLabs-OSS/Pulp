package fusion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/BananaLabs-OSS/Pulp/registry"
)

// Selection is a registry-verified fused artifact ready to be materialized by
// a deployment layer. Selection performs no filesystem writes and does not
// mutate an application manifest.
type Selection struct {
	Release     registry.ReleaseID
	Target      string
	Wasm        []byte
	Attestation Attestation
}

// SelectFromRegistry closes the resolve -> select -> attest chain. The lock is
// verified first, then the exact locked artifact and its companion fusion
// attestation are loaded by digest and checked against the requested recipe.
func SelectFromRegistry(ctx context.Context, store registry.Store, lock *registry.Lock, moduleID registry.ModuleID, recipe Recipe) (*Selection, error) {
	if store == nil {
		return nil, fmt.Errorf("fusion registry store is required")
	}
	if err := registry.Verify(ctx, store, lock); err != nil {
		return nil, fmt.Errorf("verify registry lock: %w", err)
	}
	var locked *registry.LockedModule
	for index := range lock.Modules {
		if lock.Modules[index].ID == moduleID {
			locked = &lock.Modules[index]
			break
		}
	}
	if locked == nil {
		return nil, fmt.Errorf("fusion module %q is not present in registry lock", moduleID)
	}
	version, err := registry.ParseVersion(locked.Version)
	if err != nil {
		return nil, err
	}
	release := registry.ReleaseID{ID: moduleID, Version: version}
	module, err := store.Manifest(ctx, release)
	if err != nil {
		return nil, fmt.Errorf("load fusion module %s: %w", release, err)
	}
	wasmDigest, err := registry.ParseDigest(locked.ArtifactDigest)
	if err != nil {
		return nil, err
	}
	wasm, err := readVerifiedBlob(ctx, store, wasmDigest)
	if err != nil {
		return nil, fmt.Errorf("load fused Wasm: %w", err)
	}
	attestationTarget := lock.Target + ".fusion-attestation"
	var attestationDigest registry.Digest
	found := false
	for _, artifact := range module.Artifacts {
		if artifact.Target == attestationTarget {
			attestationDigest, found = artifact.Digest, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("fusion module %s has no %q artifact", release, attestationTarget)
	}
	wire, err := readVerifiedBlob(ctx, store, attestationDigest)
	if err != nil {
		return nil, fmt.Errorf("load fusion attestation: %w", err)
	}
	attestation, err := parseAttestation(wire)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(attestation.Builder) == "" {
		return nil, fmt.Errorf("fusion attestation builder is required")
	}
	if module.FusionABI != attestation.Metadata.ABI {
		return nil, fmt.Errorf("registry fusion ABI does not match attestation")
	}
	if !equalCanonicalSet(module.Provides, attestation.Metadata.Providers) {
		return nil, fmt.Errorf("registry providers do not exactly match fusion attestation")
	}
	if !equalCanonicalSet(module.Capabilities, attestation.Metadata.Capabilities) {
		return nil, fmt.Errorf("registry capabilities do not exactly match fusion attestation")
	}
	actualWasmDigest := strings.TrimPrefix(registry.Sum(wasm).String(), "sha256:")
	if attestation.Metadata.ArtifactSHA256 != actualWasmDigest {
		return nil, fmt.Errorf("fusion attestation artifact digest does not match locked Wasm")
	}
	if err := ValidateArtifact(attestation.Metadata, recipe); err != nil {
		return nil, fmt.Errorf("validate fusion attestation: %w", err)
	}
	return &Selection{Release: release, Target: lock.Target, Wasm: wasm, Attestation: attestation}, nil
}

// ValidateCellArtifact checks the physical pulp.cell.toml contract before a
// selected artifact can replace logical placements. Exact sets are required:
// an artifact cannot gain ambient capability authority or leak providers.
func ValidateCellArtifact(spec *manifest.CellSpec, selected *Selection) error {
	if spec == nil || selected == nil {
		return fmt.Errorf("fusion cell spec and selection are required")
	}
	metadata := selected.Attestation.Metadata
	if !equalCanonicalSet(spec.Provides, metadata.Providers) {
		return fmt.Errorf("fused cell %q providers do not exactly match attestation", spec.Name)
	}
	if !equalCanonicalSet(spec.Capabilities, metadata.Capabilities) {
		return fmt.Errorf("fused cell %q capabilities do not exactly match attestation", spec.Name)
	}
	if spec.Execution.ABI != "" && spec.Execution.ABI != metadata.ABI {
		return fmt.Errorf("fused cell %q ABI %q does not match attestation ABI %q", spec.Name, spec.Execution.ABI, metadata.ABI)
	}
	actualDigest := strings.TrimPrefix(registry.Sum(selected.Wasm).String(), "sha256:")
	if spec.WASMSHA256 != "" && !strings.EqualFold(spec.WASMSHA256, actualDigest) {
		return fmt.Errorf("fused cell %q Wasm digest does not match selected artifact", spec.Name)
	}
	return nil
}

func readVerifiedBlob(ctx context.Context, store registry.Store, digest registry.Digest) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader, err := store.Blob(ctx, digest)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if registry.Sum(data) != digest {
		return nil, fmt.Errorf("registry blob digest mismatch")
	}
	return data, nil
}

func parseAttestation(data []byte) (Attestation, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var attestation Attestation
	if err := decoder.Decode(&attestation); err != nil {
		return Attestation{}, fmt.Errorf("parse fusion attestation: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Attestation{}, fmt.Errorf("parse fusion attestation: trailing value")
	}
	return attestation, nil
}
