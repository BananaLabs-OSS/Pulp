package registry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectEnsureLockAndRefreshArtifacts(t *testing.T) {
	blob := []byte("wasm-v1")
	manifest, err := ParseManifest(moduleText("example/project", "1.0.0", "wasm", blob, ""))
	if err != nil {
		t.Fatal(err)
	}
	wire, blobs, err := RefreshArtifacts(manifest, map[string][]byte{"wasm": blob})
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := ParseManifest(wire)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Artifacts[0].Digest != Sum(blob) || refreshed.Artifacts[0].Size != int64(len(blob)) {
		t.Fatal("artifact was not refreshed")
	}

	store, err := OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(context.Background(), wire, blobs); err != nil {
		t.Fatal(err)
	}
	project, err := ParseProject([]byte("schema_version = 1\ntarget = \"wasm\"\nremotes = []\n[[modules]]\nid = \"example/project\"\nrequirement = \"^1.0.0\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(t.TempDir(), "nested", DefaultLockFile)
	lock, changed, err := EnsureProjectLock(context.Background(), store, project, lockPath)
	if err != nil || !changed {
		t.Fatalf("first sync changed=%v err=%v", changed, err)
	}
	if _, changed, err = EnsureProjectLock(context.Background(), store, project, lockPath); err != nil || changed {
		t.Fatalf("second sync changed=%v err=%v", changed, err)
	}
	if lock.Modules[0].ArtifactDigest != Sum(blob).String() {
		t.Fatal("wrong locked digest")
	}
	if file, err := os.ReadFile(lockPath); err != nil || !strings.HasSuffix(string(file), "\n") {
		t.Fatalf("atomic lock output: %q %v", file, err)
	}
}

func TestProjectRejectsUnknownAndDuplicateModules(t *testing.T) {
	for _, input := range []string{
		"schema_version=1\ntarget=\"wasm\"\nwat=true\n[[modules]]\nid=\"a/a\"\nrequirement=\"*\"\n",
		"schema_version=1\ntarget=\"wasm\"\n[[modules]]\nid=\"a/a\"\nrequirement=\"*\"\n[[modules]]\nid=\"a/a\"\nrequirement=\"*\"\n",
	} {
		if _, err := ParseProject([]byte(input)); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}

func TestSourceArtifactRoundTripIsCanonical(t *testing.T) {
	blob := []byte("source archive")
	digest := Sum(blob)
	text := "schema_version=1\nprovides=[]\nconsumes=[]\ncapabilities=[]\n[module]\nid=\"example/source\"\nversion=\"1.0.0\"\n[[artifacts]]\ntarget=\"source/go\"\ndigest=\"" + digest.String() + "\"\nsize=14\n[artifacts.source]\nlanguage=\"go\"\nmodule_path=\"example/source\"\nimport_path=\"example/source/pulp\"\nentrypoint=\"Register\"\ntoolchain=\"go1.25.6\"\n"
	m, err := ParseManifest([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := ManifestTOML(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseManifest(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got.Artifacts[0].Source == nil || got.Artifacts[0].Source.ImportPath != "example/source/pulp" {
		t.Fatalf("source metadata lost: %#v", got.Artifacts[0])
	}
}
