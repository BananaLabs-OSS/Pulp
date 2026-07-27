package ext

import (
	"strings"
	"testing"
)

func TestSelectCapabilitiesPreservesUniqueLegacyProviderCompatibility(t *testing.T) {
	legacy := Capability{Name: "storage.sqlite"}
	selected, err := SelectCapabilities([]Capability{legacy}, nil)
	if err != nil {
		t.Fatalf("select unique legacy capability: %v", err)
	}
	if len(selected) != 1 || selected[0].Name != legacy.Name || selected[0].Provider != "" {
		t.Fatalf("selected = %#v, want the unique legacy capability", selected)
	}
}

func TestSelectCapabilitiesUsesExactProviderForDuplicateName(t *testing.T) {
	gin := Capability{Name: "transport.http.inbound", Provider: "github.com/BananaLabs-OSS/Pulp-ext-gin"}
	http := Capability{Name: "transport.http.inbound", Provider: "github.com/BananaLabs-OSS/Pulp-ext-http"}
	selected, err := SelectCapabilities(
		[]Capability{http, gin},
		map[string]string{"transport.http.inbound": gin.Provider},
	)
	if err != nil {
		t.Fatalf("select pinned provider: %v", err)
	}
	if len(selected) != 1 || selected[0].Provider != gin.Provider {
		t.Fatalf("selected = %#v, want %q", selected, gin.Provider)
	}
}

func TestSelectCapabilitiesFailsClosed(t *testing.T) {
	const (
		name = "transport.http.inbound"
		gin  = "github.com/BananaLabs-OSS/Pulp-ext-gin"
		http = "github.com/BananaLabs-OSS/Pulp-ext-http"
	)
	tests := []struct {
		name       string
		registered []Capability
		providers  map[string]string
		want       string
	}{
		{
			name:       "missing",
			registered: []Capability{{Name: "storage.sqlite", Provider: "sqlite"}},
			providers:  map[string]string{name: gin},
			want:       `pinned capability "transport.http.inbound" is not registered`,
		},
		{
			name:       "unselected duplicate",
			registered: []Capability{{Name: name, Provider: gin}, {Name: name, Provider: http}},
			want:       `requires explicit selection`,
		},
		{
			name:       "substituted",
			registered: []Capability{{Name: name, Provider: http}},
			providers:  map[string]string{name: gin},
			want:       `want exact provider "github.com/BananaLabs-OSS/Pulp-ext-gin"`,
		},
		{
			name:       "unidentified pinned",
			registered: []Capability{{Name: name}},
			providers:  map[string]string{name: gin},
			want:       `<legacy-unidentified>`,
		},
		{
			name:       "ambiguous exact provider",
			registered: []Capability{{Name: name, Provider: gin}, {Name: name, Provider: gin}},
			providers:  map[string]string{name: gin},
			want:       `has 2 registrations for pinned provider`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := SelectCapabilities(test.registered, test.providers)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}
