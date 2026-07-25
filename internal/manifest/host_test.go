package manifest

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestLoadHostLoadsApplicationsInstancesRoutesAndNamespaces(t *testing.T) {
	root := t.TempDir()
	writeHostApplication(t, root, "apps/evolution", "evolution")
	writeHostApplication(t, root, "apps/sessions", "sessions")
	hostPath := writeAppFile(t, root, "pulp.host.toml", `
schema_version = 1
name = "platform"

[[applications]]
id = "evolution"
manifest = "apps/evolution/pulp.app.toml"
aliases = ["evolution-web"]
storage_namespace = "evolution"
event_namespace = "evolution-events"

[[applications]]
id = "sessions"
manifest = "apps/sessions/pulp.app.toml"
instances = 2
aliases = ["sessions-public", "sessions-worker"]
storage_namespace = "sessions"
event_namespace = "sessions-events"
depends_on = ["evolution"]

[[routes]]
path = "/"
application = "evolution"

[[routes]]
path = "/sessions"
application = "sessions"
instance = "sessions-public"
`)

	host, err := LoadHost(hostPath)
	if err != nil {
		t.Fatalf("LoadHost: %v", err)
	}
	if host.Name != "platform" || len(host.Applications) != 2 || len(host.Routes) != 2 {
		t.Fatalf("host shape = %#v", host)
	}
	evolution := host.Applications[0]
	if evolution.Application.Name != "evolution" || evolution.Instances[0].Alias != "evolution-web" {
		t.Fatalf("evolution = %#v", evolution)
	}
	sessions := host.Applications[1]
	if sessions.Instances[0].Alias != "sessions-public" || sessions.Instances[1].Alias != "sessions-worker" {
		t.Fatalf("sessions instances = %#v", sessions.Instances)
	}
	if host.Routes[0].Path != "/" || host.Routes[0].Instance != "evolution-web" {
		t.Fatalf("root route = %#v", host.Routes[0])
	}
	if host.Routes[1].Path != "/sessions" || host.Routes[1].Instance != "sessions-public" {
		t.Fatalf("sessions route = %#v", host.Routes[1])
	}
	if got := host.ApplicationOrder; len(got) != 2 || got[0].ID != "evolution" || got[1].ID != "sessions" {
		t.Fatalf("application boot order = %#v", got)
	}
}

func TestLoadHostGeneratesStableInstanceAliases(t *testing.T) {
	root := t.TempDir()
	writeHostApplication(t, root, "apps/sessions", "sessions")
	hostPath := writeAppFile(t, root, "pulp.host.toml", `
name = "platform"
[[applications]]
id = "sessions"
manifest = "apps/sessions/pulp.app.toml"
instances = 3
aliases = ["public"]
storage_namespace = "sessions"
event_namespace = "sessions-events"
`)
	host, err := LoadHost(hostPath)
	if err != nil {
		t.Fatalf("LoadHost: %v", err)
	}
	got := host.Applications[0].Instances
	if got[0].Alias != "public" || got[1].Alias != "sessions-2" || got[2].Alias != "sessions-3" {
		t.Fatalf("aliases = %#v", got)
	}
}

func TestLoadHostValidatesExactHostConsumesAcrossDirectDependencies(t *testing.T) {
	const callerCell = `name = "lua-orchestrator"
version = "1"
host_consumes = ["orders.apply.v1"]
`
	const providerCell = `name = "orders"
version = "1"
provides = ["orders.apply.v1"]
`
	t.Run("exact direct dependency", func(t *testing.T) {
		root := t.TempDir()
		writeHostContractApplication(t, root, "apps/caller", "caller", callerCell)
		writeHostContractApplication(t, root, "apps/provider", "provider", providerCell)
		hostPath := writeAppFile(t, root, "pulp.host.toml", hostConsumeHostBody(true, false))
		if _, err := LoadHost(hostPath); err != nil {
			t.Fatalf("LoadHost exact host_consumes: %v", err)
		}
	})
	t.Run("missing provider", func(t *testing.T) {
		root := t.TempDir()
		writeHostContractApplication(t, root, "apps/caller", "caller", callerCell)
		writeHostContractApplication(t, root, "apps/provider", "provider", `name = "orders"
version = "1"
provides = ["orders.preview.v1"]
`)
		hostPath := writeAppFile(t, root, "pulp.host.toml", hostConsumeHostBody(true, false))
		if _, err := LoadHost(hostPath); err == nil || !strings.Contains(err.Error(), "no direct dependency application provides it") {
			t.Fatalf("LoadHost missing host provider = %v", err)
		}
	})
	t.Run("ambiguous provider", func(t *testing.T) {
		root := t.TempDir()
		writeHostContractApplication(t, root, "apps/caller", "caller", callerCell)
		writeHostContractApplication(t, root, "apps/provider", "provider", providerCell, `name = "orders-shadow"
version = "1"
provides = ["orders.apply.v1"]
`)
		hostPath := writeAppFile(t, root, "pulp.host.toml", hostConsumeHostBody(true, false))
		if _, err := LoadHost(hostPath); err == nil || !strings.Contains(err.Error(), "provide it ambiguously") {
			t.Fatalf("LoadHost ambiguous host provider = %v", err)
		}
	})
	t.Run("reverse edge is not a grant", func(t *testing.T) {
		root := t.TempDir()
		writeHostContractApplication(t, root, "apps/caller", "caller", callerCell)
		writeHostContractApplication(t, root, "apps/provider", "provider", providerCell)
		hostPath := writeAppFile(t, root, "pulp.host.toml", hostConsumeHostBody(false, true))
		if _, err := LoadHost(hostPath); err == nil || !strings.Contains(err.Error(), "no direct dependency application provides it") {
			t.Fatalf("LoadHost reverse host grant = %v", err)
		}
	})
}

func hostConsumeHostBody(callerDepends, providerDepends bool) string {
	callerEdge := ""
	if callerDepends {
		callerEdge = "depends_on = [\"provider\"]\n"
	}
	providerEdge := ""
	if providerDepends {
		providerEdge = "depends_on = [\"caller\"]\n"
	}
	return `name = "platform"
[[applications]]
id = "caller"
manifest = "apps/caller/pulp.app.toml"
storage_namespace = "caller"
event_namespace = "caller-events"
` + callerEdge + `[[applications]]
id = "provider"
manifest = "apps/provider/pulp.app.toml"
storage_namespace = "provider"
event_namespace = "provider-events"
` + providerEdge
}

func writeHostContractApplication(t *testing.T, root, relative, name string, cells ...string) {
	t.Helper()
	cellNames := make([]string, len(cells))
	for index, body := range cells {
		cellNames[index] = fmt.Sprintf("cell-%d.toml", index)
		writeAppFile(t, root, relative+"/"+cellNames[index], body)
	}
	script := "return true -- " + name
	writeAppFile(t, root, relative+"/app.lua", script)
	digest := sha256.Sum256([]byte(script))
	quoted := make([]string, len(cellNames))
	for index, cellName := range cellNames {
		quoted[index] = fmt.Sprintf("%q", cellName)
	}
	writeAppFile(t, root, relative+"/pulp.app.toml", fmt.Sprintf(`
name = %q
version = "1"
cells = [%s]
[orchestrator]
manifest = %q
script = "app.lua"
sha256 = "%x"
`, name, strings.Join(quoted, ", "), cellNames[0], digest))
}

func TestLoadHostRejectsInvalidComposition(t *testing.T) {
	root := t.TempDir()
	writeHostApplication(t, root, "apps/a", "a")
	writeHostApplication(t, root, "apps/b", "b")

	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "duplicate-id",
			body: `
[[applications]]
id = "a"
manifest = "apps/a/pulp.app.toml"
storage_namespace = "a"
event_namespace = "a-events"
[[applications]]
id = "a"
manifest = "apps/b/pulp.app.toml"
storage_namespace = "b"
event_namespace = "b-events"`,
			want: `duplicate application id "a"`,
		},
		{
			name: "shared-storage-namespace",
			body: `
[[applications]]
id = "a"
manifest = "apps/a/pulp.app.toml"
storage_namespace = "shared"
event_namespace = "a-events"
[[applications]]
id = "b"
manifest = "apps/b/pulp.app.toml"
storage_namespace = "shared"
event_namespace = "b-events"`,
			want: `storage namespace "shared" is shared`,
		},
		{
			name: "shared-event-namespace",
			body: `
[[applications]]
id = "a"
manifest = "apps/a/pulp.app.toml"
storage_namespace = "a"
event_namespace = "shared"
[[applications]]
id = "b"
manifest = "apps/b/pulp.app.toml"
storage_namespace = "b"
event_namespace = "shared"`,
			want: `event namespace "shared" is shared`,
		},
		{
			name: "unsafe-namespace",
			body: `
[[applications]]
id = "a"
manifest = "apps/a/pulp.app.toml"
storage_namespace = "../unsafe"
event_namespace = "a-events"`,
			want: "storage_namespace must match",
		},
		{
			name: "bad-count-and-aliases",
			body: `
[[applications]]
id = "a"
manifest = "apps/a/pulp.app.toml"
instances = 1
aliases = ["one", "two"]
storage_namespace = "a"
event_namespace = "a-events"`,
			want: "aliases contains 2 entries for 1 instances",
		},
		{
			name: "cycle",
			body: `
[[applications]]
id = "a"
manifest = "apps/a/pulp.app.toml"
storage_namespace = "a"
event_namespace = "a-events"
depends_on = ["b"]
[[applications]]
id = "b"
manifest = "apps/b/pulp.app.toml"
storage_namespace = "b"
event_namespace = "b-events"
depends_on = ["a"]`,
			want: "application dependency cycle: a -> b -> a",
		},
		{
			name: "unknown-dependency",
			body: `
[[applications]]
id = "a"
manifest = "apps/a/pulp.app.toml"
storage_namespace = "a"
event_namespace = "a-events"
depends_on = ["missing"]`,
			want: `depends on unknown application "missing"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hostPath := writeAppFile(t, root, test.name+".toml", "name = \"platform\"\n"+test.body)
			if _, err := LoadHost(hostPath); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadHost error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadHostRejectsInvalidRoutesAndRelativeManifest(t *testing.T) {
	root := t.TempDir()
	writeHostApplication(t, root, "apps/a", "a")
	writeHostApplication(t, root, "apps/b", "b")
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "absolute-manifest",
			body: `
[[applications]]
id = "a"
manifest = "C:/not-allowed/pulp.app.toml"
storage_namespace = "a"
event_namespace = "a-events"`,
			want: "must be relative to pulp.host.toml",
		},
		{
			name: "duplicate-route",
			body: `
[[applications]]
id = "a"
manifest = "apps/a/pulp.app.toml"
storage_namespace = "a"
event_namespace = "a-events"
[[routes]]
path = "/a"
application = "a"
[[routes]]
path = "/a"
application = "a"`,
			want: `duplicate route path "/a"`,
		},
		{
			name: "multiple-instances-needs-route-target",
			body: `
[[applications]]
id = "a"
manifest = "apps/a/pulp.app.toml"
instances = 2
storage_namespace = "a"
event_namespace = "a-events"
[[routes]]
path = "/a"
application = "a"`,
			want: "routes[0].instance is required",
		},
		{
			name: "unknown-route-instance",
			body: `
[[applications]]
id = "a"
manifest = "apps/a/pulp.app.toml"
aliases = ["public"]
storage_namespace = "a"
event_namespace = "a-events"
[[routes]]
path = "/a"
application = "a"
instance = "missing"`,
			want: `unknown instance "missing"`,
		},
		{
			name: "non-canonical-route",
			body: `
[[applications]]
id = "a"
manifest = "apps/a/pulp.app.toml"
storage_namespace = "a"
event_namespace = "a-events"
[[routes]]
path = "/a/../b"
application = "a"`,
			want: "must be canonical",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hostPath := writeAppFile(t, root, test.name+".toml", "name = \"platform\"\n"+test.body)
			if _, err := LoadHost(hostPath); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadHost error = %v, want %q", err, test.want)
			}
		})
	}
}

func writeHostApplication(t *testing.T, root, relative, name string) {
	t.Helper()
	writeAppFile(t, root, relative+"/cell.toml", fmt.Sprintf(`
name = %q
version = "1.0.0"
`, name+"-lua"))
	script := "return true -- " + name
	writeAppFile(t, root, relative+"/app.lua", script)
	digest := sha256.Sum256([]byte(script))
	writeAppFile(t, root, relative+"/pulp.app.toml", fmt.Sprintf(`
name = %q
version = "1"
cells = ["cell.toml"]
[orchestrator]
manifest = "cell.toml"
script = "app.lua"
sha256 = "%x"
`, name, digest))
}
