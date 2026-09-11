package fusion

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

const (
	RecipeSchemaV1 = "pulp.fusion.recipe.v1"
	RecipeSchemaV2 = "pulp.fusion.recipe.v2"
	MemberABIV2    = "pulp-member-v2"
)

// Source describes registry-resolved Go source that implements one logical
// cell. SourceSHA256 binds the recipe to source bytes (or a canonical source
// archive), rather than to a mutable checkout path.
type Source struct {
	ModulePath   string `json:"module_path"`
	ImportPath   string `json:"import_path"`
	Version      string `json:"version"`
	SourceSHA256 string `json:"source_sha256"`
	Entrypoint   string `json:"entrypoint"`
}

type RecipeMember struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Providers    []string `json:"providers"`
	Capabilities []string `json:"capabilities"`
	Source       Source   `json:"source"`
	Config       []byte   `json:"config_msgpack,omitempty"`
	Snapshotable bool     `json:"snapshotable,omitempty"`
}

// Recipe is the portable input to a fusion backend. It intentionally contains
// no local filesystem paths. A module registry can store it directly and use
// Digest as its content-addressed key.
type Recipe struct {
	SchemaVersion string         `json:"schema_version"`
	Group         string         `json:"group"`
	ABI           string         `json:"abi"`
	GoVersion     string         `json:"go_version"`
	Members       []RecipeMember `json:"members"`
	Dependencies  []Dependency   `json:"dependencies,omitempty"`
}

// Dependency binds a non-member Go module used by Fiber or a member. It keeps
// upstream identity and native Go checksums while Pulp binds the exact source
// archive used by the offline build.
type Dependency struct {
	ModulePath   string `json:"module_path"`
	Version      string `json:"version"`
	SourceSHA256 string `json:"source_sha256"`
	Origin       string `json:"origin,omitempty"`
	GoSum        string `json:"go_sum,omitempty"`
	GoModSum     string `json:"go_mod_sum,omitempty"`
}

// ArtifactMetadata is emitted beside a fused Wasm artifact. It is deliberately
// independent of pulp.app.toml so a registry can verify it before selection.
type ArtifactMetadata struct {
	SchemaVersion  string   `json:"schema_version"`
	Group          string   `json:"group"`
	ABI            string   `json:"abi"`
	Members        []string `json:"members"`
	Providers      []string `json:"providers"`
	Capabilities   []string `json:"capabilities"`
	RecipeSHA256   string   `json:"recipe_sha256"`
	ArtifactSHA256 string   `json:"artifact_sha256"`
}

// Attestation is the registry-facing binding between a deterministic recipe
// and the artifact produced from it.
type Attestation struct {
	Metadata ArtifactMetadata `json:"metadata"`
	Builder  string           `json:"builder"`
}

// NewRecipe derives contract fields from an accepted plan group and attaches
// registry-resolved source records. Members and sets are canonicalized.
func NewRecipe(group Group, goVersion string, sources map[string]Source) (Recipe, error) {
	r := Recipe{SchemaVersion: RecipeSchemaV1, Group: group.Name, ABI: group.ABI, GoVersion: strings.TrimSpace(goVersion)}
	if group.ABI == MemberABIV2 {
		r.SchemaVersion = RecipeSchemaV2
	}
	for _, spec := range group.Members {
		if spec == nil {
			return Recipe{}, fmt.Errorf("fusion group %q contains a nil member", group.Name)
		}
		source, ok := sources[spec.Name]
		if !ok {
			return Recipe{}, fmt.Errorf("fusion member %q has no source descriptor", spec.Name)
		}
		config, err := manifest.EncodeConfig(spec.Config)
		if err != nil {
			return Recipe{}, fmt.Errorf("fusion member %q config: %w", spec.Name, err)
		}
		member := RecipeMember{Name: spec.Name, Version: spec.Version, Providers: canonicalSet(spec.Provides), Capabilities: canonicalSet(spec.Capabilities), Source: source}
		if r.SchemaVersion == RecipeSchemaV2 {
			member.Config, member.Snapshotable = config, spec.Snapshotable
		}
		r.Members = append(r.Members, member)
	}
	canonicalizeRecipe(&r)
	if err := ValidateRecipe(r, group); err != nil {
		return Recipe{}, err
	}
	return r, nil
}

func ValidateRecipe(recipe Recipe, group Group) error {
	if recipe.SchemaVersion != RecipeSchemaV1 && recipe.SchemaVersion != RecipeSchemaV2 {
		return fmt.Errorf("unsupported fusion recipe schema %q", recipe.SchemaVersion)
	}
	if recipe.Group != group.Name {
		return fmt.Errorf("recipe group %q does not match plan group %q", recipe.Group, group.Name)
	}
	if recipe.ABI == "" || recipe.ABI != group.ABI {
		return fmt.Errorf("recipe ABI %q does not match plan ABI %q", recipe.ABI, group.ABI)
	}
	if (recipe.SchemaVersion == RecipeSchemaV2) != (recipe.ABI == MemberABIV2) {
		return fmt.Errorf("fusion recipe schema and member ABI do not match")
	}
	if strings.TrimSpace(recipe.GoVersion) == "" {
		return fmt.Errorf("fusion recipe go_version is required")
	}
	want := make(map[string]*manifest.CellSpec, len(group.Members))
	for _, spec := range group.Members {
		if spec != nil {
			if spec.Execution.ABI != "" && spec.Execution.ABI != group.ABI {
				return fmt.Errorf("plan member %q ABI %q does not match group ABI %q", spec.Name, spec.Execution.ABI, group.ABI)
			}
			want[spec.Name] = spec
		}
	}
	if len(recipe.Members) != len(want) {
		return fmt.Errorf("recipe has %d members, plan has %d", len(recipe.Members), len(want))
	}
	seen := map[string]bool{}
	providerOwner := map[string]string{}
	for _, member := range recipe.Members {
		spec := want[member.Name]
		if spec == nil {
			return fmt.Errorf("recipe contains unexpected member %q", member.Name)
		}
		if seen[member.Name] {
			return fmt.Errorf("recipe contains duplicate member %q", member.Name)
		}
		seen[member.Name] = true
		if member.Version != spec.Version {
			return fmt.Errorf("member %q version %q does not match %q", member.Name, member.Version, spec.Version)
		}
		if !equalCanonicalSet(member.Providers, spec.Provides) {
			return fmt.Errorf("member %q providers do not exactly match its manifest", member.Name)
		}
		if !equalCanonicalSet(member.Capabilities, spec.Capabilities) {
			return fmt.Errorf("member %q capabilities do not exactly match its manifest", member.Name)
		}
		if recipe.SchemaVersion == RecipeSchemaV2 {
			config, err := manifest.EncodeConfig(spec.Config)
			if err != nil {
				return err
			}
			if !strings.EqualFold(hex.EncodeToString(member.Config), hex.EncodeToString(config)) || member.Snapshotable != spec.Snapshotable {
				return fmt.Errorf("member %q v2 scope does not match its manifest", member.Name)
			}
		}
		for _, provider := range canonicalSet(member.Providers) {
			if owner := providerOwner[provider]; owner != "" {
				return fmt.Errorf("provider %q is owned by both %q and %q", provider, owner, member.Name)
			}
			providerOwner[provider] = member.Name
		}
		if err := validateSource(member.Source); err != nil {
			return fmt.Errorf("member %q source: %w", member.Name, err)
		}
		wantEntrypoint := "Register"
		if recipe.SchemaVersion == RecipeSchemaV2 {
			wantEntrypoint = "RegisterV2"
		}
		if member.Source.Entrypoint != wantEntrypoint {
			return fmt.Errorf("member %q entrypoint must be %s for %s", member.Name, wantEntrypoint, recipe.SchemaVersion)
		}
	}
	return nil
}

// validateBuildRecipe checks the self-contained invariants a backend can
// verify after the original manifest plan is no longer present.
func validateBuildRecipe(recipe Recipe) error {
	if recipe.SchemaVersion != RecipeSchemaV1 && recipe.SchemaVersion != RecipeSchemaV2 {
		return fmt.Errorf("unsupported fusion recipe schema %q", recipe.SchemaVersion)
	}
	if strings.TrimSpace(recipe.Group) == "" || strings.TrimSpace(recipe.ABI) == "" {
		return fmt.Errorf("fusion recipe group and ABI are required")
	}
	if (recipe.SchemaVersion == RecipeSchemaV2) != (recipe.ABI == MemberABIV2) {
		return fmt.Errorf("fusion recipe schema and member ABI do not match")
	}
	if strings.TrimSpace(recipe.GoVersion) == "" {
		return fmt.Errorf("fusion recipe go_version is required")
	}
	if len(recipe.Members) < 2 {
		return fmt.Errorf("fusion recipe requires at least two members")
	}
	dependencyPaths := map[string]bool{}
	for _, dependency := range recipe.Dependencies {
		if strings.TrimSpace(dependency.ModulePath) == "" || strings.TrimSpace(dependency.Version) == "" {
			return fmt.Errorf("fusion dependency module_path and version are required")
		}
		if dependencyPaths[dependency.ModulePath] {
			return fmt.Errorf("fusion dependency %q is duplicated", dependency.ModulePath)
		}
		dependencyPaths[dependency.ModulePath] = true
		if err := validateSHA256(dependency.SourceSHA256); err != nil {
			return fmt.Errorf("fusion dependency %q source SHA-256: %w", dependency.ModulePath, err)
		}
	}
	names, imports, providerOwner := map[string]bool{}, map[string]string{}, map[string]string{}
	var capabilities []string
	for index, member := range recipe.Members {
		if strings.TrimSpace(member.Name) == "" || names[member.Name] {
			return fmt.Errorf("fusion recipe member %q is empty or duplicated", member.Name)
		}
		names[member.Name] = true
		if index == 0 {
			capabilities = member.Capabilities
		} else if !equalCanonicalSet(capabilities, member.Capabilities) {
			return fmt.Errorf("fusion recipe member %q capability set differs", member.Name)
		}
		for _, provider := range canonicalSet(member.Providers) {
			if owner := providerOwner[provider]; owner != "" {
				return fmt.Errorf("provider %q is owned by both %q and %q", provider, owner, member.Name)
			}
			providerOwner[provider] = member.Name
		}
		if err := validateSource(member.Source); err != nil {
			return fmt.Errorf("member %q source: %w", member.Name, err)
		}
		wantEntrypoint := "Register"
		if recipe.SchemaVersion == RecipeSchemaV2 {
			wantEntrypoint = "RegisterV2"
		}
		if member.Source.Entrypoint != wantEntrypoint {
			return fmt.Errorf("member %q entrypoint must be %s for %s", member.Name, wantEntrypoint, recipe.SchemaVersion)
		}
		if owner := imports[member.Source.ImportPath]; owner != "" {
			return fmt.Errorf("source import %q is used by both %q and %q", member.Source.ImportPath, owner, member.Name)
		}
		imports[member.Source.ImportPath] = member.Name
	}
	return nil
}

func NewArtifactMetadata(recipe Recipe, artifactSHA256 string) (ArtifactMetadata, error) {
	canonicalizeRecipe(&recipe)
	recipeDigest, err := RecipeDigest(recipe)
	if err != nil {
		return ArtifactMetadata{}, err
	}
	if err := validateSHA256(artifactSHA256); err != nil {
		return ArtifactMetadata{}, fmt.Errorf("artifact SHA-256: %w", err)
	}
	metadata := ArtifactMetadata{SchemaVersion: recipe.SchemaVersion, Group: recipe.Group, ABI: recipe.ABI, RecipeSHA256: recipeDigest, ArtifactSHA256: strings.ToLower(artifactSHA256)}
	for _, member := range recipe.Members {
		metadata.Members = append(metadata.Members, member.Name)
		metadata.Providers = append(metadata.Providers, member.Providers...)
		metadata.Capabilities = append(metadata.Capabilities, member.Capabilities...)
	}
	metadata.Members = canonicalSet(metadata.Members)
	metadata.Providers = canonicalSet(metadata.Providers)
	metadata.Capabilities = canonicalSet(metadata.Capabilities)
	return metadata, nil
}

func ValidateArtifact(metadata ArtifactMetadata, recipe Recipe) error {
	want, err := NewArtifactMetadata(recipe, metadata.ArtifactSHA256)
	if err != nil {
		return err
	}
	if metadata.SchemaVersion != want.SchemaVersion || metadata.Group != want.Group || metadata.ABI != want.ABI {
		return fmt.Errorf("artifact identity does not match fusion recipe")
	}
	if metadata.RecipeSHA256 != want.RecipeSHA256 {
		return fmt.Errorf("artifact recipe digest does not match fusion recipe")
	}
	if !equalCanonicalSet(metadata.Members, want.Members) {
		return fmt.Errorf("artifact members do not exactly match fusion recipe")
	}
	if !equalCanonicalSet(metadata.Providers, want.Providers) {
		return fmt.Errorf("artifact providers do not exactly match fusion recipe")
	}
	if !equalCanonicalSet(metadata.Capabilities, want.Capabilities) {
		return fmt.Errorf("artifact capabilities do not exactly match fusion recipe")
	}
	return nil
}

func RecipeDigest(recipe Recipe) (string, error) {
	canonicalizeRecipe(&recipe)
	wire, err := json.Marshal(recipe)
	if err != nil {
		return "", fmt.Errorf("marshal fusion recipe: %w", err)
	}
	digest := sha256.Sum256(wire)
	return hex.EncodeToString(digest[:]), nil
}

func (a Attestation) RegistryKey() string {
	return a.Metadata.RecipeSHA256 + "/" + a.Metadata.ArtifactSHA256
}

func canonicalizeRecipe(recipe *Recipe) {
	for i := range recipe.Members {
		recipe.Members[i].Providers = canonicalSet(recipe.Members[i].Providers)
		recipe.Members[i].Capabilities = canonicalSet(recipe.Members[i].Capabilities)
		recipe.Members[i].Source.SourceSHA256 = strings.ToLower(recipe.Members[i].Source.SourceSHA256)
	}
	for i := range recipe.Dependencies {
		recipe.Dependencies[i].SourceSHA256 = strings.ToLower(recipe.Dependencies[i].SourceSHA256)
	}
	sort.Slice(recipe.Members, func(i, j int) bool { return recipe.Members[i].Name < recipe.Members[j].Name })
	sort.Slice(recipe.Dependencies, func(i, j int) bool { return recipe.Dependencies[i].ModulePath < recipe.Dependencies[j].ModulePath })
}

func canonicalSet(values []string) []string {
	seen := map[string]struct{}{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			seen[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func equalCanonicalSet(a, b []string) bool {
	a, b = canonicalSet(a), canonicalSet(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validateSource(source Source) error {
	if strings.TrimSpace(source.ModulePath) == "" {
		return fmt.Errorf("module_path is required")
	}
	if strings.TrimSpace(source.ImportPath) == "" {
		return fmt.Errorf("import_path is required")
	}
	if strings.TrimSpace(source.Version) == "" {
		return fmt.Errorf("version is required")
	}
	if source.Entrypoint != "Register" && source.Entrypoint != "RegisterV2" {
		return fmt.Errorf("entrypoint must be Register or RegisterV2")
	}
	if err := validateSHA256(source.SourceSHA256); err != nil {
		return fmt.Errorf("source SHA-256: %w", err)
	}
	return nil
}

func validateSHA256(value string) error {
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("must contain 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("invalid hexadecimal digest")
	}
	return nil
}
