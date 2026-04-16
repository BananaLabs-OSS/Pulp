package abi

import "github.com/vmihailenco/msgpack/v5"

// HTTPRequest is delivered to the plugin via the step envelope payload
// when an HTTP request arrives on a route it registered with http_register.
// ID is allocated by the host and must be echoed back in HTTPResponse.
type HTTPRequest struct {
	ID      uint64            `msgpack:"id"`
	Method  string            `msgpack:"method"`
	Path    string            `msgpack:"path"`
	Params  map[string]string `msgpack:"params"`
	Query   map[string]string `msgpack:"query"`
	Headers map[string]string `msgpack:"headers"`
	Body    []byte            `msgpack:"body"`
}

// HTTPResponse is produced by the plugin and passed to http_respond, or
// returned to the plugin by the host as the result of http_fetch. ID is
// meaningful for inbound responses (matches HTTPRequest.ID); for outbound
// fetch results it is zero.
type HTTPResponse struct {
	ID      uint64            `msgpack:"id"`
	Status  uint32            `msgpack:"status"`
	Headers map[string]string `msgpack:"headers"`
	Body    []byte            `msgpack:"body"`
}

// HTTPFetchRequest is sent by the plugin to http_fetch to perform an
// outbound HTTP call. URL must be absolute; Method defaults to GET when
// blank. Headers and Body are optional.
type HTTPFetchRequest struct {
	Method  string            `msgpack:"method"`
	URL     string            `msgpack:"url"`
	Headers map[string]string `msgpack:"headers"`
	Body    []byte            `msgpack:"body"`
}

// DecodeHTTPFetchRequest parses MessagePack bytes produced by the plugin.
func DecodeHTTPFetchRequest(data []byte) (HTTPFetchRequest, error) {
	var r HTTPFetchRequest
	err := msgpack.Unmarshal(data, &r)
	return r, err
}

// EncodeHTTPResponse marshals resp to MessagePack for delivery to the
// plugin as an outbound fetch result.
func EncodeHTTPResponse(resp HTTPResponse) ([]byte, error) {
	return msgpack.Marshal(resp)
}

// EncodeHTTPRequest marshals req to MessagePack for delivery to the plugin.
func EncodeHTTPRequest(req HTTPRequest) ([]byte, error) {
	return msgpack.Marshal(req)
}

// DecodeHTTPResponse parses MessagePack bytes produced by the plugin.
func DecodeHTTPResponse(data []byte) (HTTPResponse, error) {
	var r HTTPResponse
	err := msgpack.Unmarshal(data, &r)
	return r, err
}
