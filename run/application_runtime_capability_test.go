package run

import (
	"context"
	"log/slog"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

func TestApplicationRuntimeRejectsSetupWithoutScopedTeardownInMultiAppHost(t *testing.T) {
	runtime := &applicationRuntime{
		config:        ScopedApplicationRuntimeFactoryConfig{RequireScopedCapabilityLifecycle: true},
		declaredUnion: map[string]bool{"stateful": true},
		allCaps:       []ext.Capability{{Name: "stateful", Setup: func(ext.SetupEnv) error { return nil }}},
	}
	if err := runtime.validateCapabilityLifecycle(); err == nil {
		t.Fatal("setup without scoped teardown unexpectedly accepted")
	}
}

func TestApplicationRuntimeScopesSetupAndTeardownPerApplicationInstance(t *testing.T) {
	var setups, teardowns []ext.Scope
	capability := ext.Capability{
		Name: "stateful",
		Setup: func(env ext.SetupEnv) error {
			setups = append(setups, env.EffectiveScope())
			return nil
		},
		TeardownScope: func(_ context.Context, scope ext.Scope) error {
			teardowns = append(teardowns, scope)
			return nil
		},
	}
	newRuntime := func(instance string) *applicationRuntime {
		return &applicationRuntime{
			application: HostedApplication{Identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: instance}, StorageNamespace: "sessions"},
			config: ScopedApplicationRuntimeFactoryConfig{
				RequireScopedCapabilityLifecycle: true,
				StorageRoot:                      "data",
				Logger:                           slog.Default(),
			},
			allCaps:       []ext.Capability{capability},
			declaredUnion: map[string]bool{"stateful": true},
			setupCaps:     map[string]bool{},
		}
	}
	first, second := newRuntime("blue"), newRuntime("green")
	if err := first.setupCapabilities(); err != nil {
		t.Fatalf("first setup: %v", err)
	}
	if err := second.setupCapabilities(); err != nil {
		t.Fatalf("second setup: %v", err)
	}
	// Shutdown is idempotent: a repeated request must not release the first
	// application's scoped resources a second time, and must never affect the
	// sibling application's scope.
	first.started, second.started = true, true
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatalf("repeated first shutdown: %v", err)
	}
	if err := second.Shutdown(context.Background()); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
	if len(setups) != 2 || len(teardowns) != 2 {
		t.Fatalf("setup/teardown calls = %d/%d, want 2/2", len(setups), len(teardowns))
	}
	if setups[0] == setups[1] || teardowns[0] == teardowns[1] {
		t.Fatalf("application instances shared lifecycle scope: setups=%#v teardowns=%#v", setups, teardowns)
	}
	if setups[0] != teardowns[0] || setups[1] != teardowns[1] {
		t.Fatalf("setup/teardown scopes diverged: setups=%#v teardowns=%#v", setups, teardowns)
	}
}

func TestApplicationRuntimeRejectsLegacyGlobalTeardownInMultiAppHost(t *testing.T) {
	runtime := &applicationRuntime{config: ScopedApplicationRuntimeFactoryConfig{RequireScopedCapabilityLifecycle: true}, declaredUnion: map[string]bool{"stateful": true}, allCaps: []ext.Capability{{Name: "stateful", Teardown: func(context.Context) error { return nil }}}}
	if err := runtime.validateCapabilityLifecycle(); err == nil {
		t.Fatal("global teardown unexpectedly accepted in a multi-application host")
	}
}

func TestApplicationRuntimeScopesExtensionConfigPerApplicationInstance(t *testing.T) {
	const capabilityName = "identity.minecraft-profile.resolve"
	configs := map[string]map[string]any{}
	capability := ext.Capability{
		Name: capabilityName,
		Setup: func(env ext.SetupEnv) error {
			configs[env.EffectiveScope().ApplicationInstanceID()] = env.Config
			return nil
		},
		TeardownScope: func(context.Context, ext.Scope) error { return nil },
	}
	newRuntime := func(instance, origin, placeholder string) *applicationRuntime {
		runtime := &applicationRuntime{
			application:   HostedApplication{Identity: ApplicationIdentity{ApplicationID: "evolution", InstanceID: instance}},
			config:        ScopedApplicationRuntimeFactoryConfig{RequireScopedCapabilityLifecycle: true, Logger: slog.Default()},
			allCaps:       []ext.Capability{capability},
			declaredUnion: map[string]bool{capabilityName: true},
			setupCaps:     map[string]bool{},
		}
		runtime.resolveCapabilityConfigs([]manifest.CellPlacement{{
			Spec: &manifest.CellSpec{
				Name:         "identity",
				Capabilities: []string{capabilityName},
				Config: map[string]any{
					"minecraft_profile": map[string]any{
						"java_origin": origin,
						"credential":  placeholder,
					},
				},
			},
			InstanceID: "primary",
			Address:    "identity",
		}})
		return runtime
	}
	blue := newRuntime("blue", "https://blue.example", "${BLUE_PROFILE_CREDENTIAL}")
	green := newRuntime("green", "https://green.example", "${GREEN_PROFILE_CREDENTIAL}")
	if err := blue.setupCapabilities(); err != nil {
		t.Fatalf("blue setup: %v", err)
	}
	if err := green.setupCapabilities(); err != nil {
		t.Fatalf("green setup: %v", err)
	}
	for instance, want := range map[string]struct {
		origin      string
		placeholder string
	}{
		"blue":  {origin: "https://blue.example", placeholder: "${BLUE_PROFILE_CREDENTIAL}"},
		"green": {origin: "https://green.example", placeholder: "${GREEN_PROFILE_CREDENTIAL}"},
	} {
		profile, ok := configs[instance]["minecraft_profile"].(map[string]any)
		if !ok {
			t.Fatalf("%s minecraft_profile config = %#v", instance, configs[instance])
		}
		if profile["java_origin"] != want.origin || profile["credential"] != want.placeholder {
			t.Fatalf("%s config = %#v, want origin %q and literal placeholder %q", instance, profile, want.origin, want.placeholder)
		}
	}
}

func TestApplicationRuntimeExtensionConfigCopiesAreIsolatedBetweenInstances(t *testing.T) {
	const capabilityName = "stateful"
	shared := map[string]any{
		"nested":  map[string]any{"owner": "manifest"},
		"targets": []string{"manifest"},
	}
	seen := map[string]string{}
	capability := ext.Capability{
		Name: capabilityName,
		Setup: func(env ext.SetupEnv) error {
			instance := env.EffectiveScope().ApplicationInstanceID()
			nested := env.Config["nested"].(map[string]any)
			targets := env.Config["targets"].([]string)
			seen[instance] = nested["owner"].(string) + "/" + targets[0]
			nested["owner"] = instance
			targets[0] = instance
			return nil
		},
		TeardownScope: func(context.Context, ext.Scope) error { return nil },
	}
	newRuntime := func(instance string) *applicationRuntime {
		runtime := &applicationRuntime{
			application:   HostedApplication{Identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: instance}},
			config:        ScopedApplicationRuntimeFactoryConfig{RequireScopedCapabilityLifecycle: true, Logger: slog.Default()},
			allCaps:       []ext.Capability{capability},
			declaredUnion: map[string]bool{capabilityName: true},
			setupCaps:     map[string]bool{},
		}
		runtime.resolveCapabilityConfigs([]manifest.CellPlacement{{
			Spec: &manifest.CellSpec{Name: "owner", Capabilities: []string{capabilityName}, Config: shared},
		}})
		return runtime
	}
	blue, green := newRuntime("blue"), newRuntime("green")
	if err := blue.setupCapabilities(); err != nil {
		t.Fatalf("blue setup: %v", err)
	}
	if err := green.setupCapabilities(); err != nil {
		t.Fatalf("green setup: %v", err)
	}
	if seen["blue"] != "manifest/manifest" || seen["green"] != "manifest/manifest" {
		t.Fatalf("extension config leaked across instances: %#v", seen)
	}
	if owner := shared["nested"].(map[string]any)["owner"]; owner != "manifest" {
		t.Fatalf("extension mutated resolved cell config: owner = %#v", owner)
	}
	if target := shared["targets"].([]string)[0]; target != "manifest" {
		t.Fatalf("extension mutated resolved cell config slice: target = %#v", target)
	}
}

func TestApplicationRuntimeExtensionConfigEmptyCompatibility(t *testing.T) {
	var received map[string]any
	capability := ext.Capability{
		Name: "empty",
		Setup: func(env ext.SetupEnv) error {
			received = env.Config
			return nil
		},
	}
	runtime := &applicationRuntime{
		application:   HostedApplication{Identity: ApplicationIdentity{ApplicationID: "legacy", InstanceID: "default"}},
		config:        ScopedApplicationRuntimeFactoryConfig{Logger: slog.Default()},
		allCaps:       []ext.Capability{capability},
		declaredUnion: map[string]bool{"empty": true},
		setupCaps:     map[string]bool{},
	}
	runtime.resolveCapabilityConfigs([]manifest.CellPlacement{{
		Spec: &manifest.CellSpec{Name: "legacy-cell", Capabilities: []string{"empty"}, Config: map[string]any{}},
	}})
	if err := runtime.setupCapabilities(); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if received != nil {
		t.Fatalf("empty config compatibility changed: got %#v, want nil", received)
	}
}
