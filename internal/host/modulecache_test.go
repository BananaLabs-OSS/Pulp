package host

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tetratelabs/wazero"
)

// counterWASM has one mutable global and an exported increment function. Two
// instantiations from one CompiledModule must each start their own global at 0.
var counterWASM = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f,
	0x03, 0x02, 0x01, 0x00,
	0x06, 0x06, 0x01, 0x7f, 0x01, 0x41, 0x00, 0x0b,
	0x07, 0x07, 0x01, 0x03, 0x69, 0x6e, 0x63, 0x00, 0x00,
	0x0a, 0x0d, 0x01, 0x0b, 0x00, 0x23, 0x00, 0x41, 0x01, 0x6a, 0x24, 0x00, 0x23, 0x00, 0x0b,
}

func TestModuleCacheConcurrentAcquireCompilesOnce(t *testing.T) {
	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	defer runtime.Close(ctx)
	cache := NewModuleCache()
	scope, err := cache.NewScope(runtime, ModuleRuntimeConfig{Fingerprint: "default"})
	if err != nil {
		t.Fatal(err)
	}

	const callers = 32
	start := make(chan struct{})
	errCh := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			lease, err := scope.Acquire(ctx, counterWASM)
			if err == nil {
				err = lease.Close(ctx)
			}
			errCh <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := cache.Stats().Compilations; got != 1 {
		t.Fatalf("concurrent compilations = %d, want 1", got)
	}
}

func TestModuleCacheCompilesOnceAndInstancesAreIsolated(t *testing.T) {
	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	defer runtime.Close(ctx)

	cache := NewModuleCache()
	scope, err := cache.NewScope(runtime, ModuleRuntimeConfig{Fingerprint: "memory=64;interruptible=false"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := scope.Acquire(ctx, counterWASM)
	if err != nil {
		t.Fatal(err)
	}
	second, err := scope.Acquire(ctx, counterWASM)
	if err != nil {
		t.Fatal(err)
	}

	instanceA, err := first.Instantiate(ctx, wazero.NewModuleConfig().WithName("counter-a"))
	if err != nil {
		t.Fatal(err)
	}
	defer instanceA.Close(ctx)
	instanceB, err := second.Instantiate(ctx, wazero.NewModuleConfig().WithName("counter-b"))
	if err != nil {
		t.Fatal(err)
	}
	defer instanceB.Close(ctx)

	incA := instanceA.Module().ExportedFunction("inc")
	incB := instanceB.Module().ExportedFunction("inc")
	resultA, err := incA.Call(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resultB, err := incB.Call(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resultA[0] != 1 || resultB[0] != 1 {
		t.Fatalf("instances shared state: first results = %d, %d", resultA[0], resultB[0])
	}
	resultA, err = incA.Call(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resultA[0] != 2 {
		t.Fatalf("instance A state = %d, want 2", resultA[0])
	}
	resultB, err = incB.Call(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resultB[0] != 2 {
		t.Fatalf("instance B state = %d, want 2", resultB[0])
	}

	stats := cache.Stats()
	if stats.Compilations != 1 || stats.Entries != 1 {
		t.Fatalf("cache stats = %+v, want one compilation and entry", stats)
	}
}

func TestModuleCacheEvictionWaitsForInstanceClose(t *testing.T) {
	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	defer runtime.Close(ctx)
	cache := NewModuleCache()
	scope, err := cache.NewScope(runtime, ModuleRuntimeConfig{Fingerprint: "default"})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := scope.Acquire(ctx, counterWASM)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := lease.Instantiate(ctx, wazero.NewModuleConfig().WithName("live"))
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Evict(ctx, counterWASM); err != nil {
		t.Fatal(err)
	}
	if got, err := instance.Module().ExportedFunction("inc").Call(ctx); err != nil || got[0] != 1 {
		t.Fatalf("eviction disrupted live instance: results=%v err=%v", got, err)
	}
	if err := instance.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := scope.Acquire(ctx, counterWASM); err != nil {
		t.Fatalf("acquire after eviction: %v", err)
	}
	if got := cache.Stats().Compilations; got != 2 {
		t.Fatalf("compilations after eviction = %d, want 2", got)
	}
}

func TestModuleCacheCloseRejectsNewWorkAndLetsLiveInstanceFinish(t *testing.T) {
	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	defer runtime.Close(ctx)
	cache := NewModuleCache()
	scope, err := cache.NewScope(runtime, ModuleRuntimeConfig{Fingerprint: "default"})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := scope.Acquire(ctx, counterWASM)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := lease.Instantiate(ctx, wazero.NewModuleConfig().WithName("live"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := scope.Acquire(ctx, counterWASM); !errors.Is(err, ErrModuleCacheClosed) {
		t.Fatalf("Acquire after Close error = %v, want ErrModuleCacheClosed", err)
	}
	if got, err := instance.Module().ExportedFunction("inc").Call(ctx); err != nil || got[0] != 1 {
		t.Fatalf("close disrupted live instance: results=%v err=%v", got, err)
	}
	if err := instance.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
