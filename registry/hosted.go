package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

var (
	ErrUnauthorized = errors.New("registry authentication required")
	ErrForbidden    = errors.New("registry operation forbidden")
	ErrYanked       = errors.New("registry release is yanked")
	ErrRevoked      = errors.New("registry release is revoked")
)

// Publisher is the immutable write half of a registry storage implementation.
// LocalStore implements both Store and Publisher; hosted services may use an
// object-store-backed implementation without changing resolution semantics.
type Publisher interface {
	Publish(context.Context, []byte, map[Digest][]byte) error
}

type Principal struct {
	Subject string
	Groups  []string
}

type Operation string

const (
	OperationPublish Operation = "publish"
	OperationYank    Operation = "yank"
	OperationRevoke  Operation = "revoke"
)

// Authorizer owns namespace policy. Implementations can map a module such as
// acme/game to the acme namespace and consult an external IAM system.
type Authorizer interface {
	Authorize(context.Context, Principal, Operation, ModuleID) error
}

type AuthorizeFunc func(context.Context, Principal, Operation, ModuleID) error

func (f AuthorizeFunc) Authorize(ctx context.Context, p Principal, op Operation, id ModuleID) error {
	return f(ctx, p, op, id)
}

type ReleaseState string

const (
	ReleaseActive  ReleaseState = "active"
	ReleaseYanked  ReleaseState = "yanked"
	ReleaseRevoked ReleaseState = "revoked"
)

type ReleaseStatus struct {
	State  ReleaseState `json:"state"`
	Reason string       `json:"reason,omitempty"`
	Actor  string       `json:"actor,omitempty"`
}

// Metadata is deliberately separate from immutable package storage. Yanking,
// revocation, ownership and search indexes can evolve without rewriting signed
// manifests or content-addressed blobs.
type Metadata interface {
	Status(context.Context, ReleaseID) (ReleaseStatus, error)
	SetStatus(context.Context, ReleaseID, ReleaseStatus) error
	Record(context.Context, Manifest) error
	Search(context.Context, string, int) ([]SearchResult, error)
}

// BlobAccessPolicy optionally blocks direct digest downloads. It allows a
// hosted service to enforce revocation even when clients already know a blob
// URL. A shared blob remains available while any active or yanked release owns
// it; local caches remain immutable and are governed separately.
type BlobAccessPolicy interface {
	AllowBlob(context.Context, Digest) error
}

type SearchResult struct {
	ID            ModuleID `json:"id"`
	LatestVersion string   `json:"latest_version"`
	Yanked        bool     `json:"yanked,omitempty"`
}

// MemoryMetadata is useful for tests and single-process development servers.
// Production services should supply durable transactional metadata.
type MemoryMetadata struct {
	mu       sync.RWMutex
	status   map[string]ReleaseStatus
	versions map[ModuleID]map[string]Version
	blobs    map[Digest]map[string]struct{}
}

func NewMemoryMetadata() *MemoryMetadata {
	return &MemoryMetadata{status: map[string]ReleaseStatus{}, versions: map[ModuleID]map[string]Version{}, blobs: map[Digest]map[string]struct{}{}}
}

func (m *MemoryMetadata) Status(_ context.Context, r ReleaseID) (ReleaseStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.status[r.String()]
	if !ok {
		return ReleaseStatus{State: ReleaseActive}, nil
	}
	return s, nil
}

func (m *MemoryMetadata) SetStatus(_ context.Context, r ReleaseID, status ReleaseStatus) error {
	if status.State != ReleaseActive && status.State != ReleaseYanked && status.State != ReleaseRevoked {
		return fmt.Errorf("invalid release state %q", status.State)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status[r.String()] = status
	return nil
}

func (m *MemoryMetadata) Record(_ context.Context, manifest Manifest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.versions[manifest.ID] == nil {
		m.versions[manifest.ID] = map[string]Version{}
	}
	m.versions[manifest.ID][manifest.Version.String()] = manifest.Version
	for _, artifact := range manifest.Artifacts {
		if m.blobs[artifact.Digest] == nil {
			m.blobs[artifact.Digest] = map[string]struct{}{}
		}
		m.blobs[artifact.Digest][ReleaseID{ID: manifest.ID, Version: manifest.Version}.String()] = struct{}{}
	}
	return nil
}

func (m *MemoryMetadata) AllowBlob(_ context.Context, digest Digest) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	owners := m.blobs[digest]
	if len(owners) == 0 {
		return nil
	}
	for release := range owners {
		if m.status[release].State != ReleaseRevoked {
			return nil
		}
	}
	return ErrRevoked
}

func (m *MemoryMetadata) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query = strings.ToLower(strings.TrimSpace(query))
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]ModuleID, 0, len(m.versions))
	for id := range m.versions {
		if query == "" || strings.Contains(strings.ToLower(string(id)), query) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	results := make([]SearchResult, 0, min(limit, len(ids)))
	for _, id := range ids {
		versions := make([]Version, 0, len(m.versions[id]))
		for _, version := range m.versions[id] {
			status := m.status[ReleaseID{ID: id, Version: version}.String()]
			if status.State != ReleaseRevoked {
				versions = append(versions, version)
			}
		}
		if len(versions) == 0 {
			continue
		}
		sort.Slice(versions, func(i, j int) bool { return versions[i].Compare(versions[j]) > 0 })
		status := m.status[ReleaseID{ID: id, Version: versions[0]}.String()]
		results = append(results, SearchResult{ID: id, LatestVersion: versions[0].String(), Yanked: status.State == ReleaseYanked})
		if len(results) == limit {
			break
		}
	}
	return results, nil
}

// MirrorPolicy controls whether a read miss may consult a particular mirror.
// This is an access-policy hook, not an implicit trust decision: callers still
// verify manifests and blobs using the normal CachedStore path.
type MirrorPolicy func(context.Context, ModuleID, Store) bool

type HostedRegistry struct {
	Storage   Store
	Publisher Publisher
	Metadata  Metadata
	Authorize Authorizer
	Mirrors   []Store
	UseMirror MirrorPolicy
}

func (s *HostedRegistry) check() error {
	if s == nil || s.Storage == nil || s.Publisher == nil || s.Metadata == nil || s.Authorize == nil {
		return errors.New("hosted registry requires storage, publisher, metadata, and authorizer")
	}
	return nil
}

func (s *HostedRegistry) Publish(ctx context.Context, principal Principal, manifestBytes []byte, blobs map[Digest][]byte) error {
	if err := s.check(); err != nil {
		return err
	}
	manifest, err := ParseManifest(manifestBytes)
	if err != nil {
		return err
	}
	if err = s.Authorize.Authorize(ctx, principal, OperationPublish, manifest.ID); err != nil {
		return fmt.Errorf("%w: %v", ErrForbidden, err)
	}
	if err = s.Publisher.Publish(ctx, manifestBytes, blobs); err != nil {
		return err
	}
	return s.Metadata.Record(ctx, manifest)
}

func (s *HostedRegistry) SetReleaseStatus(ctx context.Context, principal Principal, release ReleaseID, status ReleaseStatus) error {
	if err := s.check(); err != nil {
		return err
	}
	op := OperationYank
	if status.State == ReleaseRevoked {
		op = OperationRevoke
	} else if status.State != ReleaseYanked && status.State != ReleaseActive {
		return fmt.Errorf("invalid release state %q", status.State)
	}
	if err := s.Authorize.Authorize(ctx, principal, op, release.ID); err != nil {
		return fmt.Errorf("%w: %v", ErrForbidden, err)
	}
	if _, err := s.Storage.Manifest(ctx, release); err != nil {
		return err
	}
	status.Actor = principal.Subject
	return s.Metadata.SetStatus(ctx, release, status)
}

func (s *HostedRegistry) Versions(ctx context.Context, id ModuleID) ([]Version, error) {
	versions, err := s.Storage.Versions(ctx, id)
	if errors.Is(err, ErrNotFound) {
		for _, mirror := range s.Mirrors {
			if s.UseMirror == nil || s.UseMirror(ctx, id, mirror) {
				if versions, err = mirror.Versions(ctx, id); err == nil {
					return versions, nil
				}
			}
		}
	}
	if err != nil {
		return nil, err
	}
	out := versions[:0]
	for _, version := range versions {
		status, statusErr := s.Metadata.Status(ctx, ReleaseID{ID: id, Version: version})
		if statusErr != nil {
			return nil, statusErr
		}
		if status.State == ReleaseActive || status.State == "" {
			out = append(out, version)
		}
	}
	return out, nil
}

func (s *HostedRegistry) Manifest(ctx context.Context, release ReleaseID) (Manifest, error) {
	status, err := s.Metadata.Status(ctx, release)
	if err != nil {
		return Manifest{}, err
	}
	if status.State == ReleaseRevoked {
		return Manifest{}, fmt.Errorf("%w: %s", ErrRevoked, status.Reason)
	}
	manifest, err := s.Storage.Manifest(ctx, release)
	if !errors.Is(err, ErrNotFound) {
		return manifest, err
	}
	for _, mirror := range s.Mirrors {
		if s.UseMirror == nil || s.UseMirror(ctx, release.ID, mirror) {
			if manifest, err = mirror.Manifest(ctx, release); err == nil {
				return manifest, nil
			}
		}
	}
	return Manifest{}, err
}

func (s *HostedRegistry) Blob(ctx context.Context, digest Digest) (io.ReadCloser, error) {
	if policy, ok := s.Metadata.(BlobAccessPolicy); ok {
		if err := policy.AllowBlob(ctx, digest); err != nil {
			return nil, err
		}
	}
	reader, err := s.Storage.Blob(ctx, digest)
	if !errors.Is(err, ErrNotFound) {
		return reader, err
	}
	for _, mirror := range s.Mirrors {
		if reader, err = mirror.Blob(ctx, digest); err == nil {
			return reader, nil
		}
	}
	return nil, err
}

func (s *HostedRegistry) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	return s.Metadata.Search(ctx, query, limit)
}
