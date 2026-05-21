package host

import (
	"context"
	"fmt"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/tetratelabs/wazero"
)

// Capability is an alias of ext.Capability — the public extension
// type is canonical; this alias lets internal callers keep the
// short name without importing the ext path everywhere.
type Capability = ext.Capability

// Name returns the cell's manifest name. Defined to satisfy the
// ext.Cell interface extensions receive when their Register or
// Stub functions run.
func (p *Cell) Name() string { return p.name }

// A Registry collects the Capabilities that this Pulp host knows how to
// provide. Some capabilities are always bound regardless of the manifest
// (logging, entropy); others are only bound when declared explicitly
// (transport.http.inbound, storage.sqlite, spawn.docker).
//
// The registry is created at host startup and reused across cell loads.
type Registry struct {
	always []Capability
	gated  map[string]Capability
}

// NewRegistry returns a Registry prepopulated with every extension
// that registered via ext.Register. Callers add built-in capabilities
// on top via Always / Gated before passing it to [Load].
func NewRegistry() *Registry {
	r := &Registry{gated: map[string]Capability{}}
	for _, cap := range ext.All() {
		r.gated[cap.Name] = cap
	}
	return r
}

// Always marks the capability as bound for every cell, no declaration
// required. Use for universal imports like logging.
func (r *Registry) Always(c Capability) {
	r.always = append(r.always, c)
}

// Gated marks the capability as bound only when the cell's manifest
// lists it under `capabilities`. Use for anything that touches the OS
// or the network.
func (r *Registry) Gated(c Capability) {
	r.gated[c.Name] = c
}

// bind wires the imports required by the cell's manifest into a fresh
// "pulp" host module and instantiates it against the supplied runtime.
// It must be called before instantiating the cell's own WASM module so
// the imports exist when the module resolves its imports.
func (r *Registry) bind(ctx context.Context, runtime wazero.Runtime, spec *manifest.CellSpec, cell *Cell) error {
	builder := runtime.NewHostModuleBuilder("pulp")

	for _, c := range r.always {
		if err := c.Register(builder, cell); err != nil {
			return fmt.Errorf("register always-on capability %q: %w", c.Name, err)
		}
	}

	declared := map[string]bool{}
	for _, cap := range spec.Capabilities {
		if _, ok := r.gated[cap]; !ok {
			return fmt.Errorf("cell %q declares unknown capability %q", spec.Name, cap)
		}
		declared[cap] = true
	}

	for name, c := range r.gated {
		if declared[name] {
			if err := c.Register(builder, cell); err != nil {
				return fmt.Errorf("register capability %q: %w", name, err)
			}
		} else if c.Stub != nil {
			if err := c.Stub(builder, cell); err != nil {
				return fmt.Errorf("stub capability %q: %w", name, err)
			}
		}
	}

	if _, err := builder.Instantiate(ctx); err != nil {
		return fmt.Errorf("instantiate pulp host module: %w", err)
	}
	return nil
}
