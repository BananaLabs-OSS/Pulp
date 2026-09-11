package run

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// RuntimeLease pins one admitted call to the runtime selected at admission.
type RuntimeLease struct {
	Runtime ApplicationRuntime
	release func()
}

func (l *RuntimeLease) Release() {
	if l != nil && l.release != nil {
		l.release()
		l.release = nil
	}
}

// AtomicRuntimeRouter provides the indirection required for a true live swap.
// Quiesce closes admission and waits for every previously admitted lease.
type AtomicRuntimeRouter struct {
	mu        sync.Mutex
	target    ApplicationRuntime
	accepting bool
	inflight  int
	drained   chan struct{}
}

func NewAtomicRuntimeRouter(target ApplicationRuntime) *AtomicRuntimeRouter {
	return &AtomicRuntimeRouter{target: target, accepting: true}
}
func (r *AtomicRuntimeRouter) Acquire() (RuntimeLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.accepting || r.target == nil {
		return RuntimeLease{}, errors.New("runtime admission is quiesced")
	}
	r.inflight++
	target := r.target
	return RuntimeLease{Runtime: target, release: func() { r.release() }}, nil
}
func (r *AtomicRuntimeRouter) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inflight--
	if r.inflight == 0 && r.drained != nil {
		close(r.drained)
		r.drained = nil
	}
}
func (r *AtomicRuntimeRouter) Quiesce(ctx context.Context) error {
	r.mu.Lock()
	r.accepting = false
	if r.inflight == 0 {
		r.mu.Unlock()
		return nil
	}
	if r.drained == nil {
		r.drained = make(chan struct{})
	}
	done := r.drained
	r.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *AtomicRuntimeRouter) Activate(target ApplicationRuntime) {
	r.mu.Lock()
	r.target = target
	r.accepting = true
	r.mu.Unlock()
}
func (r *AtomicRuntimeRouter) Current() ApplicationRuntime {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.target
}

// LiveSnapshotRuntime is the V2 opt-in state migration contract. Stateful
// runtimes without it are rejected before activation.
type LiveSnapshotRuntime interface {
	SnapshotLive(context.Context, []string) (any, error)
	RestoreLive(context.Context, any, []string) error
}
type LiveStatefulRuntime interface{ LiveStateful() bool }
type LiveHealthRuntime interface{ LiveHealth(context.Context) error }

type LiveApplicationFactory func(context.Context, []LiveModule) (ApplicationRuntime, error)

// ApplicationRuntimeLiveLifecycle is the concrete lifecycle used by
// LiveRuntimeController for whole applicationRuntime replacements.
type ApplicationRuntimeLiveLifecycle struct {
	Router  *AtomicRuntimeRouter
	Factory LiveApplicationFactory
}

func (l *ApplicationRuntimeLiveLifecycle) Prepare(ctx context.Context, m []LiveModule) (any, error) {
	if l.Router == nil || l.Factory == nil {
		return nil, errors.New("live application lifecycle requires router and factory")
	}
	r, e := l.Factory(ctx, m)
	if e != nil {
		return nil, e
	}
	if e = r.Start(ctx); e != nil {
		_ = r.Shutdown(context.WithoutCancel(ctx))
		return nil, e
	}
	return r, nil
}
func (l *ApplicationRuntimeLiveLifecycle) Quiesce(ctx context.Context, _ any, _ []string) error {
	return l.Router.Quiesce(ctx)
}
func (l *ApplicationRuntimeLiveLifecycle) Snapshot(ctx context.Context, old any, a []string) (any, error) {
	r, ok := old.(ApplicationRuntime)
	if !ok {
		return nil, errors.New("live deployment is not an ApplicationRuntime")
	}
	if s, ok := r.(LiveSnapshotRuntime); ok {
		return s.SnapshotLive(ctx, a)
	}
	if stateful, ok := r.(LiveStatefulRuntime); ok && stateful.LiveStateful() {
		return nil, errors.New("stateful runtime does not implement LiveSnapshotRuntime")
	}
	return nil, nil
}
func (l *ApplicationRuntimeLiveLifecycle) Migrate(ctx context.Context, n, s any, a []string) error {
	r := n.(ApplicationRuntime)
	if v, ok := r.(LiveSnapshotRuntime); ok {
		return v.RestoreLive(ctx, s, a)
	}
	if s != nil {
		return errors.New("candidate runtime cannot restore live snapshot")
	}
	if stateful, ok := r.(LiveStatefulRuntime); ok && stateful.LiveStateful() {
		return errors.New("stateful candidate does not implement LiveSnapshotRuntime")
	}
	return nil
}
func (l *ApplicationRuntimeLiveLifecycle) Activate(_ context.Context, _ any, n any) error {
	r, ok := n.(ApplicationRuntime)
	if !ok {
		return errors.New("candidate is not an ApplicationRuntime")
	}
	if app, ok := r.(*applicationRuntime); ok && app.providerAccess != nil {
		deploymentOperatorCommands.activate(app.application.Identity, app.providerAccess)
	}
	l.Router.Activate(r)
	return nil
}
func (l *ApplicationRuntimeLiveLifecycle) Health(ctx context.Context, n any) error {
	if h, ok := n.(LiveHealthRuntime); ok {
		return h.LiveHealth(ctx)
	}
	if _, ok := n.(ApplicationRuntime); !ok {
		return errors.New("candidate is not an ApplicationRuntime")
	}
	return nil
}
func (l *ApplicationRuntimeLiveLifecycle) Retire(ctx context.Context, old any) error {
	return old.(ApplicationRuntime).Shutdown(ctx)
}
func (l *ApplicationRuntimeLiveLifecycle) Rollback(ctx context.Context, old, n, _ any, stage string) error {
	var out error
	if stage == string("swap") || stage == string("health") || stage == string("commit") {
		if r, ok := old.(ApplicationRuntime); ok {
			if app, yes := r.(*applicationRuntime); yes && app.providerAccess != nil {
				deploymentOperatorCommands.activate(app.application.Identity, app.providerAccess)
			}
			l.Router.Activate(r)
		}
	} else if l.Router != nil {
		if r, ok := old.(ApplicationRuntime); ok {
			l.Router.Activate(r)
		}
	}
	if candidate, ok := n.(ApplicationRuntime); ok {
		out = candidate.Shutdown(ctx)
	}
	if out != nil {
		return fmt.Errorf("dispose candidate: %w", out)
	}
	return nil
}

func (r *applicationRuntime) LiveStateful() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.LiveStatefulUnlocked()
}
func (r *applicationRuntime) LiveHealth(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return errors.New("application runtime is not started")
	}
	for address, rt := range r.runtimes {
		if rt.failed.Load() || rt.cell == nil {
			return fmt.Errorf("runtime cell %s is unhealthy", address)
		}
	}
	return nil
}
