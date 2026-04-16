package transport

import (
	"context"

	"github.com/BananaLabs-OSS/Pulp/internal/abi"
	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/vmihailenco/msgpack/v5"
)

// HTTPInboundCapability returns the Capability that wires http_register
// and http_respond into the "pulp" host module. All plugins that declare
// transport.http.inbound share the same server instance.
//
// Host imports exposed:
//
//	http_register(req_ptr, req_len) -> error_code
//	  req bytes = MessagePack {method, path}
//
//	http_respond(resp_ptr, resp_len) -> error_code
//	  resp bytes = MessagePack HTTPResponse (id, status, headers, body)
//
// Error codes: 0 ok, 1 empty input, 2 memory read failed, 3 decode failed,
// 4 dispatch error (unknown route / no pending request).
func HTTPInboundCapability(s *HTTPServer) host.Capability {
	return host.Capability{
		Name: "transport.http.inbound",
		Register: func(b wazero.HostModuleBuilder, p *host.Plugin) error {
			b.NewFunctionBuilder().
				WithFunc(func(ctx context.Context, m api.Module, reqPtr, reqLen uint32) uint32 {
					if reqLen == 0 {
						return 1
					}
					data, ok := m.Memory().Read(reqPtr, reqLen)
					if !ok {
						return 2
					}
					var reg struct {
						Method string `msgpack:"method"`
						Path   string `msgpack:"path"`
					}
					if err := msgpack.Unmarshal(data, &reg); err != nil {
						return 3
					}
					if err := s.RegisterRoute(reg.Method, reg.Path); err != nil {
						return 4
					}
					return 0
				}).
				Export("http_register")

			b.NewFunctionBuilder().
				WithFunc(func(ctx context.Context, m api.Module, respPtr, respLen uint32) uint32 {
					if respLen == 0 {
						return 1
					}
					data, ok := m.Memory().Read(respPtr, respLen)
					if !ok {
						return 2
					}
					resp, err := abi.DecodeHTTPResponse(data)
					if err != nil {
						return 3
					}
					if err := s.Respond(resp); err != nil {
						return 4
					}
					return 0
				}).
				Export("http_respond")

			return nil
		},
	}
}

// SSECapability returns the Capability that wires sse_register and
// sse_emit into the "pulp" host module. Plugins that declare
// transport.sse register paths and broadcast events on them; the host
// serves long-poll subscribers and multiplexes the emit to all active
// connections.
//
// Host import signatures:
//
//	sse_register(path_ptr, path_len)
//	  path bytes = raw path string, e.g. "/events"
//
//	sse_emit(req_ptr, req_len)
//	  req bytes = MessagePack SSEEmitRequest (path, id, event, data)
//
// Error codes: 0 ok, 1 empty input, 2 memory read failed, 3 decode
// failed, 4 dispatch error (unknown path).
func SSECapability(s *SSEServer) host.Capability {
	return host.Capability{
		Name: "transport.sse",
		Register: func(b wazero.HostModuleBuilder, p *host.Plugin) error {
			b.NewFunctionBuilder().
				WithFunc(func(ctx context.Context, m api.Module, pathPtr, pathLen uint32) uint32 {
					if pathLen == 0 {
						return 1
					}
					data, ok := m.Memory().Read(pathPtr, pathLen)
					if !ok {
						return 2
					}
					if err := s.RegisterRoute(string(data)); err != nil {
						return 4
					}
					return 0
				}).
				Export("sse_register")

			b.NewFunctionBuilder().
				WithFunc(func(ctx context.Context, m api.Module, reqPtr, reqLen uint32) uint32 {
					if reqLen == 0 {
						return 1
					}
					data, ok := m.Memory().Read(reqPtr, reqLen)
					if !ok {
						return 2
					}
					req, err := abi.DecodeSSEEmitRequest(data)
					if err != nil {
						return 3
					}
					if err := s.Emit(req); err != nil {
						return 4
					}
					return 0
				}).
				Export("sse_emit")

			return nil
		},
	}
}

// WSInboundCapability returns the Capability that wires ws_register,
// ws_send, and ws_close into the "pulp" host module. Plugins that
// declare transport.ws.inbound use these imports to accept upgrades
// on a given path, push frames to connected clients, and close
// connections.
//
// Host import signatures (all take MessagePack input, return error code):
//
//	ws_register(path_ptr, path_len)
//	  path bytes = raw path string, e.g. "/chat"
//
//	ws_send(req_ptr, req_len)
//	  req bytes = MessagePack WSSendRequest (conn_id, opcode, payload)
//
//	ws_close(req_ptr, req_len)
//	  req bytes = MessagePack WSCloseRequest (conn_id, code, reason)
//
// Error codes: 0 ok, 1 empty input, 2 memory read failed, 3 decode
// failed, 4 dispatch error (unknown conn, unsupported opcode).
func WSInboundCapability(w *WSServer) host.Capability {
	return host.Capability{
		Name: "transport.ws.inbound",
		Register: func(b wazero.HostModuleBuilder, p *host.Plugin) error {
			b.NewFunctionBuilder().
				WithFunc(func(ctx context.Context, m api.Module, pathPtr, pathLen uint32) uint32 {
					if pathLen == 0 {
						return 1
					}
					data, ok := m.Memory().Read(pathPtr, pathLen)
					if !ok {
						return 2
					}
					if err := w.RegisterRoute(string(data)); err != nil {
						return 4
					}
					return 0
				}).
				Export("ws_register")

			b.NewFunctionBuilder().
				WithFunc(func(ctx context.Context, m api.Module, reqPtr, reqLen uint32) uint32 {
					if reqLen == 0 {
						return 1
					}
					data, ok := m.Memory().Read(reqPtr, reqLen)
					if !ok {
						return 2
					}
					req, err := abi.DecodeWSSendRequest(data)
					if err != nil {
						return 3
					}
					if err := w.Send(ctx, req); err != nil {
						return 4
					}
					return 0
				}).
				Export("ws_send")

			b.NewFunctionBuilder().
				WithFunc(func(ctx context.Context, m api.Module, reqPtr, reqLen uint32) uint32 {
					if reqLen == 0 {
						return 1
					}
					data, ok := m.Memory().Read(reqPtr, reqLen)
					if !ok {
						return 2
					}
					req, err := abi.DecodeWSCloseRequest(data)
					if err != nil {
						return 3
					}
					if err := w.Close(req); err != nil {
						return 4
					}
					return 0
				}).
				Export("ws_close")

			return nil
		},
	}
}

// HTTPOutboundCapability returns the Capability that wires http_fetch into
// the "pulp" host module. Plugins call http_fetch with a MessagePack
// HTTPFetchRequest; the host performs the request synchronously, allocates
// a response buffer inside the plugin's linear memory via pulp_alloc,
// writes the MessagePack HTTPResponse there, and stores (ptr, len) at the
// caller-supplied out-addresses.
//
// The plugin is responsible for calling pulp_free(resp_ptr, resp_len)
// once it has decoded the response.
//
// Host import signature:
//
//	http_fetch(req_ptr, req_len, resp_ptr_out, resp_len_out) -> error_code
//
// Error codes: 0 ok, 1 empty input, 2 memory read failed, 3 decode failed,
// 4 fetch failed, 5 encode failed, 6 plugin missing pulp_alloc, 7 alloc
// failed, 8 memory write failed.
func HTTPOutboundCapability(f *Fetcher) host.Capability {
	return host.Capability{
		Name: "transport.http.outbound",
		Register: func(b wazero.HostModuleBuilder, p *host.Plugin) error {
			b.NewFunctionBuilder().
				WithFunc(func(ctx context.Context, m api.Module, reqPtr, reqLen, respPtrOut, respLenOut uint32) uint32 {
					if reqLen == 0 {
						return 1
					}
					data, ok := m.Memory().Read(reqPtr, reqLen)
					if !ok {
						return 2
					}
					req, err := abi.DecodeHTTPFetchRequest(data)
					if err != nil {
						return 3
					}

					resp, err := f.Do(ctx, req)
					if err != nil {
						return 4
					}

					respBytes, err := abi.EncodeHTTPResponse(resp)
					if err != nil {
						return 5
					}

					allocFn := m.ExportedFunction("pulp_alloc")
					if allocFn == nil {
						return 6
					}
					results, err := allocFn.Call(ctx, uint64(len(respBytes)))
					if err != nil || len(results) == 0 {
						return 7
					}
					respPtr := uint32(results[0])
					if respPtr == 0 {
						return 7
					}

					if !m.Memory().Write(respPtr, respBytes) {
						return 8
					}
					if !m.Memory().WriteUint32Le(respPtrOut, respPtr) {
						return 8
					}
					if !m.Memory().WriteUint32Le(respLenOut, uint32(len(respBytes))) {
						return 8
					}
					return 0
				}).
				Export("http_fetch")
			return nil
		},
	}
}
