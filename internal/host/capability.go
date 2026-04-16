package host

import (
	"context"
	"fmt"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/tetratelabs/wazero"
)

// A Capability is a named bundle of host imports that a plugin may use.
// Every primitive — Transport, Storage, Spawn, Logging, Entropy — contributes
// one or more Capabilities to a [Registry]. Plugins opt in by listing the
// capability name in their manifest; the registry binds the imports at
// load time. Capabilities the plugin does not declare still get their
// host import names wired — to stub functions that return error code 99
// ("capability not declared") — so the plugin binary always resolves its
// imports even when Go's dead-code eliminator leaves references in.
//
// Register receives the wazero module builder for the "pulp" host module
// and the plugin being loaded. It adds import functions to the builder —
// it must NOT instantiate anything (the registry does that once for the
// whole plugin).
//
// Stub is optional; when set, it is called for plugins that import the
// capability's functions but did not declare the capability. Stub must
// bind the same function names Register does, with the same signatures,
// each returning error code 99.
type Capability struct {
	Name     string
	Register func(builder wazero.HostModuleBuilder, plugin *Plugin) error
	Stub     func(builder wazero.HostModuleBuilder, plugin *Plugin) error
}

// A Registry collects the Capabilities that this Pulp host knows how to
// provide. Some capabilities are always bound regardless of the manifest
// (logging, entropy); others are only bound when declared explicitly
// (transport.http.inbound, storage.sqlite, spawn.docker).
//
// The registry is created at host startup and reused across plugin loads.
type Registry struct {
	always []Capability
	gated  map[string]Capability
}

// NewRegistry returns an empty Registry. Call Always or Gated to add
// capabilities before passing it to [Load].
func NewRegistry() *Registry {
	return &Registry{gated: map[string]Capability{}}
}

// Always marks the capability as bound for every plugin, no declaration
// required. Use for universal imports like logging.
func (r *Registry) Always(c Capability) {
	r.always = append(r.always, c)
}

// Gated marks the capability as bound only when the plugin's manifest
// lists it under `capabilities`. Use for anything that touches the OS
// or the network.
func (r *Registry) Gated(c Capability) {
	r.gated[c.Name] = c
}

// bind wires the imports required by the plugin's manifest into a fresh
// "pulp" host module and instantiates it against the supplied runtime.
// It must be called before instantiating the plugin's own WASM module so
// the imports exist when the module resolves its imports.
func (r *Registry) bind(ctx context.Context, runtime wazero.Runtime, spec *manifest.PluginSpec, plugin *Plugin) error {
	builder := runtime.NewHostModuleBuilder("pulp")

	for _, c := range r.always {
		if err := c.Register(builder, plugin); err != nil {
			return fmt.Errorf("register always-on capability %q: %w", c.Name, err)
		}
	}

	declared := map[string]bool{}
	for _, cap := range spec.Capabilities {
		if _, ok := r.gated[cap]; !ok {
			return fmt.Errorf("plugin %q declares unknown capability %q", spec.Name, cap)
		}
		declared[cap] = true
	}

	for name, c := range r.gated {
		if declared[name] {
			if err := c.Register(builder, plugin); err != nil {
				return fmt.Errorf("register capability %q: %w", name, err)
			}
		} else if c.Stub != nil {
			if err := c.Stub(builder, plugin); err != nil {
				return fmt.Errorf("stub capability %q: %w", name, err)
			}
		}
	}

	if _, err := builder.Instantiate(ctx); err != nil {
		return fmt.Errorf("instantiate pulp host module: %w", err)
	}
	return nil
}
