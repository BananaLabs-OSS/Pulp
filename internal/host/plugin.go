package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/BananaLabs-OSS/Pulp/internal/abi"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Plugin is a single loaded WASM module with the three required Pulp exports.
type Plugin struct {
	name    string
	runtime wazero.Runtime
	module  api.Module

	initFn     api.Function
	stepFn     api.Function
	shutdownFn api.Function

	log *slog.Logger
}

// Load reads a .wasm file from path, instantiates it, and resolves the three
// required exports. Returns an error if the module is missing any of them.
func Load(ctx context.Context, path string, logger *slog.Logger) (*Plugin, error) {
	wasmBytes, err := os.ReadFile(path)
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

	startFn := "_start"
	if _, ok := compiled.ExportedFunctions()["_initialize"]; ok {
		startFn = "_initialize"
	}

	cfg := wazero.NewModuleConfig().
		WithStdout(os.Stdout).
		WithStderr(os.Stderr).
		WithStartFunctions(startFn).
		WithName(path)

	mod, err := r.InstantiateModule(ctx, compiled, cfg)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("instantiate wasm: %w", err)
	}

	p := &Plugin{
		name:       path,
		runtime:    r,
		module:     mod,
		initFn:     mod.ExportedFunction("pulp_init"),
		stepFn:     mod.ExportedFunction("pulp_step"),
		shutdownFn: mod.ExportedFunction("pulp_shutdown"),
		log:        logger.With("plugin", path),
	}

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
// WASM linear memory; the plugin receives (ptr, len).
func (p *Plugin) Init(ctx context.Context, config []byte) error {
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
func (p *Plugin) Step(ctx context.Context, env abi.StepEnvelope) (outputHandle int32, err error) {
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

// Shutdown calls pulp_shutdown. Error if the plugin returns nonzero or traps.
func (p *Plugin) Shutdown(ctx context.Context) error {
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

// ProbeLastCall reads the plugin's probe_last_call export if present.
// Diagnostic only — returns ok=false if the plugin does not expose it.
func (p *Plugin) ProbeLastCall(ctx context.Context) (uint64, bool) {
	fn := p.module.ExportedFunction("probe_last_call")
	if fn == nil {
		return 0, false
	}
	results, err := fn.Call(ctx)
	if err != nil || len(results) == 0 {
		return 0, false
	}
	return results[0], true
}

// Close tears down the wazero runtime and releases all plugin resources.
func (p *Plugin) Close(ctx context.Context) error {
	if p.runtime == nil {
		return nil
	}
	err := p.runtime.Close(ctx)
	p.runtime = nil
	return err
}

// writeBytes allocates space in the plugin's linear memory via its exported
// malloc (if present) or a fallback scratch region, and writes the bytes.
// For v0.1 we use the plugin's exported malloc to keep memory layout honest.
func (p *Plugin) writeBytes(ctx context.Context, data []byte) (uint32, error) {
	if len(data) == 0 {
		return 0, nil
	}
	malloc := p.module.ExportedFunction("pulp_alloc")
	if malloc == nil {
		return 0, errors.New("plugin does not export pulp_alloc")
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

// free calls the plugin's pulp_free if exported. Silently skips if absent.
func (p *Plugin) free(ctx context.Context, ptr, size uint32) {
	if ptr == 0 {
		return
	}
	free := p.module.ExportedFunction("pulp_free")
	if free == nil {
		return
	}
	_, _ = free.Call(ctx, uint64(ptr), uint64(size))
}
