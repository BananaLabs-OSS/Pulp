package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type auth struct {
	err error
	ops []Operation
}

func (a *auth) Authorize(_ context.Context, o Operation) (Principal, error) {
	a.ops = append(a.ops, o)
	return Principal{ID: "user"}, a.err
}

type svc struct{ called, token string }

func (s *svc) reply(n string) (json.RawMessage, error) {
	s.called = n
	return json.RawMessage(`{"ok":true}`), nil
}
func (s *svc) Inspect(context.Context, Principal) (json.RawMessage, error) { return s.reply("inspect") }
func (s *svc) Propose(context.Context, Principal, json.RawMessage) (json.RawMessage, error) {
	return s.reply("propose")
}
func (s *svc) Validate(context.Context, Principal, json.RawMessage) (json.RawMessage, error) {
	return s.reply("validate")
}
func (s *svc) BuildTest(context.Context, Principal, json.RawMessage) (json.RawMessage, error) {
	return s.reply("build_test")
}
func (s *svc) Approve(context.Context, Principal, json.RawMessage) (json.RawMessage, error) {
	return s.reply("approve")
}
func (s *svc) Deploy(_ context.Context, _ Principal, _ json.RawMessage, t string) (json.RawMessage, error) {
	s.token = t
	return s.reply("deploy")
}
func (s *svc) Rollback(_ context.Context, _ Principal, _ json.RawMessage, t string) (json.RawMessage, error) {
	s.token = t
	return s.reply("rollback")
}
func (s *svc) ReadResource(_ context.Context, _ Principal, u string) (json.RawMessage, error) {
	s.called = u
	return json.RawMessage(`{"revision":"r1"}`), nil
}
func server(t *testing.T) (*Server, *svc, *auth) {
	t.Helper()
	v := &svc{}
	a := &auth{}
	s, e := New(v, a, "pulp", "1")
	if e != nil {
		t.Fatal(e)
	}
	return s, v, a
}
func rpc(t *testing.T, s *Server, in string) map[string]any {
	t.Helper()
	out := s.Handle(context.Background(), []byte(in))
	var v map[string]any
	if e := json.Unmarshal(out, &v); e != nil {
		t.Fatalf("%s: %v", out, e)
	}
	return v
}
func TestInitializeAndLists(t *testing.T) {
	s, _, _ := server(t)
	v := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	r := v["result"].(map[string]any)
	if r["protocolVersion"] != ProtocolVersion {
		t.Fatal(r)
	}
	v = rpc(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	tools := v["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 7 {
		t.Fatalf("tools=%d", len(tools))
	}
	v = rpc(t, s, `{"jsonrpc":"2.0","id":3,"method":"resources/list","params":{}}`)
	if len(v["result"].(map[string]any)["resources"].([]any)) != 3 {
		t.Fatal(v)
	}
}
func TestDeployRequiresAndPassesApprovalToken(t *testing.T) {
	s, v, a := server(t)
	r := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pulp.deploy","arguments":{"payload":{}}}}`)
	if r["error"].(map[string]any)["message"] != "Approval token required" {
		t.Fatal(r)
	}
	r = rpc(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"pulp.deploy","arguments":{"payload":{},"approval_token":"bound-token"}}}`)
	if r["error"] != nil {
		t.Fatal(r)
	}
	if v.called != "deploy" || v.token != "bound-token" || !a.ops[len(a.ops)-1].Destructive {
		t.Fatalf("service=%#v auth=%#v", v, a.ops)
	}
}
func TestResourceReadAndNotification(t *testing.T) {
	s, v, _ := server(t)
	r := rpc(t, s, `{"jsonrpc":"2.0","id":"x","method":"resources/read","params":{"uri":"pulp://application/current"}}`)
	text := r["result"].(map[string]any)["contents"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "revision") || v.called != "pulp://application/current" {
		t.Fatal(r)
	}
	if out := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); out != nil {
		t.Fatalf("notification response=%s", out)
	}
}
func TestAuthAndProtocolFailures(t *testing.T) {
	s, _, a := server(t)
	a.err = errors.New("no")
	r := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pulp.inspect","arguments":{}}}`)
	if r["error"].(map[string]any)["code"].(float64) != -32001 {
		t.Fatal(r)
	}
	r = rpc(t, s, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"old"}}`)
	if r["error"].(map[string]any)["code"].(float64) != -32602 {
		t.Fatal(r)
	}
}
