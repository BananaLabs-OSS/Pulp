package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestResolveFocusedProduct(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"application/pulp.app.toml", "host/go.mod", "public/index.html", "desktop/Cargo.toml"} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	descriptor := `{"schema":"pulp.product/v1","id":"banana.timer","name":"Timer","version":"1.0.0","applications":[{"id":"timer","manifest":"application/pulp.app.toml"}],"entrypoint":{"application":"timer","surface":"web","path":"/"},"host":{"module":"host/go.mod","extensions":["storage.sqlite","transport.http.inbound"]},"capabilities":{"required":["storage.sqlite"],"optional":["notification.show.v1"]},"surfaces":[{"id":"web","kind":"web","root":"public"},{"id":"desktop","kind":"tauri","root":"desktop"},{"id":"service","kind":"headless","root":""}]}`
	path := filepath.Join(root, "pulp.product.json")
	if err := os.WriteFile(path, []byte(descriptor), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := Resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ID != "banana.timer" || len(plan.Applications) != 1 || len(plan.Surfaces) != 3 || plan.Extensions[0] != "storage.sqlite" {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestAssembleProducesRunnableMultiApplicationHost(t *testing.T) {
	root := t.TempDir()
	writeProductFixture := func(relative, body string) {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeProductFixture("host/go.mod", "module host")
	writeProductFixture("public/index.html", "ok")
	for _, app := range []string{"state", "ui"} {
		writeProductFixture("apps/"+app+"/app.lua", "return true")
		writeProductFixture("apps/"+app+"/pulp.app.toml", "name = \""+app+"\"\nversion = \"1\"\ncells = []\norchestrator = \"none\"\nscript = \"app.lua\"\nsha256 = \"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\"\n")
	}
	descriptor := `{"schema":"pulp.product/v1","id":"banana.multi","name":"Multi","version":"1","applications":[{"id":"state","manifest":"apps/state/pulp.app.toml"},{"id":"ui","manifest":"apps/ui/pulp.app.toml","instance":"root","dependencies":["state"]}],"entrypoint":{"application":"ui","surface":"web","path":"/"},"host":{"module":"host/go.mod","extensions":["storage.sqlite"]},"capabilities":{"required":["storage.sqlite"]},"surfaces":[{"id":"web","kind":"web","root":"public"}]}`
	writeProductFixture("pulp.product.json", descriptor)
	assembly, err := Assemble(filepath.Join(root, "pulp.product.json"), filepath.Join(root, ".pulp/product"), "web")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(assembly.HostManifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `depends_on = ["state"]`) || !strings.Contains(string(body), `instance = "root"`) {
		t.Fatalf("host manifest =\n%s", body)
	}
	var generated struct {
		Applications []struct {
			ID        string   `toml:"id"`
			DependsOn []string `toml:"depends_on"`
		} `toml:"applications"`
	}
	if _, err := toml.DecodeFile(assembly.HostManifest, &generated); err != nil {
		t.Fatalf("generated host TOML: %v", err)
	}
	if len(generated.Applications) != 2 || generated.Applications[1].ID != "ui" || len(generated.Applications[1].DependsOn) != 1 {
		t.Fatalf("generated applications = %#v", generated.Applications)
	}
}

func TestResolveRejectsMissingRequiredHostCapability(t *testing.T) {
	root := t.TempDir()
	for _, relative := range []string{"app.toml", "go.mod", "public/index.html"} {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	descriptor := `{"schema":"pulp.product/v1","id":"bad.product","name":"Bad","version":"1","applications":[{"id":"bad","manifest":"app.toml"}],"entrypoint":{"application":"bad","surface":"web"},"host":{"module":"go.mod"},"capabilities":{"required":["storage.sqlite"]},"surfaces":[{"id":"web","kind":"web","root":"public"}]}`
	path := filepath.Join(root, "pulp.product.json")
	if err := os.WriteFile(path, []byte(descriptor), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(path); err == nil || !strings.Contains(err.Error(), "has no host extension") {
		t.Fatalf("Resolve missing required capability = %v", err)
	}
}

func TestDescriptorRejectsWorkbenchEscape(t *testing.T) {
	d := Descriptor{Schema: SchemaV1, ID: "bad.product", Name: "Bad", Version: "1", Applications: []Application{{ID: "bad", Manifest: "../projx/application.toml"}}, Entrypoint: Entrypoint{Application: "bad", Surface: "web"}, Host: Host{Module: "host/go.mod"}, Surfaces: []Surface{{ID: "web", Kind: "web", Root: "public"}}}
	if err := d.Validate(); err == nil {
		t.Fatal("escaping application accepted")
	}
}
