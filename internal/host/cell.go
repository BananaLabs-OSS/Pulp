package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/BananaLabs-OSS/Pulp/abi"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

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

	log *slog.Logger
}

// Load reads the cell's WASM file, binds the host capabilities declared in
// its manifest, instantiates the module, and resolves the three required
// exports. Host capabilities are bound via the [Registry] before the cell
// module is instantiated so its imports resolve correctly.
//
// Passing a nil registry is valid — the cell gets only the WASI imports
// wazero provides by default and no Pulp host imports. Useful for tests.
func Load(ctx context.Context, spec *manifest.CellSpec, registry *Registry, logger *slog.Logger) (*Cell, error) {
	wasmBytes, err := os.ReadFile(spec.WASMPath)
	if err != nil {
		return nil, fmt.Errorf("read wasm: %w", err)
	}

	r := wazero.NewRuntime(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	// Compile first so we can inspect exports and decide which start function
	// to invoke (command "_start" vs reactor "_initialize").
	compiled, err := r.CompileModule(ctx, wasmBytes)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("compile wasm: %w", err)
	}

	p := &Cell{
		name:    spec.Name,
		runtime: r,
		log:     logger.With("cell", spec.Name),
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

	ptr, err := p.writeBytes(ctx, config)
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	defer p.free(ctx, ptr, uint32(len(config)))

	results, err := p.initFn.Call(ctx, uint64(ptr), uint64(len(config)))
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

	input := env.Encode()
	ptr, err := p.writeBytes(ctx, input)
	if err != nil {
		return 0, fmt.Errorf("write envelope: %w", err)
	}
	defer p.free(ctx, ptr, uint32(len(input)))

	results, err := p.stepFn.Call(ctx, uint64(ptr), uint64(len(input)))
	if err != nil {
		return 0, fmt.Errorf("pulp_step trap: %w", err)
	}
	return int32(results[0]), nil
}

// Call invokes the cell's pulp_on_call export. Used by the sibling
// call path: when cell B calls pulp_call on cell A, the host
// routes it here, serialized against A's own step loop by the module
// mutex.
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

func (p *Cell) Call(ctx context.Context, funcName string, args []byte) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.onCallFn == nil {
		return nil, fmt.Errorf("cell %q does not export pulp_on_call (or is closed)", p.name)
	}

	nameBytes := []byte(funcName)
	namePtr, err := p.writeBytes(ctx, nameBytes)
	if err != nil {
		return nil, fmt.Errorf("write name: %w", err)
	}
	defer p.free(ctx, namePtr, uint32(len(nameBytes)))

	var argsPtr uint32
	if len(args) > 0 {
		argsPtr, err = p.writeBytes(ctx, args)
		if err != nil {
			return nil, fmt.Errorf("write args: %w", err)
		}
		defer p.free(ctx, argsPtr, uint32(len(args)))
	}

	// Allocate 8 bytes for (respPtr, respLen) out-params.
	outPtr, err := p.alloc(ctx, 8)
	if err != nil {
		return nil, fmt.Errorf("alloc out-params: %w", err)
	}
	defer p.free(ctx, outPtr, 8)

	results, err := p.onCallFn.Call(ctx,
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
	results, err := p.shutdownFn.Call(ctx)
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
