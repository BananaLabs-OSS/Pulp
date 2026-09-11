package fusionabi

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type fake struct {
	name  string
	log   *[]string
	fail  string
	state []byte
}

func (f *fake) Init([]byte) error {
	*f.log = append(*f.log, "init:"+f.name)
	if f.fail == "init" {
		return errors.New("boom")
	}
	return nil
}
func (f *fake) Shutdown() error { *f.log = append(*f.log, "stop:"+f.name); return nil }
func (f *fake) Call(c Call) ([]byte, error) {
	*f.log = append(*f.log, "call:"+c.Member.Name)
	return []byte(c.Provider), nil
}
func (f *fake) Snapshot() ([]byte, error) { return f.state, nil }
func (f *fake) Restore(b []byte) error    { f.state = append([]byte(nil), b...); return nil }
func TestRuntimeScopesLifecycleRoutingAndSnapshots(t *testing.T) {
	var log []string
	a := &fake{name: "a", log: &log, state: []byte("A")}
	b := &fake{name: "b", log: &log, state: []byte("B")}
	r, err := New([]Registration{{Context: Context{Name: "a", Providers: []string{"a.v1"}}, Snapshotable: true, Member: a}, {Context: Context{Name: "b", Providers: []string{"b.v1"}}, Snapshotable: true, Member: b}})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Call(context.Background(), "b.v1", nil); err != nil {
		t.Fatal(err)
	}
	parts, err := r.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if string(parts["a"]) != "A" || string(parts["b"]) != "B" {
		t.Fatalf("parts=%q", parts)
	}
	if err = r.Shutdown(); err != nil {
		t.Fatal(err)
	}
	want := []string{"init:a", "init:b", "call:b", "stop:b", "stop:a"}
	if !reflect.DeepEqual(log, want) {
		t.Fatalf("log=%v want %v", log, want)
	}
}
func TestRuntimeAttributesInitFaultAndRollsBack(t *testing.T) {
	var log []string
	r, _ := New([]Registration{{Context: Context{Name: "a"}, Member: &fake{name: "a", log: &log}}, {Context: Context{Name: "b"}, Member: &fake{name: "b", log: &log, fail: "init"}}})
	err := r.Init()
	var fault *Fault
	if !errors.As(err, &fault) || fault.Member != "b" {
		t.Fatalf("fault=%v", err)
	}
	want := []string{"init:a", "init:b", "stop:a"}
	if !reflect.DeepEqual(log, want) {
		t.Fatalf("log=%v", log)
	}
}
