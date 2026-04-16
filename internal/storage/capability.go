package storage

import (
	"context"
	"errors"
	"os"

	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/vmihailenco/msgpack/v5"
)

// FSCapability returns the Capability that wires fs_read, fs_write, and
// fs_delete into the "pulp" host module. The plugin sees a single
// scoped root — all paths are relative; escape attempts fail at the
// host boundary before any syscall.
//
// Host import signatures:
//
//	fs_read(path_ptr, path_len, data_ptr_out, data_len_out) -> error_code
//	  Reads the file; allocates the result buffer via pulp_alloc and
//	  stores (ptr, len) at the out-addresses.
//
//	fs_write(path_ptr, path_len, data_ptr, data_len) -> error_code
//
//	fs_delete(path_ptr, path_len) -> error_code
//
// Error codes: 0 ok, 1 empty input, 2 memory read failed, 4 path
// invalid / escape, 5 fs operation failed, 6 not found, 7 alloc failed,
// 8 memory write failed.
func FSCapability(fs *FS) host.Capability {
	return host.Capability{
		Name: "storage.fs",
		Stub: func(b wazero.HostModuleBuilder, _ *host.Plugin) error {
			b.NewFunctionBuilder().
				WithFunc(func(_ context.Context, _ api.Module, _, _, _, _ uint32) uint32 { return 99 }).
				Export("fs_read")
			b.NewFunctionBuilder().
				WithFunc(func(_ context.Context, _ api.Module, _, _, _, _ uint32) uint32 { return 99 }).
				Export("fs_write")
			b.NewFunctionBuilder().
				WithFunc(func(_ context.Context, _ api.Module, _, _ uint32) uint32 { return 99 }).
				Export("fs_delete")
			return nil
		},
		Register: func(b wazero.HostModuleBuilder, p *host.Plugin) error {
			b.NewFunctionBuilder().
				WithFunc(func(ctx context.Context, m api.Module, pathPtr, pathLen, dataPtrOut, dataLenOut uint32) uint32 {
					if pathLen == 0 {
						return 1
					}
					pathData, ok := m.Memory().Read(pathPtr, pathLen)
					if !ok {
						return 2
					}
					content, err := fs.Read(string(pathData))
					if err != nil {
						if errors.Is(err, os.ErrNotExist) {
							return 6
						}
						return fsErrCode(err)
					}

					allocFn := m.ExportedFunction("pulp_alloc")
					if allocFn == nil {
						return 7
					}
					var dataPtr uint32
					if len(content) > 0 {
						results, err := allocFn.Call(ctx, uint64(len(content)))
						if err != nil || len(results) == 0 {
							return 7
						}
						dataPtr = uint32(results[0])
						if dataPtr == 0 {
							return 7
						}
						if !m.Memory().Write(dataPtr, content) {
							return 8
						}
					}
					if !m.Memory().WriteUint32Le(dataPtrOut, dataPtr) {
						return 8
					}
					if !m.Memory().WriteUint32Le(dataLenOut, uint32(len(content))) {
						return 8
					}
					return 0
				}).
				Export("fs_read")

			b.NewFunctionBuilder().
				WithFunc(func(ctx context.Context, m api.Module, pathPtr, pathLen, dataPtr, dataLen uint32) uint32 {
					if pathLen == 0 {
						return 1
					}
					pathData, ok := m.Memory().Read(pathPtr, pathLen)
					if !ok {
						return 2
					}
					var content []byte
					if dataLen > 0 {
						content, ok = m.Memory().Read(dataPtr, dataLen)
						if !ok {
							return 2
						}
					}
					if err := fs.Write(string(pathData), content); err != nil {
						return fsErrCode(err)
					}
					return 0
				}).
				Export("fs_write")

			b.NewFunctionBuilder().
				WithFunc(func(ctx context.Context, m api.Module, pathPtr, pathLen uint32) uint32 {
					if pathLen == 0 {
						return 1
					}
					pathData, ok := m.Memory().Read(pathPtr, pathLen)
					if !ok {
						return 2
					}
					if err := fs.Delete(string(pathData)); err != nil {
						if errors.Is(err, os.ErrNotExist) {
							return 6
						}
						return fsErrCode(err)
					}
					return 0
				}).
				Export("fs_delete")

			return nil
		},
	}
}

// SQLiteCapability returns the Capability that wires sqlite_exec and
// sqlite_query into the "pulp" host module. One database per plugin
// lives at the host's chosen path.
//
// Host import signatures:
//
//	sqlite_exec(query_ptr, query_len, params_ptr, params_len,
//	            res_ptr_out, res_len_out) -> error_code
//	  query   = SQL string bytes
//	  params  = MessagePack []any, or empty when no parameters
//	  result  = MessagePack ExecResult {rows_affected, last_insert_id}
//	            written to plugin memory via pulp_alloc; pointer and
//	            length are stored at the caller-supplied out-addresses.
//
//	sqlite_query(query_ptr, query_len, params_ptr, params_len,
//	             rows_ptr_out, rows_len_out) -> error_code
//	  rows bytes (written to plugin memory via pulp_alloc) =
//	    MessagePack [][]any — outer slice is rows, inner slice is
//	    column values in declaration order.
//
// Error codes: 0 ok, 1 empty query, 2 memory read failed, 3 params
// decode failed, 5 sql error, 7 alloc failed, 8 memory write failed.
func SQLiteCapability(s *SQLite) host.Capability {
	return host.Capability{
		Name: "storage.sqlite",
		Stub: func(b wazero.HostModuleBuilder, _ *host.Plugin) error {
			b.NewFunctionBuilder().
				WithFunc(func(_ context.Context, _ api.Module, _, _, _, _, _, _ uint32) uint32 { return 99 }).
				Export("sqlite_exec")
			b.NewFunctionBuilder().
				WithFunc(func(_ context.Context, _ api.Module, _, _, _, _, _, _ uint32) uint32 { return 99 }).
				Export("sqlite_query")
			return nil
		},
		Register: func(b wazero.HostModuleBuilder, p *host.Plugin) error {
			b.NewFunctionBuilder().
				WithFunc(func(ctx context.Context, m api.Module, qPtr, qLen, pPtr, pLen, resPtrOut, resLenOut uint32) uint32 {
					if qLen == 0 {
						return 1
					}
					q, ok := m.Memory().Read(qPtr, qLen)
					if !ok {
						return 2
					}
					args, code := decodeArgs(m, pPtr, pLen)
					if code != 0 {
						return code
					}
					result, err := s.Exec(ctx, string(q), args)
					if err != nil {
						return 5
					}
					encoded, err := msgpack.Marshal(result)
					if err != nil {
						return 5
					}
					allocFn := m.ExportedFunction("pulp_alloc")
					if allocFn == nil {
						return 7
					}
					var ptr uint32
					if len(encoded) > 0 {
						res, err := allocFn.Call(ctx, uint64(len(encoded)))
						if err != nil || len(res) == 0 {
							return 7
						}
						ptr = uint32(res[0])
						if ptr == 0 {
							return 7
						}
						if !m.Memory().Write(ptr, encoded) {
							return 8
						}
					}
					if !m.Memory().WriteUint32Le(resPtrOut, ptr) {
						return 8
					}
					if !m.Memory().WriteUint32Le(resLenOut, uint32(len(encoded))) {
						return 8
					}
					return 0
				}).
				Export("sqlite_exec")

			b.NewFunctionBuilder().
				WithFunc(func(ctx context.Context, m api.Module, qPtr, qLen, pPtr, pLen, rowsPtrOut, rowsLenOut uint32) uint32 {
					if qLen == 0 {
						return 1
					}
					q, ok := m.Memory().Read(qPtr, qLen)
					if !ok {
						return 2
					}
					args, code := decodeArgs(m, pPtr, pLen)
					if code != 0 {
						return code
					}
					result, err := s.Query(ctx, string(q), args)
					if err != nil {
						return 5
					}
					encoded, err := msgpack.Marshal(result)
					if err != nil {
						return 5
					}
					allocFn := m.ExportedFunction("pulp_alloc")
					if allocFn == nil {
						return 7
					}
					var ptr uint32
					if len(encoded) > 0 {
						res, err := allocFn.Call(ctx, uint64(len(encoded)))
						if err != nil || len(res) == 0 {
							return 7
						}
						ptr = uint32(res[0])
						if ptr == 0 {
							return 7
						}
						if !m.Memory().Write(ptr, encoded) {
							return 8
						}
					}
					if !m.Memory().WriteUint32Le(rowsPtrOut, ptr) {
						return 8
					}
					if !m.Memory().WriteUint32Le(rowsLenOut, uint32(len(encoded))) {
						return 8
					}
					return 0
				}).
				Export("sqlite_query")

			return nil
		},
	}
}

func decodeArgs(m api.Module, ptr, ln uint32) ([]any, uint32) {
	if ln == 0 {
		return nil, 0
	}
	data, ok := m.Memory().Read(ptr, ln)
	if !ok {
		return nil, 2
	}
	var args []any
	if err := msgpack.Unmarshal(data, &args); err != nil {
		return nil, 3
	}
	return args, 0
}

func fsErrCode(err error) uint32 {
	if err == nil {
		return 0
	}
	msg := err.Error()
	switch {
	case stringContains(msg, "absolute path"), stringContains(msg, "escapes root"), stringContains(msg, "null byte"), stringContains(msg, "empty path"):
		return 4
	default:
		return 5
	}
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
