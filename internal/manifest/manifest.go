// Package manifest reads and validates pulp.plugin.toml files.
//
// A manifest declares everything the host needs to know about a plugin
// before instantiating it: identity, what functions the plugin provides
// to siblings, what it consumes from siblings, which host primitives
// ("capabilities") it touches, and its free-form [config] table.
//
// The parser does NOT resolve cross-plugin dependencies or touch WASM.
// It produces a [PluginSpec] that downstream code (loader, dependency
// resolver, capability binder) consumes.
package manifest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// PluginSpec is the parsed, validated, normalized form of a pulp.plugin.toml.
//
// It contains everything the host needs to decide how to instantiate the
// plugin. Config is returned as the raw TOML table so it can be encoded to
// whatever wire format the host chooses — MessagePack in v0.2.
type PluginSpec struct {
	// Identity.
	Name    string
	Version string

	// Dependency graph inputs. Resolved by the dependency resolver (not here).
	Provides []string
	Consumes []string

	// Capability declarations. Normalized to lowercase. The host binds only
	// imports that match declared capabilities; everything else fails loudly.
	Capabilities []string

	// SharedMemoryGroups — opt-in zero-copy regions between cooperating
	// plugins. Absent from v0.2 linking but parsed so manifests are
	// forward-compatible.
	SharedMemoryGroups []string

	// Operational knobs.
	DedicatedThread bool
	Snapshotable    bool

	// Free-form plugin config. The TOML [config] table as a generic map —
	// the host encodes it to MessagePack before handing it to pulp_init.
	// Absent or empty table => nil map.
	Config map[string]any

	// ManifestPath is the absolute path the manifest was loaded from.
	// Used to resolve relative WASM paths.
	ManifestPath string

	// WASMPath is the absolute path to the plugin's .wasm file. Resolved
	// from the manifest's `wasm =` field (relative paths are relative to
	// the manifest). If the field is absent, defaults to plugin.wasm next
	// to the manifest.
	WASMPath string
}

// raw mirrors the TOML schema exactly. It's the only struct BurntSushi/toml
// unmarshals into. Normalization happens afterward in Load.
type raw struct {
	Name    string `toml:"name"`
	Version string `toml:"version"`

	WASM string `toml:"wasm"`

	Provides     []string `toml:"provides"`
	Consumes     []string `toml:"consumes"`
	Capabilities []string `toml:"capabilities"`

	SharedMemoryGroups []string `toml:"shared_memory_groups"`

	DedicatedThread bool `toml:"dedicated_thread"`
	Snapshotable    bool `toml:"snapshotable"`

	// Reserved for federation (v0.4+). Parsed so v0.1/v0.2 manifests that
	// declare them work unchanged when federation lands.
	FederatedCallers  []string `toml:"federated_callers"`
	FederatedConsumes []string `toml:"federated_consumes"`
	Migratable        bool     `toml:"migratable"`

	Config map[string]any `toml:"config"`
}

// Load reads, parses, and validates a pulp.plugin.toml at path. Returns a
// [PluginSpec] ready for the host to instantiate.
//
// Unknown top-level keys are treated as errors — catches typos at boot
// rather than silently ignoring mis-spelled capabilities or provides.
func Load(path string) (*PluginSpec, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("manifest path: %w", err)
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var r raw
	meta, err := toml.Decode(string(data), &r)
	if err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}

	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		names := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			names = append(names, k.String())
		}
		return nil, fmt.Errorf("unknown manifest fields: %s", strings.Join(names, ", "))
	}

	spec, err := normalize(&r, abs)
	if err != nil {
		return nil, fmt.Errorf("validate manifest: %w", err)
	}
	return spec, nil
}

// normalize validates required fields, lowercases capabilities, dedupes
// string slices, and resolves paths.
func normalize(r *raw, manifestPath string) (*PluginSpec, error) {
	if strings.TrimSpace(r.Name) == "" {
		return nil, errors.New("name is required")
	}
	if strings.TrimSpace(r.Version) == "" {
		return nil, errors.New("version is required")
	}

	dir := filepath.Dir(manifestPath)
	wasmPath := r.WASM
	if wasmPath == "" {
		wasmPath = "plugin.wasm"
	}
	if !filepath.IsAbs(wasmPath) {
		wasmPath = filepath.Join(dir, wasmPath)
	}

	return &PluginSpec{
		Name:               strings.TrimSpace(r.Name),
		Version:            strings.TrimSpace(r.Version),
		Provides:           dedupe(r.Provides),
		Consumes:           dedupe(r.Consumes),
		Capabilities:       dedupe(lowerAll(r.Capabilities)),
		SharedMemoryGroups: dedupe(r.SharedMemoryGroups),
		DedicatedThread:    r.DedicatedThread,
		Snapshotable:       r.Snapshotable,
		Config:             r.Config,
		ManifestPath:       manifestPath,
		WASMPath:           wasmPath,
	}, nil
}

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(strings.TrimSpace(s))
	}
	return out
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
