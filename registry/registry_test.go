package registry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func moduleText(id, version, target string, blob []byte, deps string) []byte {
	return []byte(`schema_version = 1
provides = []
consumes = []
capabilities = []
[module]
id = "` + id + `"
version = "` + version + `"
` + deps + `
[[artifacts]]
target = "` + target + `"
digest = "` + Sum(blob).String() + `"
size = ` + itoa(len(blob)) + `
`)
}
func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	b := []byte{}
	for n > 0 {
		b = append([]byte{digits[n%10]}, b...)
		n /= 10
	}
	return string(b)
}
func publish(t *testing.T, s *LocalStore, id, version string, blob []byte, deps string) {
	t.Helper()
	text := moduleText(id, version, "wasm", blob, deps)
	m, e := ParseManifest(text)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Publish(context.Background(), text, map[Digest][]byte{m.Artifacts[0].Digest: blob}); e != nil {
		t.Fatal(e)
	}
}

func TestParseManifestStrictAndCanonical(t *testing.T) {
	b := []byte("x")
	m, e := ParseManifest(moduleText("io.test/a", "1.2.3", "wasm", b, ""))
	if e != nil {
		t.Fatal(e)
	}
	if m.ID != "io.test/a" || m.Version.String() != "1.2.3" {
		t.Fatalf("manifest = %#v", m)
	}
	bad := append(moduleText("io.test/a", "1.2.3", "wasm", b, ""), []byte("unknown = true\n")...)
	if _, e = ParseManifest(bad); e == nil {
		t.Fatal("unknown field accepted")
	}
	a, _ := CanonicalManifest(m)
	z, _ := CanonicalManifest(m)
	if string(a) != string(z) {
		t.Fatal("canonical output changed")
	}
}
func TestLocalPublishImmutableAndDigestChecked(t *testing.T) {
	s, _ := OpenLocal(t.TempDir())
	blob := []byte("wasm")
	text := moduleText("io.test/a", "1.0.0", "wasm", blob, "")
	m, _ := ParseManifest(text)
	blobs := map[Digest][]byte{m.Artifacts[0].Digest: blob}
	if e := s.Publish(context.Background(), text, blobs); e != nil {
		t.Fatal(e)
	}
	if e := s.Publish(context.Background(), text, blobs); e != nil {
		t.Fatalf("idempotent publish: %v", e)
	}
	changed := moduleText("io.test/a", "1.0.0", "wasm", []byte("other"), "")
	cm, _ := ParseManifest(changed)
	if e := s.Publish(context.Background(), changed, map[Digest][]byte{cm.Artifacts[0].Digest: []byte("other")}); !errors.Is(e, ErrConflict) {
		t.Fatalf("conflict = %v", e)
	}
	if e := s.Publish(context.Background(), moduleText("io.test/b", "1.0.0", "wasm", blob, ""), map[Digest][]byte{m.Artifacts[0].Digest: []byte("wrong")}); e == nil {
		t.Fatal("bad digest accepted")
	}
}
func TestResolveBacktracksLocksAndVerifies(t *testing.T) {
	s, _ := OpenLocal(t.TempDir())
	publish(t, s, "io.test/b", "1.0.0", []byte("b1"), "")
	publish(t, s, "io.test/b", "2.0.0", []byte("b2"), "")
	deps := "[[dependencies]]\nid = \"io.test/b\"\nrequirement = \"^1.0.0\"\n"
	publish(t, s, "io.test/a", "1.0.0", []byte("a"), deps)
	l, e := Resolve(context.Background(), s, Request{Target: "wasm", Roots: []Dependency{{ID: "io.test/a", Requirement: "^1.0.0"}}})
	if e != nil {
		t.Fatal(e)
	}
	if len(l.Modules) != 2 || l.Modules[1].Version != "1.0.0" {
		t.Fatalf("lock = %#v", l.Modules)
	}
	if e = Verify(context.Background(), s, l); e != nil {
		t.Fatal(e)
	}
	b, _ := l.Marshal()
	b2, _ := l.Marshal()
	if string(b) != string(b2) {
		t.Fatal("lock is nondeterministic")
	}
	parsed, e := ParseLock(b)
	if e != nil || len(parsed.Modules) != 2 {
		t.Fatalf("parse lock = %#v, %v", parsed, e)
	}
	if _, e = ParseLock(append(b, []byte(` {}`)...)); e == nil {
		t.Fatal("trailing lock value accepted")
	}
	d, _ := ParseDigest(l.Modules[0].ArtifactDigest)
	path := s.blobPath(d)
	if _, e = os.Stat(path); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, []byte("tamper"), 0644); e != nil {
		t.Fatal(e)
	}
	if e = Verify(context.Background(), s, l); e == nil || !strings.Contains(e.Error(), "digest mismatch") {
		t.Fatalf("tamper verify = %v", e)
	}
}
func TestCycleAndPathIdentityRejected(t *testing.T) {
	if _, e := ParseModuleID("../escape"); e == nil {
		t.Fatal("path traversal id accepted")
	}
	s, _ := OpenLocal(t.TempDir())
	da := "[[dependencies]]\nid=\"io.test/b\"\nrequirement=\"1.0.0\"\n"
	db := "[[dependencies]]\nid=\"io.test/a\"\nrequirement=\"1.0.0\"\n"
	publish(t, s, "io.test/a", "1.0.0", []byte("a"), da)
	publish(t, s, "io.test/b", "1.0.0", []byte("b"), db)
	_, e := Resolve(context.Background(), s, Request{Target: "wasm", Roots: []Dependency{{ID: "io.test/a", Requirement: "1.0.0"}}})
	if e == nil || !strings.Contains(e.Error(), "cycle") {
		t.Fatalf("cycle = %v", e)
	}
	if strings.Contains(filepath.Clean(s.releasePath(ReleaseID{"io.test/a", Version{1, 0, 0, ""}})), "..") {
		t.Fatal("unsafe path")
	}
}

func TestSemVerPrecedenceAndUnsupportedMetadata(t *testing.T) {
	ordered := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0"}
	for i := 1; i < len(ordered); i++ {
		a, _ := ParseVersion(ordered[i-1])
		b, _ := ParseVersion(ordered[i])
		if a.Compare(b) >= 0 {
			t.Fatalf("%s must precede %s", a, b)
		}
	}
	if _, e := ParseVersion("1.0.0+build"); e == nil {
		t.Fatal("build metadata alias accepted")
	}
	if _, e := ParseVersion("1.0.0-01"); e == nil {
		t.Fatal("leading-zero prerelease accepted")
	}
}

func TestFusionArtifactPublishResolveVerify(t *testing.T) {
	d := t.TempDir()
	s, _ := OpenLocal(d)
	blob := []byte("fused wasm artifact")
	text := []byte(`schema_version = 1
provides = ["logical.a.v1", "logical.b.v1"]
consumes = []
capabilities = ["storage.sqlite"]
[module]
id = "io.test/fused-state"
version = "1.0.0"
[execution]
abi = "pulp-linear-v1"
[[artifacts]]
target = "wasm32-wasip1+pulp-linear-v1"
digest = "` + Sum(blob).String() + `"
size = 19
`)
	m, e := ParseManifest(text)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Publish(context.Background(), text, map[Digest][]byte{m.Artifacts[0].Digest: blob}); e != nil {
		t.Fatal(e)
	}
	lock, e := Resolve(context.Background(), s, Request{Target: "wasm32-wasip1+pulp-linear-v1", Roots: []Dependency{{ID: m.ID, Requirement: "1.0.0"}}})
	if e != nil {
		t.Fatal(e)
	}
	if e = Verify(context.Background(), s, lock); e != nil {
		t.Fatal(e)
	}
	if m.FusionABI != "pulp-linear-v1" || len(m.Provides) != 2 {
		t.Fatalf("fusion metadata lost: %#v", m)
	}
}

func TestStoreRejectsSymlinkedModulePath(t *testing.T) {
	d := t.TempDir()
	s, e := OpenLocal(filepath.Join(d, "registry"))
	if e != nil {
		t.Fatal(e)
	}
	outside := filepath.Join(d, "outside")
	if e = os.Mkdir(outside, 0755); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(d, "registry", "v1", "modules", "io.test")
	if e = os.Symlink(outside, link); e != nil {
		t.Skipf("symlink unavailable: %v", e)
	}
	blob := []byte("x")
	text := moduleText("io.test/a", "1.0.0", "wasm", blob, "")
	m, _ := ParseManifest(text)
	if e = s.Publish(context.Background(), text, map[Digest][]byte{m.Artifacts[0].Digest: blob}); e == nil || !strings.Contains(e.Error(), "symlink") {
		t.Fatalf("symlink publish = %v", e)
	}
}
