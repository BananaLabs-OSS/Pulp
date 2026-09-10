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
)

const CacheEnv = "PULP_REGISTRY_CACHE"

// DefaultCacheRoot returns Pulp's automatically managed registry cache.
func DefaultCacheRoot() (string, error) {
	if root := os.Getenv(CacheEnv); root != "" {
		return filepath.Abs(root)
	}
	root, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate user cache: %w", err)
	}
	return filepath.Join(root, "pulp", "registry"), nil
}

// OpenDefaultLocal creates and opens the default local registry cache.
func OpenDefaultLocal() (*LocalStore, error) {
	root, err := DefaultCacheRoot()
	if err != nil {
		return nil, err
	}
	return OpenLocal(root)
}

// CachedStore resolves against the cache and its upstreams. Releases fetched
// from an upstream are verified and published atomically into Cache before use.
type CachedStore struct {
	Cache     *LocalStore
	Upstreams []Store
}

func (s *CachedStore) Versions(ctx context.Context, id ModuleID) ([]Version, error) {
	if s == nil || s.Cache == nil {
		return nil, fmt.Errorf("registry cache is required")
	}
	stores := append([]Store{s.Cache}, s.Upstreams...)
	versions := map[string]Version{}
	found := false
	var sourceErrors []error
	for _, store := range stores {
		values, err := store.Versions(ctx, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			sourceErrors = append(sourceErrors, err)
			continue
		}
		found = true
		for _, version := range values {
			versions[version.String()] = version
		}
	}
	if !found {
		if len(sourceErrors) != 0 {
			return nil, errors.Join(sourceErrors...)
		}
		return nil, ErrNotFound
	}
	out := make([]Version, 0, len(versions))
	for _, version := range versions {
		out = append(out, version)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Compare(out[j]) > 0 })
	return out, nil
}

func (s *CachedStore) Manifest(ctx context.Context, release ReleaseID) (Manifest, error) {
	if s == nil || s.Cache == nil {
		return Manifest{}, fmt.Errorf("registry cache is required")
	}
	manifest, err := s.Cache.Manifest(ctx, release)
	if err == nil || !errors.Is(err, ErrNotFound) {
		return manifest, err
	}
	for _, upstream := range s.Upstreams {
		manifest, err = upstream.Manifest(ctx, release)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return Manifest{}, err
		}
		if manifest.ID != release.ID || manifest.Version.Compare(release.Version) != 0 {
			return Manifest{}, fmt.Errorf("upstream returned the wrong release for %s", release.String())
		}
		blobs := make(map[Digest][]byte, len(manifest.Artifacts))
		for _, artifact := range manifest.Artifacts {
			reader, blobErr := upstream.Blob(ctx, artifact.Digest)
			if blobErr != nil {
				return Manifest{}, fmt.Errorf("fetch %s artifact %s: %w", release.String(), artifact.Target, blobErr)
			}
			data, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if readErr != nil {
				return Manifest{}, readErr
			}
			if closeErr != nil {
				return Manifest{}, closeErr
			}
			if Sum(data) != artifact.Digest || int64(len(data)) != artifact.Size {
				return Manifest{}, fmt.Errorf("upstream artifact verification failed for %s target %s", release.String(), artifact.Target)
			}
			blobs[artifact.Digest] = data
		}
		wire, canonicalErr := ManifestTOML(manifest)
		if canonicalErr != nil {
			return Manifest{}, canonicalErr
		}
		if err = s.Cache.Publish(ctx, wire, blobs); err != nil {
			return Manifest{}, fmt.Errorf("cache %s: %w", release.String(), err)
		}
		return s.Cache.Manifest(ctx, release)
	}
	return Manifest{}, ErrNotFound
}

func (s *CachedStore) Blob(ctx context.Context, digest Digest) (io.ReadCloser, error) {
	if s == nil || s.Cache == nil {
		return nil, fmt.Errorf("registry cache is required")
	}
	reader, err := s.Cache.Blob(ctx, digest)
	if err == nil || !errors.Is(err, ErrNotFound) {
		return reader, err
	}
	for _, upstream := range s.Upstreams {
		reader, err = upstream.Blob(ctx, digest)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if Sum(data) != digest {
			return nil, fmt.Errorf("upstream blob digest mismatch for %s", digest.String())
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	return nil, ErrNotFound
}
