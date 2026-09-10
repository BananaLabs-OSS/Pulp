package registry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestHTTPRegistryFeedsAutomaticLocalCache(t *testing.T) {
	ctx := context.Background()
	data := []byte("remote package")
	digest := Sum(data)
	id, _ := ParseModuleID("games.example/movement")
	version, _ := ParseVersion("2.1.0")
	manifest := Manifest{SchemaVersion: 1, ID: id, Version: version, Provides: []string{"movement.v1"}, Artifacts: []Artifact{{Target: "wasm", Digest: digest, DigestText: digest.String(), Size: int64(len(data))}}}
	wire, err := ManifestTOML(manifest)
	if err != nil {
		t.Fatal(err)
	}
	hosted, _ := OpenLocal(filepath.Join(t.TempDir(), "hosted"))
	if err = hosted.Publish(ctx, wire, map[Digest][]byte{digest: data}); err != nil {
		t.Fatal(err)
	}
	handler := Handler(hosted)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Result(), nil
	})}
	remote, err := OpenHTTP("https://registry.example", client)
	if err != nil {
		t.Fatal(err)
	}
	local, _ := OpenLocal(filepath.Join(t.TempDir(), "automatic-cache"))
	cache := &CachedStore{Cache: local, Upstreams: []Store{remote}}
	versions, err := cache.Versions(ctx, id)
	if err != nil || len(versions) != 1 || versions[0].Compare(version) != 0 {
		t.Fatalf("versions = %v, %v", versions, err)
	}
	if _, err = cache.Manifest(ctx, ReleaseID{ID: id, Version: version}); err != nil {
		t.Fatal(err)
	}
	cache.Upstreams = nil
	reader, err := cache.Blob(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(reader)
	reader.Close()
	if string(got) != string(data) {
		t.Fatalf("cached bytes = %q", got)
	}
}
