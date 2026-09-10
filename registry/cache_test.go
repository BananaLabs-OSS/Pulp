package registry

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type testStore struct {
	manifest Manifest
	blobs    map[Digest][]byte
	calls    int
}

func (s *testStore) Versions(context.Context, ModuleID) ([]Version, error) {
	s.calls++
	return []Version{s.manifest.Version}, nil
}
func (s *testStore) Manifest(_ context.Context, release ReleaseID) (Manifest, error) {
	s.calls++
	if release.ID != s.manifest.ID || release.Version.Compare(s.manifest.Version) != 0 {
		return Manifest{}, ErrNotFound
	}
	return s.manifest, nil
}
func (s *testStore) Blob(_ context.Context, digest Digest) (io.ReadCloser, error) {
	s.calls++
	data, ok := s.blobs[digest]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(newByteReader(data)), nil
}

type byteReader struct {
	data   []byte
	offset int
}

func newByteReader(data []byte) *byteReader { return &byteReader{data: data} }
func (r *byteReader) Read(p []byte) (int, error) {
	if r.offset == len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}

func TestCachedStoreFetchesVerifiesAndBecomesOffline(t *testing.T) {
	ctx := context.Background()
	data := []byte("verified wasm")
	digest := Sum(data)
	id, _ := ParseModuleID("example/game/module")
	version, _ := ParseVersion("1.2.3")
	manifest := Manifest{SchemaVersion: 1, ID: id, Version: version, Artifacts: []Artifact{{Target: "wasm", Digest: digest, DigestText: digest.String(), Size: int64(len(data))}}}
	upstream := &testStore{manifest: manifest, blobs: map[Digest][]byte{digest: data}}
	local, err := OpenLocal(filepath.Join(t.TempDir(), "missing-cache"))
	if err != nil {
		t.Fatal(err)
	}
	store := &CachedStore{Cache: local, Upstreams: []Store{upstream}}
	got, err := store.Manifest(ctx, ReleaseID{ID: id, Version: version})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id {
		t.Fatalf("manifest ID = %s", got.ID)
	}
	store.Upstreams = nil
	if _, err = store.Manifest(ctx, ReleaseID{ID: id, Version: version}); err != nil {
		t.Fatalf("offline manifest: %v", err)
	}
	reader, err := store.Blob(ctx, digest)
	if err != nil {
		t.Fatalf("offline blob: %v", err)
	}
	cached, _ := io.ReadAll(reader)
	reader.Close()
	if string(cached) != string(data) {
		t.Fatalf("cached blob = %q", cached)
	}
}

func TestCachedStoreRejectsCorruptUpstream(t *testing.T) {
	data := []byte("expected")
	digest := Sum(data)
	id, _ := ParseModuleID("example/corrupt")
	version, _ := ParseVersion("1.0.0")
	manifest := Manifest{SchemaVersion: 1, ID: id, Version: version, Artifacts: []Artifact{{Target: "wasm", Digest: digest, DigestText: digest.String(), Size: int64(len(data))}}}
	upstream := &testStore{manifest: manifest, blobs: map[Digest][]byte{digest: []byte("tampered")}}
	local, _ := OpenLocal(t.TempDir())
	_, err := (&CachedStore{Cache: local, Upstreams: []Store{upstream}}).Manifest(context.Background(), ReleaseID{ID: id, Version: version})
	if err == nil {
		t.Fatal("corrupt artifact accepted")
	}
	if _, localErr := local.Manifest(context.Background(), ReleaseID{ID: id, Version: version}); !errors.Is(localErr, ErrNotFound) {
		t.Fatalf("partial release cached: %v", localErr)
	}
}

func TestOpenDefaultLocalCreatesConfiguredCache(t *testing.T) {
	root := filepath.Join(t.TempDir(), "new", "cache")
	t.Setenv(CacheEnv, root)
	if _, err := OpenDefaultLocal(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "v1", "modules")); err != nil {
		t.Fatal(err)
	}
}
