package run

import (
	"context"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/ext"
)

func TestEndpointRegistryScopesReadyEndpointsByApplicationInstance(t *testing.T) {
	registry := NewEndpointRegistry()
	blue := endpointRegistryScope(t, "sessions", "blue", "api")
	green := endpointRegistryScope(t, "sessions", "green", "api")
	blueEndpoint := ext.Endpoint{Scope: blue, Capability: "transport.http.inbound", Name: "public", Address: "127.0.0.1:41001"}
	greenEndpoint := ext.Endpoint{Scope: green, Capability: "transport.http.inbound", Name: "public", Address: "127.0.0.1:41002"}
	if err := registry.Ready(blueEndpoint); err != nil {
		t.Fatal(err)
	}
	if err := registry.Ready(greenEndpoint); err != nil {
		t.Fatal(err)
	}
	if got, ok := registry.ApplicationAddress("sessions", "blue", "transport.http.inbound", "public"); !ok || got != blueEndpoint.Address {
		t.Fatalf("blue endpoint = (%q, %v), want (%q, true)", got, ok, blueEndpoint.Address)
	}
	if got, ok := registry.ApplicationAddress("sessions", "green", "transport.http.inbound", "public"); !ok || got != greenEndpoint.Address {
		t.Fatalf("green endpoint = (%q, %v), want (%q, true)", got, ok, greenEndpoint.Address)
	}
}

func TestScopedApplicationRuntimeHTTPAddressRequiresReadyEndpoint(t *testing.T) {
	registry := NewEndpointRegistry()
	application := HostedApplication{Identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "blue"}}
	runtime := &scopedApplicationRuntime{
		application: application,
		config:      ScopedApplicationRuntimeFactoryConfig{Endpoints: registry},
	}
	if got := runtime.HTTPAddress(); got != "" {
		t.Fatalf("unstarted runtime HTTPAddress = %q, want empty", got)
	}
	endpoint := ext.Endpoint{
		Scope:      endpointRegistryScope(t, "sessions", "blue", "api"),
		Capability: "transport.http.inbound",
		Name:       "public",
		Address:    "127.0.0.1:41001",
	}
	if err := registry.Ready(endpoint); err != nil {
		t.Fatal(err)
	}
	runtime.started = true
	if got, want := runtime.HTTPAddress(), endpoint.Address; got != want {
		t.Fatalf("ready runtime HTTPAddress = %q, want %q", got, want)
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := runtime.HTTPAddress(); got != "" {
		t.Fatalf("stopped runtime HTTPAddress = %q, want empty", got)
	}
	if _, ok := registry.ApplicationAddress("sessions", "blue", "transport.http.inbound", "public"); ok {
		t.Fatal("runtime shutdown did not clear endpoint readiness")
	}
}

func TestEndpointRegistryRejectsSecondPublicEndpointAndClearsOnGone(t *testing.T) {
	registry := NewEndpointRegistry()
	first := ext.Endpoint{Scope: endpointRegistryScope(t, "sessions", "blue", "api"), Capability: "transport.http.inbound", Name: "public", Address: "127.0.0.1:41001"}
	second := ext.Endpoint{Scope: endpointRegistryScope(t, "sessions", "blue", "other-api"), Capability: "transport.http.inbound", Name: "public", Address: "127.0.0.1:41002"}
	if err := registry.Ready(first); err != nil {
		t.Fatal(err)
	}
	if err := registry.Ready(second); err == nil {
		t.Fatal("second application public endpoint was accepted")
	}
	registry.Gone(first)
	if _, ok := registry.ApplicationAddress("sessions", "blue", "transport.http.inbound", "public"); ok {
		t.Fatal("gone endpoint remained discoverable")
	}
}

func endpointRegistryScope(t *testing.T, applicationID, instanceID, cellID string) ext.Scope {
	t.Helper()
	scope, err := ext.NewScope(applicationID, instanceID, cellID, "default")
	if err != nil {
		t.Fatal(err)
	}
	return scope
}
