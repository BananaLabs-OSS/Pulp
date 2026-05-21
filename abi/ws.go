package abi

import "github.com/vmihailenco/msgpack/v5"

// WebSocket frame opcodes delivered to and accepted from cells.
const (
	WSOpCodeText   uint8 = 1
	WSOpCodeBinary uint8 = 2
)

// WSOpen is the ws.open event — one per accepted connection. ConnID is
// the host-assigned connection identifier cells pass back to ws_send
// and ws_close.
type WSOpen struct {
	ConnID  uint64            `msgpack:"conn_id"`
	Path    string            `msgpack:"path"`
	Query   map[string]string `msgpack:"query"`
	Headers map[string]string `msgpack:"headers"`
}

// WSFrame is the ws.frame event — one per inbound frame.
type WSFrame struct {
	ConnID  uint64 `msgpack:"conn_id"`
	OpCode  uint8  `msgpack:"opcode"`
	Payload []byte `msgpack:"payload"`
}

// WSClose is the ws.close event — one per disconnect, whether initiated
// by the client, the cell, or the host.
type WSClose struct {
	ConnID uint64 `msgpack:"conn_id"`
	Code   uint16 `msgpack:"code"`
	Reason string `msgpack:"reason"`
}

// WSSendRequest is what the cell passes to ws_send: which connection,
// what opcode, the payload bytes.
type WSSendRequest struct {
	ConnID  uint64 `msgpack:"conn_id"`
	OpCode  uint8  `msgpack:"opcode"`
	Payload []byte `msgpack:"payload"`
}

// WSCloseRequest is what the cell passes to ws_close.
type WSCloseRequest struct {
	ConnID uint64 `msgpack:"conn_id"`
	Code   uint16 `msgpack:"code"`
	Reason string `msgpack:"reason"`
}

// EncodeWSOpen marshals a WSOpen event for delivery via StepEvent.
func EncodeWSOpen(e WSOpen) ([]byte, error) { return msgpack.Marshal(e) }

// EncodeWSFrame marshals a WSFrame event for delivery via StepEvent.
func EncodeWSFrame(e WSFrame) ([]byte, error) { return msgpack.Marshal(e) }

// EncodeWSClose marshals a WSClose event for delivery via StepEvent.
func EncodeWSClose(e WSClose) ([]byte, error) { return msgpack.Marshal(e) }

// DecodeWSSendRequest parses ws_send input.
func DecodeWSSendRequest(data []byte) (WSSendRequest, error) {
	var r WSSendRequest
	err := msgpack.Unmarshal(data, &r)
	return r, err
}

// DecodeWSCloseRequest parses ws_close input.
func DecodeWSCloseRequest(data []byte) (WSCloseRequest, error) {
	var r WSCloseRequest
	err := msgpack.Unmarshal(data, &r)
	return r, err
}
