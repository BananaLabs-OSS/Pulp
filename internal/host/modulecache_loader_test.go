package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/tetratelabs/wazero"
)

func TestLoadScopedCachedCompilesOnceAndKeepsScopedCellsIndependent(t *testing.T) {
	ctx := context.Background()
	wasmPath := filepath.Join(t.TempDir(), "minimal-pulp.wasm")
	if err := os.WriteFile(wasmPath, minimalPulpLifecycleWASM, 0o600); err != nil {
		t.Fatalf("write wasm: %v", err)
	}
	runtime := wazero.NewRuntime(ctx)
	defer runtime.Close(ctx)
	cache := NewModuleCache()
	defer cache.Close(ctx)
	cacheScope, err := cache.NewScope(runtime, ModuleRuntimeConfig{Fingerprint: "test"})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	spec := &manifest.CellSpec{Name: "stateful", WASMPath: wasmPath}
	firstScope, err := ext.NewScope("sessions", "blue", "stateful", "primary")
	if err != nil {
		t.Fatal(err)
	}
	secondScope, err := ext.NewScope("sessions", "green", "stateful", "primary")
	if err != nil {
		t.Fatal(err)
	}
	first, err := LoadScopedCached(ctx, spec, nil, nil, nil, firstScope, cacheScope)
	if err != nil {
		t.Fatalf("first LoadScopedCached: %v", err)
	}
	second, err := LoadScopedCached(ctx, spec, nil, nil, nil, secondScope, cacheScope)
	if err != nil {
		_ = first.Close(ctx)
		t.Fatalf("second LoadScopedCached: %v", err)
	}
	if got := cache.Stats().Compilations; got != 1 {
		t.Fatalf("compilations = %d, want 1", got)
	}
	if first.Scope().RoutingID() == second.Scope().RoutingID() {
		t.Fatalf("scoped cells share routing ID %q", first.Scope().RoutingID())
	}
	if err := first.Close(ctx); err != nil {
		t.Fatalf("close first: %v", err)
	}
	if err := second.Close(ctx); err != nil {
		t.Fatalf("close second: %v", err)
	}
	if runtime.Module("wasi_snapshot_preview1") == nil {
		t.Fatal("closing a cached cell closed the shared cache runtime")
	}
}

// minimalPulpLifecycleWASM has the three required Pulp lifecycle exports.
// It deliberately imports nothing: this proves the cache/instance path while
// capability-bearing cells continue to use the compatibility-aware registry
// binding documented by LoadScopedCached.
var minimalPulpLifecycleWASM = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f,
	0x03, 0x04, 0x03, 0x00, 0x00, 0x00,
	0x07, 0x29, 0x03,
	0x09, 0x70, 0x75, 0x6c, 0x70, 0x5f, 0x69, 0x6e, 0x69, 0x74, 0x00, 0x00,
	0x09, 0x70, 0x75, 0x6c, 0x70, 0x5f, 0x73, 0x74, 0x65, 0x70, 0x00, 0x01,
	0x0d, 0x70, 0x75, 0x6c, 0x70, 0x5f, 0x73, 0x68, 0x75, 0x74, 0x64, 0x6f, 0x77, 0x6e, 0x00, 0x02,
	0x0a, 0x10, 0x03,
	0x04, 0x00, 0x41, 0x00, 0x0b,
	0x04, 0x00, 0x41, 0x00, 0x0b,
	0x04, 0x00, 0x41, 0x00, 0x0b,
}
