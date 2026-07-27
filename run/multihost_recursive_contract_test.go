package run

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

// This contract is the concise form of the production ownership model:
// one host owns multiple applications, an application may have multiple
// instances, and each application instance may place multiple independently
// stateful copies of one reusable WASM package. Lua remains application-local.
func TestRecursiveMultiApplicationInstanceContract(t *testing.T) {
	root := t.TempDir()
	sharedWASM := multiHostWriteFixture(t, root, "packages/worker.wasm", "shared package code")
	writeRecursiveContractApp(t, root, "apps/resolver", "minecraft-resolver", "../../packages/worker.wasm", nil)
	writeRecursiveContractApp(t, root, "apps/sessions", "sessions-gene", "../../packages/worker.wasm", []string{"b1", "b2"})
	writeRecursiveContractApp(t, root, "apps/evolution", "evolution", "../../packages/worker.wasm", nil)

	hostPath := multiHostWriteFixture(t, root, "pulp.host.toml", `
schema_version = 1
name = "sessions-platform"

[[applications]]
id = "evolution"
manifest = "apps/evolution/pulp.app.toml"
aliases = ["primary"]
storage_namespace = "evolution-storage"
event_namespace = "evolution-events"
depends_on = ["minecraft-resolver", "sessions-gene"]

[[applications]]
id = "sessions-gene"
manifest = "apps/sessions/pulp.app.toml"
instances = 2
aliases = ["blue", "green"]
storage_namespace = "sessions-storage"
event_namespace = "sessions-events"

[[applications]]
id = "minecraft-resolver"
manifest = "apps/resolver/pulp.app.toml"
aliases = ["primary"]
storage_namespace = "resolver-storage"
event_namespace = "resolver-events"
`)

	hostManifest, err := manifest.LoadHost(hostPath)
	if err != nil {
		t.Fatalf("LoadHost: %v", err)
	}
	if got, want := recursiveContractApplicationIDs(hostManifest.ApplicationOrder), []string{"minecraft-resolver", "sessions-gene", "evolution"}; !sameStringSlice(got, want) {
		t.Fatalf("dependency-safe application order = %v, want %v", got, want)
	}

	byID := make(map[string]*manifest.HostedApplication, len(hostManifest.Applications))
	for _, application := range hostManifest.Applications {
		byID[application.ID] = application
		if got := application.Application.Cells.Lookup("worker").WASMPath; got != sharedWASM {
			t.Fatalf("%s worker package = %q, want shared %q", application.ID, got, sharedWASM)
		}
	}
	sessions := byID["sessions-gene"]
	if sessions == nil || len(sessions.Instances) != 2 || len(sessions.Application.Placements) != 3 {
		t.Fatalf("sessions recursive instances = %#v", sessions)
	}

	loader := ManifestHostLoader{}
	applications, err := loader.LoadHostApplications(context.Background(), hostPath)
	if err != nil {
		t.Fatalf("LoadHostApplications: %v", err)
	}
	if got, want := multiHostIdentities(applications), []string{
		"minecraft-resolver/primary",
		"sessions-gene/blue",
		"sessions-gene/green",
		"evolution/primary",
	}; !sameStringSlice(got, want) {
		t.Fatalf("application instances = %v, want %v", got, want)
	}

	scopes := []ext.Scope{
		mustRecursiveContractScope(t, "evolution", "primary", "worker", "primary"),
		mustRecursiveContractScope(t, "sessions-gene", "blue", "worker", "b1"),
		mustRecursiveContractScope(t, "sessions-gene", "blue", "worker", "b2"),
		mustRecursiveContractScope(t, "sessions-gene", "green", "worker", "b1"),
	}
	resourceKeys := make(map[ext.ResourceKey]struct{}, len(scopes))
	for _, scope := range scopes {
		key, err := scope.ResourceKey("worker.queue", "default")
		if err != nil {
			t.Fatal(err)
		}
		if _, collision := resourceKeys[key]; collision {
			t.Fatalf("recursive worker scope collided: %s", key)
		}
		resourceKeys[key] = struct{}{}
	}
}

func writeRecursiveContractApp(t *testing.T, root, relative, name, wasm string, workerInstances []string) {
	t.Helper()
	multiHostWriteFixture(t, root, relative+"/lua.cell.toml", `
name = "lua-orchestrator"
version = "1"
`)
	multiHostWriteFixture(t, root, relative+"/worker.cell.toml", fmt.Sprintf(`
name = "worker"
version = "1"
wasm = %q
`, wasm))
	script := "return { application = " + fmt.Sprintf("%q", name) + " }\n"
	multiHostWriteFixture(t, root, relative+"/app.lua", script)
	digest := sha256.Sum256([]byte(script))
	placements := ""
	if len(workerInstances) > 0 {
		placements = fmt.Sprintf(`
[[cell_placements]]
cell = "worker"
instances = [%q, %q]
`, workerInstances[0], workerInstances[1])
	}
	multiHostWriteFixture(t, root, relative+"/pulp.app.toml", fmt.Sprintf(`
schema_version = 1
name = %q
version = "1"
cells = ["lua.cell.toml", "worker.cell.toml"]
%s
[orchestrator]
manifest = "lua.cell.toml"
script = "app.lua"
sha256 = "%x"
`, name, placements, digest))
}

func recursiveContractApplicationIDs(applications []*manifest.HostedApplication) []string {
	ids := make([]string, len(applications))
	for index, application := range applications {
		ids[index] = application.ID
	}
	return ids
}

func mustRecursiveContractScope(t *testing.T, application, applicationInstance, cell, cellInstance string) ext.Scope {
	t.Helper()
	scope, err := ext.NewScope(application, applicationInstance, cell, cellInstance)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}
