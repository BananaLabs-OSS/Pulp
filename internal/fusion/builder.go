package fusion

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BananaLabs-OSS/Pulp/registry"
)

// CommandRunner makes the toolchain boundary injectable. Tests and remote
// builders can implement it without installing or invoking Go locally.
type CommandRunner interface {
	Run(ctx context.Context, directory string, environment []string, command string, arguments ...string) error
}

type ExecCommandRunner struct{}

func (ExecCommandRunner) Run(ctx context.Context, directory string, environment []string, command string, arguments ...string) error {
	cmd := exec.CommandContext(ctx, command, arguments...)
	cmd.Dir = directory
	cmd.Env = append(os.Environ(), environment...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", command, strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

type Toolchain struct {
	GoCommand    string
	FiberVersion string
	BuilderID    string
	Environment  []string
	// FiberSource binds the build-time ABI dependency to verified source.
	FiberSource Source
}

type Builder struct {
	Toolchain Toolchain
	Runner    CommandRunner
	TempRoot  string
	Archives  SourceArchives
}

type BuildResult struct {
	Recipe      Recipe
	MainSource  []byte
	GoModule    []byte
	Wasm        []byte
	Attestation Attestation
}

// Build creates an isolated temporary module, invokes the injected Go
// toolchain once, hashes the resulting Wasm, and returns all publication data.
func (b Builder) Build(ctx context.Context, recipe Recipe) (*BuildResult, error) {
	if b.Runner == nil {
		return nil, fmt.Errorf("fusion command runner is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateBuildRecipe(recipe); err != nil {
		return nil, err
	}
	goCommand := strings.TrimSpace(b.Toolchain.GoCommand)
	if goCommand == "" {
		goCommand = "go"
	}
	if strings.TrimSpace(b.Toolchain.BuilderID) == "" {
		return nil, fmt.Errorf("fusion builder ID is required")
	}
	mainSource, err := GenerateGoAggregator(recipe)
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp(b.TempRoot, "pulp-fusion-")
	if err != nil {
		return nil, fmt.Errorf("create fusion workspace: %w", err)
	}
	defer os.RemoveAll(directory)
	if b.Archives == nil {
		return nil, fmt.Errorf("fusion source archive provider is required")
	}
	sources := append([]Source{b.Toolchain.FiberSource}, recipeSources(recipe)...)
	for _, dependency := range recipe.Dependencies {
		sources = append(sources, Source{ModulePath: dependency.ModulePath, ImportPath: dependency.ModulePath, Version: dependency.Version, SourceSHA256: dependency.SourceSHA256, Entrypoint: "Register"})
	}
	if err := validateSource(sources[0]); err != nil || sources[0].ModulePath != "github.com/BananaLabs-OSS/Fiber" || sources[0].Version != b.Toolchain.FiberVersion {
		return nil, fmt.Errorf("verified Fiber source matching toolchain version is required")
	}
	replacements := make(map[string]string, len(sources))
	for index, source := range sources {
		archive, err := b.Archives.Archive(ctx, source)
		if err != nil {
			return nil, fmt.Errorf("read source archive %q: %w", source.ModulePath, err)
		}
		destination := filepath.Join(directory, "sources", fmt.Sprintf("%03d", index))
		if err := materializeSourceArchive(destination, source, archive); err != nil {
			return nil, err
		}
		replacements[source.ModulePath] = filepath.ToSlash(filepath.Join("sources", fmt.Sprintf("%03d", index)))
	}
	goModule, err := generateGoModule(recipe, b.Toolchain.FiberVersion, replacements)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(directory, "main.go"), mainSource, 0o600); err != nil {
		return nil, fmt.Errorf("write fusion main: %w", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "go.mod"), goModule, 0o600); err != nil {
		return nil, fmt.Errorf("write fusion go.mod: %w", err)
	}
	artifactPath := filepath.Join(directory, "fusion.wasm")
	environment := append([]string(nil), b.Toolchain.Environment...)
	// Security-critical settings come last so callers cannot re-enable network
	// resolution or reuse ambient module/build caches.
	environment = append(environment, "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOENV=off", "GOMODCACHE="+filepath.Join(directory, "modcache"), "GOPATH="+filepath.Join(directory, "gopath"), "GOCACHE="+filepath.Join(directory, "buildcache"))
	if err := b.Runner.Run(ctx, directory, environment, goCommand, "build", "-trimpath", "-buildvcs=false", "-buildmode=c-shared", "-ldflags=-buildid=", "-o", artifactPath, "."); err != nil {
		return nil, fmt.Errorf("build fused artifact: %w", err)
	}
	wasm, err := os.ReadFile(artifactPath)
	if err != nil {
		return nil, fmt.Errorf("read fused artifact: %w", err)
	}
	artifactDigest := registry.Sum(wasm)
	metadata, err := NewArtifactMetadata(recipe, strings.TrimPrefix(artifactDigest.String(), "sha256:"))
	if err != nil {
		return nil, err
	}
	return &BuildResult{Recipe: recipe, MainSource: mainSource, GoModule: goModule, Wasm: wasm, Attestation: Attestation{Metadata: metadata, Builder: b.Toolchain.BuilderID}}, nil
}

// GenerateGoModule returns a stable module file for the generated aggregator.
// Conflicting versions of the same source module are rejected rather than
// delegated to Go's minimum-version selection.
func GenerateGoModule(recipe Recipe, fiberVersion string) ([]byte, error) {
	return generateGoModule(recipe, fiberVersion, nil)
}

func generateGoModule(recipe Recipe, fiberVersion string, replacements map[string]string) ([]byte, error) {
	canonicalizeRecipe(&recipe)
	if strings.TrimSpace(recipe.GoVersion) == "" {
		return nil, fmt.Errorf("fusion recipe go_version is required")
	}
	fiberVersion = strings.TrimSpace(fiberVersion)
	if fiberVersion == "" {
		return nil, fmt.Errorf("Fiber module version is required")
	}
	digest, err := RecipeDigest(recipe)
	if err != nil {
		return nil, err
	}
	modules := map[string]string{"github.com/BananaLabs-OSS/Fiber": fiberVersion}
	for _, member := range recipe.Members {
		if err := validateSource(member.Source); err != nil {
			return nil, fmt.Errorf("member %q source: %w", member.Name, err)
		}
		if version, exists := modules[member.Source.ModulePath]; exists && version != member.Source.Version {
			return nil, fmt.Errorf("source module %q has conflicting versions %q and %q", member.Source.ModulePath, version, member.Source.Version)
		}
		modules[member.Source.ModulePath] = member.Source.Version
	}
	for _, dependency := range recipe.Dependencies {
		if version, exists := modules[dependency.ModulePath]; exists && version != dependency.Version {
			return nil, fmt.Errorf("dependency module %q has conflicting versions %q and %q", dependency.ModulePath, version, dependency.Version)
		}
		modules[dependency.ModulePath] = dependency.Version
	}
	paths := make([]string, 0, len(modules))
	for path := range modules {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	goVersion := strings.TrimPrefix(strings.TrimSpace(recipe.GoVersion), "go")
	var output strings.Builder
	fmt.Fprintf(&output, "module pulp.local/fusion/%s\n\ngo %s\n\nrequire (\n", digest, goVersion)
	for _, path := range paths {
		fmt.Fprintf(&output, "\t%s %s\n", path, modules[path])
	}
	output.WriteString(")\n")
	if len(replacements) > 0 {
		output.WriteString("\nreplace (\n")
		for _, modulePath := range paths {
			if replacement := replacements[modulePath]; replacement != "" {
				fmt.Fprintf(&output, "\t%s => ./%s\n", modulePath, replacement)
			}
		}
		output.WriteString(")\n")
	}
	return []byte(output.String()), nil
}

func recipeSources(recipe Recipe) []Source {
	canonicalizeRecipe(&recipe)
	out := make([]Source, 0, len(recipe.Members))
	for _, member := range recipe.Members {
		out = append(out, member.Source)
	}
	return out
}

// RegistryPublication converts a build result into the exact immutable input
// accepted by registry.LocalStore.Publish. The attestation is a second
// content-addressed artifact, so registry transport does not discard it.
type RegistryPublication struct {
	Manifest []byte
	Blobs    map[registry.Digest][]byte
}

func (r *BuildResult) RegistryPublication(moduleID, version, target string) (*RegistryPublication, error) {
	if r == nil {
		return nil, fmt.Errorf("fusion build result is nil")
	}
	id, err := registry.ParseModuleID(moduleID)
	if err != nil {
		return nil, err
	}
	parsedVersion, err := registry.ParseVersion(version)
	if err != nil {
		return nil, err
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, fmt.Errorf("registry artifact target is required")
	}
	attestation, err := json.Marshal(r.Attestation)
	if err != nil {
		return nil, fmt.Errorf("marshal fusion attestation: %w", err)
	}
	wasmDigest, attestationDigest := registry.Sum(r.Wasm), registry.Sum(attestation)
	var manifestText strings.Builder
	fmt.Fprintf(&manifestText, "schema_version = 1\nprovides = %s\nconsumes = []\ncapabilities = %s\n\n[module]\nid = %s\nversion = %s\n\n[execution]\nabi = %s\n", tomlStrings(r.Attestation.Metadata.Providers), tomlStrings(r.Attestation.Metadata.Capabilities), strconv.Quote(string(id)), strconv.Quote(parsedVersion.String()), strconv.Quote(r.Attestation.Metadata.ABI))
	fmt.Fprintf(&manifestText, "\n[[artifacts]]\ntarget = %s\ndigest = %s\nsize = %d\n", strconv.Quote(target), strconv.Quote(wasmDigest.String()), len(r.Wasm))
	fmt.Fprintf(&manifestText, "\n[[artifacts]]\ntarget = %s\ndigest = %s\nsize = %d\n", strconv.Quote(target+".fusion-attestation"), strconv.Quote(attestationDigest.String()), len(attestation))
	publication := &RegistryPublication{Manifest: []byte(manifestText.String()), Blobs: map[registry.Digest][]byte{wasmDigest: r.Wasm, attestationDigest: attestation}}
	if _, err := registry.ParseManifest(publication.Manifest); err != nil {
		return nil, fmt.Errorf("generated registry manifest: %w", err)
	}
	return publication, nil
}

func tomlStrings(values []string) string {
	values = canonicalSet(values)
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = strconv.Quote(value)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
