package storage

import (
	"context"
	"errors"
	"os"

	"github.com/BananaLabs-OSS/Pulp/internal/host"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
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
