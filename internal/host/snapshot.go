package host

import (
	"context"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// ErrNoSnapshotABI means the cell does not opt into live state migration.
var ErrNoSnapshotABI = errors.New("cell does not export the Pulp snapshot ABI")

func exactSignature(fn api.Function, params, results []api.ValueType) bool {
	if fn == nil {
		return false
	}
	d := fn.Definition()
	return equalValueTypes(d.ParamTypes(), params) && equalValueTypes(d.ResultTypes(), results)
}

func equalValueTypes(a, b []api.ValueType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (p *Cell) validateSnapshotABI() error {
	present := p.snapshotFn != nil || p.restoreFn != nil
	if !present {
		return nil
	}
	if p.snapshotFn == nil || p.restoreFn == nil {
		return errors.New("snapshot ABI requires both pulp_snapshot and pulp_restore")
	}
	i32 := api.ValueTypeI32
	if !exactSignature(p.snapshotFn, []api.ValueType{i32}, []api.ValueType{i32}) {
		return errors.New("pulp_snapshot must have signature (i32) -> i32")
	}
	if !exactSignature(p.restoreFn, []api.ValueType{i32, i32}, []api.ValueType{i32}) {
		return errors.New("pulp_restore must have signature (i32, i32) -> i32")
	}
	for name, pair := range map[string][2]api.Function{
		"snapshot": {p.snapshotErrorPtrFn, p.snapshotErrorLenFn},
		"restore":  {p.restoreErrorPtrFn, p.restoreErrorLenFn},
	} {
		if (pair[0] == nil) != (pair[1] == nil) {
			return fmt.Errorf("%s diagnostic ABI requires both pointer and length exports", name)
		}
		for _, fn := range pair {
			if fn != nil && !exactSignature(fn, nil, []api.ValueType{i32}) {
				return fmt.Errorf("%s diagnostic exports must have signature () -> i32", name)
			}
		}
	}
	return nil
}

// HasSnapshotABI reports whether the cell supports bounded snapshot and
// restore across the Wasm boundary.
func (p *Cell) HasSnapshotABI() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshotFn != nil && p.restoreFn != nil
}

// Snapshot invokes pulp_snapshot. The guest writes a (pointer,length) pair
// into the supplied eight-byte out area. The host copies and frees guest
// memory before returning, so no guest pointer escapes this call.
func (p *Cell) Snapshot(ctx context.Context) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.snapshotFn == nil || p.restoreFn == nil {
		return nil, ErrNoSnapshotABI
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	outPtr, err := p.alloc(callCtx, 8)
	if err != nil {
		return nil, fmt.Errorf("allocate snapshot out-params: %w", err)
	}
	defer p.free(callCtx, outPtr, 8)
	results, err := p.snapshotFn.Call(callCtx, uint64(outPtr))
	if err != nil {
		return nil, fmt.Errorf("pulp_snapshot trap: %w", err)
	}
	if code := int32(results[0]); code != 0 {
		if diagnostic := p.snapshotDiagnostic(callCtx, p.snapshotErrorPtrFn, p.snapshotErrorLenFn); diagnostic != "" {
			return nil, fmt.Errorf("pulp_snapshot returned %d: %s", code, diagnostic)
		}
		return nil, fmt.Errorf("pulp_snapshot returned %d", code)
	}
	ptr, ok := p.module.Memory().ReadUint32Le(outPtr)
	if !ok {
		return nil, errors.New("read snapshot pointer failed")
	}
	size, ok := p.module.Memory().ReadUint32Le(outPtr + 4)
	if !ok {
		return nil, errors.New("read snapshot length failed")
	}
	if size > DefaultMaxMessageBytes {
		if ptr != 0 {
			p.free(callCtx, ptr, size)
		}
		return nil, ErrMessageTooLarge
	}
	if size == 0 {
		if ptr != 0 {
			p.free(callCtx, ptr, 0)
		}
		return nil, nil
	}
	if ptr == 0 {
		return nil, errors.New("snapshot returned a null pointer with non-zero length")
	}
	raw, ok := p.module.Memory().Read(ptr, size)
	if !ok {
		p.free(callCtx, ptr, size)
		return nil, errors.New("snapshot bytes are outside guest memory")
	}
	out := append([]byte(nil), raw...)
	p.free(callCtx, ptr, size)
	return out, nil
}

// Restore copies snapshot bytes into guest-owned memory only for the duration
// of pulp_restore. Empty snapshots use the canonical null/zero pair.
func (p *Cell) Restore(ctx context.Context, snapshot []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.snapshotFn == nil || p.restoreFn == nil {
		return ErrNoSnapshotABI
	}
	if len(snapshot) > DefaultMaxMessageBytes {
		return ErrMessageTooLarge
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	ptr, err := p.writeBytes(callCtx, snapshot)
	if err != nil {
		return fmt.Errorf("write restore snapshot: %w", err)
	}
	defer p.free(callCtx, ptr, uint32(len(snapshot)))
	results, err := p.restoreFn.Call(callCtx, uint64(ptr), uint64(len(snapshot)))
	if err != nil {
		return fmt.Errorf("pulp_restore trap: %w", err)
	}
	if code := int32(results[0]); code != 0 {
		if diagnostic := p.snapshotDiagnostic(callCtx, p.restoreErrorPtrFn, p.restoreErrorLenFn); diagnostic != "" {
			return fmt.Errorf("pulp_restore returned %d: %s", code, diagnostic)
		}
		return fmt.Errorf("pulp_restore returned %d", code)
	}
	return nil
}

func (p *Cell) snapshotDiagnostic(ctx context.Context, ptrFn, lenFn api.Function) string {
	if ptrFn == nil || lenFn == nil || p.module == nil || p.module.Memory() == nil {
		return ""
	}
	ptrResult, err := ptrFn.Call(ctx)
	if err != nil || len(ptrResult) != 1 {
		return ""
	}
	lenResult, err := lenFn.Call(ctx)
	if err != nil || len(lenResult) != 1 {
		return ""
	}
	ptr, size := uint32(ptrResult[0]), uint32(lenResult[0])
	if size == 0 || size > 4096 {
		return ""
	}
	b, ok := p.module.Memory().Read(ptr, size)
	if !ok {
		return ""
	}
	return string(append([]byte(nil), b...))
}
