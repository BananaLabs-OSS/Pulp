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
