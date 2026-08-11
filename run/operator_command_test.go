package run

import (
	"context"
	"errors"
	"testing"
)

type operatorCommandCaller struct { identity ApplicationIdentity; calls int; cell, provider string }
func (c *operatorCommandCaller) Identity() ApplicationIdentity { return c.identity }
func (c *operatorCommandCaller) CallProvider(_ context.Context, cell, provider string, request []byte) ([]byte, error) {
	c.calls++; c.cell, c.provider = cell, provider
	if string(request) != "ok" { return nil, errors.New("unexpected request") }
	return []byte("done"), nil
}

func TestOperatorCommandIsExplicitScopedAndRevocable(t *testing.T) {
	r := newOperatorCommandRegistry()
	identity := ApplicationIdentity{ApplicationID: "evolution", InstanceID: "primary"}
	if err := r.register(OperatorCommandDescriptor{Name: "pfc", Application: identity, Cell: "evolution", Provider: "pfc.batch", MaxRequestBytes: 8}); err != nil { t.Fatal(err) }
	if _, err := r.invoke(context.Background(), "pfc", []byte("ok")); err == nil { t.Fatal("inactive command invoked") }
	caller := &operatorCommandCaller{identity: identity}
	if err := r.bind(identity, caller); err != nil { t.Fatal(err) }
	got, err := r.invoke(context.Background(), "pfc", []byte("ok")); if err != nil || string(got) != "done" { t.Fatalf("invoke = %q, %v", got, err) }
	if caller.calls != 1 || caller.cell != "evolution" || caller.provider != "pfc.batch" { t.Fatalf("target = %d %s/%s", caller.calls, caller.cell, caller.provider) }
	if _, err := r.invoke(context.Background(), "pfc", make([]byte, 9)); err == nil { t.Fatal("oversized request invoked") }
	r.unbind(identity)
	if _, err := r.invoke(context.Background(), "pfc", []byte("ok")); err == nil { t.Fatal("revoked command invoked") }
}

func TestOperatorCommandRejectsMismatchedApplicationBind(t *testing.T) {
	r := newOperatorCommandRegistry()
	identity := ApplicationIdentity{ApplicationID: "evolution", InstanceID: "primary"}
	if err := r.register(OperatorCommandDescriptor{Name: "pfc", Application: identity, Cell: "evolution", Provider: "pfc.batch", MaxRequestBytes: 8}); err != nil { t.Fatal(err) }
	if err := r.bind(identity, &operatorCommandCaller{identity: ApplicationIdentity{ApplicationID: "sessions", InstanceID: "primary"}}); err == nil { t.Fatal("foreign caller bound") }
}
