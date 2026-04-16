# Pulp

A minimal, universal application runtime. Go host, WASM plugins, Docker-shaped boundary.

## Status

**v0.2 — runtime + manifest.** Loads a plugin via its `pulp.plugin.toml`, serializes the `[config]` table to MessagePack, calls `pulp_init` with the encoded config, calls `pulp_step` in a loop with the step envelope, calls `pulp_shutdown` on signal, exits cleanly. No transport yet. No dependency resolver yet — each manifest describes one plugin for now.

See `plans/velvet-bouncing-horizon.md` for the full design. This version covers steps 1–2 of the build order.

## Running

```sh
go build -o pulp ./cmd/pulp
./pulp -manifest path/to/pulp.plugin.toml
```

Send `SIGINT` (Ctrl+C) or `SIGTERM` to shut down. On Windows, `Ctrl+Break` works (the runtime listens for `SIGBREAK` too).

## Plugin contract

A valid plugin ships with a `pulp.plugin.toml` next to a WASM module. The TOML declares identity, dependencies, capabilities, and a free-form `[config]` table:

```toml
name = "ant-farm"
version = "0.1.0"
wasm = "ant-farm.wasm"          # defaults to plugin.wasm if omitted

provides = ["game.tick"]
consumes = ["identity.verify"]
capabilities = ["transport.http", "storage.sqlite"]

shared_memory_groups = []
dedicated_thread = false
snapshotable = false

[config]
ant_count = 500
world_size = 100
```

The WASM module must export:

```c
pulp_alloc(size u32) -> u32                      // returns a pointer into the plugin's linear memory
pulp_free(ptr u32, size u32)                     // (optional) frees what pulp_alloc returned
pulp_init(cfg_ptr u32, cfg_len u32) -> i32       // receives MessagePack-encoded [config] bytes
pulp_step(in_ptr u32, in_len u32)  -> i32        // returns an arena handle, 0 = no output
pulp_shutdown()                    -> i32
```

The step envelope passed to `pulp_step` is little-endian:

```
call_number  u64
wall_time    u64  (unix nanoseconds)
payload_len  u32
payload      bytes
```

The `[config]` table is encoded to MessagePack (`github.com/vmihailenco/msgpack/v5`) before delivery, so plugins in any language can decode it with any MessagePack library.

## Test plugin

`testdata/heartbeat/` is a Go-wasip1 plugin that implements every required export plus probes that the integration test uses to verify the envelope and config round-trip. Build it manually with:

```sh
cd testdata/heartbeat
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o heartbeat.wasm .
```

The integration test rebuilds it automatically:

```sh
go test ./...
```

## Dependencies

- `github.com/tetratelabs/wazero` — pure-Go WASM runtime.
- `github.com/BurntSushi/toml` — manifest parser.
- `github.com/vmihailenco/msgpack/v5` — config encoder.
