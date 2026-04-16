# Pulp

A minimal, universal application runtime. Go host, WASM plugins, Docker-shaped boundary.

## Status

**v0.1 — runtime core only.** Loads a WASM file, calls `pulp_init`, calls `pulp_step` in a loop with the step envelope, calls `pulp_shutdown` on signal, exits cleanly. No plugins included. No transport. No manifests yet.

See `plans/velvet-bouncing-horizon.md` for the full design. This version only implements step 1 of the build order.

## Running

```sh
go build -o pulp ./cmd/pulp
./pulp -plugin path/to/plugin.wasm
```

Send `SIGINT` (Ctrl+C) or `SIGTERM` to shut down. On Windows, `Ctrl+Break` works (the runtime listens for `SIGBREAK` too).

## Plugin contract (v0.1)

A valid v0.1 plugin is a WebAssembly module exporting:

```c
pulp_alloc(size u32) -> u32            // returns a pointer into the plugin's linear memory
pulp_free(ptr u32, size u32)           // (optional) frees what pulp_alloc returned
pulp_init(cfg_ptr u32, cfg_len u32) -> i32
pulp_step(in_ptr u32, in_len u32)  -> i32       // returns an arena handle, 0 = no output
pulp_shutdown()                    -> i32
```

The step envelope passed to `pulp_step` is little-endian:

```
call_number  u64
wall_time    u64  (unix nanoseconds)
payload_len  u32
payload      bytes
```

## Test plugin

`testdata/heartbeat/` is a Go-wasip1 plugin that implements all five exports and returns 0 from every call. Build:

```sh
cd testdata/heartbeat
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o heartbeat.wasm .
```

Then run the host against it. An end-to-end lifecycle test lives in `cmd/pulp/integration_test.go`:

```sh
go test ./cmd/pulp
```

## Dependencies

- `github.com/tetratelabs/wazero` — pure-Go WASM runtime.

That's it.
