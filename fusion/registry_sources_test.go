package fusion_test

import (
	"context"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/fusion"
	"github.com/BananaLabs-OSS/Pulp/registry"
)

func TestRegistrySourcesUsesGoVersionAndVerifiedArchive(t *testing.T) {
	ctx := context.Background()
	store, err := registry.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blob := []byte("source archive")
	digest := registry.Sum(blob)
	manifest := []byte("schema_version=1\nprovides=[]\nconsumes=[]\ncapabilities=[]\n[module]\nid=\"example/source\"\nversion=\"1.2.3\"\n[[artifacts]]\ntarget=\"source/go\"\ndigest=\"" + digest.String() + "\"\nsize=14\n[artifacts.source]\nlanguage=\"go\"\nmodule_path=\"example/source\"\nimport_path=\"example/source/core\"\nentrypoint=\"Register\"\n")
	if err := store.Publish(ctx, manifest, map[registry.Digest][]byte{digest: blob}); err != nil {
		t.Fatal(err)
	}
	sources := fusion.RegistrySources{Store: store}
	source, err := sources.Source(ctx, "example/source", "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if source.Version != "v1.2.3" {
		t.Fatalf("Go source version = %q", source.Version)
	}
	got, err := sources.Archive(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(blob) {
		t.Fatal("archive mismatch")
	}
}
