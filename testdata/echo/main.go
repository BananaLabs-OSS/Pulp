// Echo — demo cell for Pulp v0.3 HTTP inbound. Registers GET /echo/:msg
// and POST /echo at init time, then echoes the request back on step.
//
// Build:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o echo.wasm .
package main

import (
	"encoding/binary"
	"runtime"
	"unsafe"

	"github.com/vmihailenco/msgpack/v5"
)

func main() {}

//go:wasmimport pulp http_register
func hostHTTPRegister(ptr, ln uint32) uint32

//go:wasmimport pulp http_respond
func hostHTTPRespond(ptr, ln uint32) uint32

// Local copies of the host's ABI structs. When the Fiber cell SDK lands
// these will live in a shared package the cell imports instead.

type stepEvent struct {
	Kind    string             `msgpack:"kind"`
	Payload msgpack.RawMessage `msgpack:"payload"`
}

type httpRequest struct {
	ID      uint64            `msgpack:"id"`
	Method  string            `msgpack:"method"`
	Path    string            `msgpack:"path"`
	Params  map[string]string `msgpack:"params"`
	Query   map[string]string `msgpack:"query"`
	Headers map[string]string `msgpack:"headers"`
	Body    []byte            `msgpack:"body"`
}

type httpResponse struct {
	ID      uint64            `msgpack:"id"`
	Status  uint32            `msgpack:"status"`
	Headers map[string]string `msgpack:"headers"`
	Body    []byte            `msgpack:"body"`
}

//go:wasmexport pulp_alloc
func pulpAlloc(size uint32) uint32 {
	if size == 0 {
		return 0
	}
	buf := make([]byte, size)
	return uint32(uintptr(unsafe.Pointer(&buf[0])))
}

//go:wasmexport pulp_free
func pulpFree(ptr uint32, size uint32) {
	_ = ptr
	_ = size
}

//go:wasmexport pulp_init
func pulpInit(configPtr uint32, configLen uint32) int32 {
	_ = configPtr
	_ = configLen
	if code := registerRoute("GET", "/echo/:msg"); code != 0 {
		return int32(100 + code)
	}
	if code := registerRoute("POST", "/echo"); code != 0 {
		return int32(200 + code)
	}
	return 0
}

//go:wasmexport pulp_step
func pulpStep(inputPtr uint32, inputLen uint32) int32 {
	if inputLen < 20 {
		return 1
	}
	raw := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(inputPtr))), inputLen)
	payloadLen := binary.LittleEndian.Uint32(raw[16:20])
	if payloadLen == 0 {
		return 0
	}
	payload := raw[20 : 20+payloadLen]

	var ev stepEvent
	if err := msgpack.Unmarshal(payload, &ev); err != nil {
		return 2
	}
	if ev.Kind != "http.request" {
		return 0
	}

	var req httpRequest
	if err := msgpack.Unmarshal(ev.Payload, &req); err != nil {
		return 3
	}

	resp := handleEcho(req)
	respBytes, err := msgpack.Marshal(resp)
	if err != nil {
		return 4
	}
	code := hostHTTPRespond(uint32(uintptr(unsafe.Pointer(&respBytes[0]))), uint32(len(respBytes)))
	runtime.KeepAlive(respBytes)
	if code != 0 {
		return int32(300 + code)
	}
	return 0
}

//go:wasmexport pulp_shutdown
func pulpShutdown() int32 {
	return 0
}

func registerRoute(method, path string) uint32 {
	reg := struct {
		Method string `msgpack:"method"`
		Path   string `msgpack:"path"`
	}{Method: method, Path: path}
	data, err := msgpack.Marshal(reg)
	if err != nil || len(data) == 0 {
		return 99
	}
	code := hostHTTPRegister(uint32(uintptr(unsafe.Pointer(&data[0]))), uint32(len(data)))
	runtime.KeepAlive(data)
	return code
}

func handleEcho(req httpRequest) httpResponse {
	switch req.Method {
	case "GET":
		msg := req.Params["msg"]
		return httpResponse{
			ID:      req.ID,
			Status:  200,
			Headers: map[string]string{"Content-Type": "text/plain; charset=utf-8"},
			Body:    []byte(msg),
		}
	case "POST":
		return httpResponse{
			ID:      req.ID,
			Status:  200,
			Headers: map[string]string{"Content-Type": "application/octet-stream"},
			Body:    req.Body,
		}
	default:
		return httpResponse{ID: req.ID, Status: 405}
	}
}
