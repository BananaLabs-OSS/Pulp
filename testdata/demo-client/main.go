// demo-client is a minimal WASM cell that exercises a compile-time
// extension: it calls demo_greet during pulp_init. Boot this under a
// Pulp binary that blank-imports github.com/BananaLabs-OSS/Pulp/ext/demo
// (e.g. ./cmd/pulp-demo) and you should see a greeting on stdout.
package main

import (
	"runtime"
	"unsafe"

	"github.com/vmihailenco/msgpack/v5"
)

func main() {}

//go:wasmimport pulp demo_greet
func hostDemoGreet(ptr, ln uint32) uint32

//go:wasmexport pulp_alloc
func pulpAlloc(size uint32) uint32 {
	if size == 0 {
		return 0
	}
	buf := make([]byte, size)
	return uint32(uintptr(unsafe.Pointer(&buf[0])))
}

//go:wasmexport pulp_free
func pulpFree(ptr, size uint32) { _ = ptr; _ = size }

//go:wasmexport pulp_init
func pulpInit(cfgPtr, cfgLen uint32) int32 {
	name := "world"
	if cfgLen > 0 {
		raw := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(cfgPtr))), cfgLen)
		var cfg struct {
			Name string `msgpack:"name"`
		}
		if err := msgpack.Unmarshal(raw, &cfg); err == nil && cfg.Name != "" {
			name = cfg.Name
		}
	}

	nameBytes := []byte(name)
	code := hostDemoGreet(uint32(uintptr(unsafe.Pointer(&nameBytes[0]))), uint32(len(nameBytes)))
	runtime.KeepAlive(nameBytes)
	if code != 0 {
		return int32(code)
	}
	return 0
}

//go:wasmexport pulp_step
func pulpStep(_, _ uint32) int32 { return 0 }

//go:wasmexport pulp_shutdown
func pulpShutdown() int32 { return 0 }
