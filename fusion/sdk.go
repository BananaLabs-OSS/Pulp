// Package fusion is Pulp's stable SDK for building registry-publishable fused
// Wasm artifacts. Runtime planner and manifest implementation types remain
// internal; callers describe only portable modules, contracts and source.
package fusion

import (
	"context"
	"fmt"

	internal "github.com/BananaLabs-OSS/Pulp/internal/fusion"
	"github.com/BananaLabs-OSS/Pulp/registry"
)

const (
	RecipeV1 = internal.RecipeSchemaV1
	RecipeV2 = internal.RecipeSchemaV2
	MemberV2 = internal.MemberABIV2
)

type Source struct {
	ModulePath, ImportPath, Version, SHA256, Entrypoint string
}

type Member struct {
	Name, Version           string
	Providers, Capabilities []string
	Config                  []byte
	Snapshotable            bool
	Source                  Source
}

type Request struct {
	Schema, Group, ABI, GoVersion string
	Members                       []Member
	Dependencies                  []Dependency
	Toolchain                     Toolchain
	Archives                      ArchiveProvider
	TempRoot                      string
}

type Dependency struct {
	ModulePath string `json:"module_path"`
	Version    string `json:"version"`
	SHA256     string `json:"source_sha256"`
	Origin     string `json:"origin,omitempty"`
	GoSum      string `json:"go_sum,omitempty"`
	GoModSum   string `json:"go_mod_sum,omitempty"`
}

type Toolchain struct {
	GoCommand, FiberVersion, BuilderID string
	Environment                        []string
	FiberSource                        Source
	Runner                             CommandRunner
}

type CommandRunner interface {
	Run(context.Context, string, []string, string, ...string) error
}

type ArchiveProvider interface {
	Archive(context.Context, Source) ([]byte, error)
}

// RegistrySources resolves and reads verified source artifacts from the same
// immutable registry protocol used by runtime modules. It is the supported
// boundary for registry-driven fusion builders.
type RegistrySources struct{ Store registry.Store }

func (r RegistrySources) Source(ctx context.Context, moduleID, version string) (Source, error) {
	id, err := registry.ParseModuleID(moduleID)
	if err != nil {
		return Source{}, err
	}
	v, err := registry.ParseVersion(version)
	if err != nil {
		return Source{}, err
	}
	source, err := (internal.RegistrySourceCatalog{Store: r.Store}).FusionSource(ctx, registry.ReleaseID{ID: id, Version: v})
	if err != nil {
		return Source{}, err
	}
	return fromInternalSource(source), nil
}

func (r RegistrySources) Archive(ctx context.Context, source Source) ([]byte, error) {
	return (internal.RegistrySourceArchives{Store: r.Store}).Archive(ctx, toInternalSource(source))
}

type Result struct {
	Wasm, GeneratedSource, GeneratedModule []byte
	ArtifactSHA256, RecipeSHA256           string
	inner                                  *internal.BuildResult
}

type Publication struct {
	Manifest []byte
	Blobs    map[registry.Digest][]byte
}

func Build(ctx context.Context, request Request) (*Result, error) {
	if request.Archives == nil {
		return nil, fmt.Errorf("fusion archive provider is required")
	}
	recipe := internal.Recipe{SchemaVersion: request.Schema, Group: request.Group, ABI: request.ABI, GoVersion: request.GoVersion}
	for _, member := range request.Members {
		recipe.Members = append(recipe.Members, internal.RecipeMember{Name: member.Name, Version: member.Version, Providers: append([]string(nil), member.Providers...), Capabilities: append([]string(nil), member.Capabilities...), Config: append([]byte(nil), member.Config...), Snapshotable: member.Snapshotable, Source: toInternalSource(member.Source)})
	}
	for _, dependency := range request.Dependencies {
		recipe.Dependencies = append(recipe.Dependencies, internal.Dependency{ModulePath: dependency.ModulePath, Version: dependency.Version, SourceSHA256: dependency.SHA256, Origin: dependency.Origin, GoSum: dependency.GoSum, GoModSum: dependency.GoModSum})
	}
	runner := request.Toolchain.Runner
	if runner == nil {
		runner = internal.ExecCommandRunner{}
	}
	builder := internal.Builder{TempRoot: request.TempRoot, Runner: runnerAdapter{runner}, Archives: archiveAdapter{request.Archives}, Toolchain: internal.Toolchain{GoCommand: request.Toolchain.GoCommand, FiberVersion: request.Toolchain.FiberVersion, BuilderID: request.Toolchain.BuilderID, Environment: append([]string(nil), request.Toolchain.Environment...), FiberSource: toInternalSource(request.Toolchain.FiberSource)}}
	built, err := builder.Build(ctx, recipe)
	if err != nil {
		return nil, err
	}
	return &Result{Wasm: append([]byte(nil), built.Wasm...), GeneratedSource: append([]byte(nil), built.MainSource...), GeneratedModule: append([]byte(nil), built.GoModule...), ArtifactSHA256: built.Attestation.Metadata.ArtifactSHA256, RecipeSHA256: built.Attestation.Metadata.RecipeSHA256, inner: built}, nil
}

func (r *Result) Publication(moduleID, version, target string) (*Publication, error) {
	if r == nil || r.inner == nil {
		return nil, fmt.Errorf("fusion build result is invalid")
	}
	publication, err := r.inner.RegistryPublication(moduleID, version, target)
	if err != nil {
		return nil, err
	}
	blobs := make(map[registry.Digest][]byte, len(publication.Blobs))
	for digest, data := range publication.Blobs {
		blobs[digest] = append([]byte(nil), data...)
	}
	return &Publication{Manifest: append([]byte(nil), publication.Manifest...), Blobs: blobs}, nil
}

func (p *Publication) Publish(ctx context.Context, publisher registry.Publisher) error {
	if p == nil || publisher == nil {
		return fmt.Errorf("fusion publication and registry publisher are required")
	}
	return publisher.Publish(ctx, p.Manifest, p.Blobs)
}

// ResolveAndVerify proves that an exact fused release is available for target
// and that every locked manifest and artifact still matches its digest.
func ResolveAndVerify(ctx context.Context, store registry.Store, moduleID, version, target string) (*registry.Lock, error) {
	lock, err := registry.Resolve(ctx, store, registry.Request{
		Target: target,
		Roots:  []registry.Dependency{{ID: registry.ModuleID(moduleID), Requirement: registry.Requirement(version)}},
	})
	if err != nil {
		return nil, err
	}
	if err := registry.Verify(ctx, store, lock); err != nil {
		return nil, err
	}
	return lock, nil
}

func toInternalSource(source Source) internal.Source {
	return internal.Source{ModulePath: source.ModulePath, ImportPath: source.ImportPath, Version: source.Version, SourceSHA256: source.SHA256, Entrypoint: source.Entrypoint}
}
func fromInternalSource(source internal.Source) Source {
	return Source{ModulePath: source.ModulePath, ImportPath: source.ImportPath, Version: source.Version, SHA256: source.SourceSHA256, Entrypoint: source.Entrypoint}
}

type archiveAdapter struct{ provider ArchiveProvider }

func (a archiveAdapter) Archive(ctx context.Context, source internal.Source) ([]byte, error) {
	return a.provider.Archive(ctx, fromInternalSource(source))
}

type runnerAdapter struct{ runner CommandRunner }

func (a runnerAdapter) Run(ctx context.Context, directory string, environment []string, command string, arguments ...string) error {
	return a.runner.Run(ctx, directory, environment, command, arguments...)
}
