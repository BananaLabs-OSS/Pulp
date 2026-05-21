package abi

import "github.com/vmihailenco/msgpack/v5"

// HTTPRequest is delivered to the cell via the step envelope payload
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
	// RemoteAddr is the peer address as observed by the host (typically
	// the TCP source — "host:port" on IPv4, "[host]:port" on IPv6). The
	// host populates this from the incoming net.Conn so cells can do
	// rate-limiting keyed on the real peer when no proxy headers are
	// present. Empty when the host couldn't determine one.
	RemoteAddr string `msgpack:"remote_addr,omitempty"`
}

// HTTPResponse is produced by the cell and passed to http_respond, or
// returned to the cell by the host as the result of http_fetch. ID is
// meaningful for inbound responses (matches HTTPRequest.ID); for outbound
// fetch results it is zero.
//
// Cookies carries fully-formatted Set-Cookie header values — one per
// entry — so handlers can emit multiple cookies in a single response.
// Headers is a single-valued map and would overwrite duplicates, so any
// Set-Cookie emitted there is merged into Cookies by the host.
type HTTPResponse struct {
	ID      uint64            `msgpack:"id"`
	Status  uint32            `msgpack:"status"`
	Headers map[string]string `msgpack:"headers"`
	Cookies []string          `msgpack:"cookies,omitempty"`
	Body    []byte            `msgpack:"body"`
}

// HTTPFetchRequest is sent by the cell to http_fetch to perform an
// outbound HTTP call. URL must be absolute; Method defaults to GET when
// blank. Headers and Body are optional.
//
// Timeout is the per-request deadline in nanoseconds. Zero means "use the
// default fetch timeout." Non-zero values override the default — applied
// by the fetcher via context.WithTimeout on http.Client.Do. Matches the
// Go time.Duration wire shape on the cell side (int64 nanoseconds).
type HTTPFetchRequest struct {
	Method  string            `msgpack:"method"`
	URL     string            `msgpack:"url"`
	Headers map[string]string `msgpack:"headers"`
	Body    []byte            `msgpack:"body"`
	Timeout int64             `msgpack:"timeout,omitempty"`
}

// DecodeHTTPFetchRequest parses MessagePack bytes produced by the cell.
func DecodeHTTPFetchRequest(data []byte) (HTTPFetchRequest, error) {
	var r HTTPFetchRequest
	err := msgpack.Unmarshal(data, &r)
	return r, err
}

// EncodeHTTPResponse marshals resp to MessagePack for delivery to the
// cell as an outbound fetch result.
func EncodeHTTPResponse(resp HTTPResponse) ([]byte, error) {
	return msgpack.Marshal(resp)
}

// EncodeHTTPRequest marshals req to MessagePack for delivery to the cell.
func EncodeHTTPRequest(req HTTPRequest) ([]byte, error) {
	return msgpack.Marshal(req)
}

// DecodeHTTPResponse parses MessagePack bytes produced by the cell.
func DecodeHTTPResponse(data []byte) (HTTPResponse, error) {
	var r HTTPResponse
	err := msgpack.Unmarshal(data, &r)
	return r, err
}

// HTTPFetchStreamHeader is the response of http_fetch_begin. ID is the
// opaque stream handle the cell passes to http_fetch_read /
// http_fetch_close. Status + Headers come from the live response; the
// body is NOT included — the cell pulls it via repeated reads.
type HTTPFetchStreamHeader struct {
	ID      uint64            `msgpack:"id"`
	Status  uint32            `msgpack:"status"`
	Headers map[string]string `msgpack:"headers"`
}

// EncodeHTTPFetchStreamHeader marshals h for delivery to the cell.
func EncodeHTTPFetchStreamHeader(h HTTPFetchStreamHeader) ([]byte, error) {
	return msgpack.Marshal(h)
}

// DecodeHTTPFetchStreamHeader parses MessagePack bytes from the host.
func DecodeHTTPFetchStreamHeader(data []byte) (HTTPFetchStreamHeader, error) {
	var h HTTPFetchStreamHeader
	err := msgpack.Unmarshal(data, &h)
	return h, err
}

// HTTPFetchChunk is the response of http_fetch_read. Bytes is the data
// (possibly empty if the underlying reader returned 0 bytes without
// error — cell should retry). EOF is true after the final chunk; the
// cell should call http_fetch_close to release the stream afterward.
// Err is a human-readable string set only when a non-EOF read error
// occurred; cells should treat it as terminal.
type HTTPFetchChunk struct {
	Bytes []byte `msgpack:"bytes"`
	EOF   bool   `msgpack:"eof"`
	Err   string `msgpack:"err,omitempty"`
}

// EncodeHTTPFetchChunk marshals c for delivery to the cell.
func EncodeHTTPFetchChunk(c HTTPFetchChunk) ([]byte, error) {
	return msgpack.Marshal(c)
}

// DecodeHTTPFetchChunk parses MessagePack bytes from the host.
func DecodeHTTPFetchChunk(data []byte) (HTTPFetchChunk, error) {
	var c HTTPFetchChunk
	err := msgpack.Unmarshal(data, &c)
	return c, err
}
