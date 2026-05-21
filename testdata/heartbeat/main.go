// Heartbeat — trivial test cell for Pulp v0.2 validation.
//
// Implements the five exports the host calls (init/step/shutdown plus
// alloc/free for envelope marshaling) and a set of probe exports used by
// the integration test to verify envelope decoding and config delivery.
//
// Build:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o heartbeat.wasm .
package main

import (
	"encoding/binary"
	"unsafe"

	"github.com/vmihailenco/msgpack/v5"
)

func main() {}

var (
	lastCall      uint64
	lastWall      uint64
	lastPayload   uint32
	configMarker  int64
	configGreeted bool
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
	if configLen == 0 {
		return 0
	}
	raw := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(configPtr))), configLen)
	cfg := make(map[string]any)
	if err := msgpack.Unmarshal(raw, &cfg); err != nil {
		return 2
	}
	if v, ok := cfg["marker"].(int64); ok {
		configMarker = v
	}
	if _, ok := cfg["greeting"].(string); ok {
		configGreeted = true
	}
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

// Probe exports — read by the integration test to confirm envelope and
// config decoding round-tripped correctly. Not part of the required ABI.

//go:wasmexport probe_last_call
func probeLastCall() uint64 { return lastCall }

//go:wasmexport probe_last_wall
func probeLastWall() uint64 { return lastWall }

//go:wasmexport probe_last_payload
func probeLastPayload() uint32 { return lastPayload }

//go:wasmexport probe_config_marker
func probeConfigMarker() int64 { return configMarker }

//go:wasmexport probe_config_greeted
func probeConfigGreeted() uint32 {
	if configGreeted {
		return 1
	}
	return 0
}
