package product

import (
	"os"
	"path/filepath"
	"testing"
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

func TestDescriptorRejectsWorkbenchEscape(t *testing.T) {
	d := Descriptor{Schema: SchemaV1, ID: "bad.product", Name: "Bad", Version: "1", Applications: []Application{{ID: "bad", Manifest: "../projx/application.toml"}}, Entrypoint: Entrypoint{Application: "bad", Surface: "web"}, Host: Host{Module: "host/go.mod"}, Surfaces: []Surface{{ID: "web", Kind: "web", Root: "public"}}}
	if err := d.Validate(); err == nil {
		t.Fatal("escaping application accepted")
	}
}
