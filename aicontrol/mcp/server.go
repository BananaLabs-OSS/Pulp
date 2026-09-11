// Package mcp exposes Pulp's protocol-neutral AI control plane through MCP.
// It contains no deployment authority: authentication and every operation are
// delegated to injected interfaces.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
)

const ProtocolVersion = "2025-11-25"

type Principal struct {
	ID     string
	Claims map[string]string
}
type Operation struct {
	Name        string
	Destructive bool
}
type Authorizer interface {
	Authorize(context.Context, Operation) (Principal, error)
}

// Service is the intentionally narrow seam between MCP and aicontrol. Values
// are JSON documents so this adapter cannot mutate core domain objects.
type Service interface {
	Inspect(context.Context, Principal) (json.RawMessage, error)
	Propose(context.Context, Principal, json.RawMessage) (json.RawMessage, error)
	Validate(context.Context, Principal, json.RawMessage) (json.RawMessage, error)
	BuildTest(context.Context, Principal, json.RawMessage) (json.RawMessage, error)
	Approve(context.Context, Principal, json.RawMessage) (json.RawMessage, error)
	Deploy(context.Context, Principal, json.RawMessage, string) (json.RawMessage, error)
	Rollback(context.Context, Principal, json.RawMessage, string) (json.RawMessage, error)
	ReadResource(context.Context, Principal, string) (json.RawMessage, error)
}

// Transport is injectable; stdio, HTTP/SSE, and WebSocket transports may all
// carry the same JSON-RPC messages without changing this server.
type Transport interface {
	Receive(context.Context) ([]byte, error)
	Send(context.Context, []byte) error
}

type Server struct {
	service       Service
	auth          Authorizer
	name, version string
}

func New(service Service, auth Authorizer, name, version string) (*Server, error) {
	if service == nil || auth == nil {
		return nil, errors.New("MCP server requires service and authorizer")
	}
	if name == "" {
		name = "pulp"
	}
	return &Server{service: service, auth: auth, name: name, version: version}, nil
}
func (s *Server) Serve(ctx context.Context, t Transport) error {
	if t == nil {
		return errors.New("MCP transport is nil")
	}
	for {
		b, e := t.Receive(ctx)
		if e != nil {
			return e
		}
		out := s.Handle(ctx, b)
		if len(out) > 0 {
			if e = t.Send(ctx, out); e != nil {
				return e
			}
		}
	}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (s *Server) Handle(ctx context.Context, input []byte) []byte {
	var r request
	if err := strict(input, &r); err != nil {
		return marshal(response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "Parse error"}})
	}
	if r.JSONRPC != "2.0" || r.Method == "" {
		return marshal(response{JSONRPC: "2.0", ID: r.ID, Error: &rpcError{Code: -32600, Message: "Invalid Request"}})
	}
	// Notifications have no response, including initialized and cancellation.
	if len(r.ID) == 0 {
		return nil
	}
	result, err := s.dispatch(ctx, r)
	if err != nil {
		return marshal(response{JSONRPC: "2.0", ID: r.ID, Error: err})
	}
	return marshal(response{JSONRPC: "2.0", ID: r.ID, Result: result})
}

func (s *Server) dispatch(ctx context.Context, r request) (any, *rpcError) {
	switch r.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if e := decode(r.Params, &p); e != nil {
			return nil, invalid(e)
		}
		if p.ProtocolVersion != ProtocolVersion {
			return nil, &rpcError{Code: -32602, Message: "Unsupported protocol version", Data: map[string]any{"supported": []string{ProtocolVersion}}}
		}
		return map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}, "resources": map[string]any{}}, "serverInfo": map[string]string{"name": s.name, "version": s.version}}, nil
	case "tools/list":
		return map[string]any{"tools": tools()}, nil
	case "tools/call":
		return s.callTool(ctx, r.Params)
	case "resources/list":
		return map[string]any{"resources": resources()}, nil
	case "resources/read":
		return s.readResource(ctx, r.Params)
	default:
		return nil, &rpcError{Code: -32601, Message: "Method not found"}
	}
}

func strict(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	var trailing any
	if e := d.Decode(&trailing); e != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func decode(b []byte, v any) error {
	if len(b) == 0 {
		return errors.New("missing parameters")
	}
	return strict(b, v)
}
func marshal(v any) []byte { b, _ := json.Marshal(v); return b }
func invalid(e error) *rpcError {
	return &rpcError{Code: -32602, Message: "Invalid params", Data: e.Error()}
}
func internal(e error) *rpcError {
	return &rpcError{Code: -32603, Message: "Operation failed", Data: e.Error()}
}
