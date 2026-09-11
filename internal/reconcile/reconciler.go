// Package reconcile transactionally replaces a live dependency graph.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/BananaLabs-OSS/Pulp/internal/dependency"
)

// Deployment is the graph and opaque host value currently serving traffic.
type Deployment struct {
	Plan      *dependency.Plan
	Value     any
	Revisions map[string]string
}

type Node struct {
	ID        string
	DependsOn []string
	Revision  string
}

// ChangeSet describes the logical impact of replacing one graph with another.
// Affected includes changed nodes and all of their old or new dependants.
type ChangeSet struct {
	Added, Updated, Removed, Unchanged, Affected []string
}

// Stage identifies the last phase entered by a failed transaction.
type Stage string

const (
	StagePrepare  Stage = "prepare"
	StageQuiesce  Stage = "quiesce"
	StageSnapshot Stage = "snapshot"
	StageMigrate  Stage = "migrate"
	StageSwap     Stage = "swap"
	StageHealth   Stage = "health"
	StageCommit   Stage = "commit"
)

// Lifecycle is the host-specific seam. Prepare must not make the candidate
// visible. Swap changes routing atomically. Rollback must restore previous
// routing when Swap was entered and dispose of the candidate in every stage.
type Lifecycle interface {
	Prepare(context.Context, *dependency.Plan, ChangeSet) (any, error)
	Quiesce(context.Context, Deployment, ChangeSet) error
	Snapshot(context.Context, Deployment, ChangeSet) (any, error)
	Migrate(context.Context, any, any, ChangeSet) error
	Swap(context.Context, Deployment, any, ChangeSet) error
	Health(context.Context, any, ChangeSet) error
	Commit(context.Context, Deployment, any, ChangeSet) error
	Rollback(context.Context, Deployment, any, any, ChangeSet, Stage) error
}

// Reconciler serializes mutations while Snapshot remains safe for concurrent readers.
type Reconciler struct {
	mu        sync.RWMutex
	reconcile sync.Mutex
	current   Deployment
	lifecycle Lifecycle
	recorder  Recorder
}

func New(current Deployment, lifecycle Lifecycle) (*Reconciler, error) {
	return NewWithRecorder(current, lifecycle, nil)
}

func NewWithRecorder(current Deployment, lifecycle Lifecycle, recorder Recorder) (*Reconciler, error) {
	if current.Plan == nil {
		return nil, errors.New("live reconciliation requires a current dependency plan")
	}
	if lifecycle == nil {
		return nil, errors.New("live reconciliation requires a lifecycle")
	}
	return &Reconciler{current: current, lifecycle: lifecycle, recorder: recorder}, nil
}

func (r *Reconciler) Snapshot() Deployment {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current
}

// Replace validates and transactionally activates desired. Calls are serialized;
// cancellation or any phase failure leaves the previous deployment authoritative.
func (r *Reconciler) Replace(ctx context.Context, items []dependency.Item) (ChangeSet, error) {
	nodes := make([]Node, len(items))
	for i, item := range items {
		nodes[i] = Node{ID: item.ID, DependsOn: item.DependsOn}
	}
	return r.Reconcile(ctx, nodes)
}

func (r *Reconciler) Reconcile(ctx context.Context, nodes []Node) (ChangeSet, error) {
	return r.ReconcileWithEvidence(ctx, nodes, Evidence{})
}

func (r *Reconciler) ReconcileWithEvidence(ctx context.Context, nodes []Node, evidence Evidence) (ChangeSet, error) {
	items := make([]dependency.Item, len(nodes))
	revisions := make(map[string]string, len(nodes))
	for i, node := range nodes {
		items[i] = dependency.Item{ID: node.ID, DependsOn: node.DependsOn}
		revisions[node.ID] = node.Revision
	}
	desired, err := dependency.Build(items)
	if err != nil {
		return ChangeSet{}, fmt.Errorf("validate replacement graph: %w", err)
	}
	r.reconcile.Lock()
	defer r.reconcile.Unlock()
	previous := r.Snapshot()
	changes := diff(previous.Plan, desired, previous.Revisions, revisions)
	if len(changes.Added)+len(changes.Updated)+len(changes.Removed) == 0 {
		return changes, nil
	}
	tx := newTransactionID()
	record := func(stage Stage, status RecordStatus, recordErr error, snapshot any) error {
		if r.recorder == nil {
			return nil
		}
		e := Record{TransactionID: tx, Stage: stage, Status: status, Previous: deploymentDescriptor(previous), Desired: graphDescriptor(desired, revisions), Changes: changes}
		e.LockDigest, e.ArtifactDigests, e.FusionDigests = evidence.LockDigest, append([]string(nil), evidence.ArtifactDigests...), append([]string(nil), evidence.FusionDigests...)
		if recordErr != nil {
			e.Error = recordErr.Error()
		}
		if ref, ok := snapshot.(SnapshotReference); ok {
			e.SnapshotRef = ref.DeploymentSnapshotReference()
		}
		return r.recorder.Record(context.WithoutCancel(ctx), e)
	}
	if err := record(StagePrepare, StatusStarted, nil, nil); err != nil {
		return changes, fmt.Errorf("record deployment start: %w", err)
	}

	var candidate, snapshot any
	stage := StagePrepare
	rollback := func(cause error) error {
		rbErr := r.lifecycle.Rollback(context.WithoutCancel(ctx), previous, candidate, snapshot, changes, stage)
		recordErr := record(stage, StatusRolledBack, errors.Join(cause, rbErr), snapshot)
		if rbErr != nil {
			return errors.Join(cause, fmt.Errorf("rollback after %s: %w", stage, rbErr), recordErr)
		}
		return errors.Join(cause, recordErr)
	}
	if candidate, err = r.lifecycle.Prepare(ctx, desired, changes); err != nil {
		return changes, rollback(fmt.Errorf("prepare replacement: %w", err))
	}
	if err = record(StagePrepare, StatusCompleted, nil, nil); err != nil {
		return changes, rollback(fmt.Errorf("record prepare: %w", err))
	}
	stage = StageQuiesce
	if err = r.lifecycle.Quiesce(ctx, previous, changes); err != nil {
		return changes, rollback(fmt.Errorf("quiesce current graph: %w", err))
	}
	if err = record(StageQuiesce, StatusCompleted, nil, nil); err != nil {
		return changes, rollback(fmt.Errorf("record quiesce: %w", err))
	}
	stage = StageSnapshot
	if snapshot, err = r.lifecycle.Snapshot(ctx, previous, changes); err != nil {
		return changes, rollback(fmt.Errorf("snapshot current graph: %w", err))
	}
	if err = record(StageSnapshot, StatusCompleted, nil, snapshot); err != nil {
		return changes, rollback(fmt.Errorf("record snapshot: %w", err))
	}
	stage = StageMigrate
	if err = r.lifecycle.Migrate(ctx, candidate, snapshot, changes); err != nil {
		return changes, rollback(fmt.Errorf("migrate replacement: %w", err))
	}
	if err = record(StageMigrate, StatusCompleted, nil, snapshot); err != nil {
		return changes, rollback(fmt.Errorf("record migration: %w", err))
	}
	stage = StageSwap
	if err = r.lifecycle.Swap(ctx, previous, candidate, changes); err != nil {
		return changes, rollback(fmt.Errorf("swap replacement: %w", err))
	}
	if err = record(StageSwap, StatusCompleted, nil, snapshot); err != nil {
		return changes, rollback(fmt.Errorf("record swap: %w", err))
	}
	stage = StageHealth
	if err = r.lifecycle.Health(ctx, candidate, changes); err != nil {
		return changes, rollback(fmt.Errorf("health check replacement: %w", err))
	}
	if err = record(StageHealth, StatusCompleted, nil, snapshot); err != nil {
		return changes, rollback(fmt.Errorf("record health: %w", err))
	}
	stage = StageCommit
	if err = r.lifecycle.Commit(ctx, previous, candidate, changes); err != nil {
		return changes, rollback(fmt.Errorf("commit replacement: %w", err))
	}
	r.mu.Lock()
	r.current = Deployment{Plan: desired, Value: candidate, Revisions: revisions}
	r.mu.Unlock()
	if err = record(StageCommit, StatusCommitted, nil, snapshot); err != nil {
		return changes, fmt.Errorf("record committed deployment: %w", err)
	}
	return changes, nil
}

func diff(old, next *dependency.Plan, oldRevisions, nextRevisions map[string]string) ChangeSet {
	oldIDs, nextIDs := old.StartOrder(), next.StartOrder()
	oldSet, nextSet := make(map[string]bool), make(map[string]bool)
	for _, id := range oldIDs {
		oldSet[id] = true
	}
	for _, id := range nextIDs {
		nextSet[id] = true
	}
	c := ChangeSet{}
	changed := make(map[string]bool)
	for _, id := range nextIDs {
		if !oldSet[id] {
			c.Added = append(c.Added, id)
			changed[id] = true
			continue
		}
		if !slices.Equal(dependencies(old, id), dependencies(next, id)) || oldRevisions[id] != nextRevisions[id] {
			c.Updated = append(c.Updated, id)
			changed[id] = true
		} else {
			c.Unchanged = append(c.Unchanged, id)
		}
	}
	for _, id := range oldIDs {
		if !nextSet[id] {
			c.Removed = append(c.Removed, id)
			changed[id] = true
		}
	}
	queue := append([]string(nil), c.Added...)
	queue = append(queue, c.Updated...)
	queue = append(queue, c.Removed...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, p := range []*dependency.Plan{old, next} {
			for _, d := range dependents(p, id) {
				if !changed[d] {
					changed[d] = true
					queue = append(queue, d)
				}
			}
		}
	}
	for _, id := range append(append([]string(nil), oldIDs...), nextIDs...) {
		if changed[id] && !slices.Contains(c.Affected, id) {
			c.Affected = append(c.Affected, id)
		}
	}
	return c
}

func dependencies(plan *dependency.Plan, id string) []string {
	node, ok := plan.Node(id)
	if !ok {
		return nil
	}
	return node.Dependencies()
}
func dependents(plan *dependency.Plan, id string) []string {
	node, ok := plan.Node(id)
	if !ok {
		return nil
	}
	return node.Dependents()
}
