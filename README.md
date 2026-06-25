# Pulp

A minimal, universal application runtime. Go host, WASM cells, Docker-shaped boundary.

## Status

**v0.4 — storage.** v0.3 stood up transport (HTTP / HTTPS / WebSocket / SSE) on a shared port. v0.4 adds a scoped filesystem and a per-cell SQLite database. The combination is enough to host Hytale-Auth; the existing port lives in `Hytale-Auth/pulp-cell/`.

## Running

```sh
go build -o pulp ./cmd/pulp
./pulp -manifest path/to/pulp.cell.toml
./pulp -manifest path/to/pulp.cell.toml -http-port 9090
```

Send `SIGINT` (Ctrl+C) or `SIGTERM` to shut down. On Windows, `Ctrl+Break` works (the runtime listens for `SIGBREAK` too).

Flags:

- `-manifest` — path to `pulp.cell.toml` (required). May be repeated or comma-separated for multi-cell deployments.
- `-http-port` — HTTP / WS / SSE listener port (default `8080`). Used only when the cell declares an inbound transport capability. Overridden by `HTTP_PORT` env var.
- `-storage-root` — root directory for cell-scoped storage (default `./data`). Each cell gets `{root}/{cell_name}/`.

TLS is controlled by `HTTP_CERT` and `HTTP_KEY` environment variables (paths to PEM files), read by `Pulp-ext-http` during Setup — not by command-line flags.

## Cell contract

A valid cell ships with a `pulp.cell.toml` next to a WASM module. The TOML declares identity, dependencies, capabilities, and a free-form `[config]` table:

```toml
name = "echo"
version = "0.1.0"
wasm = "echo.wasm"                # defaults to cell.wasm if omitted

provides = []
consumes = []
capabilities = ["transport.http.inbound"]

shared_memory_groups = []
dedicated_thread = false
snapshotable = false

[config]
```

The WASM module must export:

```
pulp_alloc(size u32) -> u32                      // returns a pointer into the cell's linear memory
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
payload      bytes            // MessagePack-encoded StepEvent (see below) or empty
```

When an inbound transport event is pending, the payload is a MessagePack-encoded `StepEvent`:

```
StepEvent {
  kind:    string    // "http.request", "ws.open", "ws.frame", "ws.close"
  payload: bytes     // MessagePack-encoded per-kind struct
}
```

## Transport capabilities (v0.3)

Cells opt into transport by listing capabilities in the manifest and calling the matching host imports (from the `pulp` import module).

### `transport.http.inbound`

Cell calls `http_register` during `pulp_init` to declare routes; incoming requests arrive as `StepEvent{kind:"http.request"}`; cell replies with `http_respond`.

```
http_register(req_ptr, req_len) -> error_code     // req = msgpack {method, path}
http_respond (resp_ptr, resp_len) -> error_code    // resp = msgpack HTTPResponse {id, status, headers, body}
```

Path patterns support `:param` segments (e.g. `/echo/:msg`).

### `transport.http.outbound`

Cell calls `http_fetch` to make an outbound HTTP request. The host performs the call synchronously and writes a MessagePack `HTTPResponse` into the cell's linear memory via `pulp_alloc`.

```
http_fetch(req_ptr, req_len, resp_ptr_out, resp_len_out) -> error_code
// req  = msgpack HTTPFetchRequest {method, url, headers, body}
// resp = msgpack HTTPResponse {status, headers, body}
```

### `transport.ws.inbound`

Cell registers a WebSocket path; the host upgrades matching HTTP requests. Connection events (`ws.open`, `ws.frame`, `ws.close`) are delivered through `StepEvent`. Cell sends frames via `ws_send` and closes via `ws_close`.

```
ws_register(path_ptr, path_len)         -> error_code
ws_send    (req_ptr, req_len)           -> error_code  // msgpack {conn_id, opcode, payload}
ws_close   (req_ptr, req_len)           -> error_code  // msgpack {conn_id, code, reason}
```

### `transport.sse`

Cell registers an SSE path; clients that GET the path receive a long-poll event stream. Cell pushes events to every subscriber on that path via `sse_emit`. A 15-second keepalive comment is written automatically.

```
sse_register(path_ptr, path_len) -> error_code
sse_emit    (req_ptr, req_len)   -> error_code  // msgpack {path, id?, event?, data}
```

All four capabilities share a single listener (`-http-port`). The dispatcher routes WS upgrades and SSE long-polls before falling back to HTTP route matching.

## Storage capabilities (v0.4)

Both capabilities confine the cell to `{-storage-root}/{cell_name}/`.

### `storage.fs`

Per-cell scoped filesystem. Absolute paths, `..`, and null bytes are rejected at the host boundary.

```
fs_read  (path_ptr, path_len, data_ptr_out, data_len_out) -> error_code
         // host allocates the result buffer via pulp_alloc and stores (ptr, len) at the out-addresses
fs_write (path_ptr, path_len, data_ptr, data_len)          -> error_code
fs_delete(path_ptr, path_len)                              -> error_code
```

### `storage.sqlite`

Per-cell SQLite database (`{-storage-root}/{cell_name}/data.db`), backed by `modernc.org/sqlite` — pure Go, no CGo.

```
sqlite_exec (query_ptr, query_len, params_ptr, params_len)                             -> error_code
sqlite_query(query_ptr, query_len, params_ptr, params_len, rows_ptr_out, rows_len_out) -> error_code
// params = msgpack []any (empty when no parameters)
// rows   = msgpack [][]any — outer slice is rows, inner slice is column values in declaration order
```

The `[config]` table is encoded to MessagePack (`github.com/vmihailenco/msgpack/v5`) before delivery, so cells in any language can decode it with any MessagePack library.

## Test cells

- `testdata/heartbeat/` — minimal cell for lifecycle / envelope / config verification.
- `testdata/echo/` — HTTP demo cell. Declares `transport.http.inbound`, registers `GET /echo/:msg` and `POST /echo`.

Build either manually:

```sh
cd testdata/echo
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o echo.wasm .
```

The integration tests rebuild them automatically:

```sh
go test ./...
```

## Dependencies

- `github.com/tetratelabs/wazero` — pure-Go WASM runtime.
- `github.com/BurntSushi/toml` — manifest parser.
- `github.com/vmihailenco/msgpack/v5` — envelope and config encoding.
- `github.com/coder/websocket` — inbound WebSocket upgrade and framing.
- `modernc.org/sqlite` — pure-Go SQLite driver (storage.sqlite capability).
