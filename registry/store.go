package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var ErrNotFound = errors.New("registry entry not found")
var ErrConflict = errors.New("immutable release conflict")

type Store interface {
	Versions(context.Context, ModuleID) ([]Version, error)
	Manifest(context.Context, ReleaseID) (Manifest, error)
	Blob(context.Context, Digest) (io.ReadCloser, error)
}
type LocalStore struct{ root string }

func OpenLocal(root string) (*LocalStore, error) {
	a, e := filepath.Abs(root)
	if e != nil {
		return nil, e
	}
	if e = os.MkdirAll(filepath.Join(a, "v1", "modules"), 0755); e != nil {
		return nil, e
	}
	if e = os.MkdirAll(filepath.Join(a, "v1", "blobs", "sha256"), 0755); e != nil {
		return nil, e
	}
	real, e := filepath.EvalSymlinks(a)
	if e != nil {
		return nil, e
	}
	s := &LocalStore{real}
	if e = s.safePath(filepath.Join(real, "v1", "modules")); e != nil {
		return nil, e
	}
	if e = s.safePath(filepath.Join(real, "v1", "blobs", "sha256")); e != nil {
		return nil, e
	}
	return s, nil
}

// safePath rejects pre-existing symlink components. This prevents untrusted
// module IDs and a modified local index from redirecting reads or writes outside
// the opened registry root. Callers still need ordinary filesystem permissions
// to protect against concurrent hostile mutation.
func (s *LocalStore) safePath(path string) error {
	rel, e := filepath.Rel(s.root, path)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("registry path escapes root")
	}
	cur := s.root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, e := os.Lstat(cur)
		if errors.Is(e, os.ErrNotExist) {
			return nil
		}
		if e != nil {
			return e
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("registry path contains symlink: %s", cur)
		}
	}
	return nil
}
func (s *LocalStore) releasePath(r ReleaseID) string {
	return filepath.Join(s.root, "v1", "modules", string(r.ID), r.Version.String(), "manifest.json")
}
func (s *LocalStore) blobPath(d Digest) string {
	h := d.String()[7:]
	return filepath.Join(s.root, "v1", "blobs", "sha256", h[:2], h[2:])
}

// Publish atomically adds an immutable release and its content-addressed blobs.
func (s *LocalStore) Publish(ctx context.Context, manifestBytes []byte, blobs map[Digest][]byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m, e := ParseManifest(manifestBytes)
	if e != nil {
		return e
	}
	canonical, e := CanonicalManifest(m)
	if e != nil {
		return e
	}
	for _, a := range m.Artifacts {
		b, ok := blobs[a.Digest]
		if !ok {
			return fmt.Errorf("artifact %s: %w", a.Digest.String(), ErrNotFound)
		}
		if Sum(b) != a.Digest {
			return fmt.Errorf("artifact %s digest mismatch", a.Target)
		}
		if a.Size != int64(len(b)) {
			return fmt.Errorf("artifact %s size mismatch", a.Target)
		}
		if e = s.safePath(s.blobPath(a.Digest)); e != nil {
			return e
		}
		if e = writeImmutable(s.blobPath(a.Digest), b); e != nil {
			return e
		}
	}
	path := s.releasePath(ReleaseID{m.ID, m.Version})
	if e = s.safePath(path); e != nil {
		return e
	}
	return writeImmutable(path, canonical)
}
func writeImmutable(path string, data []byte) error {
	if old, e := os.ReadFile(path); e == nil {
		if bytes.Equal(old, data) {
			return nil
		}
		return fmt.Errorf("%w: %s", ErrConflict, path)
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if e := os.MkdirAll(filepath.Dir(path), 0755); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".publish-")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, e = f.Write(data); e == nil {
		e = f.Sync()
	}
	if ce := f.Close(); e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Link(tmp, path); e != nil {
		if errors.Is(e, os.ErrExist) {
			return writeImmutable(path, data)
		}
		return e
	}
	return nil
}
func (s *LocalStore) Versions(_ context.Context, id ModuleID) ([]Version, error) {
	if _, e := ParseModuleID(string(id)); e != nil {
		return nil, e
	}
	dir := filepath.Dir(filepath.Dir(s.releasePath(ReleaseID{id, Version{}})))
	if e := s.safePath(dir); e != nil {
		return nil, e
	}
	entries, e := os.ReadDir(dir)
	if errors.Is(e, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if e != nil {
		return nil, e
	}
	out := []Version{}
	for _, x := range entries {
		if !x.IsDir() {
			continue
		}
		if v, e := ParseVersion(x.Name()); e == nil {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Compare(out[j]) > 0 })
	return out, nil
}
func (s *LocalStore) Manifest(_ context.Context, r ReleaseID) (Manifest, error) {
	if _, e := ParseModuleID(string(r.ID)); e != nil {
		return Manifest{}, e
	}
	if _, e := ParseVersion(r.Version.String()); e != nil {
		return Manifest{}, e
	}
	path := s.releasePath(r)
	if e := s.safePath(path); e != nil {
		return Manifest{}, e
	}
	b, e := os.ReadFile(path)
	if errors.Is(e, os.ErrNotExist) {
		return Manifest{}, ErrNotFound
	}
	if e != nil {
		return Manifest{}, e
	}
	return parseCanonical(b)
}
func (s *LocalStore) Blob(_ context.Context, d Digest) (io.ReadCloser, error) {
	path := s.blobPath(d)
	if e := s.safePath(path); e != nil {
		return nil, e
	}
	f, e := os.Open(path)
	if errors.Is(e, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, e
}
