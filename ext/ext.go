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
	"log/slog"
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
	CellName  string
	StorageRoot string
	Config      map[string]any
	Logger      *slog.Logger
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
	Kind     string
	Payload  []byte
	ID       uint64
	CellID string
}

// Capability is a named bundle of host imports plus optional
// lifecycle hooks and event sourcing. Extensions register one or
// more Capabilities via Register() in their init().
//
// Register/Stub bind host import functions into the WASM module.
// Setup/Teardown manage host-side resources (servers, connections).
// Poll/Finalize let the extension feed events into the step loop.
type Capability struct {
	Name     string
	Register func(builder wazero.HostModuleBuilder, cell Cell) error
	Stub     func(builder wazero.HostModuleBuilder, cell Cell) error

	// Setup is called when the cell declares this capability,
	// before the WASM module is loaded. Use it to start servers,
	// open connections, create directories, etc.
	Setup func(env SetupEnv) error

	// Teardown is called on shutdown. Nil = no cleanup needed.
	Teardown func(ctx context.Context) error

	// Poll returns the next pending event from this extension, if
	// any. The step loop calls Poll on every active extension each
	// iteration. First extension to return ok=true wins that step.
	// Nil = this extension never generates events (passive).
	Poll func() (event StepEvent, ok bool)

	// Finalize is called after the step loop processes an event
	// from this extension. The id matches StepEvent.ID.
	// Nil = no post-processing needed.
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

var (
	mu      sync.Mutex
	globals []Capability
)
