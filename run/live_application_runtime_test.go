package run

import (
	"context"
	"sync"
	"testing"
	"time"
)

type routedRuntime struct {
	id               string
	stateful         bool
	started, stopped bool
	health           error
}
type snapshotRuntime struct {
	routedRuntime
	snapshot, restored any
}

func (r *snapshotRuntime) SnapshotLive(context.Context, []string) (any, error) {
	return r.snapshot, nil
}
func (r *snapshotRuntime) RestoreLive(_ context.Context, s any, _ []string) error {
	r.restored = s
	return nil
}

func (r *routedRuntime) Identity() ApplicationIdentity {
	return ApplicationIdentity{ApplicationID: r.id, InstanceID: "default"}
}
func (r *routedRuntime) Start(context.Context) error      { r.started = true; return nil }
func (r *routedRuntime) Shutdown(context.Context) error   { r.stopped = true; return nil }
func (r *routedRuntime) LiveStateful() bool               { return r.stateful }
func (r *routedRuntime) LiveHealth(context.Context) error { return r.health }

func TestAtomicRuntimeRouterQuiescesAndDrainsBeforeSwap(t *testing.T) {
	old := &routedRuntime{id: "old"}
	next := &routedRuntime{id: "next"}
	router := NewAtomicRuntimeRouter(old)
	lease, e := router.Acquire()
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- router.Quiesce(context.Background()) }()
	time.Sleep(time.Millisecond)
	if _, e = router.Acquire(); e == nil {
		t.Fatal("admitted during quiesce")
	}
	lease.Release()
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	router.Activate(next)
	l, e := router.Acquire()
	if e != nil || l.Runtime != next {
		t.Fatalf("lease=%v err=%v", l.Runtime, e)
	}
	l.Release()
}

func TestApplicationLiveLifecycleSwapsStatelessRuntime(t *testing.T) {
	old := &routedRuntime{id: "old"}
	next := &routedRuntime{id: "next"}
	router := NewAtomicRuntimeRouter(old)
	life := &ApplicationRuntimeLiveLifecycle{Router: router, Factory: func(context.Context, []LiveModule) (ApplicationRuntime, error) { return next, nil }}
	c, e := NewLiveRuntimeController([]LiveModule{{ID: "cell", Revision: "1"}}, old, life)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Reconcile(context.Background(), []LiveModule{{ID: "cell", Revision: "2"}}); e != nil {
		t.Fatal(e)
	}
	if router.Current() != next || !next.started || !old.stopped {
		t.Fatalf("old=%#v next=%#v current=%v", old, next, router.Current())
	}
}

func TestApplicationLiveLifecycleRejectsStatefulRuntimeWithoutV2Snapshot(t *testing.T) {
	old := &routedRuntime{id: "old", stateful: true}
	next := &routedRuntime{id: "next", stateful: true}
	router := NewAtomicRuntimeRouter(old)
	life := &ApplicationRuntimeLiveLifecycle{Router: router, Factory: func(context.Context, []LiveModule) (ApplicationRuntime, error) { return next, nil }}
	c, _ := NewLiveRuntimeController([]LiveModule{{ID: "cell", Revision: "1"}}, old, life)
	if _, e := c.Reconcile(context.Background(), []LiveModule{{ID: "cell", Revision: "2"}}); e == nil {
		t.Fatal("wanted migration contract error")
	}
	if router.Current() != old || !next.stopped || old.stopped {
		t.Fatalf("rollback old=%#v next=%#v", old, next)
	}
}

func TestApplicationLiveLifecycleMigratesV2SnapshotBeforeSwap(t *testing.T) {
	old := &snapshotRuntime{routedRuntime: routedRuntime{id: "old", stateful: true}, snapshot: "state-v1"}
	next := &snapshotRuntime{routedRuntime: routedRuntime{id: "next", stateful: true}}
	router := NewAtomicRuntimeRouter(old)
	life := &ApplicationRuntimeLiveLifecycle{Router: router, Factory: func(context.Context, []LiveModule) (ApplicationRuntime, error) { return next, nil }}
	c, _ := NewLiveRuntimeController([]LiveModule{{ID: "cell", Revision: "1"}}, old, life)
	if _, e := c.Reconcile(context.Background(), []LiveModule{{ID: "cell", Revision: "2"}}); e != nil {
		t.Fatal(e)
	}
	if next.restored != "state-v1" || router.Current() != next {
		t.Fatalf("restored=%v current=%v", next.restored, router.Current())
	}
}

func TestAtomicRuntimeRouterConcurrentLeases(t *testing.T) {
	r := NewAtomicRuntimeRouter(&routedRuntime{id: "old"})
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, e := r.Acquire()
			if e == nil {
				l.Release()
			}
		}()
	}
	wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e := r.Quiesce(ctx); e != nil {
		t.Fatal(e)
	}
}
