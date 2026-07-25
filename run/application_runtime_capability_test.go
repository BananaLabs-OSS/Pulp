package run

import (
	"context"
	"log/slog"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/ext"
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
