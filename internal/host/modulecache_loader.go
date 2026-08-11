package host

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// LoadScopedCached constructs a scoped cell from a cached compiled module.
// Every call acquires a fresh ModuleCache lease and therefore gets a distinct
// WASM module instance (linear memory and globals are never shared); matching
// bytes in the same ModuleCacheScope compile only once.
//
// The supplied scope's runtime is shared by every cached cell. Consequently a
// caller must only reuse a ModuleCacheScope when its host-import bindings are
// compatible. In particular, a Registry binds the fixed "pulp" import module
// into the wazero Runtime, so applications whose extension bindings differ
// must receive different ModuleCacheScopes. The disk compilation cache used by
// LoadScoped still shares machine code across those isolated runtimes.
func LoadScopedCached(ctx context.Context, spec *manifest.CellSpec, registry *Registry, limits *Limits, logger *slog.Logger, scope ext.Scope, cacheScope *ModuleCacheScope) (*Cell, error) {
	if cacheScope == nil || cacheScope.runtime == nil {
		return nil, fmt.Errorf("cached cell loader: module cache scope is required")
	}
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("cell scope: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	wasmBytes, err := os.ReadFile(spec.WASMPath)
	if err != nil {
		return nil, fmt.Errorf("read wasm: %w", err)
	}
	if cacheScope.runtime.Module("wasi_snapshot_preview1") == nil {
		if _, err := wasi_snapshot_preview1.Instantiate(ctx, cacheScope.runtime); err != nil {
			return nil, fmt.Errorf("instantiate WASI: %w", err)
		}
	}

	p := &Cell{
		name:        spec.Name,
		scope:       scope,
		runtime:     cacheScope.runtime,
		callTimeout: limits.callTimeout(),
		log:         logger.With("cell", spec.Name),
	}
	if registry != nil {
		if err := registry.bind(ctx, cacheScope.runtime, spec, p); err != nil {
			return nil, fmt.Errorf("bind capabilities: %w", err)
		}
	}

	lease, err := cacheScope.Acquire(ctx, wasmBytes)
	if err != nil {
		return nil, err
	}
	startFn := "_start"
	if _, ok := lease.entry.compiled.ExportedFunctions()["_initialize"]; ok {
		startFn = "_initialize"
	}
	instance, err := lease.Instantiate(ctx, cachedCellModuleConfig(scope.RoutingID(), startFn))
	if err != nil {
		return nil, fmt.Errorf("instantiate cached wasm: %w", err)
	}
	p.module = instance.Module()
	p.closeModule = instance.Close
	if err := bindCellExports(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func cachedCellModuleConfig(name, startFn string) wazero.ModuleConfig {
	cfg := wazero.NewModuleConfig().
		WithStdin(bytes.NewReader(nil)).
		WithStdout(io.Discard).
		WithStderr(io.Discard).
		WithStartFunctions(startFn).
		WithName(name).
		WithSysWalltime().
		WithSysNanotime().
		WithSysNanosleep()
	for _, key := range []string{"HTTP_PORT", "TZ", "PROJX_PROJECT_ROOT", "PROJX_ACTIVE_PROJECT", "PROJX_HOST_EXE", "PROJX_SRC_DIR", "PROJX_WASM_OUT", "PROJX_GOWORK", "PROJX_HOST_OS", "PROJX_HOST_PID", "PROJX_RELAUNCH_SPEC", "PROJX_DESKTOP_EXE", "PROJX_XVFB_DIR", "PROJX_AI_KEY", "PROJX_AI_MODEL", "PROJX_AI_BASE_URL", "PROJX_SMART_CONTEXT", "PROJX_HOST_HOME", "PROJX_ROOT", "PROJX_AGENT", "PROJX_ALLOW_HOST"} {
		if value := os.Getenv(key); value != "" {
			cfg = cfg.WithEnv(key, value)
		}
	}
	return cfg
}

func bindCellExports(ctx context.Context, cell *Cell) error {
	cell.initFn = cell.module.ExportedFunction("pulp_init")
	cell.stepFn = cell.module.ExportedFunction("pulp_step")
	cell.shutdownFn = cell.module.ExportedFunction("pulp_shutdown")
	cell.initErrorPtrFn = cell.module.ExportedFunction("pulp_init_error_ptr")
	cell.initErrorLenFn = cell.module.ExportedFunction("pulp_init_error_len")
	cell.callErrorPtrFn = cell.module.ExportedFunction("pulp_on_call_error_ptr")
	cell.callErrorLenFn = cell.module.ExportedFunction("pulp_on_call_error_len")
	cell.onCallFn = cell.module.ExportedFunction("pulp_on_call")
	cell.postReturnFn = cell.module.ExportedFunction("pulp_post_return")

	var missing []string
	if cell.initFn == nil {
		missing = append(missing, "pulp_init")
	}
	if cell.stepFn == nil {
		missing = append(missing, "pulp_step")
	}
	if cell.shutdownFn == nil {
		missing = append(missing, "pulp_shutdown")
	}
	if len(missing) == 0 {
		return nil
	}
	if err := cell.Close(ctx); err != nil {
		return fmt.Errorf("missing required exports %v; close cached cell: %w", missing, err)
	}
	return fmt.Errorf("missing required exports: %v", missing)
}

// Keep api imported at compile time: cached module instances expose api.Module
// and the assertion protects the Cell assignment above from widening.
var _ api.Module = (*CachedModuleInstance)(nil).Module()
