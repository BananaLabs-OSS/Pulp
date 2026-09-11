package manifest

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefreshCellDigestMakesChangedCellLoadable(t *testing.T) {
	root := t.TempDir()
	wasmPath := filepath.Join(root, "cell.wasm")
	manifestPath := filepath.Join(root, "pulp.cell.toml")
	if err := os.WriteFile(wasmPath, []byte("current wasm"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte("name = \"test\"\nversion = \"1\"\nwasm = \"cell.wasm\"\nwasm_sha256 = \"stale\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RefreshCellDigest(manifestPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(manifestPath); err != nil {
		t.Fatalf("refreshed cell does not load: %v", err)
	}
}

func TestReplaceTopLevelDigestPreservesPrefixWhenDigestStartsWithLetter(t *testing.T) {
	manifest := []byte("name=\"cell\"\nwasm_sha256=\"old\"\n[config]\nprefix=\"ledger\"\n")
	updated, err := replaceTopLevelValue(manifest, "wasm_sha256", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	want := "name=\"cell\"\nwasm_sha256=\"deadbeef\"\n[config]\nprefix=\"ledger\"\n"
	if string(updated) != want {
		t.Fatalf("updated manifest = %q, want %q", updated, want)
	}
}

func TestReplaceTopLevelDigestInsertionPreservesFollowingSection(t *testing.T) {
	manifest := []byte("name=\"cell\"\n[config]\nprefix=\"ledger\"\n")
	updated, err := replaceTopLevelValue(manifest, "wasm_sha256", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	want := "name=\"cell\"\nwasm_sha256 = \"deadbeef\"\n[config]\nprefix=\"ledger\"\n"
	if string(updated) != want {
		t.Fatalf("updated manifest = %q, want %q", updated, want)
	}
}

func TestRefreshAppDigestsMakesChangedAppLoadable(t *testing.T) {
	root := t.TempDir()
	write := func(name, value string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("cell.wasm", "first wasm")
	write("cell.toml", "name = \"lua\"\nversion = \"1\"\nwasm = \"cell.wasm\"\nwasm_sha256 = \"stale\"\n")
	write("app.lua", "return true")
	write("pulp.app.toml", "name = \"test\"\nversion = \"1\"\nrequire_wasm_sha256 = true\ncells = [\"cell.toml\"]\n[orchestrator]\nmanifest = \"cell.toml\"\nscript = \"app.lua\"\nsha256 = \"stale\"\n")
	path := filepath.Join(root, "pulp.app.toml")
	if err := RefreshAppDigests(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApp(path); err != nil {
		t.Fatalf("refreshed app does not load: %v", err)
	}
	write("cell.wasm", "second wasm")
	write("app.lua", "return false")
	if err := RefreshAppDigests(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApp(path); err != nil {
		t.Fatalf("re-refreshed app does not load: %v", err)
	}
	cell, _ := os.ReadFile(filepath.Join(root, "cell.toml"))
	if strings.Contains(string(cell), "stale") {
		t.Fatalf("cell digest was not replaced: %s", cell)
	}
}

func TestRefreshHostDigestsPlansWholeGraphBeforeWriting(t *testing.T) {
	root := t.TempDir()
	write := func(name, value string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("one/cell.wasm", "wasm-one")
	write("one/app.lua", "return true")
	write("one/cell.toml", "name = \"one\"\nversion = \"1\"\nwasm = \"cell.wasm\"\nwasm_sha256 = \"stale-one\"\n")
	write("one/pulp.app.toml", "name = \"one\"\nversion = \"1\"\nrequire_wasm_sha256 = true\ncells = [\"cell.toml\"]\n[orchestrator]\nmanifest = \"cell.toml\"\nscript = \"app.lua\"\nsha256 = \"stale-one\"\n")
	write("two/pulp.app.toml", "this is not toml")
	write("pulp.host.toml", "schema_version = 1\nname = \"test\"\n[[applications]]\nid = \"one\"\nmanifest = \"one/pulp.app.toml\"\naliases = [\"primary\"]\nstorage_namespace = \"one\"\nevent_namespace = \"one\"\n[[applications]]\nid = \"two\"\nmanifest = \"two/pulp.app.toml\"\naliases = [\"primary\"]\nstorage_namespace = \"two\"\nevent_namespace = \"two\"\n")

	before, err := os.ReadFile(filepath.Join(root, "one/pulp.app.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := RefreshHostDigests(filepath.Join(root, "pulp.host.toml")); err == nil {
		t.Fatal("malformed second application unexpectedly refreshed")
	}
	after, err := os.ReadFile(filepath.Join(root, "one/pulp.app.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("first application changed before complete graph planned")
	}

	write("two/cell.wasm", "wasm-two")
	write("two/app.lua", "return true")
	write("two/cell.toml", "name = \"two\"\nversion = \"1\"\nwasm = \"cell.wasm\"\nwasm_sha256 = \"stale-two\"\n")
	write("two/pulp.app.toml", "name = \"two\"\nversion = \"1\"\nrequire_wasm_sha256 = true\ncells = [\"cell.toml\"]\n[orchestrator]\nmanifest = \"cell.toml\"\nscript = \"app.lua\"\nsha256 = \"stale-two\"\n")
	if err := RefreshHostDigests(filepath.Join(root, "pulp.host.toml")); err != nil {
		t.Fatal(err)
	}
	if err := CheckHostDigests(filepath.Join(root, "pulp.host.toml")); err != nil {
		t.Fatalf("refreshed host does not load: %v", err)
	}
}
