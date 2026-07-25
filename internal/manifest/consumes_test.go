package manifest

import (
	"strings"
	"testing"
)

func TestLoadAllRejectsConsumeWithoutExactProvider(t *testing.T) {
	consumer := writeManifest(t, `
name = "consumer"
version = "1"
consumes = ["orders.apply"]
`)
	provider := writeManifest(t, `
name = "provider"
version = "1"
provides = ["orders.preview"]
`)
	_, err := LoadAll([]string{consumer, provider})
	if err == nil || !strings.Contains(err.Error(), "no cell provides that exact provider") {
		t.Fatalf("LoadAll error = %v, want missing exact provider", err)
	}
}

func TestLoadAllRejectsAmbiguousConsumedProvider(t *testing.T) {
	consumer := writeManifest(t, `
name = "consumer"
version = "1"
consumes = ["orders.apply"]
`)
	first := writeManifest(t, `
name = "first"
version = "1"
provides = ["orders.apply"]
`)
	second := writeManifest(t, `
name = "second"
version = "1"
provides = ["orders.apply"]
`)
	_, err := LoadAll([]string{consumer, first, second})
	if err == nil || !strings.Contains(err.Error(), "ambiguous providers") {
		t.Fatalf("LoadAll error = %v, want ambiguous provider", err)
	}
}
