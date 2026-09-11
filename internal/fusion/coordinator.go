package fusion

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/BananaLabs-OSS/Pulp/registry"
)

type Publisher interface {
	Publish(context.Context, []byte, map[registry.Digest][]byte) error
}

type Materializer interface {
	Materialize(context.Context, registry.Digest, []byte) (string, error)
}

type FallbackPolicy uint8

const (
	FusionRequired FallbackPolicy = iota
	AllowIsolatedFallback
)

type PrepareRequest struct {
	Group          Group
	SourceLock     *registry.Lock
	SourceBindings map[string]registry.ModuleID
	GoVersion      string
	ModuleID       registry.ModuleID
	ModuleVersion  string
	Target         string
	PhysicalName   string
	Fallback       FallbackPolicy
}

type Activation struct {
	Fused     bool
	Fallback  error
	Recipe    Recipe
	Selection *Selection
	Spec      *manifest.CellSpec
	Wasm      []byte
}

// Coordinator implements the deployment path around the runtime: resolve
// sources, select a verified immutable cached release or build and publish a
// missing one, materialize its bytes, and derive the exact physical CellSpec.
type Coordinator struct {
	Store        registry.Store
	Publisher    Publisher
	Sources      SourceCatalog
	Builder      Builder
	Materializer Materializer
}

func (c Coordinator) Prepare(ctx context.Context, request PrepareRequest) (*Activation, error) {
	activation, err := c.prepareFused(ctx, request)
	if err == nil {
		return activation, nil
	}
	if request.Fallback == AllowIsolatedFallback {
		return &Activation{Fused: false, Fallback: err}, nil
	}
	return nil, err
}

func (c Coordinator) prepareFused(ctx context.Context, request PrepareRequest) (*Activation, error) {
	if c.Store == nil || c.Materializer == nil {
		return nil, fmt.Errorf("fusion registry store and materializer are required")
	}
	if _, err := registry.ParseModuleID(string(request.ModuleID)); err != nil {
		return nil, err
	}
	version, err := registry.ParseVersion(request.ModuleVersion)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.Target) == "" || strings.TrimSpace(request.PhysicalName) == "" {
		return nil, fmt.Errorf("fusion target and physical name are required")
	}
	recipe, err := RecipeFromLock(ctx, request.Group, request.GoVersion, request.SourceLock, request.SourceBindings, c.Sources)
	if err != nil {
		return nil, err
	}
	present, err := releaseExists(ctx, c.Store, request.ModuleID, version)
	if err != nil {
		return nil, err
	}
	if !present {
		if c.Publisher == nil {
			return nil, fmt.Errorf("fused release %s@%s is absent and no publisher is configured", request.ModuleID, version.String())
		}
		result, err := c.Builder.Build(ctx, recipe)
		if err != nil {
			return nil, err
		}
		publication, err := result.RegistryPublication(string(request.ModuleID), version.String(), request.Target)
		if err != nil {
			return nil, err
		}
		if err := c.Publisher.Publish(ctx, publication.Manifest, publication.Blobs); err != nil {
			return nil, fmt.Errorf("publish fused release: %w", err)
		}
	}
	lock, err := registry.Resolve(ctx, c.Store, registry.Request{Target: request.Target, Roots: []registry.Dependency{{ID: request.ModuleID, Requirement: registry.Requirement(version.String())}}})
	if err != nil {
		return nil, fmt.Errorf("resolve fused release: %w", err)
	}
	selected, err := SelectFromRegistry(ctx, c.Store, lock, request.ModuleID, recipe)
	if err != nil {
		return nil, err
	}
	digest := registry.Sum(selected.Wasm)
	path, err := c.Materializer.Materialize(ctx, digest, selected.Wasm)
	if err != nil {
		return nil, fmt.Errorf("materialize fused artifact: %w", err)
	}
	metadata := selected.Attestation.Metadata
	spec := &manifest.CellSpec{Name: request.PhysicalName, Version: version.String(), Provides: append([]string(nil), metadata.Providers...), Capabilities: append([]string(nil), metadata.Capabilities...), WASMPath: path, WASMSHA256: strings.TrimPrefix(digest.String(), "sha256:"), Execution: manifest.ExecutionSpec{Mode: manifest.ExecutionFusible, Group: metadata.Group, ABI: metadata.ABI}}
	if err := ValidateCellArtifact(spec, selected); err != nil {
		return nil, err
	}
	return &Activation{Fused: true, Recipe: recipe, Selection: selected, Spec: spec, Wasm: append([]byte(nil), selected.Wasm...)}, nil
}

func releaseExists(ctx context.Context, store registry.Store, id registry.ModuleID, want registry.Version) (bool, error) {
	versions, err := store.Versions(ctx, id)
	if errors.Is(err, registry.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("list fused releases: %w", err)
	}
	for _, version := range versions {
		if version.Compare(want) == 0 {
			return true, nil
		}
	}
	return false, nil
}

// DirectoryMaterializer stores verified Wasm under its content digest. It is
// idempotent and refuses to replace different existing bytes.
type DirectoryMaterializer struct{ Root string }

func (m DirectoryMaterializer) Materialize(ctx context.Context, digest registry.Digest, wasm []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if registry.Sum(wasm) != digest {
		return "", fmt.Errorf("materialized Wasm digest mismatch")
	}
	root, err := filepath.Abs(m.Root)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(root, strings.TrimPrefix(digest.String(), "sha256:")+".wasm")
	if existing, err := os.ReadFile(path); err == nil {
		if registry.Sum(existing) != digest {
			return "", fmt.Errorf("materialized artifact path contains different bytes")
		}
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	temporary, err := os.CreateTemp(root, ".fusion-")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err = temporary.Write(wasm); err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return m.Materialize(ctx, digest, wasm)
		}
		return "", err
	}
	return path, nil
}
