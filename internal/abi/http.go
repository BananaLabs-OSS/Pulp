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

// HTTPResponse is produced by the plugin and passed to http_respond. ID
// must match the request being answered; Status defaults to 200 if zero.
type HTTPResponse struct {
	ID      uint64            `msgpack:"id"`
	Status  uint32            `msgpack:"status"`
	Headers map[string]string `msgpack:"headers"`
	Body    []byte            `msgpack:"body"`
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
