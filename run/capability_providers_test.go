package run

import (
	"strings"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/ext"
)

func TestCapabilityProviderSelectorInstallsExactContractBeforeUse(t *testing.T) {
	const (
		name = "transport.http.inbound"
		gin  = "github.com/BananaLabs-OSS/Pulp-ext-gin"
		http = "github.com/BananaLabs-OSS/Pulp-ext-http"
	)
	registered := []ext.Capability{{Name: name, Provider: http}, {Name: name, Provider: gin}}
	var selector capabilityProviderSelector
	if err := selector.install(registered, map[string]string{name: gin}); err != nil {
		t.Fatalf("install: %v", err)
	}
	selected, err := selector.selectCapabilities(registered)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(selected) != 1 || selected[0].Provider != gin {
		t.Fatalf("selected = %#v, want exact Gin provider", selected)
	}
}

func TestCapabilityProviderSelectorRejectsLateOrReplacementContract(t *testing.T) {
	registered := []ext.Capability{{Name: "stateful", Provider: "provider-a"}}

	t.Run("replacement", func(t *testing.T) {
		var selector capabilityProviderSelector
		if err := selector.install(registered, map[string]string{"stateful": "provider-a"}); err != nil {
			t.Fatal(err)
		}
		err := selector.install(registered, map[string]string{"stateful": "provider-b"})
		if err == nil || !strings.Contains(err.Error(), "different capability provider contract") {
			t.Fatalf("replacement error = %v", err)
		}
	})

	t.Run("late", func(t *testing.T) {
		var selector capabilityProviderSelector
		if _, err := selector.selectCapabilities(registered); err != nil {
			t.Fatal(err)
		}
		err := selector.install(registered, map[string]string{"stateful": "provider-a"})
		if err == nil || !strings.Contains(err.Error(), "already selected") {
			t.Fatalf("late install error = %v", err)
		}
	})
}

func TestCapabilityProviderSelectorDefaultUniqueLegacyCompatibility(t *testing.T) {
	var selector capabilityProviderSelector
	selected, err := selector.selectCapabilities([]ext.Capability{{Name: "legacy"}})
	if err != nil {
		t.Fatalf("default select: %v", err)
	}
	if len(selected) != 1 || selected[0].Name != "legacy" {
		t.Fatalf("selected = %#v", selected)
	}
}
