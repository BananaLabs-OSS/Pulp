package ext

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Scope identifies one live placement of a cell in a Pulp application.
//
// Package bytes may be shared by many applications, but mutable host-side
// resources must never be shared implicitly. Scope is deliberately immutable:
// construct it with NewScope and use the accessor methods when an extension
// needs to select a resource namespace.
type Scope struct {
	applicationID         string
	applicationInstanceID string
	cellID                string
	cellInstanceID        string
}

// ScopedCell is an optional extension to Cell. New hosts implement it to give
// extensions an application-aware ownership namespace; existing Cell values
// continue to work through ScopeOf's legacy fallback.
type ScopedCell interface {
	Cell
	Scope() Scope
}

// NewScope creates a validated, immutable application/cell placement.
func NewScope(applicationID, applicationInstanceID, cellID, cellInstanceID string) (Scope, error) {
	scope := Scope{
		applicationID:         applicationID,
		applicationInstanceID: applicationInstanceID,
		cellID:                cellID,
		cellInstanceID:        cellInstanceID,
	}
	if err := scope.Validate(); err != nil {
		return Scope{}, err
	}
	return scope, nil
}

// LegacyScope supplies the stable default namespace for extensions that only
// know a cell name. It keeps the existing registration and setup API valid
// while callers incrementally adopt explicit application scopes.
func LegacyScope(cellID string) Scope {
	if strings.TrimSpace(cellID) == "" {
		cellID = "default"
	}
	return Scope{
		applicationID:         "legacy",
		applicationInstanceID: "default",
		cellID:                cellID,
		cellInstanceID:        "default",
	}
}

// ScopeOf returns the scope of cell. A legacy Cell that does not implement
// ScopedCell receives the same stable, per-cell legacy namespace used before
// application scoping existed.
func ScopeOf(cell Cell) Scope {
	scope, err := ValidatedScopeOf(cell)
	if err == nil {
		return scope
	}
	if cell == nil {
		return LegacyScope("default")
	}
	return LegacyScope(cell.Name())
}

// ValidatedScopeOf is the strict form of ScopeOf. Hosts should use it while
// constructing a scoped cell; extensions can use ScopeOf to retain backwards
// compatibility with old host implementations.
func ValidatedScopeOf(cell Cell) (Scope, error) {
	if cell == nil {
		return Scope{}, errors.New("ext: nil cell has no scope")
	}
	if scoped, ok := cell.(ScopedCell); ok {
		scope := scoped.Scope()
		if err := scope.Validate(); err != nil {
			return Scope{}, fmt.Errorf("ext: scoped cell: %w", err)
		}
		return scope, nil
	}
	return LegacyScope(cell.Name()), nil
}

// ApplicationID is the stable application definition identifier.
func (s Scope) ApplicationID() string { return s.applicationID }

// ApplicationInstanceID identifies one running instance of an application.
func (s Scope) ApplicationInstanceID() string { return s.applicationInstanceID }

// CellID is the cell definition identifier within the application.
func (s Scope) CellID() string { return s.cellID }

// CellInstanceID identifies one running instance of a cell.
func (s Scope) CellInstanceID() string { return s.cellInstanceID }

// IsLegacy reports whether the scope was synthesized for an older host/cell
// API. Legacy scopes retain name-based event routing so existing extensions
// and single-application deployments keep their exact target format.
func (s Scope) IsLegacy() bool {
	return s.applicationID == "legacy" && s.applicationInstanceID == "default" && s.cellInstanceID == "default"
}

// RoutingID is the unique event-routing target for this cell placement. New
// multi-application hosts use it as StepEvent.CellID. It is deliberately
// distinct from CellID because a cell name alone is ambiguous across
// applications and instances.
func (s Scope) RoutingID() string {
	parts := []string{
		s.applicationID,
		s.applicationInstanceID,
		s.cellID,
		s.cellInstanceID,
	}
	var b strings.Builder
	b.WriteString("pulp-scope/v1/")
	for i, part := range parts {
		if i > 0 {
			b.WriteByte('|')
		}
		fmt.Fprintf(&b, "%d:%s", len(part), part)
	}
	return b.String()
}

// CellIDOf returns the StepEvent.CellID routing target for cell. Legacy cells
// retain their historical name target. Scoped cells use Scope.RoutingID so two
// equal cell names in different applications never receive each other's event.
func CellIDOf(cell Cell) string {
	if cell == nil {
		return ""
	}
	if _, ok := cell.(ScopedCell); !ok {
		return cell.Name()
	}
	scope := ScopeOf(cell)
	if scope.IsLegacy() {
		return cell.Name()
	}
	return scope.RoutingID()
}

// Validate reports whether s can safely namespace mutable host resources.
func (s Scope) Validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"application ID", s.applicationID},
		{"application instance ID", s.applicationInstanceID},
		{"cell ID", s.cellID},
		{"cell instance ID", s.cellInstanceID},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("ext: scope %s is required", field.name)
		}
		if strings.ContainsRune(field.value, '\x00') {
			return fmt.Errorf("ext: scope %s contains NUL", field.name)
		}
	}
	return nil
}

// ResourceKey uniquely names one mutable host resource. It is comparable, so
// it can be used directly as a map key without lossy string concatenation.
type ResourceKey struct {
	scope        Scope
	resourceType string
	resourceID   string
}

// ResourceKey returns the namespaced key for a host resource such as an HTTP
// listener, a storage handle, or an extension-owned session. resourceType is
// the extension-defined category and resourceID is its local name.
func (s Scope) ResourceKey(resourceType, resourceID string) (ResourceKey, error) {
	if err := s.Validate(); err != nil {
		return ResourceKey{}, err
	}
	if strings.TrimSpace(resourceType) == "" {
		return ResourceKey{}, errors.New("ext: resource type is required")
	}
	if strings.TrimSpace(resourceID) == "" {
		return ResourceKey{}, errors.New("ext: resource ID is required")
	}
	if strings.ContainsRune(resourceType, '\x00') || strings.ContainsRune(resourceID, '\x00') {
		return ResourceKey{}, errors.New("ext: resource key contains NUL")
	}
	return ResourceKey{scope: s, resourceType: resourceType, resourceID: resourceID}, nil
}

// Scope returns the immutable placement that owns this resource.
func (k ResourceKey) Scope() Scope { return k.scope }

// ResourceType returns the extension-defined resource category.
func (k ResourceKey) ResourceType() string { return k.resourceType }

// ResourceID returns the extension-local resource name.
func (k ResourceKey) ResourceID() string { return k.resourceID }

// String returns an injective, diagnostic representation. Length prefixes
// ensure delimiters inside identifiers cannot cause two keys to collide.
func (k ResourceKey) String() string {
	parts := []string{
		k.scope.applicationID,
		k.scope.applicationInstanceID,
		k.scope.cellID,
		k.scope.cellInstanceID,
		k.resourceType,
		k.resourceID,
	}
	var b strings.Builder
	for i, part := range parts {
		if i > 0 {
			b.WriteByte('|')
		}
		fmt.Fprintf(&b, "%d:%s", len(part), part)
	}
	return b.String()
}

// ScopedFactory creates and retains one resource per ResourceKey. It gives an
// extension a small, concurrency-safe ownership boundary: equal keys share an
// instance; any application, application-instance, cell, or cell-instance
// difference creates a different instance.
type ScopedFactory[T any] struct {
	mu        sync.Mutex
	resources map[ResourceKey]T
	new       func(ResourceKey) (T, error)
}

// NewScopedFactory creates a factory for a single extension resource type.
// new is called at most once for each key.
func NewScopedFactory[T any](new func(ResourceKey) (T, error)) *ScopedFactory[T] {
	return &ScopedFactory[T]{
		resources: make(map[ResourceKey]T),
		new:       new,
	}
}

// GetOrCreate returns the resource owned by key. The first caller creates it;
// concurrent callers for the same key receive the same retained resource.
func (f *ScopedFactory[T]) GetOrCreate(key ResourceKey) (resource T, created bool, err error) {
	var zero T
	if err := key.scope.Validate(); err != nil {
		return zero, false, err
	}
	if strings.TrimSpace(key.resourceType) == "" || strings.TrimSpace(key.resourceID) == "" {
		return zero, false, errors.New("ext: invalid resource key")
	}
	if f == nil || f.new == nil {
		return zero, false, errors.New("ext: scoped factory is not configured")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if resource, ok := f.resources[key]; ok {
		return resource, false, nil
	}
	resource, err = f.new(key)
	if err != nil {
		return zero, false, err
	}
	f.resources[key] = resource
	return resource, true, nil
}

// Count reports how many independently-owned resources the factory retains.
func (f *ScopedFactory[T]) Count() int {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.resources)
}
