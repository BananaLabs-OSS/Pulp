package host

import (
	"context"
	"strings"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/tetratelabs/wazero"
)

func TestRegistry_BindsAlwaysAndDeclaredCapabilities(t *testing.T) {
	var boundAlways, boundGated bool

	r := NewRegistry()
	r.Always(Capability{
		Name: "log",
		Register: func(b wazero.HostModuleBuilder, _ ext.Plugin) error {
			boundAlways = true
			return nil
		},
	})
	r.Gated(Capability{
		Name: "transport.http.inbound",
		Register: func(b wazero.HostModuleBuilder, _ ext.Plugin) error {
			boundGated = true
			return nil
		},
	})

	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	t.Cleanup(func() { runtime.Close(ctx) })

	spec := &manifest.PluginSpec{
		Name:         "test",
		Version:      "0.0.0",
		Capabilities: []string{"transport.http.inbound"},
	}

	if err := r.bind(ctx, runtime, spec, &Plugin{}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if !boundAlways {
		t.Error("always-on capability was not bound")
	}
	if !boundGated {
		t.Error("declared capability was not bound")
	}
}

func TestRegistry_FailsOnUnknownCapability(t *testing.T) {
	r := NewRegistry()

	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	t.Cleanup(func() { runtime.Close(ctx) })

	spec := &manifest.PluginSpec{
		Name:         "test",
		Version:      "0.0.0",
		Capabilities: []string{"storage.postgres"},
	}

	err := r.bind(ctx, runtime, spec, &Plugin{})
	if err == nil {
		t.Fatal("expected error for unknown capability")
	}
	if !strings.Contains(err.Error(), "storage.postgres") {
		t.Errorf("error should name the offending capability: %v", err)
	}
}

func TestRegistry_SkipsUndeclaredGatedCapabilities(t *testing.T) {
	var bound bool
	r := NewRegistry()
	r.Gated(Capability{
		Name: "spawn.docker",
		Register: func(b wazero.HostModuleBuilder, _ ext.Plugin) error {
			bound = true
			return nil
		},
	})

	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	t.Cleanup(func() { runtime.Close(ctx) })

	spec := &manifest.PluginSpec{
		Name:    "test",
		Version: "0.0.0",
		// Capabilities empty — plugin did not declare spawn.docker.
	}

	if err := r.bind(ctx, runtime, spec, &Plugin{}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if bound {
		t.Error("undeclared gated capability should not be bound")
	}
}
