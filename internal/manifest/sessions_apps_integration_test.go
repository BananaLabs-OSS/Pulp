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
		host.ApplicationOrder[0].ID != "sessions" ||
		host.ApplicationOrder[1].ID != "minecraft-resolver" ||
		host.ApplicationOrder[2].ID != "evolution" {
		t.Fatalf("host application order = %#v", host.ApplicationOrder)
	}
	if len(host.ApplicationOrder[1].DependsOn) != 1 ||
		host.ApplicationOrder[1].DependsOn[0] != "sessions" {
		t.Fatalf("resolver dependencies = %#v, want [sessions]", host.ApplicationOrder[1].DependsOn)
	}
}

func TestSessionsBananauthHumanAuthParityHarnessResolvesOnlyDeclaredProviders(t *testing.T) {
	workspace, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	hostPath := filepath.Join(workspace, "Sessions-Gene", "application", "testdata", "bananauth-human-auth-parity.pulp.host.toml")
	host, err := LoadHost(hostPath)
	if err != nil {
		t.Fatalf("LoadHost(%s): %v", hostPath, err)
	}
	if len(host.Routes) != 0 {
		t.Fatalf("parity harness routes = %#v, want none", host.Routes)
	}
	if len(host.ApplicationOrder) != 2 ||
		host.ApplicationOrder[0].ID != "bananauth" ||
		host.ApplicationOrder[1].ID != "sessions-auth-parity" {
		t.Fatalf("host application order = %#v, want [bananauth sessions-auth-parity]", host.ApplicationOrder)
	}

	var caller *CellSpec
	for _, cell := range host.ApplicationOrder[1].Application.Cells.Cells {
		if cell.Name == "sessions-human-auth-parity" {
			caller = cell
			break
		}
	}
	if caller == nil {
		t.Fatal("Sessions auth parity caller cell is missing")
	}
	want := []string{
		"auth.identity.v1.native.authenticate",
		"auth.identity.v1.oauth.resolve",
		"auth.identity.v1.email-verification.issue",
		"auth.identity.v1.email-verification.consume",
		"auth.session.v1.create",
		"auth.session.v1.get",
		"auth.session.v1.revoke",
	}
	if len(caller.HostConsumes) != len(want) {
		t.Fatalf("host_consumes = %#v, want %#v", caller.HostConsumes, want)
	}
	for index, provider := range want {
		if caller.HostConsumes[index] != provider {
			t.Fatalf("host_consumes[%d] = %q, want %q", index, caller.HostConsumes[index], provider)
		}
	}

	providers := map[string]string{}
	for _, cell := range host.ApplicationOrder[0].Application.Cells.Cells {
		for _, provider := range cell.Provides {
			providers[provider] = cell.Name
		}
	}
	for _, provider := range want {
		if providers[provider] == "" {
			t.Fatalf("Bananauth does not provide %q", provider)
		}
	}
}
