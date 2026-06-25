// Package manifest reads and validates pulp.cell.toml files.
//
// A manifest declares everything the host needs to know about a cell
// before instantiating it: identity, what functions the cell provides
// to siblings, what it consumes from siblings, which host primitives
// ("capabilities") it touches, and its free-form [config] table.
//
// The parser does NOT resolve cross-cell dependencies or touch WASM.
// It produces a [CellSpec] that downstream code (loader, dependency
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

// CurrentSchemaVersion is the manifest schema the host knows how to parse.
// Manifests that declare a higher schema_version are rejected.
const CurrentSchemaVersion = 1

// RestartNever / RestartOnCrash / RestartAlways are the accepted values of
// the manifest's restart field. The supervisor is not yet implemented;
// the field is parsed and validated now so manifests written today survive
// the v2 supervisor drop without rewrites.
const (
	RestartNever   = "never"
	RestartOnCrash = "on_crash"
	RestartAlways  = "always"
)

// CellSpec is the parsed, validated, normalized form of a pulp.cell.toml.
//
// It contains everything the host needs to decide how to instantiate the
// cell. Config is returned as the raw TOML table so it can be encoded to
// whatever wire format the host chooses — MessagePack in v0.2.
type CellSpec struct {
	// SchemaVersion is the manifest schema this cell was written against.
	// Defaults to CurrentSchemaVersion when absent. Manifests declaring a
	// higher version than the host supports are rejected.
	SchemaVersion int

	// Identity.
	Name    string
	Version string

	// Dependency graph inputs. Resolved by the dependency resolver (not here).
	Provides []string
	Consumes []string

	// DependsOn lists cell names (not capabilities) that must finish Init
	// before this cell starts. The host refuses to boot on cycles or
	// references to cell names absent from the manifest set.
	DependsOn []string

	// Capability declarations. Normalized to lowercase. The host binds only
	// imports that match declared capabilities; everything else fails loudly.
	Capabilities []string

	// SharedMemoryGroups — opt-in zero-copy regions between cooperating
	// cells. Absent from v0.2 linking but parsed so manifests are
	// forward-compatible.
	SharedMemoryGroups []string

	// Operational knobs.
	DedicatedThread bool
	Snapshotable    bool

	// MaxMemoryPages caps the cell's WASM linear memory, in 64 KiB pages.
	// 0 means "use the host default" (see host.DefaultMaxMemoryPages). The
	// host enforces this at instantiation so a runaway cell cannot grow
	// memory until it OOM-kills the host and every co-located cell.
	MaxMemoryPages uint32

	// CallTimeoutMS bounds a single pulp_init / pulp_step / pulp_on_call
	// invocation, in milliseconds. 0 means "use the host default" (see
	// host.DefaultCallTimeout). When the deadline elapses the host-side
	// call context is cancelled, which propagates cancellation to any
	// blocking host calls (I/O, sleep) the cell is waiting on. A runaway
	// pure-wasm loop is NOT interrupted — the host deliberately does NOT
	// set wazero's WithCloseOnContextDone (that would tear the whole module
	// down on first timeout, killing a long-lived reactor). Bounding a
	// runaway wasm loop requires an out-of-band supervisor.
	CallTimeoutMS uint32

	// Restart is the post-exit policy: "never" (default), "on_crash", or
	// "always". Parsed + validated now; the supervisor that honors it ships
	// in a later Pulp version.
	Restart string

	// Free-form cell config. The TOML [config] table as a generic map —
	// the host encodes it to MessagePack before handing it to pulp_init.
	// Absent or empty table => nil map.
	Config map[string]any

	// ManifestPath is the absolute path the manifest was loaded from.
	// Used to resolve relative WASM paths.
	ManifestPath string

	// WASMPath is the absolute path to the cell's .wasm file. Resolved
	// from the manifest's `wasm =` field (relative paths are relative to
	// the manifest). If the field is absent, defaults to cell.wasm next
	// to the manifest.
	WASMPath string
}

// raw mirrors the TOML schema exactly. It's the only struct BurntSushi/toml
// unmarshals into. Normalization happens afterward in Load.
type raw struct {
	SchemaVersion int `toml:"schema_version"`

	Name    string `toml:"name"`
	Version string `toml:"version"`

	WASM string `toml:"wasm"`

	Provides     []string `toml:"provides"`
	Consumes     []string `toml:"consumes"`
	DependsOn    []string `toml:"depends_on"`
	Capabilities []string `toml:"capabilities"`

	SharedMemoryGroups []string `toml:"shared_memory_groups"`

	DedicatedThread bool   `toml:"dedicated_thread"`
	Snapshotable    bool   `toml:"snapshotable"`
	Restart         string `toml:"restart"`

	MaxMemoryPages uint32 `toml:"max_memory_pages"`
	CallTimeoutMS  uint32 `toml:"call_timeout_ms"`

	// Reserved for federation (v0.4+). Parsed so v0.1/v0.2 manifests that
	// declare them work unchanged when federation lands.
	FederatedCallers  []string `toml:"federated_callers"`
	FederatedConsumes []string `toml:"federated_consumes"`
	Migratable        bool     `toml:"migratable"`

	Config map[string]any `toml:"config"`
}

// Load reads, parses, and validates a pulp.cell.toml at path. Returns a
// [CellSpec] ready for the host to instantiate.
//
// Unknown top-level keys are treated as errors — catches typos at boot
// rather than silently ignoring mis-spelled capabilities or provides.
func Load(path string) (*CellSpec, error) {
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
func normalize(r *raw, manifestPath string) (*CellSpec, error) {
	if strings.TrimSpace(r.Name) == "" {
		return nil, errors.New("name is required")
	}
	if strings.TrimSpace(r.Version) == "" {
		return nil, errors.New("version is required")
	}

	schemaVersion := r.SchemaVersion
	if schemaVersion == 0 {
		schemaVersion = CurrentSchemaVersion
	}
	if schemaVersion > CurrentSchemaVersion {
		return nil, fmt.Errorf("schema_version %d is newer than host supports (max %d)", schemaVersion, CurrentSchemaVersion)
	}
	if schemaVersion < 1 {
		return nil, fmt.Errorf("schema_version must be >= 1 (got %d)", schemaVersion)
	}

	restart := strings.TrimSpace(r.Restart)
	if restart == "" {
		restart = RestartNever
	}
	switch restart {
	case RestartNever, RestartOnCrash, RestartAlways:
	default:
		return nil, fmt.Errorf("restart %q is not one of %q, %q, %q", restart, RestartNever, RestartOnCrash, RestartAlways)
	}

	dir := filepath.Dir(manifestPath)
	wasmPath := r.WASM
	if wasmPath == "" {
		wasmPath = "cell.wasm"
	}
	if !filepath.IsAbs(wasmPath) {
		wasmPath = filepath.Join(dir, wasmPath)
	}

	return &CellSpec{
		SchemaVersion:      schemaVersion,
		Name:               strings.TrimSpace(r.Name),
		Version:            strings.TrimSpace(r.Version),
		Provides:           dedupe(r.Provides),
		Consumes:           dedupe(r.Consumes),
		DependsOn:          dedupe(r.DependsOn),
		Capabilities:       dedupe(lowerAll(r.Capabilities)),
		SharedMemoryGroups: dedupe(r.SharedMemoryGroups),
		DedicatedThread:    r.DedicatedThread,
		Snapshotable:       r.Snapshotable,
		MaxMemoryPages:     r.MaxMemoryPages,
		CallTimeoutMS:      r.CallTimeoutMS,
		Restart:            restart,
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
