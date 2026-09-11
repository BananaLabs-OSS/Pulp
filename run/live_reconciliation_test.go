package run

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type liveFake struct {
	calls []string
	fail  string
}

func (f *liveFake) c(s string) error {
	f.calls = append(f.calls, s)
	if f.fail == s {
		return errors.New("fail")
	}
	return nil
}
func (f *liveFake) Prepare(context.Context, []LiveModule) (any, error) { return "new", f.c("prepare") }
func (f *liveFake) Quiesce(context.Context, any, []string) error       { return f.c("quiesce") }
func (f *liveFake) Snapshot(context.Context, any, []string) (any, error) {
	return "state", f.c("snapshot")
}
func (f *liveFake) Migrate(context.Context, any, any, []string) error     { return f.c("migrate") }
func (f *liveFake) Activate(context.Context, any, any) error              { return f.c("activate") }
func (f *liveFake) Health(context.Context, any) error                     { return f.c("health") }
func (f *liveFake) Retire(context.Context, any) error                     { return f.c("retire") }
func (f *liveFake) Rollback(context.Context, any, any, any, string) error { return f.c("rollback") }
func TestLiveRuntimeControllerUpdatesSameTopologyByRevision(t *testing.T) {
	f := &liveFake{}
	c, e := NewLiveRuntimeController([]LiveModule{{ID: "engine", Revision: "sha-old"}}, "old", f)
	if e != nil {
		t.Fatal(e)
	}
	x, e := c.Reconcile(context.Background(), []LiveModule{{ID: "engine", Revision: "sha-new"}})
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(x.Updated, []string{"engine"}) || c.Current() != "new" {
		t.Fatalf("change=%#v current=%v", x, c.Current())
	}
}
func TestLiveRuntimeControllerRollsBackRoutingOnUnhealthyCandidate(t *testing.T) {
	f := &liveFake{fail: "health"}
	c, _ := NewLiveRuntimeController([]LiveModule{{ID: "engine", Revision: "1"}}, "old", f)
	_, e := c.Reconcile(context.Background(), []LiveModule{{ID: "engine", Revision: "2"}})
	if e == nil {
		t.Fatal("wanted error")
	}
	if c.Current() != "old" {
		t.Fatal("published failed candidate")
	}
	if f.calls[len(f.calls)-1] != "rollback" {
		t.Fatalf("calls=%v", f.calls)
	}
}
