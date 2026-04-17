// Package ext is Pulp's public extension API.
//
// Extensions add host-side capabilities to Pulp that plugins can
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
	"sync"

	"github.com/tetratelabs/wazero"
)

// Plugin is the minimal view of a loaded WASM plugin that an
// extension's Register / Stub functions see. The full plugin type is
// internal to Pulp; extensions rarely need more than the plugin's
// name (for logging) and occasionally the module handle — more
// surface can be added here as extensions demand it.
type Plugin interface {
	// Name returns the plugin's manifest name.
	Name() string
}

// Capability is a named bundle of host imports — the same shape Pulp
// uses for its built-in primitives (transport.*, storage.*). An
// extension typically provides one Capability whose Register function
// binds a handful of import functions into the "pulp" host module and
// whose Stub binds error-99 no-ops so plugins that import the
// capability's functions but do not declare the capability still
// instantiate.
//
// Register is always called for plugins that declare the capability
// in their manifest. Stub, when non-nil, is called for plugins that
// do not — this keeps Go's dead-code eliminator from leaving stray
// unresolved imports in a WASM binary that pulled in a package like
// pulpgin which references all host imports.
type Capability struct {
	Name     string
	Register func(builder wazero.HostModuleBuilder, plugin Plugin) error
	Stub     func(builder wazero.HostModuleBuilder, plugin Plugin) error
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
