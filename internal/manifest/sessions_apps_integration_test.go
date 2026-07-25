package manifest

import (
	"path/filepath"
	"testing"
)

func TestSessionsSourceMonolithAndSplitCompositionsLoad(t *testing.T) {
	workspace, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, path, application string
		singleApp               bool
	}{
		{"sessions", filepath.Join(workspace, "Sessions-Gene", "application", "pulp.app.toml"), "sessions", false},
		{"evolution-monolith", filepath.Join(workspace, "Evolution", "pulp-cell", "pulp.app.toml"), "evolution", true},
		{"evolution-split", filepath.Join(workspace, "Evolution", "pulp-cell", "pulp.host-app.toml"), "evolution", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, err := LoadApp(test.path)
			if err != nil {
				t.Fatalf("LoadApp(%s): %v", test.path, err)
			}
			if app.Name != test.application {
				t.Fatalf("application name = %q, want %q", app.Name, test.application)
			}
			if test.application == "evolution" {
				var evolution *CellSpec
				for _, cell := range app.Cells.Cells {
					if cell.Name == "evolution" {
						evolution = cell
						break
					}
				}
				if evolution == nil {
					t.Fatal("Evolution application has no evolution cell")
				}
				got, _ := evolution.Config["legacy_owner_imports_single_app"].(bool)
				if got != test.singleApp {
					t.Fatalf("legacy_owner_imports_single_app = %v, want %v", got, test.singleApp)
				}
			}
		})
	}

	hostPath := filepath.Join(workspace, "Evolution", "pulp.host.toml")
	host, err := LoadHost(hostPath)
	if err != nil {
		t.Fatalf("LoadHost(%s): %v", hostPath, err)
	}
	if len(host.ApplicationOrder) != 3 ||
		host.ApplicationOrder[0].ID != "minecraft-resolver" ||
		host.ApplicationOrder[1].ID != "sessions" ||
		host.ApplicationOrder[2].ID != "evolution" {
		t.Fatalf("host application order = %#v", host.ApplicationOrder)
	}
}
