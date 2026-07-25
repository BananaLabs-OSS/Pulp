package manifest

import "testing"

func TestLoadAll_DependencyOrder(t *testing.T) {
	consumer := writeManifest(t, `
name = "evolution"
version = "0.1.0"
provides = ["evolution"]
consumes = ["sessions"]
depends_on = ["sessions"]
`)
	provider := writeManifest(t, `
name = "sessions"
version = "0.1.0"
provides = ["sessions"]
consumes = ["evolution"]
`)

	set, err := LoadAll([]string{consumer, provider})
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(set.Order) != 2 {
		t.Fatalf("order length = %d, want 2", len(set.Order))
	}
	if set.Order[0].Name != "sessions" || set.Order[1].Name != "evolution" {
		t.Fatalf("init order = [%s, %s], want [sessions, evolution]",
			set.Order[0].Name, set.Order[1].Name)
	}
}
