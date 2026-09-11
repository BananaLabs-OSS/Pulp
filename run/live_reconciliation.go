package run

import (
	"context"
	"errors"
	"github.com/BananaLabs-OSS/Pulp/internal/dependency"
	"github.com/BananaLabs-OSS/Pulp/internal/reconcile"
	"sync"
)

type LiveModule struct {
	ID        string
	DependsOn []string
	Revision  string
}

// LiveRuntimeLifecycle controls the concrete runtime boundary. Activate must
// atomically redirect new admissions; Quiesce must reject them and drain calls.
type LiveRuntimeLifecycle interface {
	Prepare(context.Context, []LiveModule) (any, error)
	Quiesce(context.Context, any, []string) error
	Snapshot(context.Context, any, []string) (any, error)
	Migrate(context.Context, any, any, []string) error
	Activate(context.Context, any, any) error
	Health(context.Context, any) error
	Retire(context.Context, any) error
	Rollback(context.Context, any, any, any, string) error
}

type LiveRuntimeController struct {
	mu         sync.Mutex
	reconciler *reconcile.Reconciler
	adapter    *liveLifecycleAdapter
}

func NewLiveRuntimeController(current []LiveModule, value any, lifecycle LiveRuntimeLifecycle) (*LiveRuntimeController, error) {
	return newLiveRuntimeController(current, value, lifecycle, nil)
}
func newLiveRuntimeController(current []LiveModule, value any, lifecycle LiveRuntimeLifecycle, recorder reconcile.Recorder) (*LiveRuntimeController, error) {
	if lifecycle == nil {
		return nil, errors.New("live runtime lifecycle is required")
	}
	p, rev, err := livePlan(current)
	if err != nil {
		return nil, err
	}
	a := &liveLifecycleAdapter{host: lifecycle}
	r, err := reconcile.NewWithRecorder(reconcile.Deployment{Plan: p, Value: value, Revisions: rev}, a, recorder)
	if err != nil {
		return nil, err
	}
	return &LiveRuntimeController{reconciler: r, adapter: a}, nil
}

type LiveDeploymentEvidence struct {
	LockDigest                     string
	ArtifactDigests, FusionDigests []string
}

func (c *LiveRuntimeController) ReconcileWithEvidence(ctx context.Context, desired []LiveModule, evidence LiveDeploymentEvidence) (reconcile.ChangeSet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.adapter.mu.Lock()
	c.adapter.desired = append([]LiveModule(nil), desired...)
	c.adapter.mu.Unlock()
	n := make([]reconcile.Node, len(desired))
	for i, m := range desired {
		n[i] = reconcile.Node{ID: m.ID, DependsOn: m.DependsOn, Revision: m.Revision}
	}
	return c.reconciler.ReconcileWithEvidence(ctx, n, reconcile.Evidence{LockDigest: evidence.LockDigest, ArtifactDigests: evidence.ArtifactDigests, FusionDigests: evidence.FusionDigests})
}
func (c *LiveRuntimeController) Reconcile(ctx context.Context, desired []LiveModule) (reconcile.ChangeSet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.adapter.mu.Lock()
	c.adapter.desired = append([]LiveModule(nil), desired...)
	c.adapter.mu.Unlock()
	n := make([]reconcile.Node, len(desired))
	for i, m := range desired {
		n[i] = reconcile.Node{ID: m.ID, DependsOn: m.DependsOn, Revision: m.Revision}
	}
	return c.reconciler.Reconcile(ctx, n)
}
func (c *LiveRuntimeController) Current() any { return c.reconciler.Snapshot().Value }

type liveLifecycleAdapter struct {
	mu      sync.Mutex
	host    LiveRuntimeLifecycle
	desired []LiveModule
}

func (a *liveLifecycleAdapter) Prepare(c context.Context, _ *dependency.Plan, _ reconcile.ChangeSet) (any, error) {
	a.mu.Lock()
	d := append([]LiveModule(nil), a.desired...)
	a.mu.Unlock()
	return a.host.Prepare(c, d)
}
func (a *liveLifecycleAdapter) Quiesce(c context.Context, d reconcile.Deployment, x reconcile.ChangeSet) error {
	return a.host.Quiesce(c, d.Value, x.Affected)
}
func (a *liveLifecycleAdapter) Snapshot(c context.Context, d reconcile.Deployment, x reconcile.ChangeSet) (any, error) {
	return a.host.Snapshot(c, d.Value, x.Affected)
}
func (a *liveLifecycleAdapter) Migrate(c context.Context, n, s any, x reconcile.ChangeSet) error {
	return a.host.Migrate(c, n, s, x.Affected)
}
func (a *liveLifecycleAdapter) Swap(c context.Context, d reconcile.Deployment, n any, _ reconcile.ChangeSet) error {
	return a.host.Activate(c, d.Value, n)
}
func (a *liveLifecycleAdapter) Health(c context.Context, n any, _ reconcile.ChangeSet) error {
	return a.host.Health(c, n)
}
func (a *liveLifecycleAdapter) Commit(c context.Context, d reconcile.Deployment, _ any, _ reconcile.ChangeSet) error {
	return a.host.Retire(c, d.Value)
}
func (a *liveLifecycleAdapter) Rollback(c context.Context, d reconcile.Deployment, n, s any, _ reconcile.ChangeSet, p reconcile.Stage) error {
	return a.host.Rollback(c, d.Value, n, s, string(p))
}
func livePlan(n []LiveModule) (*dependency.Plan, map[string]string, error) {
	i := make([]dependency.Item, len(n))
	r := map[string]string{}
	for x, m := range n {
		i[x] = dependency.Item{ID: m.ID, DependsOn: m.DependsOn}
		r[m.ID] = m.Revision
	}
	p, e := dependency.Build(i)
	return p, r, e
}
