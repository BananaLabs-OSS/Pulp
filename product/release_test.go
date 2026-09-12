package product

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestReleaseInstallActivationAndRollback(t *testing.T) {
	root := t.TempDir()
	first := makeFrozenReleaseFixture(t, root, "first")
	second := makeFrozenReleaseFixture(t, root, "second")
	store := filepath.Join(root, "store")
	firstDigest, err := InstallRelease(first, store)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := InstallRelease(second, store)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest == secondDigest {
		t.Fatal("distinct releases have equal digest")
	}
	state, err := ActivateRelease(store, firstDigest)
	if err != nil || state.Active != firstDigest || state.Previous != "" {
		t.Fatalf("first activation = %#v, %v", state, err)
	}
	state, err = ActivateRelease(store, secondDigest)
	if err != nil || state.Active != secondDigest || state.Previous != firstDigest {
		t.Fatalf("second activation = %#v, %v", state, err)
	}
	state, err = RollbackRelease(store)
	if err != nil || state.Active != firstDigest || state.Previous != secondDigest {
		t.Fatalf("rollback = %#v, %v", state, err)
	}
}

func TestActivationRejectsTamperedInstalledRelease(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	digest, err := InstallRelease(makeFrozenReleaseFixture(t, root, "candidate"), store)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store, "releases", digest, "pulp.product.plan.json")
	if err := os.WriteFile(path, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateRelease(store, digest); err == nil {
		t.Fatal("tampered release activated")
	}
}

func makeFrozenReleaseFixture(t *testing.T, root, value string) string {
	t.Helper()
	source := filepath.Join(root, "source-"+value)
	write := func(relative, body string) {
		path := filepath.Join(source, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := "return true -- " + value
	digest := sha256.Sum256([]byte(script))
	write("host/go.mod", "module host")
	write("public/index.html", value)
	write("application/app.lua", script)
	write("application/cell.wasm", "wasm-"+value)
	write("application/cell.toml", "name = \"cell\"\nversion = \"1\"\nwasm = \"cell.wasm\"\n")
	write("application/pulp.app.toml", fmt.Sprintf("name = \"app\"\nversion = \"1\"\ncells = [\"cell.toml\"]\n[orchestrator]\nmanifest = \"cell.toml\"\nscript = \"app.lua\"\nsha256 = %q\n", fmt.Sprintf("%x", digest)))
	write("pulp.product.json", `{"schema":"pulp.product/v1","id":"test.release","name":"Release","version":"1","applications":[{"id":"app","manifest":"application/pulp.app.toml"}],"entrypoint":{"application":"app","surface":"web","path":"/"},"host":{"module":"host/go.mod"},"capabilities":{},"surfaces":[{"id":"web","kind":"web","root":"public"}]}`)
	assembly := filepath.Join(root, value)
	if _, err := AssembleFrozen(filepath.Join(source, "pulp.product.json"), assembly, "web"); err != nil {
		t.Fatal(err)
	}
	return assembly
}
