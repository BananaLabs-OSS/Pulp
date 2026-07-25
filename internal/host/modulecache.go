package host

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// ModuleRuntimeConfig identifies the wazero configuration used by a scope.
//
// A compiled wazero module is tied to its Runtime, so code can only be reused
// in-memory by cells that share a Runtime. Fingerprint makes that constraint
// explicit at the call site: include every runtime option that changes the
// generated code or its safety envelope (for example memory limit, core
// features, and interruptibility). The cache still keeps scopes separate, even
// if their fingerprints match.
type ModuleRuntimeConfig struct {
	Fingerprint string
}

// ModuleCacheScope is one runtime's application-independent compilation
// domain. An application gets distinct module instances from a shared scope;
// its linear memory, globals, imported state, and lifecycle remain private to
// that instance.
//
// Do not share a scope between applications whose host-extension bindings
// differ. A wazero Runtime owns its imported modules as well as compiled code.
// The long-lived wazero disk compilation cache can still share machine code
// between such isolated runtimes.
type ModuleCacheScope struct {
	cache       *ModuleCache
	runtime     wazero.Runtime
	fingerprint string
	id          uint64
}

// ModuleCache stores compiled modules. It never stores instantiated modules:
// Acquire followed by Instantiate always creates a fresh WASM instance.
//
// It is safe for concurrent use. Eviction and Close defer closing a compiled
// module until every acquired instance has been closed, preventing a concurrent
// instantiate or active cell from observing freed compiled code.
type ModuleCache struct {
	mu      sync.Mutex
	entries map[moduleCacheKey]*moduleCacheEntry
	closed  bool
	nextID  atomic.Uint64
	stats   moduleCacheStats
}

type moduleCacheKey struct {
	scopeID     uint64
	digest      [sha256.Size]byte
	fingerprint string
}

type moduleCacheEntry struct {
	ready    chan struct{}
	compiled wazero.CompiledModule
	err      error
	refs     uint64
	evicted  bool
}

type moduleCacheStats struct {
	compilations atomic.Uint64
}

// ModuleCacheStats is a point-in-time cache snapshot. Entries counts modules
// currently retained by the cache, while Compilations counts actual calls to
// wazero CompileModule since the cache was created.
type ModuleCacheStats struct {
	Entries      int
	Compilations uint64
	Closed       bool
}

var (
	// ErrModuleCacheClosed means the owner has begun closing the cache. It is
	// intentionally distinct from an instantiation failure so a supervisor can
	// create a replacement host/cache rather than retry a dead one.
	ErrModuleCacheClosed = errors.New("wasm module cache is closed")
	// ErrModuleCacheEvicted means an entry was evicted while it was compiling
	// or before it could be leased.
	ErrModuleCacheEvicted = errors.New("wasm module cache entry was evicted")
)

// NewModuleCache returns an empty cache. The owner normally creates one per
// Pulp host process and creates a scope for every compatible host Runtime.
func NewModuleCache() *ModuleCache {
	return &ModuleCache{entries: make(map[moduleCacheKey]*moduleCacheEntry)}
}

var sharedModuleCache = NewModuleCache()

// SharedModuleCache is the process-level cache for hosts that choose to share
// a Runtime. It is not closed by cells; the host process owns its lifecycle.
func SharedModuleCache() *ModuleCache { return sharedModuleCache }

// NewScope creates a compilation scope for runtime. The fingerprint is
// required to prevent an accidental mix of modules compiled under materially
// different runtime configurations.
func (c *ModuleCache) NewScope(runtime wazero.Runtime, config ModuleRuntimeConfig) (*ModuleCacheScope, error) {
	if c == nil {
		return nil, errors.New("nil wasm module cache")
	}
	if runtime == nil {
		return nil, errors.New("nil wazero runtime")
	}
	if config.Fingerprint == "" {
		return nil, errors.New("empty wasm runtime configuration fingerprint")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrModuleCacheClosed
	}
	return &ModuleCacheScope{
		cache:       c,
		runtime:     runtime,
		fingerprint: config.Fingerprint,
		id:          c.nextID.Add(1),
	}, nil
}

// Acquire compiles wasm at most once for this scope, digest, and runtime
// configuration. The returned lease owns one isolated instance after
// Instantiate; close that instance to release the lease.
func (s *ModuleCacheScope) Acquire(ctx context.Context, wasm []byte) (*CachedModule, error) {
	if s == nil || s.cache == nil || s.runtime == nil {
		return nil, errors.New("nil wasm module cache scope")
	}
	if len(wasm) == 0 {
		return nil, errors.New("empty wasm module")
	}
	key := moduleCacheKey{
		scopeID:     s.id,
		digest:      sha256.Sum256(wasm),
		fingerprint: s.fingerprint,
	}
	return s.cache.acquire(ctx, s, key, wasm)
}

func (c *ModuleCache) acquire(ctx context.Context, scope *ModuleCacheScope, key moduleCacheKey, wasm []byte) (*CachedModule, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrModuleCacheClosed
	}
	entry := c.entries[key]
	if entry == nil {
		entry = &moduleCacheEntry{ready: make(chan struct{})}
		c.entries[key] = entry
		c.mu.Unlock()

		compiled, err := scope.runtime.CompileModule(ctx, wasm)
		c.stats.compilations.Add(1)

		var closeNow wazero.CompiledModule
		c.mu.Lock()
		entry.compiled, entry.err = compiled, err
		close(entry.ready)
		if err != nil {
			delete(c.entries, key)
		} else if c.closed || entry.evicted {
			delete(c.entries, key)
			closeNow = entry.compiled
		}
		c.mu.Unlock()
		if closeNow != nil {
			_ = closeNow.Close(context.Background())
		}
	} else {
		ready := entry.ready
		c.mu.Unlock()
		select {
		case <-ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrModuleCacheClosed
	}
	if entry.err != nil {
		return nil, fmt.Errorf("compile wasm: %w", entry.err)
	}
	if entry.evicted {
		return nil, ErrModuleCacheEvicted
	}
	// The entry can only be removed while refs == 0. We increment while holding
	// the cache lock, so Evict/Close cannot close its compiled module first.
	entry.refs++
	return &CachedModule{cache: c, key: key, entry: entry, scope: scope}, nil
}

// CachedModule is a single-use lease on compiled code. Instantiate creates one
// new module instance; it never shares guest state with another lease.
type CachedModule struct {
	cache *ModuleCache
	key   moduleCacheKey
	entry *moduleCacheEntry
	scope *ModuleCacheScope

	mu           sync.Mutex
	instantiated bool
	released     bool
}

// Instantiate starts a fresh module with config. Close the returned instance
// when its cell/application stops; that closes the module and releases this
// cached-module lease atomically with respect to eviction.
func (m *CachedModule) Instantiate(ctx context.Context, config wazero.ModuleConfig) (*CachedModuleInstance, error) {
	if m == nil {
		return nil, errors.New("nil cached wasm module")
	}
	m.mu.Lock()
	if m.released {
		m.mu.Unlock()
		return nil, errors.New("cached wasm module is released")
	}
	if m.instantiated {
		m.mu.Unlock()
		return nil, errors.New("cached wasm module lease already instantiated")
	}
	m.instantiated = true
	m.mu.Unlock()

	module, err := m.scope.runtime.InstantiateModule(ctx, m.entry.compiled, config)
	if err != nil {
		_ = m.release(context.Background())
		return nil, fmt.Errorf("instantiate wasm: %w", err)
	}
	return &CachedModuleInstance{module: module, lease: m}, nil
}

// Close releases an unused lease. Once Instantiate succeeds, the returned
// CachedModuleInstance exclusively owns the lease and must be closed instead.
func (m *CachedModule) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.instantiated && !m.released {
		m.mu.Unlock()
		return errors.New("cached wasm module was instantiated; close its instance")
	}
	m.mu.Unlock()
	return m.release(ctx)
}

func (m *CachedModule) release(ctx context.Context) error {
	m.mu.Lock()
	if m.released {
		m.mu.Unlock()
		return nil
	}
	m.released = true
	m.mu.Unlock()
	return m.cache.release(ctx, m.key, m.entry)
}

// CachedModuleInstance is one isolated WASM instantiation made from a cached
// compiled module. Module exposes its own linear memory and globals.
type CachedModuleInstance struct {
	module api.Module
	lease  *CachedModule
	once   sync.Once
	err    error
}

// Module returns the isolated wazero module instance.
func (i *CachedModuleInstance) Module() api.Module {
	if i == nil {
		return nil
	}
	return i.module
}

// Close stops this instance and then releases its compiled-module lease. It is
// idempotent and safe to call concurrently.
func (i *CachedModuleInstance) Close(ctx context.Context) error {
	if i == nil {
		return nil
	}
	i.once.Do(func() {
		var moduleErr error
		if i.module != nil {
			moduleErr = i.module.Close(ctx)
		}
		i.err = errors.Join(moduleErr, i.lease.release(ctx))
	})
	return i.err
}

// Evict retires the compiled code for wasm in this scope. Existing instances
// keep running; the compiled module is closed only after their leases close.
// A later Acquire recompiles the bytes into a new cache entry.
func (s *ModuleCacheScope) Evict(ctx context.Context, wasm []byte) error {
	if s == nil || s.cache == nil {
		return errors.New("nil wasm module cache scope")
	}
	key := moduleCacheKey{
		scopeID:     s.id,
		digest:      sha256.Sum256(wasm),
		fingerprint: s.fingerprint,
	}
	return s.cache.evict(ctx, key)
}

func (c *ModuleCache) evict(ctx context.Context, key moduleCacheKey) error {
	c.mu.Lock()
	entry := c.entries[key]
	if entry == nil {
		c.mu.Unlock()
		return nil
	}
	entry.evicted = true
	delete(c.entries, key)
	var closeNow wazero.CompiledModule
	if entry.refs == 0 && entry.compiled != nil {
		closeNow = entry.compiled
	}
	c.mu.Unlock()
	if closeNow != nil {
		return closeNow.Close(ctx)
	}
	return nil
}

func (c *ModuleCache) release(ctx context.Context, key moduleCacheKey, entry *moduleCacheEntry) error {
	c.mu.Lock()
	if entry.refs == 0 {
		c.mu.Unlock()
		return errors.New("cached wasm module lease released twice")
	}
	entry.refs--
	var closeNow wazero.CompiledModule
	if entry.refs == 0 && (entry.evicted || c.closed) {
		closeNow = entry.compiled
	}
	c.mu.Unlock()
	if closeNow != nil {
		return closeNow.Close(ctx)
	}
	return nil
}

// Stats returns a race-safe point-in-time snapshot.
func (c *ModuleCache) Stats() ModuleCacheStats {
	if c == nil {
		return ModuleCacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return ModuleCacheStats{
		Entries:      len(c.entries),
		Compilations: c.stats.compilations.Load(),
		Closed:       c.closed,
	}
}

// Close prevents new scopes and leases, evicts every entry, and closes idle
// compiled modules. Busy entries close when their last isolated instance does.
func (c *ModuleCache) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	var closers []wazero.CompiledModule
	for key, entry := range c.entries {
		entry.evicted = true
		delete(c.entries, key)
		if entry.refs == 0 && entry.compiled != nil {
			closers = append(closers, entry.compiled)
		}
	}
	c.mu.Unlock()

	var errs []error
	for _, compiled := range closers {
		if err := compiled.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
