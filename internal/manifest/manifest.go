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
	// HostConsumes lists exact provider/functions this cell may call through
	// pulp_app_call_v1. Unlike Consumes, these are resolved only by LoadHost
	// against direct dependency applications.
	HostConsumes []string

	// DependsOn lists cell names (not capabilities) that must finish Init
	// before this cell starts. The host refuses to boot on cycles or
	// references to cell names absent from the manifest set.
	DependsOn []string

	// Capability declarations. Normalized to lowercase. The host binds only
	// imports that match declared capabilities; everything else fails loudly.
	Capabilities []string

	// Execution declares how Pulp may place this logical cell at runtime.
	// It never changes the cell's public provides/consumes contract. The
	// zero value is isolated, preserving the existing one-cell/one-instance
	// behaviour for every manifest written before execution planning existed.
	Execution ExecutionSpec

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
	// call context is cancelled. Application runtimes load supervised cells
	// interruptibly, so a pure-Wasm loop is trapped at this deadline and only
	// that placement is eligible for re-instantiation. Low-level embedders that
	// call host.Load directly may explicitly choose non-interruptible behavior.
	CallTimeoutMS uint32

	// Restart is the post-exit policy: "on_crash" (default), "never", or
	// "always". Parsed + validated now; the supervisor that honors it ships
	// in a later Pulp version.
	Restart string

	// Free-form cell config. The TOML [config] table as a generic map —
	// the host encodes it to MessagePack before handing it to pulp_init.
	// Absent or empty table => nil map.
	Config map[string]any

	// ConfigEnv maps config keys to host environment variable names. Only
	// explicitly mapped values are copied into this cell's config, avoiding
	// secret exposure to unrelated WASM modules. An unset variable leaves the
	// manifest value intact.
	ConfigEnv map[string]string

	// ManifestPath is the absolute path the manifest was loaded from.
	// Used to resolve relative WASM paths.
	ManifestPath string

	// WASMPath is the absolute path to the cell's .wasm file. Resolved
	// from the manifest's `wasm =` field (relative paths are relative to
	// the manifest). If the field is absent, defaults to cell.wasm next
	// to the manifest.
	WASMPath string

	// WASMSHA256 optionally pins the exact bytes of WASMPath. It is enforced
	// when the cell is loaded as part of an application; an application can
	// require this field for every cell with require_wasm_sha256 = true.
	WASMSHA256 string
}

// ExecutionMode is the requested physical-layout policy for one logical cell.
const (
	ExecutionIsolated = "isolated"
	ExecutionFusible  = "fusible"
)

// ExecutionSpec is deliberately small: a fusible cell opts into Pulp's
// internal ABI and names the desired execution group. A planner may still
// reject a group (and run it isolated) when its security or lifecycle
// constraints do not match. This makes optimisation safe-by-default.
type ExecutionSpec struct {
	Mode  string
	Group string
	ABI   string
}

// raw mirrors the TOML schema exactly. It's the only struct BurntSushi/toml
// unmarshals into. Normalization happens afterward in Load.
type raw struct {
	SchemaVersion int `toml:"schema_version"`

	Name    string `toml:"name"`
	Version string `toml:"version"`

	WASM       string  `toml:"wasm"`
	WASMSHA256 *string `toml:"wasm_sha256"`

	Provides     []string     `toml:"provides"`
	Consumes     []string     `toml:"consumes"`
	HostConsumes []string     `toml:"host_consumes"`
	DependsOn    []string     `toml:"depends_on"`
	Capabilities []string     `toml:"capabilities"`
	Execution    rawExecution `toml:"execution"`

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

	Config    map[string]any    `toml:"config"`
	ConfigEnv map[string]string `toml:"config_env"`
}

type rawExecution struct {
	Mode  string `toml:"mode"`
	Group string `toml:"group"`
	ABI   string `toml:"abi"`
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
			// Config is intentionally free-form. BurntSushi/TOML records nested
			// map descendants as undecoded even after placing them in Config;
			// only reject unknown manifest schema fields outside that boundary.
			name := k.String()
			if name == "config" || strings.HasPrefix(name, "config.") {
				continue
			}
			names = append(names, name)
		}
		if len(names) > 0 {
			return nil, fmt.Errorf("unknown manifest fields: %s", strings.Join(names, ", "))
		}
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
		// A cell is an independently recoverable unit. Leaving the default at
		// "never" turns one guest OOM or trap into a permanent application
		// outage until the whole host is restarted. The runtime's supervisor has
		// a bounded five-restarts-per-30-seconds circuit breaker, so on_crash is
		// both the safe default and still protects the host from crash loops.
		restart = RestartOnCrash
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

	hostConsumes, err := normalizeHostConsumes(r.HostConsumes)
	if err != nil {
		return nil, err
	}
	wasmSHA256 := ""
	if r.WASMSHA256 != nil {
		wasmSHA256, err = normalizeSHA256(*r.WASMSHA256, "wasm_sha256")
		if err != nil {
			return nil, err
		}
	}
	execution, err := normalizeExecution(r.Execution)
	if err != nil {
		return nil, err
	}

	config := cloneConfig(r.Config)
	for key, envName := range r.ConfigEnv {
		key = strings.TrimSpace(key)
		envName = strings.TrimSpace(envName)
		if key == "" || envName == "" {
			return nil, errors.New("config_env keys and environment variable names must be non-empty")
		}
		if value, ok := os.LookupEnv(envName); ok {
			if config == nil {
				config = map[string]any{}
			}
			if err := setConfigPath(config, key, value); err != nil {
				return nil, fmt.Errorf("config_env %q: %w", key, err)
			}
		}
	}

	return &CellSpec{
		SchemaVersion:      schemaVersion,
		Name:               strings.TrimSpace(r.Name),
		Version:            strings.TrimSpace(r.Version),
		Provides:           dedupe(r.Provides),
		Consumes:           dedupe(r.Consumes),
		HostConsumes:       hostConsumes,
		DependsOn:          dedupe(r.DependsOn),
		Capabilities:       dedupe(lowerAll(r.Capabilities)),
		Execution:          execution,
		SharedMemoryGroups: dedupe(r.SharedMemoryGroups),
		DedicatedThread:    r.DedicatedThread,
		Snapshotable:       r.Snapshotable,
		MaxMemoryPages:     r.MaxMemoryPages,
		CallTimeoutMS:      r.CallTimeoutMS,
		Restart:            restart,
		Config:             config,
		ConfigEnv:          r.ConfigEnv,
		ManifestPath:       manifestPath,
		WASMPath:           wasmPath,
		WASMSHA256:         wasmSHA256,
	}, nil
}

func setConfigPath(config map[string]any, path, value string) error {
	parts := strings.Split(path, ".")
	cursor := config
	for _, part := range parts[:len(parts)-1] {
		if part == "" {
			return errors.New("config path contains an empty segment")
		}
		next, exists := cursor[part]
		if !exists {
			child := map[string]any{}
			cursor[part] = child
			cursor = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("config path segment %q is not a table", part)
		}
		cursor = child
	}
	leaf := parts[len(parts)-1]
	if leaf == "" {
		return errors.New("config path contains an empty segment")
	}
	cursor[leaf] = value
	return nil
}

func cloneConfig(config map[string]any) map[string]any {
	if config == nil {
		return nil
	}
	clone := make(map[string]any, len(config))
	for key, value := range config {
		clone[key] = value
	}
	return clone
}

func normalizeExecution(r rawExecution) (ExecutionSpec, error) {
	mode := strings.ToLower(strings.TrimSpace(r.Mode))
	if mode == "" {
		mode = ExecutionIsolated
	}
	group := strings.TrimSpace(r.Group)
	abi := strings.TrimSpace(r.ABI)
	switch mode {
	case ExecutionIsolated:
		if group != "" || abi != "" {
			return ExecutionSpec{}, errors.New("execution.group and execution.abi require execution.mode = \"fusible\"")
		}
	case ExecutionFusible:
		if group == "" {
			return ExecutionSpec{}, errors.New("execution.group is required when execution.mode = \"fusible\"")
		}
		if abi == "" {
			return ExecutionSpec{}, errors.New("execution.abi is required when execution.mode = \"fusible\"")
		}
		if strings.IndexFunc(group, func(r rune) bool { return r <= ' ' }) >= 0 {
			return ExecutionSpec{}, errors.New("execution.group must not contain whitespace")
		}
	default:
		return ExecutionSpec{}, fmt.Errorf("execution.mode %q is not one of %q, %q", mode, ExecutionIsolated, ExecutionFusible)
	}
	return ExecutionSpec{Mode: mode, Group: group, ABI: abi}, nil
}

func normalizeHostConsumes(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for index, value := range values {
		normalized := strings.TrimSpace(value)
		if normalized == "" {
			return nil, fmt.Errorf("host_consumes[%d] must be a non-empty exact provider", index)
		}
		if normalized != value || strings.IndexFunc(normalized, func(r rune) bool { return r <= ' ' }) >= 0 {
			return nil, fmt.Errorf("host_consumes[%d] %q must not contain whitespace", index, value)
		}
		if strings.Count(normalized, "::") > 1 {
			return nil, fmt.Errorf("host_consumes[%d] %q has an invalid application qualifier", index, value)
		}
		if application, provider, qualified := strings.Cut(normalized, "::"); qualified &&
			(!hostIdentifier.MatchString(application) || provider == "") {
			return nil, fmt.Errorf("host_consumes[%d] %q has an invalid application qualifier", index, value)
		}
		if _, duplicate := seen[normalized]; duplicate {
			return nil, fmt.Errorf("duplicate host_consumes provider %q", normalized)
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out, nil
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
