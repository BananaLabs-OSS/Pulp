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
				if test.singleApp {
					assertEvolutionSQLiteExecutionUnit(t, app)
					assertPlacementConfigValue(t, app, "lua-orchestrator", "minecraft_resolver_scope", "local")
				}
			}
		})
	}

	hostPath := filepath.Join(workspace, "Evolution", "pulp.host.toml")
	host, err := LoadHost(hostPath)
	if err != nil {
		t.Fatalf("LoadHost(%s): %v", hostPath, err)
	}
	if len(host.ApplicationOrder) != 4 ||
		host.ApplicationOrder[0].ID != "bananauth" ||
		host.ApplicationOrder[1].ID != "minecraft-resolver" ||
		host.ApplicationOrder[2].ID != "sessions" ||
		host.ApplicationOrder[3].ID != "evolution" {
		t.Fatalf("host application order = %#v", host.ApplicationOrder)
	}
	if len(host.ApplicationOrder[2].DependsOn) != 2 ||
		host.ApplicationOrder[2].DependsOn[0] != "bananauth" ||
		host.ApplicationOrder[2].DependsOn[1] != "minecraft-resolver" {
		t.Fatalf("Sessions dependencies = %#v, want [bananauth minecraft-resolver]", host.ApplicationOrder[2].DependsOn)
	}
}

func assertPlacementConfigValue(t *testing.T, app *Application, cell, key string, want any) {
	t.Helper()
	for _, placement := range app.Placements {
		if placement.Spec.Name != cell {
			continue
		}
		values, ok := placement.Spec.Config["values"].(map[string]any)
		if !ok {
			t.Fatalf("placement %q config values = %#v, want a table", cell, placement.Spec.Config["values"])
		}
		if got := values[key]; got != want {
			t.Fatalf("placement %q config values[%q] = %#v, want %#v", cell, key, got, want)
		}
		return
	}
	t.Fatalf("application is missing placement %q", cell)
}

func assertEvolutionSQLiteExecutionUnit(t *testing.T, app *Application) {
	t.Helper()
	var unit *ExecutionUnit
	for index := range app.ExecutionUnits {
		if app.ExecutionUnits[index].Name == "shared-sqlite-core" {
			unit = &app.ExecutionUnits[index]
			break
		}
	}
	if unit == nil {
		t.Fatal("Evolution application is missing the shared-sqlite-core execution unit")
	}
	if unit.Artifact == nil || unit.Artifact.Name != "evolution-state-engine" {
		t.Fatalf("Evolution SQLite unit artifact = %#v, want evolution-state-engine", unit.Artifact)
	}
	want := []string{
		"configuration-registry", "fixed-window-counter", "workload-inventory", "capacity-scheduler",
		"archive-lifecycle", "artifact-lifecycle", "observation-registry", "runtime-control", "workload-provisioning", "notification-outbox",
	}
	if len(unit.Members) != len(want) {
		t.Fatalf("Evolution SQLite unit members = %#v, want %#v", unit.Members, want)
	}
	for index, member := range want {
		if unit.Members[index] != member {
			t.Fatalf("Evolution SQLite unit member[%d] = %q, want %q", index, unit.Members[index], member)
		}
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
		"auth.identity.v1.retention-lease.create",
		"auth.identity.v1.retention-lease.renew",
		"auth.identity.v1.retention-lease.release",
		"auth.identity.v1.retention-eligibility.get",
		"auth.identity.v1.account.erase",
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
