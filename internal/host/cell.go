package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/BananaLabs-OSS/Pulp/abi"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Resource-limit defaults applied to every cell unless the manifest
// overrides them. They bound a single cell so a buggy or hostile cell
// cannot OOM the host or wedge all co-located cells with an infinite loop.
const (
	// DefaultMaxMemoryPages caps a cell's WASM linear memory. wazero's bare
	// default is the wasm maximum (65536 pages = 4 GiB), which lets one cell
	// grow until the host process is OOM-killed. 4096 pages = 256 MiB is a
	// generous per-cell budget that still leaves headroom for co-located
	// cells; override per-cell via manifest `max_memory_pages`.
	DefaultMaxMemoryPages uint32 = 4096

	// DefaultCallTimeout bounds a single pulp_init / pulp_step / pulp_on_call
	// invocation. Combined with WithCloseOnContextDone, a cell that loops
	// forever inside a call traps and returns at the deadline instead of
	// pinning a core and the cell mutex; override via `call_timeout_ms`.
	DefaultCallTimeout = 30 * time.Second

	// reentrantCallGrace bounds how long Call waits for the cell mutex before
	// declaring a (likely re-entrant / loopback) busy condition. A legitimate
	// concurrent step releases the mutex in microseconds; a re-entrant
	// A->B->A loopback never will, so we fail fast instead of deadlocking.
	reentrantCallGrace = 250 * time.Millisecond
)

// Limits are the per-cell resource bounds applied at instantiation and on
// every WASM call. A nil *Limits (or a zero field) falls back to the
// Default* values above.
type Limits struct {
	// MaxMemoryPages caps linear memory in 64 KiB pages. 0 => default.
	MaxMemoryPages uint32
	// CallTimeout bounds a single WASM entry point. 0 => default.
	CallTimeout time.Duration
}

func (l *Limits) maxMemoryPages() uint32 {
	if l != nil && l.MaxMemoryPages != 0 {
		return l.MaxMemoryPages
	}
	return DefaultMaxMemoryPages
}

func (l *Limits) callTimeout() time.Duration {
	if l != nil && l.CallTimeout != 0 {
		return l.CallTimeout
	}
	return DefaultCallTimeout
}

// Cell is a single loaded WASM module with the three required Pulp exports.
type Cell struct {
	name    string
	runtime wazero.Runtime
	module  api.Module

	initFn     api.Function
	stepFn     api.Function
	shutdownFn api.Function

	// onCallFn is the optional pulp_on_call export. Cells that
	// declare `provides = [...]` in their manifest must export this;
	// cells that only consume do not need it. Resolved at Load time.
	onCallFn api.Function

	// mu serializes entry points that execute WASM code. wazero modules
	// are not goroutine-safe, and sibling calls can enter the cell
	// concurrently with its own step loop — the mutex prevents race
	// conditions and re-entrant trap corruption.
	mu sync.Mutex

	// callTimeout bounds a single WASM entry point (Init/Step/Call/
	// Shutdown). With WithCloseOnContextDone set on the runtime, exceeding
	// it traps the call instead of pinning a core + mu indefinitely.
	callTimeout time.Duration

	log *slog.Logger
}

// callContext derives a deadline-bounded context for a single WASM call.
// The returned cancel must be called once the call returns. Because the
// runtime is built WithCloseOnContextDone, a runaway call traps when this
// deadline elapses rather than blocking forever.
func (p *Cell) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if p.callTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, p.callTimeout)
}

// lockForCall acquires p.mu for a sibling Call without blocking forever.
// It returns true once the mutex is held. It returns false if the mutex is
// still held after reentrantCallGrace (or ctx is cancelled) — the signature
// of a re-entrant / loopback call that would otherwise deadlock the host.
//
// We poll TryLock rather than block on Lock so a true A->B->A loopback
// (same goroutine, mutex never released) fails fast instead of hanging,
// while brief legitimate contention from another cell's concurrent step
// (released in microseconds) still succeeds.
func (p *Cell) lockForCall(ctx context.Context) bool {
	if p.mu.TryLock() {
		return true
	}
	deadline := time.NewTimer(reentrantCallGrace)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Microsecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-tick.C:
			if p.mu.TryLock() {
				return true
			}
		}
	}
}

// Load reads the cell's WASM file, binds the host capabilities declared in
// its manifest, instantiates the module, and resolves the three required
// exports. Host capabilities are bound via the [Registry] before the cell
// module is instantiated so its imports resolve correctly.
//
// Passing a nil registry is valid — the cell gets only the WASI imports
// wazero provides by default and no Pulp host imports. Useful for tests.
//
// limits bounds the cell's memory; a nil *Limits applies the Default* values.
// WithMemoryLimitPages caps the cell so it cannot grow memory until it
// OOM-kills the host and every co-located cell.
//
// NOTE: we deliberately do NOT set wazero's WithCloseOnContextDone here. That
// flag closes the whole MODULE when a call's context is Done — fine for a
// one-shot run, but fatal for a long-lived REACTOR cell that we call
// repeatedly (Init→Step→…): the first per-call context's deferred cancel()
// tears the module down, so the next pulp_alloc traps with "module closed
// with exit_code(0)" — total cell death at first use. (Verified: enabling it
// regressed real-cell instantiation; the cell harness traps, plain instantiate
// works.) Bounding a runaway *wasm loop* therefore needs a supervisor that
// kills+restarts the cell out-of-band, not a per-call context close — deferred.
// The per-call callContext below still propagates host-side cancellation.
func Load(ctx context.Context, spec *manifest.CellSpec, registry *Registry, limits *Limits, logger *slog.Logger) (*Cell, error) {
	wasmBytes, err := os.ReadFile(spec.WASMPath)
	if err != nil {
		return nil, fmt.Errorf("read wasm: %w", err)
	}

	rtCfg := wazero.NewRuntimeConfig().
		WithMemoryLimitPages(limits.maxMemoryPages())
	r := wazero.NewRuntimeWithConfig(ctx, rtCfg)
	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	// Compile first so we can inspect exports and decide which start function
	// to invoke (command "_start" vs reactor "_initialize").
	compiled, err := r.CompileModule(ctx, wasmBytes)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("compile wasm: %w", err)
	}

	p := &Cell{
		name:        spec.Name,
		runtime:     r,
		callTimeout: limits.callTimeout(),
		log:         logger.With("cell", spec.Name),
	}

	// Bind host capabilities BEFORE instantiating the cell module so the
	// "pulp" host module exists when the cell's imports are resolved.
	if registry != nil {
		if err := registry.bind(ctx, r, spec, p); err != nil {
			r.Close(ctx)
			return nil, fmt.Errorf("bind capabilities: %w", err)
		}
	}

	startFn := "_start"
	if _, ok := compiled.ExportedFunctions()["_initialize"]; ok {
		startFn = "_initialize"
	}

	// WithSysWalltime / Nanotime / Nanosleep wire wazero's sys clocks
	// to the host OS. Without these the runtime returns a fixed
	// 2022-01-01 anchor for time.Now() inside cells — a trap that
	// silently corrupts anything involving real timestamps (DB rows,
	// TTLs, JWT exp checks).
	cfg := wazero.NewModuleConfig().
		WithStdout(os.Stdout).
		WithStderr(os.Stderr).
		WithStartFunctions(startFn).
		WithName(spec.Name).
		WithSysWalltime().
		WithSysNanotime().
		WithSysNanosleep()
	// Forward a narrow, curated subset of host env to the cell so
	// WASIP1 `os.Getenv` works for the values cells expect. We
	// don't pass everything (cells are sandboxed, and arbitrary env
	// leakage is a supply-chain hazard) — just the keys that the
	// current cell ecosystem reads. Expand carefully as needed.
	for _, key := range []string{"HTTP_PORT", "TZ"} {
		if v := os.Getenv(key); v != "" {
			cfg = cfg.WithEnv(key, v)
		}
	}

	mod, err := r.InstantiateModule(ctx, compiled, cfg)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("instantiate wasm: %w", err)
	}

	p.module = mod
	p.initFn = mod.ExportedFunction("pulp_init")
	p.stepFn = mod.ExportedFunction("pulp_step")
	p.shutdownFn = mod.ExportedFunction("pulp_shutdown")
	p.onCallFn = mod.ExportedFunction("pulp_on_call")

	var missing []string
	if p.initFn == nil {
		missing = append(missing, "pulp_init")
	}
	if p.stepFn == nil {
		missing = append(missing, "pulp_step")
	}
	if p.shutdownFn == nil {
		missing = append(missing, "pulp_shutdown")
	}
	if len(missing) > 0 {
		p.Close(ctx)
		return nil, fmt.Errorf("missing required exports: %v", missing)
	}

	return p, nil
}

// Init calls pulp_init with the given config bytes. Config is written into
// WASM linear memory; the cell receives (ptr, len).
func (p *Cell) Init(ctx context.Context, config []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.initFn == nil {
		return errors.New("cell is closed or not initialized")
	}

	callCtx, cancel := p.callContext(ctx)
	defer cancel()

	ptr, err := p.writeBytes(callCtx, config)
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	defer p.free(callCtx, ptr, uint32(len(config)))

	results, err := p.initFn.Call(callCtx, uint64(ptr), uint64(len(config)))
	if err != nil {
		return fmt.Errorf("pulp_init trap: %w", err)
	}
	if code := int32(results[0]); code != 0 {
		return fmt.Errorf("pulp_init returned %d", code)
	}
	p.log.Info("init complete")
	return nil
}

// Step encodes the envelope, writes it into WASM linear memory, and calls
// pulp_step. Returns the output handle (0 means no output).
func (p *Cell) Step(ctx context.Context, env abi.StepEnvelope) (outputHandle int32, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stepFn == nil {
		return 0, errors.New("cell is closed")
	}

	callCtx, cancel := p.callContext(ctx)
	defer cancel()

	input := env.Encode()
	ptr, err := p.writeBytes(callCtx, input)
	if err != nil {
		return 0, fmt.Errorf("write envelope: %w", err)
	}
	defer p.free(callCtx, ptr, uint32(len(input)))

	results, err := p.stepFn.Call(callCtx, uint64(ptr), uint64(len(input)))
	if err != nil {
		return 0, fmt.Errorf("pulp_step trap: %w", err)
	}
	return int32(results[0]), nil
}

// Call invokes the cell's pulp_on_call export. Used by the sibling
// call path: when cell B calls pulp_call on cell A, the host routes it
// here, serialized against A's own step loop by the module mutex.
//
// Re-entrant cycles are rejected, not deadlocked: if A's step calls into
// B and B calls back into A (A->B->A), or a cell targets itself, the
// re-entry lands here while the same goroutine already holds A's mutex. A
// plain Lock() would hang the host forever; instead Call acquires via
// lockForCall and returns ErrReentrantCall when the mutex stays held past
// reentrantCallGrace. Synchronous sibling-call cycles are therefore
// forbidden at runtime and surface as a clear error.
//
// The cell receives (funcName, args) and writes a msgpack-encoded
// response via pulp_alloc. Returns the raw response bytes (copied out
// of WASM memory) or an error on trap / nonzero return code / missing
// export.
//
// HasProvider returns true iff the cell exports pulp_on_call.
func (p *Cell) HasProvider() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.onCallFn != nil
}

// ErrReentrantCall is returned by Call when the target cell is already
// executing WASM and does not release its mutex within reentrantCallGrace.
// This is the A->B->A (or self-targeted) sibling loopback: the calling
// goroutine is already inside this cell's step/call holding p.mu, so a
// blocking Lock would deadlock permanently. We fail fast with this error so
// the cell author sees a clear "cycle" signal instead of a hung host.
var ErrReentrantCall = errors.New("re-entrant / loopback sibling call rejected (cell already executing)")

func (p *Cell) Call(ctx context.Context, funcName string, args []byte) ([]byte, error) {
	// Do NOT block indefinitely on p.mu: a sibling call cycle (A->B->A) or
	// a self-targeted call re-enters this method on the same goroutine that
	// already holds p.mu, which a plain Lock() would deadlock on forever.
	// TryLock with a short grace window distinguishes brief legitimate
	// contention (another cell's step, released in microseconds) from a
	// true loopback (never released) and fails fast on the latter.
	if !p.lockForCall(ctx) {
		return nil, ErrReentrantCall
	}
	defer p.mu.Unlock()

	if p.onCallFn == nil {
		return nil, fmt.Errorf("cell %q does not export pulp_on_call (or is closed)", p.name)
	}

	callCtx, cancel := p.callContext(ctx)
	defer cancel()

	nameBytes := []byte(funcName)
	namePtr, err := p.writeBytes(callCtx, nameBytes)
	if err != nil {
		return nil, fmt.Errorf("write name: %w", err)
	}
	defer p.free(callCtx, namePtr, uint32(len(nameBytes)))

	var argsPtr uint32
	if len(args) > 0 {
		argsPtr, err = p.writeBytes(callCtx, args)
		if err != nil {
			return nil, fmt.Errorf("write args: %w", err)
		}
		defer p.free(callCtx, argsPtr, uint32(len(args)))
	}

	// Allocate 8 bytes for (respPtr, respLen) out-params.
	outPtr, err := p.alloc(callCtx, 8)
	if err != nil {
		return nil, fmt.Errorf("alloc out-params: %w", err)
	}
	defer p.free(callCtx, outPtr, 8)

	results, err := p.onCallFn.Call(callCtx,
		uint64(namePtr), uint64(len(nameBytes)),
		uint64(argsPtr), uint64(len(args)),
		uint64(outPtr), uint64(outPtr+4),
	)
	if err != nil {
		return nil, fmt.Errorf("pulp_on_call trap: %w", err)
	}
	if code := uint32(results[0]); code != 0 {
		return nil, fmt.Errorf("pulp_on_call returned %d", code)
	}

	respPtr, ok := p.module.Memory().ReadUint32Le(outPtr)
	if !ok {
		return nil, errors.New("read respPtr failed")
	}
	respLen, ok := p.module.Memory().ReadUint32Le(outPtr + 4)
	if !ok {
		return nil, errors.New("read respLen failed")
	}
	if respLen == 0 {
		return nil, nil
	}
	resp, ok := p.module.Memory().Read(respPtr, respLen)
	if !ok {
		return nil, errors.New("read response bytes failed")
	}
	out := make([]byte, len(resp))
	copy(out, resp)
	// The cell's pulp_on_call allocated the response via pulp_alloc;
	// the cell is responsible for its own cleanup via pulp_free-like
	// semantics. We do NOT free it here because the cell might be
	// holding a reference. If the cell wants to pool, it will.
	return out, nil
}

// Shutdown calls pulp_shutdown. Error if the cell returns nonzero or traps.
// Serialized against Init, Step, and Call via the module mutex so wazero
// never sees concurrent calls into the same module.
func (p *Cell) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.shutdownFn == nil {
		// Close already tore the module down; nothing left to shut down.
		return nil
	}

	callCtx, cancel := p.callContext(ctx)
	defer cancel()

	results, err := p.shutdownFn.Call(callCtx)
	if err != nil {
		return fmt.Errorf("pulp_shutdown trap: %w", err)
	}
	if code := int32(results[0]); code != 0 {
		return fmt.Errorf("pulp_shutdown returned %d", code)
	}
	p.log.Info("shutdown complete")
	return nil
}

// ProbeLastCall reads the cell's probe_last_call export if present.
// Diagnostic only — returns ok=false if the cell does not expose it.
func (p *Cell) ProbeLastCall(ctx context.Context) (uint64, bool) {
	return p.probeUint64(ctx, "probe_last_call")
}

// ProbeConfigMarker reads the cell's probe_config_marker export if present.
// Diagnostic only — used by the integration test to verify the manifest
// [config] table round-tripped through MessagePack into the cell.
func (p *Cell) ProbeConfigMarker(ctx context.Context) (int64, bool) {
	v, ok := p.probeUint64(ctx, "probe_config_marker")
	return int64(v), ok
}

func (p *Cell) probeUint64(ctx context.Context, name string) (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.module == nil {
		return 0, false
	}
	fn := p.module.ExportedFunction(name)
	if fn == nil {
		return 0, false
	}
	results, err := fn.Call(ctx)
	if err != nil || len(results) == 0 {
		return 0, false
	}
	return results[0], true
}

// Close tears down the wazero runtime and releases all cell resources.
// Serialized against every other entry point via the module mutex so a
// concurrent Step / Call / Shutdown never races with runtime teardown.
// Safe to call more than once; later calls return nil.
func (p *Cell) Close(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.runtime == nil {
		return nil
	}
	err := p.runtime.Close(ctx)
	p.runtime = nil
	p.module = nil
	p.initFn = nil
	p.stepFn = nil
	p.shutdownFn = nil
	p.onCallFn = nil
	return err
}

// writeBytes allocates space in the cell's linear memory via its exported
// malloc (if present) or a fallback scratch region, and writes the bytes.
// For v0.1 we use the cell's exported malloc to keep memory layout honest.
func (p *Cell) writeBytes(ctx context.Context, data []byte) (uint32, error) {
	if len(data) == 0 {
		return 0, nil
	}
	malloc := p.module.ExportedFunction("pulp_alloc")
	if malloc == nil {
		return 0, errors.New("cell does not export pulp_alloc")
	}
	results, err := malloc.Call(ctx, uint64(len(data)))
	if err != nil {
		return 0, fmt.Errorf("pulp_alloc trap: %w", err)
	}
	ptr := uint32(results[0])
	if ptr == 0 {
		return 0, errors.New("pulp_alloc returned null")
	}
	if !p.module.Memory().Write(ptr, data) {
		return 0, fmt.Errorf("memory write out of range at %d", ptr)
	}
	return ptr, nil
}

// alloc calls the cell's pulp_alloc to reserve size bytes in WASM
// linear memory. Returns the allocated pointer.
func (p *Cell) alloc(ctx context.Context, size uint32) (uint32, error) {
	if size == 0 {
		return 0, nil
	}
	malloc := p.module.ExportedFunction("pulp_alloc")
	if malloc == nil {
		return 0, errors.New("cell does not export pulp_alloc")
	}
	results, err := malloc.Call(ctx, uint64(size))
	if err != nil {
		return 0, fmt.Errorf("pulp_alloc trap: %w", err)
	}
	ptr := uint32(results[0])
	if ptr == 0 {
		return 0, errors.New("pulp_alloc returned null")
	}
	return ptr, nil
}

// free calls the cell's pulp_free if exported. Silently skips if absent.
func (p *Cell) free(ctx context.Context, ptr, size uint32) {
	if ptr == 0 {
		return
	}
	free := p.module.ExportedFunction("pulp_free")
	if free == nil {
		return
	}
	_, _ = free.Call(ctx, uint64(ptr), uint64(size))
}
