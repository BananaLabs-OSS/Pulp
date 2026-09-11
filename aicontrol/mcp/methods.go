package mcp

import (
	"context"
	"encoding/json"
	"errors"
)

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func schema(required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"payload": map[string]string{"type": "object"}, "approval_token": map[string]string{"type": "string"}}, "required": required, "additionalProperties": false}
}
func tools() []tool {
	return []tool{
		{"pulp.inspect", "Inspect the current immutable application revision and graph", schema()},
		{"pulp.propose", "Create a revision-bound change proposal", schema("payload")},
		{"pulp.validate", "Validate policy, graph, contracts, and capabilities", schema("payload")},
		{"pulp.build_test", "Build an isolated candidate and collect bound evidence", schema("payload")},
		{"pulp.approve", "Issue an approval bound to proposal, evidence, and policy", schema("payload")},
		{"pulp.deploy", "Transactionally deploy an approved candidate", schema("payload", "approval_token")},
		{"pulp.rollback", "Transactionally roll back a deployment", schema("payload", "approval_token")},
	}
}

type resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MimeType    string `json:"mimeType"`
}

func resources() []resource {
	return []resource{{"pulp://application/current", "Current application", "Current revision and dependency graph", "application/json"}, {"pulp://control/schema", "AI control schema", "Supported ChangeSet operations and policy boundaries", "application/json"}, {"pulp://audit/recent", "Recent audit events", "Recent control-plane decisions and operations", "application/json"}}
}

func (s *Server) callTool(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if e := decode(raw, &p); e != nil {
		return nil, invalid(e)
	}
	destructive := p.Name == "pulp.deploy" || p.Name == "pulp.rollback"
	principal, e := s.auth.Authorize(ctx, Operation{Name: p.Name, Destructive: destructive})
	if e != nil {
		return nil, &rpcError{Code: -32001, Message: "Unauthorized", Data: e.Error()}
	}
	var a struct {
		Payload       json.RawMessage `json:"payload"`
		ApprovalToken string          `json:"approval_token"`
	}
	if len(p.Arguments) > 0 {
		if e := strict(p.Arguments, &a); e != nil {
			return nil, invalid(e)
		}
	}
	if destructive && a.ApprovalToken == "" {
		return nil, &rpcError{Code: -32602, Message: "Approval token required"}
	}
	var out json.RawMessage
	switch p.Name {
	case "pulp.inspect":
		out, e = s.service.Inspect(ctx, principal)
	case "pulp.propose":
		out, e = s.service.Propose(ctx, principal, a.Payload)
	case "pulp.validate":
		out, e = s.service.Validate(ctx, principal, a.Payload)
	case "pulp.build_test":
		out, e = s.service.BuildTest(ctx, principal, a.Payload)
	case "pulp.approve":
		out, e = s.service.Approve(ctx, principal, a.Payload)
	case "pulp.deploy":
		out, e = s.service.Deploy(ctx, principal, a.Payload, a.ApprovalToken)
	case "pulp.rollback":
		out, e = s.service.Rollback(ctx, principal, a.Payload, a.ApprovalToken)
	default:
		return nil, &rpcError{Code: -32602, Message: "Unknown tool"}
	}
	if e != nil {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": e.Error()}}, "isError": true}, nil
	}
	var structured any
	if len(out) > 0 && json.Unmarshal(out, &structured) != nil {
		return nil, internal(errors.New("service returned invalid JSON"))
	}
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(out)}}, "structuredContent": structured, "isError": false}, nil
}
func (s *Server) readResource(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var p struct {
		URI string `json:"uri"`
	}
	if e := decode(raw, &p); e != nil {
		return nil, invalid(e)
	}
	known := false
	for _, r := range resources() {
		if r.URI == p.URI {
			known = true
		}
	}
	if !known {
		return nil, &rpcError{Code: -32602, Message: "Unknown resource"}
	}
	principal, e := s.auth.Authorize(ctx, Operation{Name: "resources/read"})
	if e != nil {
		return nil, &rpcError{Code: -32001, Message: "Unauthorized", Data: e.Error()}
	}
	out, e := s.service.ReadResource(ctx, principal, p.URI)
	if e != nil {
		return nil, internal(e)
	}
	if !json.Valid(out) {
		return nil, internal(errors.New("service returned invalid JSON"))
	}
	return map[string]any{"contents": []any{map[string]any{"uri": p.URI, "mimeType": "application/json", "text": string(out)}}}, nil
}
