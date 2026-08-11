// Package ext is Pulp's public extension API.
//
// Extensions add host-side capabilities to Pulp that cells can
// declare in their manifest — for example, wrapping the AWS SDK to
// expose a storage.s3 capability, or wrapping Stripe to expose a
// payment.stripe capability. Extensions live in their own Go
// packages and register themselves via an init() call to Register.
// The deployment's main.go blank-imports the extensions it wants
// and builds a single binary with everything statically linked.
//
// Typical extension package:
//
//	package s3ext
//
//	import "github.com/BananaLabs-OSS/Pulp/ext"
//
//	func init() {
//		ext.Register(ext.Capability{
//			Name:     "storage.s3",
//			Register: bindHostImports,
//			Stub:     stubHostImports,
//		})
//	}
//
// Typical deployment main.go:
//
//	package main
//
//	import (
//		_ "github.com/BananaLabs-OSS/Pulp-ext-s3"
//		_ "github.com/BananaLabs-OSS/Pulp-ext-stripe"
//
//		"github.com/BananaLabs-OSS/Pulp/cmd/pulp"
//	)
//
//	func main() { pulp.Main() }
package ext

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero"
)

// Cell is the minimal view of a loaded WASM cell that an
// extension's Register / Stub functions see.
type Cell interface {
	Name() string
}

// SetupEnv is passed to a capability's Setup function with everything
// it needs to initialize host-side resources (HTTP servers, databases,
// filesystem roots, etc.).
type SetupEnv struct {
	// Scope identifies the application placement that owns resources created
	// during setup. Zero retains the legacy CellName-derived namespace.
	Scope Scope
	// Endpoints receives lifecycle notices for host-visible endpoints created
	// by this extension. Nil preserves legacy extension behavior: endpoint
	// discovery is optional and extensions may operate without a host gateway.
	Endpoints   EndpointReporter
	CellName    string
	StorageRoot string
	// StorageNamespaces optionally aliases selected cell IDs into a stable
	// shared storage namespace. It is host-owned topology, not guest input.
	// Extensions that do not support shared namespaces ignore it.
	StorageNamespaces map[string]string
	// HTTPPort is an explicit host-selected listener port for legacy/direct
	// single-application operation. It avoids a process-global HTTP_PORT env
	// mutation; endpoint-reporter (multi-host) extensions may ignore it.
	HTTPPort string
	Config   map[string]any
	Logger   *slog.Logger
}

// Endpoint is a host-visible address owned by one scoped capability. Name
// distinguishes a capability's endpoint roles (for example, "public" from a
// future administration listener). Address is the actual bound address, not
// merely a requested listen value, so it is valid when the host assigned port
// zero dynamically.
type Endpoint struct {
	Scope      Scope
	Capability string
	Name       string
	Address    string
}

// EndpointReporter is host-owned, concurrency-safe endpoint discovery. Ready
// must reject an attempt to replace a live scoped endpoint with a different
// owner or address. Gone is best-effort notification when that endpoint is no
// longer available. A nil reporter means endpoint discovery is disabled.
type EndpointReporter interface {
	Ready(Endpoint) error
	Gone(Endpoint)
}

// EffectiveScope returns the validated explicit setup scope when one was
// supplied. Existing extensions that only populated CellName retain their
// stable legacy namespace without any registration API change.
func (env SetupEnv) EffectiveScope() Scope {
	if err := env.Scope.Validate(); err == nil {
		return env.Scope
	}
	return LegacyScope(env.CellName)
}

// StepEvent is a pending event an extension wants to deliver to the
// cell via the step loop. Kind is the event discriminator (e.g.
// "http.request", "ws.open"). Payload is the pre-encoded msgpack
// event data. ID is an opaque handle the extension uses to track
// which event was processed (passed back via Finalize).
//
// CellID names the cell this event is destined for. In
// single-cell deployments the field can be left empty; the host
// routes empty-CellID events to whichever cell declares the
// capability. In multi-cell deployments the extension is expected
// to tag each event with the correct cell name so the fanout
// router delivers it to that cell's step goroutine and no other.
// Extensions that do not yet populate CellID keep working with a
// deprecation log from the host.
type StepEvent struct {
	Kind    string
	Payload []byte
	ID      uint64
	CellID  string
}

// Capability is a named bundle of host imports plus optional
// lifecycle hooks and event sourcing. Extensions register one or
// more Capabilities via Register() in their init().
//
// Register/Stub bind host import functions into the WASM module.
// Setup/Teardown manage host-side resources (servers, connections).
// Poll/Finalize let the extension feed events into the step loop.
type Capability struct {
	Name string
	// Provider is the stable, deployment-selectable identity of the extension
	// implementation, normally its canonical Go module path (for example,
	// "github.com/BananaLabs-OSS/Pulp-ext-gin"). It must not be derived from
	// callback symbols or package initialization order.
	//
	// Empty preserves compatibility for a single unpinned legacy provider.
	// Pinned capabilities and duplicate capability names require explicit
	// Provider values.
	Provider string

	Register func(builder wazero.HostModuleBuilder, cell Cell) error
	Stub     func(builder wazero.HostModuleBuilder, cell Cell) error

	// Setup is called when the cell declares this capability,
	// before the WASM module is loaded. Use it to start servers,
	// open connections, create directories, etc.
	Setup func(env SetupEnv) error

	// Teardown is called on shutdown. Nil = no cleanup needed.
	Teardown func(ctx context.Context) error

	// TeardownScope releases resources belonging to exactly one application
	// scope. Multi-application hosts prefer it over legacy process-wide
	// Teardown so one application cannot tear down another's handles.
	TeardownScope func(ctx context.Context, scope Scope) error

	// Poll returns the next pending event from this extension, if
	// any. The step loop calls Poll on every active extension each
	// iteration. First extension to return ok=true wins that step.
	// Nil = this extension never generates events (passive).
	Poll func() (event StepEvent, ok bool)

	// Finalize is called after the step loop processes an event
	// from this extension. The id matches StepEvent.ID.
	// Nil = no post-processing needed.
	//
	// Implementations MUST be idempotent: in multi-cell broadcast
	// scenarios the step loop calls Finalize once per cell that
	// dequeues the event, so the same ID may arrive multiple times.
	// A non-idempotent Finalize (e.g. decrement a semaphore, charge a
	// credit) must not be used on a Poll-based capability that allows
	// broadcast (empty CellID).
	Finalize func(id uint64)

	// TeardownCell, if non-nil, is called to drop only the named
	// cell's state while other cells on the same extension keep
	// running. Used by the control socket for graceful per-cell
	// shutdown in multi-cell deployments. Nil = extension does not
	// distinguish per-cell shutdown from full Teardown; per-cell
	// shutdown becomes a no-op for this capability.
	TeardownCell func(ctx context.Context, cellID string) error
}

// Register adds cap to the global extension set. Called from an
// extension package's init(). Pulp folds all registered extensions
// into its gated capability registry at startup.
func Register(cap Capability) {
	mu.Lock()
	defer mu.Unlock()
	globals = append(globals, cap)
}

// All returns a copy of the currently registered extensions. Called
// by the Pulp runtime at startup to fold them into its gated
// capability set alongside built-ins.
func All() []Capability {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Capability, len(globals))
	copy(out, globals)
	return out
}

// SelectCapabilities resolves exactly one registered provider for every
// capability name. A provider pin is an exact stable Provider match; callback
// symbols and registration order are never used as provider identity.
//
// Unique, unpinned legacy capabilities remain compatible even when Provider is
// empty. Duplicate names require a pin, and pinned names fail closed when the
// provider is missing, unidentified, substituted, or registered more than once.
func SelectCapabilities(registered []Capability, providers map[string]string) ([]Capability, error) {
	byName := make(map[string][]Capability, len(registered))
	order := make([]string, 0, len(registered))
	for _, capability := range registered {
		if _, seen := byName[capability.Name]; !seen {
			order = append(order, capability.Name)
		}
		byName[capability.Name] = append(byName[capability.Name], capability)
	}

	pinnedNames := make([]string, 0, len(providers))
	for name, provider := range providers {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("capability provider selection contains an empty capability name")
		}
		if strings.TrimSpace(provider) == "" {
			return nil, fmt.Errorf("capability %q has an empty provider selection", name)
		}
		pinnedNames = append(pinnedNames, name)
	}
	sort.Strings(pinnedNames)
	for _, name := range pinnedNames {
		if len(byName[name]) == 0 {
			return nil, fmt.Errorf("pinned capability %q is not registered", name)
		}
	}

	selected := make([]Capability, 0, len(byName))
	for _, name := range order {
		candidates := byName[name]
		want, pinned := providers[name]
		if !pinned {
			if len(candidates) != 1 {
				return nil, fmt.Errorf("capability %q has %d registered providers and requires explicit selection", name, len(candidates))
			}
			selected = append(selected, candidates[0])
			continue
		}

		var matches []Capability
		available := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			provider := candidate.Provider
			if provider == "" {
				provider = "<legacy-unidentified>"
			}
			available = append(available, provider)
			if candidate.Provider == want {
				matches = append(matches, candidate)
			}
		}
		sort.Strings(available)
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("capability %q provider is %q, want exact provider %q", name, strings.Join(available, ", "), want)
		case 1:
			selected = append(selected, matches[0])
		default:
			return nil, fmt.Errorf("capability %q has %d registrations for pinned provider %q", name, len(matches), want)
		}
	}
	return selected, nil
}

var (
	mu      sync.Mutex
	globals []Capability
)
