package run

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

// This is a cross-package contract test. The two app manifests intentionally
// point their player-manager cells at one file, while their host identities,
// namespaces, Lua cells, and runtime instances remain separate.
func TestMultiHostManifestSeparatesInstancesOverSharedPackageBytes(t *testing.T) {
	root := t.TempDir()
	sharedWASM := multiHostWriteFixture(t, root, "shared/packages/player-manager.wasm", "same package bytes")
	multiHostWriteApp(t, root, "apps/evolution", "evolution", "../../shared/packages/player-manager.wasm")
	multiHostWriteApp(t, root, "apps/sessions", "sessions", "../../shared/packages/player-manager.wasm")
	hostPath := multiHostWriteFixture(t, root, "testdata/pulp.host.toml", `
schema_version = 1
name = "platform"

[[applications]]
id = "evolution"
manifest = "../apps/evolution/pulp.app.toml"
aliases = ["evolution-web"]
storage_namespace = "evolution-store"
event_namespace = "evolution-events"

[[applications]]
id = "sessions"
manifest = "../apps/sessions/pulp.app.toml"
instances = 2
aliases = ["sessions-public", "sessions-workers"]
storage_namespace = "sessions-store"
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

	host, err := manifest.LoadHost(hostPath)
	if err != nil {
		t.Fatalf("LoadHost: %v", err)
	}
	if got := multiHostCellWASMPath(t, host.Applications[0]); got != sharedWASM {
		t.Fatalf("evolution player-manager WASM = %q, want %q", got, sharedWASM)
	}
	if got := multiHostCellWASMPath(t, host.Applications[1]); got != sharedWASM {
		t.Fatalf("sessions player-manager WASM = %q, want %q", got, sharedWASM)
	}
	if host.Applications[0].Application == host.Applications[1].Application {
		t.Fatal("host loaded two applications into the same in-memory composition")
	}
	if len(host.Routes) != 2 || host.Routes[0].Application != "evolution" || host.Routes[1].Instance != "sessions-public" {
		t.Fatalf("routes = %#v", host.Routes)
	}

	loader := ManifestHostLoader{}
	applications, err := loader.LoadHostApplications(context.Background(), hostPath)
	if err != nil {
		t.Fatalf("LoadHostApplications: %v", err)
	}
	if got := multiHostIdentities(applications); !sameStringSlice(got, []string{
		"evolution/evolution-web", "sessions/sessions-public", "sessions/sessions-workers",
	}) {
		t.Fatalf("runtime identities = %#v", got)
	}
	for _, app := range applications {
		switch app.Identity.ApplicationID {
		case "evolution":
			if app.StorageNamespace != "evolution-store" || app.EventNamespace != "evolution-events" {
				t.Fatalf("evolution namespace = %#v", app)
			}
		case "sessions":
			if app.StorageNamespace != "sessions-store" || app.EventNamespace != "sessions-events" {
				t.Fatalf("sessions namespace = %#v", app)
			}
			if !sameStringSlice(app.DependsOn, []string{"evolution"}) {
				t.Fatalf("sessions lifecycle graph = %#v", app.DependsOn)
			}
		default:
			t.Fatalf("unexpected app = %#v", app)
		}
	}

	var lifecycle []string
	var created []HostedApplication
	var mu sync.Mutex
	supervisor := multiHostManifestSupervisor(t, loader, ApplicationRuntimeFactoryFunc(func(_ context.Context, app HostedApplication) (ApplicationRuntime, error) {
		mu.Lock()
		created = append(created, app)
		mu.Unlock()
		return &multiHostManifestRuntime{identity: app.Identity, lifecycle: &lifecycle}, nil
	}))
	if err := supervisor.Start(context.Background(), hostPath); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got, want := lifecycle, []string{
		"start:evolution/evolution-web",
		"start:sessions/sessions-public",
		"start:sessions/sessions-workers",
		"shutdown:sessions/sessions-workers",
		"shutdown:sessions/sessions-public",
		"shutdown:evolution/evolution-web",
	}; !sameStringSlice(got, want) {
		t.Fatalf("lifecycle = %#v, want %#v", got, want)
	}
	if len(created) != 3 || created[1].Identity == created[2].Identity {
		t.Fatalf("created runtimes are not distinct = %#v", created)
	}
}

func TestMultiHostManifestRejectsCrossApplicationAmbiguity(t *testing.T) {
	root := t.TempDir()
	multiHostWriteApp(t, root, "apps/evolution", "evolution", "../../shared/package.wasm")
	multiHostWriteApp(t, root, "apps/sessions", "sessions", "../../shared/package.wasm")

	tests := []struct {
		name string
		host string
		want string
	}{
		{
			name: "duplicate-route",
			host: `
name = "platform"
[[applications]]
id = "evolution"
manifest = "../apps/evolution/pulp.app.toml"
storage_namespace = "evolution-store"
event_namespace = "evolution-events"
[[applications]]
id = "sessions"
manifest = "../apps/sessions/pulp.app.toml"
storage_namespace = "sessions-store"
event_namespace = "sessions-events"
[[routes]]
path = "/api"
application = "evolution"
[[routes]]
path = "/api"
application = "sessions"`,
			want: `duplicate route path "/api"`,
		},
		{
			name: "instance-omitted-for-multiple-copies",
			host: `
name = "platform"
[[applications]]
id = "sessions"
manifest = "../apps/sessions/pulp.app.toml"
instances = 2
storage_namespace = "sessions-store"
event_namespace = "sessions-events"
[[routes]]
path = "/sessions"
application = "sessions"`,
			want: "routes[0].instance is required",
		},
		{
			name: "cross-application-instance",
			host: `
name = "platform"
[[applications]]
id = "evolution"
manifest = "../apps/evolution/pulp.app.toml"
aliases = ["evolution-web"]
storage_namespace = "evolution-store"
event_namespace = "evolution-events"
[[applications]]
id = "sessions"
manifest = "../apps/sessions/pulp.app.toml"
aliases = ["sessions-public"]
storage_namespace = "sessions-store"
event_namespace = "sessions-events"
[[routes]]
path = "/sessions"
application = "sessions"
instance = "evolution-web"`,
			want: `unknown instance "evolution-web" of application "sessions"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hostPath := multiHostWriteFixture(t, root, "testdata/"+test.name+".toml", test.host)
			if _, err := (ManifestHostLoader{}).LoadHostApplications(context.Background(), hostPath); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadHostApplications error = %v, want %q", err, test.want)
			}
		})
	}
}

func multiHostManifestSupervisor(t *testing.T, loader HostManifestLoader, factory ApplicationRuntimeFactory) *MultiHostSupervisor {
	t.Helper()
	supervisor, err := NewMultiHostSupervisor(loader, factory)
	if err != nil {
		t.Fatalf("NewMultiHostSupervisor: %v", err)
	}
	return supervisor
}

func multiHostWriteApp(t *testing.T, root, relative, name, playerManagerWASM string) {
	t.Helper()
	multiHostWriteFixture(t, root, relative+"/lua.cell.toml", `
name = "lua"
version = "1.0.0"
`)
	multiHostWriteFixture(t, root, relative+"/player-manager.cell.toml", fmt.Sprintf(`
name = "player-manager"
version = "1.0.0"
wasm = %q
`, playerManagerWASM))
	script := "return true -- " + name
	multiHostWriteFixture(t, root, relative+"/app.lua", script)
	digest := sha256.Sum256([]byte(script))
	multiHostWriteFixture(t, root, relative+"/pulp.app.toml", fmt.Sprintf(`
name = %q
version = "1"
cells = ["lua.cell.toml", "player-manager.cell.toml"]
[orchestrator]
manifest = "lua.cell.toml"
script = "app.lua"
sha256 = "%x"
`, name, digest))
}

func multiHostWriteFixture(t *testing.T, root, relative, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("absolute %s: %v", path, err)
	}
	return absolute
}

func multiHostCellWASMPath(t *testing.T, app *manifest.HostedApplication) string {
	t.Helper()
	cell := app.Application.Cells.Lookup("player-manager")
	if cell == nil {
		t.Fatalf("application %q does not contain player-manager", app.ID)
	}
	return cell.WASMPath
}

func multiHostIdentities(applications []HostedApplication) []string {
	identities := make([]string, len(applications))
	for index, app := range applications {
		identities[index] = app.Identity.String()
	}
	return identities
}

func sameStringSlice(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

type multiHostManifestRuntime struct {
	identity  ApplicationIdentity
	lifecycle *[]string
}

func (r *multiHostManifestRuntime) Identity() ApplicationIdentity { return r.identity }

func (r *multiHostManifestRuntime) Start(context.Context) error {
	*r.lifecycle = append(*r.lifecycle, "start:"+r.identity.String())
	return nil
}

func (r *multiHostManifestRuntime) Shutdown(context.Context) error {
	*r.lifecycle = append(*r.lifecycle, "shutdown:"+r.identity.String())
	return nil
}
