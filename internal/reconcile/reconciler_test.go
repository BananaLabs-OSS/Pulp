package reconcile

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/BananaLabs-OSS/Pulp/internal/dependency"
)

type fakeLifecycle struct {
	mu    sync.Mutex
	calls []string
	fail  string
	gate  chan struct{}
}

func (f *fakeLifecycle) call(name string) error {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
	if f.fail == name {
		return errors.New("boom")
	}
	return nil
}
func (f *fakeLifecycle) Prepare(context.Context, *dependency.Plan, ChangeSet) (any, error) {
	if err := f.call("prepare"); err != nil {
		return nil, err
	}
	if f.gate != nil {
		<-f.gate
	}
	return "candidate", nil
}
func (f *fakeLifecycle) Quiesce(context.Context, Deployment, ChangeSet) error {
	return f.call("quiesce")
}
func (f *fakeLifecycle) Snapshot(context.Context, Deployment, ChangeSet) (any, error) {
	return "snapshot", f.call("snapshot")
}
func (f *fakeLifecycle) Migrate(context.Context, any, any, ChangeSet) error { return f.call("migrate") }
func (f *fakeLifecycle) Swap(context.Context, Deployment, any, ChangeSet) error {
	return f.call("swap")
}
func (f *fakeLifecycle) Health(context.Context, any, ChangeSet) error { return f.call("health") }
func (f *fakeLifecycle) Commit(context.Context, Deployment, any, ChangeSet) error {
	return f.call("commit")
}
func (f *fakeLifecycle) Rollback(_ context.Context, _ Deployment, _ any, _ any, _ ChangeSet, s Stage) error {
	return f.call("rollback:" + string(s))
}

func plan(t *testing.T, items ...dependency.Item) *dependency.Plan {
	t.Helper()
	p, e := dependency.Build(items)
	if e != nil {
		t.Fatal(e)
	}
	return p
}

func TestReplaceCommitsTransactionAndCalculatesImpact(t *testing.T) {
	f := &fakeLifecycle{}
	old := plan(t, dependency.Item{ID: "db"}, dependency.Item{ID: "api", DependsOn: []string{"db"}}, dependency.Item{ID: "ui", DependsOn: []string{"api"}})
	r, _ := New(Deployment{Plan: old, Value: "old"}, f)
	c, err := r.Replace(context.Background(), []dependency.Item{{ID: "db"}, {ID: "cache"}, {ID: "api", DependsOn: []string{"db", "cache"}}, {ID: "ui", DependsOn: []string{"api"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Added, []string{"cache"}) || !reflect.DeepEqual(c.Updated, []string{"api"}) || !reflect.DeepEqual(c.Affected, []string{"api", "ui", "cache"}) {
		t.Fatalf("changes = %#v", c)
	}
	want := []string{"prepare", "quiesce", "snapshot", "migrate", "swap", "health", "commit"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls=%v", f.calls)
	}
	if r.Snapshot().Value != "candidate" {
		t.Fatal("candidate not committed")
	}
}

func TestReplaceRejectsInvalidGraphBeforeLifecycle(t *testing.T) {
	f := &fakeLifecycle{}
	r, _ := New(Deployment{Plan: plan(t, dependency.Item{ID: "old"}), Value: "old"}, f)
	_, err := r.Replace(context.Background(), []dependency.Item{{ID: "a", DependsOn: []string{"missing"}}})
	if err == nil || len(f.calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, f.calls)
	}
}

func TestEveryFailureRollsBackAndKeepsOldDeployment(t *testing.T) {
	for _, phase := range []string{"prepare", "quiesce", "snapshot", "migrate", "swap", "health", "commit"} {
		t.Run(phase, func(t *testing.T) {
			f := &fakeLifecycle{fail: phase}
			r, _ := New(Deployment{Plan: plan(t, dependency.Item{ID: "old"}), Value: "old"}, f)
			_, err := r.Replace(context.Background(), []dependency.Item{{ID: "new"}})
			if err == nil {
				t.Fatal("wanted error")
			}
			if r.Snapshot().Value != "old" {
				t.Fatal("failed transaction published")
			}
			if got := f.calls[len(f.calls)-1]; got != "rollback:"+phase {
				t.Fatalf("last call=%s", got)
			}
		})
	}
}

func TestConcurrentReplacementsAreSerializedAndReadersSeeCommittedState(t *testing.T) {
	gate := make(chan struct{})
	f := &fakeLifecycle{gate: gate}
	r, _ := New(Deployment{Plan: plan(t, dependency.Item{ID: "v1"}), Value: "v1"}, f)
	done := make(chan error, 2)
	go func() { _, e := r.Replace(context.Background(), []dependency.Item{{ID: "v2"}}); done <- e }()
	for {
		f.mu.Lock()
		started := len(f.calls) > 0
		f.mu.Unlock()
		if started {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if r.Snapshot().Value != "v1" {
		t.Fatal("reader observed uncommitted deployment")
	}
	go func() { _, e := r.Replace(context.Background(), []dependency.Item{{ID: "v3"}}); done <- e }()
	close(gate)
	for range 2 {
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	}
	if r.Snapshot().Value != "candidate" {
		t.Fatal("last commit missing")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 14 {
		t.Fatalf("calls=%v", f.calls)
	}
}
