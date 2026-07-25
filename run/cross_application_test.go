package run

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCrossApplicationCallAllowsExactDeclaredDependency(t *testing.T) {
	registry := newCrossApplicationRegistry()
	target := ApplicationIdentity{ApplicationID: "commerce", InstanceID: "blue"}
	var gotCell, gotProvider string
	var gotArgs []byte
	registerCrossApplicationFake(t, registry, target, func(_ context.Context, cell, provider string, args []byte) ([]byte, error) {
		gotCell, gotProvider, gotArgs = cell, provider, append([]byte(nil), args...)
		return []byte("ok"), nil
	})
	caller := testCrossApplicationCaller([]string{"commerce"}, "cart.apply")

	response, err := registry.call(context.Background(), caller, target, "cart", "cart.apply", []byte("request"))
	if err != nil {
		t.Fatalf("exact declared call: %v", err)
	}
	if string(response) != "ok" || gotCell != "cart" || gotProvider != "cart.apply" || string(gotArgs) != "request" {
		t.Fatalf("call = response=%q cell=%q provider=%q args=%q", response, gotCell, gotProvider, gotArgs)
	}
}

func TestCrossApplicationCallDeniesUndeclaredDependencyAndDoesNotFallback(t *testing.T) {
	registry := newCrossApplicationRegistry()
	target := ApplicationIdentity{ApplicationID: "commerce", InstanceID: "primary"}
	var invoked atomic.Int32
	registerCrossApplicationFake(t, registry, target, func(_ context.Context, _, _ string, _ []byte) ([]byte, error) {
		invoked.Add(1)
		return []byte("must not run"), nil
	})
	caller := crossApplicationCaller{
		application:  HostedApplication{Identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "blue"}, DependsOn: []string{"identity"}},
		cellAddress:  "lua-orchestrator",
		hostConsumes: []string{"cart.apply"},
	}

	_, err := registry.call(context.Background(), caller, target, "cart", "cart.apply", nil)
	if !errors.Is(err, errCrossApplicationDenied) {
		t.Fatalf("undeclared dependency error = %v, want denied", err)
	}
	if got := invoked.Load(); got != 0 {
		t.Fatalf("undeclared call invoked target %d times", got)
	}
	if _, err := registry.call(context.Background(), caller, ApplicationIdentity{ApplicationID: "commerce"}, "cart", "cart.apply", nil); !errors.Is(err, errCrossApplicationInvalidRoute) {
		t.Fatalf("missing exact instance error = %v, want invalid route", err)
	}
}

func TestCrossApplicationSplitLuaRequiresExactConsumedProvider(t *testing.T) {
	registry := newCrossApplicationRegistry()
	target := ApplicationIdentity{ApplicationID: "commerce", InstanceID: "primary"}
	var invoked atomic.Int32
	registerCrossApplicationFake(t, registry, target, func(_ context.Context, _, _ string, _ []byte) ([]byte, error) {
		invoked.Add(1)
		return []byte("ok"), nil
	})
	missingConsume := testCrossApplicationCaller([]string{"commerce"})
	if _, err := registry.call(context.Background(), missingConsume, target, "cart", "cart.apply", nil); !errors.Is(err, errCrossApplicationDenied) {
		t.Fatalf("split Lua missing consumes error = %v, want denied", err)
	}
	if invoked.Load() != 0 {
		t.Fatal("split Lua missing consumes reached target")
	}
	exactConsume := testCrossApplicationCaller([]string{"commerce"}, "cart.apply")
	if response, err := registry.call(context.Background(), exactConsume, target, "cart", "cart.apply", nil); err != nil || string(response) != "ok" {
		t.Fatalf("split Lua exact consumes call = %q, %v", response, err)
	}
	if invoked.Load() != 1 {
		t.Fatalf("exact consumed provider invoked %d times, want 1", invoked.Load())
	}
}

func TestCrossApplicationCallDoesNotReplayStaleTargetAfterReplacement(t *testing.T) {
	registry := newCrossApplicationRegistry()
	target := ApplicationIdentity{ApplicationID: "commerce", InstanceID: "primary"}
	caller := testCrossApplicationCaller([]string{"commerce"}, "cart.apply")
	var oldCalls, newCalls atomic.Int32
	registerCrossApplicationFake(t, registry, target, func(_ context.Context, _, _ string, _ []byte) ([]byte, error) {
		oldCalls.Add(1)
		return []byte("old"), nil
	})
	if response, err := registry.call(context.Background(), caller, target, "cart", "cart.apply", nil); err != nil || string(response) != "old" {
		t.Fatalf("first call = %q, %v", response, err)
	}
	registry.markUnavailable(target)
	if _, err := registry.call(context.Background(), caller, target, "cart", "cart.apply", nil); !errors.Is(err, errCrossApplicationUnavailable) {
		t.Fatalf("stale target call error = %v, want unavailable", err)
	}
	registerCrossApplicationFake(t, registry, target, func(_ context.Context, _, _ string, _ []byte) ([]byte, error) {
		newCalls.Add(1)
		return []byte("new"), nil
	})
	if response, err := registry.call(context.Background(), caller, target, "cart", "cart.apply", nil); err != nil || string(response) != "new" {
		t.Fatalf("replacement call = %q, %v", response, err)
	}
	if oldCalls.Load() != 1 || newCalls.Load() != 1 {
		t.Fatalf("calls old=%d new=%d, want exactly one each", oldCalls.Load(), newCalls.Load())
	}
}

func TestCrossApplicationCallRevocationJoinsInFlightCalls(t *testing.T) {
	registry := newCrossApplicationRegistry()
	target := ApplicationIdentity{ApplicationID: "commerce", InstanceID: "primary"}
	caller := testCrossApplicationCaller([]string{"commerce"}, "cart.apply")
	entered := make(chan struct{})
	release := make(chan struct{})
	registerCrossApplicationFake(t, registry, target, func(_ context.Context, _, _ string, _ []byte) ([]byte, error) {
		close(entered)
		<-release
		return []byte("ok"), nil
	})
	callDone := make(chan error, 1)
	go func() {
		_, err := registry.call(context.Background(), caller, target, "cart", "cart.apply", nil)
		callDone <- err
	}()
	<-entered
	stopped := make(chan struct{})
	go func() { registry.markUnavailable(target); close(stopped) }()
	deadline := time.Now().Add(time.Second)
	for {
		entry, err := registry.acquire(target)
		if errors.Is(err, errCrossApplicationUnavailable) {
			break
		}
		if err != nil {
			t.Fatalf("acquire after revoke begins = %v, want unavailable", err)
		}
		entry.calls.Done()
		if time.Now().After(deadline) {
			t.Fatal("revocation did not remove target")
		}
		runtime.Gosched()
	}
	select {
	case <-stopped:
		t.Fatal("target revoked before in-flight call completed")
	default:
	}
	close(release)
	if err := <-callDone; err != nil {
		t.Fatalf("in-flight call: %v", err)
	}
	<-stopped
}

func TestCrossApplicationRegistryConcurrentCallsAndRevocation(t *testing.T) {
	registry := newCrossApplicationRegistry()
	target := ApplicationIdentity{ApplicationID: "commerce", InstanceID: "primary"}
	caller := testCrossApplicationCaller([]string{"commerce"}, "cart.apply")
	registerCrossApplicationFake(t, registry, target, func(_ context.Context, _, _ string, _ []byte) ([]byte, error) { return nil, nil })
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = registry.call(context.Background(), caller, target, "cart", "cart.apply", nil)
		}()
	}
	registry.markUnavailable(target)
	wg.Wait()
}

func registerCrossApplicationFake(t *testing.T, registry *crossApplicationRegistry, identity ApplicationIdentity, invoke func(context.Context, string, string, []byte) ([]byte, error)) {
	t.Helper()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.entries[identity] != nil {
		t.Fatalf("target %s already registered", identity)
	}
	registry.entries[identity] = &crossApplicationEntry{active: true, invoke: invoke}
}

func testCrossApplicationCaller(dependsOn []string, hostConsumes ...string) crossApplicationCaller {
	return crossApplicationCaller{
		application: HostedApplication{
			Identity:  ApplicationIdentity{ApplicationID: "evolution", InstanceID: "primary"},
			DependsOn: append([]string(nil), dependsOn...),
		},
		cellAddress:  "lua-orchestrator",
		hostConsumes: append([]string(nil), hostConsumes...),
	}
}
