package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// CurrentAppSchemaVersion is the pulp.app.toml schema supported by this host.
const CurrentAppSchemaVersion = 1

// Application is a loaded, verified application composition.
type Application struct {
	SchemaVersion int
	Name          string
	Version       string
	ManifestPath  string

	CellManifestPaths    []string
	OrchestratorCell     string
	OrchestratorManifest string
	OrchestrationScript  string
	OrchestrationSHA256  string
	RequireWASMSHA256    bool
	Cells                *Set
	// Placements expands reusable cell manifests into independently stateful
	// instances for this application. Legacy apps have one `primary` placement
	// per entry in Cells.Order.
	Placements []CellPlacement
}

// CellPlacement is one independently instantiated copy of a reusable cell
// package. Address is the local sibling-call target: singleton packages keep
// their legacy cell name; repeated packages require `cell@instance`.
type CellPlacement struct {
	Spec       *CellSpec
	InstanceID string
	Address    string
}

type rawApplication struct {
	SchemaVersion     int                `toml:"schema_version"`
	Name              string             `toml:"name"`
	Version           string             `toml:"version"`
	Cells             []string           `toml:"cells"`
	RequireWASMSHA256 bool               `toml:"require_wasm_sha256"`
	CellPlacements    []rawCellPlacement `toml:"cell_placements"`
	Orchestrator      rawOrchestrator    `toml:"orchestrator"`
}

// rawCellPlacement intentionally references a template cell by its manifest
// name rather than duplicating a manifest path. `instances` is the most
// explicit form; `count` with `aliases` is accepted for ergonomic generated
// fleets. Config merges over the template config for every placement.
type rawCellPlacement struct {
	Cell      string         `toml:"cell"`
	Instances []string       `toml:"instances"`
	Count     int            `toml:"count"`
	Aliases   []string       `toml:"aliases"`
	Config    map[string]any `toml:"config"`
}

type rawOrchestrator struct {
	Manifest string `toml:"manifest"`
	Script   string `toml:"script"`
	SHA256   string `toml:"sha256"`
}

// LoadApp reads a pulp.app.toml, resolves all referenced paths relative to the
// app manifest, verifies the Lua script digest, injects the script into the
// selected orchestrator cell's config, and runs the ordinary cell-set
// validation and topological ordering.
func LoadApp(path string) (*Application, error) {
	manifestPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("app manifest path: %w", err)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read app manifest: %w", err)
	}

	var raw rawApplication
	meta, err := toml.Decode(string(data), &raw)
	if err != nil {
		return nil, fmt.Errorf("parse app manifest: %w", err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		names := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			names = append(names, key.String())
		}
		return nil, fmt.Errorf("unknown app manifest fields: %s", strings.Join(names, ", "))
	}

	schemaVersion := raw.SchemaVersion
	if schemaVersion == 0 {
		schemaVersion = CurrentAppSchemaVersion
	}
	if schemaVersion < 1 {
		return nil, fmt.Errorf("app schema_version must be >= 1 (got %d)", schemaVersion)
	}
	if schemaVersion > CurrentAppSchemaVersion {
		return nil, fmt.Errorf("app schema_version %d is newer than host supports (max %d)",
			schemaVersion, CurrentAppSchemaVersion)
	}
	name := strings.TrimSpace(raw.Name)
	if name == "" {
		return nil, errors.New("app name is required")
	}
	version := strings.TrimSpace(raw.Version)
	if version == "" {
		return nil, errors.New("app version is required")
	}
	if len(raw.Cells) == 0 {
		return nil, errors.New("app cells must contain at least one cell manifest")
	}

	baseDir := filepath.Dir(manifestPath)
	cellPaths := make([]string, 0, len(raw.Cells))
	seenPaths := make(map[string]struct{}, len(raw.Cells))
	for index, relative := range raw.Cells {
		resolved, err := resolveAppRelativePath(baseDir, relative, fmt.Sprintf("cells[%d]", index))
		if err != nil {
			return nil, err
		}
		key := pathKey(resolved)
		if _, exists := seenPaths[key]; exists {
			return nil, fmt.Errorf("duplicate cell manifest path %q", relative)
		}
		seenPaths[key] = struct{}{}
		cellPaths = append(cellPaths, resolved)
	}

	orchestratorManifest, err := resolveAppRelativePath(
		baseDir, raw.Orchestrator.Manifest, "orchestrator.manifest")
	if err != nil {
		return nil, err
	}
	if _, exists := seenPaths[pathKey(orchestratorManifest)]; !exists {
		return nil, fmt.Errorf("orchestrator manifest %q is not listed in app cells", raw.Orchestrator.Manifest)
	}
	scriptPath, err := resolveAppRelativePath(baseDir, raw.Orchestrator.Script, "orchestrator.script")
	if err != nil {
		return nil, err
	}
	expectedDigest, err := parseAppSHA256(raw.Orchestrator.SHA256)
	if err != nil {
		return nil, err
	}
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		return nil, fmt.Errorf("read orchestration script: %w", err)
	}
	actualDigest := sha256.Sum256(script)
	if actualDigest != expectedDigest {
		return nil, fmt.Errorf("orchestrator script SHA-256 mismatch: got %x, want %x",
			actualDigest, expectedDigest)
	}

	specs := make([]*CellSpec, 0, len(cellPaths))
	var orchestrator *CellSpec
	for _, cellPath := range cellPaths {
		spec, err := Load(cellPath)
		if err != nil {
			return nil, fmt.Errorf("app cell %s: %w", cellPath, err)
		}
		if err := verifyWASMDigest(spec, raw.RequireWASMSHA256); err != nil {
			return nil, fmt.Errorf("app cell %s: %w", cellPath, err)
		}
		specs = append(specs, spec)
		if samePath(spec.ManifestPath, orchestratorManifest) {
			orchestrator = spec
		}
	}
	if orchestrator == nil {
		return nil, fmt.Errorf("orchestrator manifest %q did not load as an app cell", raw.Orchestrator.Manifest)
	}
	if orchestrator.Config == nil {
		orchestrator.Config = map[string]any{}
	}
	orchestrator.Config["script"] = string(script)

	set, err := buildSet(specs)
	if err != nil {
		return nil, fmt.Errorf("validate app cells: %w", err)
	}
	placements, err := buildAppPlacements(set, raw.CellPlacements)
	if err != nil {
		return nil, fmt.Errorf("validate app cell placements: %w", err)
	}
	orchestratorPlacements := 0
	for _, placement := range placements {
		if placement.Spec.Name == orchestrator.Name {
			orchestratorPlacements++
		}
	}
	if orchestratorPlacements != 1 {
		return nil, fmt.Errorf("orchestrator cell %q must have exactly one placement (got %d)", orchestrator.Name, orchestratorPlacements)
	}
	return &Application{
		SchemaVersion:        schemaVersion,
		Name:                 name,
		Version:              version,
		ManifestPath:         manifestPath,
		CellManifestPaths:    cellPaths,
		OrchestratorCell:     orchestrator.Name,
		OrchestratorManifest: orchestratorManifest,
		OrchestrationScript:  scriptPath,
		OrchestrationSHA256:  hex.EncodeToString(expectedDigest[:]),
		RequireWASMSHA256:    raw.RequireWASMSHA256,
		Cells:                set,
		Placements:           placements,
	}, nil
}

func buildAppPlacements(set *Set, rawPlacements []rawCellPlacement) ([]CellPlacement, error) {
	byName := make(map[string]*CellSpec, len(set.Cells))
	for _, spec := range set.Cells {
		byName[spec.Name] = spec
	}
	configured := make(map[string]rawCellPlacement, len(rawPlacements))
	for _, placement := range rawPlacements {
		placement.Cell = strings.TrimSpace(placement.Cell)
		if placement.Cell == "" {
			return nil, errors.New("cell_placements.cell is required")
		}
		if _, exists := byName[placement.Cell]; !exists {
			return nil, fmt.Errorf("cell_placements references unknown cell %q", placement.Cell)
		}
		if _, duplicate := configured[placement.Cell]; duplicate {
			return nil, fmt.Errorf("duplicate cell_placements entry for %q", placement.Cell)
		}
		configured[placement.Cell] = placement
	}

	placements := make([]CellPlacement, 0, len(set.Cells))
	counts := make(map[string]int, len(set.Cells))
	instancesByCell := make(map[string][]string, len(set.Cells))
	for _, spec := range set.Order {
		placement, configuredHere := configured[spec.Name]
		instances, err := placementInstances(placement, configuredHere)
		if err != nil {
			return nil, fmt.Errorf("cell %q: %w", spec.Name, err)
		}
		counts[spec.Name] = len(instances)
		instancesByCell[spec.Name] = instances
	}
	for _, spec := range set.Order {
		placement, configuredHere := configured[spec.Name]
		instances, _ := placementInstances(placement, configuredHere)
		for _, instance := range instances {
			cloned := clonePlacementSpec(spec, placement.Config)
			address := spec.Name
			if counts[spec.Name] > 1 {
				address += "@" + instance
			}
			placements = append(placements, CellPlacement{Spec: cloned, InstanceID: instance, Address: address})
		}
	}
	_ = instancesByCell // retained as an explicit construction boundary for future per-instance dependency selectors.
	return placements, nil
}

func placementInstances(placement rawCellPlacement, configured bool) ([]string, error) {
	if !configured {
		return []string{"primary"}, nil
	}
	instances := append([]string(nil), placement.Instances...)
	if len(instances) == 0 && len(placement.Aliases) > 0 {
		instances = append(instances, placement.Aliases...)
	}
	if placement.Count < 0 {
		return nil, errors.New("count must not be negative")
	}
	if placement.Count > 0 {
		if len(instances) > 0 && len(instances) != placement.Count {
			return nil, fmt.Errorf("count %d does not match %d instance aliases", placement.Count, len(instances))
		}
		for index := len(instances); index < placement.Count; index++ {
			instances = append(instances, fmt.Sprintf("%d", index+1))
		}
	}
	if len(instances) == 0 {
		return nil, errors.New("requires instances or count")
	}
	seen := make(map[string]struct{}, len(instances))
	for index, instance := range instances {
		instance = strings.TrimSpace(instance)
		if instance == "" {
			return nil, fmt.Errorf("instance %d is empty", index)
		}
		if strings.Contains(instance, "@") {
			return nil, fmt.Errorf("instance %q may not contain @", instance)
		}
		if _, duplicate := seen[instance]; duplicate {
			return nil, fmt.Errorf("duplicate instance %q", instance)
		}
		seen[instance] = struct{}{}
		instances[index] = instance
	}
	return instances, nil
}

func clonePlacementSpec(source *CellSpec, override map[string]any) *CellSpec {
	cloned := *source
	if len(source.Config) == 0 && len(override) == 0 {
		return &cloned
	}
	cloned.Config = make(map[string]any, len(source.Config)+len(override))
	for key, value := range source.Config {
		cloned.Config[key] = value
	}
	for key, value := range override {
		cloned.Config[key] = value
	}
	return &cloned
}

func resolveAppRelativePath(baseDir, relative, field string) (string, error) {
	relative = strings.TrimSpace(relative)
	if relative == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("%s must be relative to pulp.app.toml", field)
	}
	resolved, err := filepath.Abs(filepath.Join(baseDir, relative))
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", field, err)
	}
	return resolved, nil
}

func parseAppSHA256(value string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	normalized, err := normalizeSHA256(value, "orchestrator.sha256")
	if err != nil {
		return digest, err
	}
	decoded, _ := hex.DecodeString(normalized)
	copy(digest[:], decoded)
	return digest, nil
}

func pathKey(path string) string {
	return filepath.Clean(path)
}

func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}
