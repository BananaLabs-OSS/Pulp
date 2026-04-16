// Heartbeat — trivial test plugin for Pulp v0.1 validation.
//
// Implements the five exports the host calls (init/step/shutdown plus
// alloc/free for envelope marshaling) and nothing else. No stdio, no
// syscalls — pure WASM computation. The host's structured logger reports
// each call. If init/step/shutdown are all called with the expected
// cadence and return 0, v0.1 is working.
//
// Build:
//
//	GOOS=wasip1 GOARCH=wasm go build -o heartbeat.wasm .
package main

import (
	"encoding/binary"
	"unsafe"
)

func main() {}

// lastSeen is written by pulp_step so the host can sanity-check envelope
// decoding by calling a probe export. Not strictly needed for v0.1 but
// makes debugging easier if the envelope math is wrong.
var (
	lastCall    uint64
	lastWall    uint64
	lastPayload uint32
)

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
	return 0
}

//go:wasmexport pulp_step
func pulpStep(inputPtr uint32, inputLen uint32) int32 {
	if inputLen < 20 {
		return 1
	}
	header := (*[20]byte)(unsafe.Pointer(uintptr(inputPtr)))
	lastCall = binary.LittleEndian.Uint64(header[0:8])
	lastWall = binary.LittleEndian.Uint64(header[8:16])
	lastPayload = binary.LittleEndian.Uint32(header[16:20])
	return 0
}

//go:wasmexport pulp_shutdown
func pulpShutdown() int32 {
	return 0
}

// Probe exports — the host reads these at shutdown to confirm the last step
// envelope was decoded correctly. Not part of the required ABI; just
// diagnostic for v0.1 validation.

//go:wasmexport probe_last_call
func probeLastCall() uint64 { return lastCall }

//go:wasmexport probe_last_wall
func probeLastWall() uint64 { return lastWall }

//go:wasmexport probe_last_payload
func probeLastPayload() uint32 { return lastPayload }
