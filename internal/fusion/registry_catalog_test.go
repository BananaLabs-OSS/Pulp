package fusion

import (
	"context"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/registry"
)

func TestRegistrySourceCatalogVerifiesAndResolves(t *testing.T) {
	data := []byte("verified source archive")
	digest := registry.Sum(data)
	store, err := registry.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wire := []byte("schema_version=1\nprovides=[]\nconsumes=[]\ncapabilities=[]\n[module]\nid=\"example/source\"\nversion=\"1.2.3\"\n[[artifacts]]\ntarget=\"source/go\"\ndigest=\"" + digest.String() + "\"\nsize=23\n[artifacts.source]\nlanguage=\"go\"\nmodule_path=\"example/source\"\nimport_path=\"example/source/pulp\"\nentrypoint=\"Register\"\n")
	if err := store.Publish(context.Background(), wire, map[registry.Digest][]byte{digest: data}); err != nil {
		t.Fatal(err)
	}
	version, _ := registry.ParseVersion("1.2.3")
	source, err := (RegistrySourceCatalog{Store: store}).FusionSource(context.Background(), registry.ReleaseID{ID: "example/source", Version: version})
	if err != nil {
		t.Fatal(err)
	}
	if source.Version != "v1.2.3" || source.SourceSHA256 != digest.String()[7:] || source.Entrypoint != "Register" {
		t.Fatalf("unexpected source: %#v", source)
	}
}
