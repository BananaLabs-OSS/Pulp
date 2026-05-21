package abi

import "github.com/vmihailenco/msgpack/v5"

// SSEEmitRequest is what the cell passes to sse_emit. Path selects the
// SSE route to broadcast on; Data is the event payload. ID and Event are
// optional SSE fields — ID sets "id:", Event sets "event:" before data.
type SSEEmitRequest struct {
	Path  string `msgpack:"path"`
	ID    string `msgpack:"id,omitempty"`
	Event string `msgpack:"event,omitempty"`
	Data  string `msgpack:"data"`
}

// DecodeSSEEmitRequest parses sse_emit input.
func DecodeSSEEmitRequest(data []byte) (SSEEmitRequest, error) {
	var r SSEEmitRequest
	err := msgpack.Unmarshal(data, &r)
	return r, err
}
