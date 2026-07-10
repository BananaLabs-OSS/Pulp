// runaway — a deliberately hostile test cell for the Supervisor's runaway
// guard. Its pulp_step spins in an unbounded loop the moment it receives an
// event payload (a busy loop with a live memory write so the wasm back-edge is
// real and not optimised away), modelling a cell that hangs or infinite-loops
// inside a call. An IDLE step (nil payload) returns immediately, so the cell
// still boots and behaves like a normal reactor until it is poked — letting a
// test prove that only the runaway call is bounded, not the whole cell.
//
// Build (done automatically by the host harness via BuildCell):
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o cell.wasm .
package main

import (
	"encoding/binary"
	"unsafe"
)

func main() {}

var sink uint64

//go:wasmexport pulp_alloc
func pulpAlloc(size uint32) uint32 {
	if size == 0 {
		return 0
	}
	buf := make([]byte, size)
	return uint32(uintptr(unsafe.Pointer(&buf[0])))
}

//go:wasmexport pulp_free
func pulpFree(ptr, size uint32) { _, _ = ptr, size }

//go:wasmexport pulp_init
func pulpInit(cfgPtr, cfgLen uint32) int32 { return 0 }

//go:wasmexport pulp_step
func pulpStep(inputPtr, inputLen uint32) int32 {
	// Envelope layout (abi.StepEnvelope): [callNumber u64][wallTime u64][payloadLen u32][payload].
	if inputLen < 20 {
		return 0
	}
	header := (*[20]byte)(unsafe.Pointer(uintptr(inputPtr)))
	payloadLen := binary.LittleEndian.Uint32(header[16:20])
	if payloadLen == 0 {
		// Idle tick — behave like a healthy reactor.
		return 0
	}
	// Event delivered — run away. The memory write keeps the loop from being
	// elided and gives wazero a back-edge at which to observe the deadline.
	for {
		sink++
	}
}

//go:wasmexport pulp_shutdown
func pulpShutdown() int32 { return 0 }
