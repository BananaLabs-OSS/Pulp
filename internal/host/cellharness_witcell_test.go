package host

// Canonical-ABI (witcell) sibling-call harness. Proves the ADDITIVE graft:
//
//   - NEW PATH: a Fiber-backed witcell cell (testdata/witcell-resolver) that
//     exports pulp_post_return round-trips the full rich ResolveResponse
//     through Cell.CallTyped — the host LIFTS the typed pointer-tree value
//     while it is still pinned, then pulp_post_return tree-frees it, leaving
//     ZERO leaked pins (asserted via the cell's probe_alloc_live).
//
//   - UNCHANGED PATH + GATE: a legacy msgpack Fiber cell (testdata/fiber-msgpack)
//     that does NOT export pulp_post_return still serves a msgpack Provider via
//     the opaque Cell.Call exactly as before, and CallTyped refuses it with
//     ErrNoPostReturn. This is the same opaque path every deployed cell
//     (Evolution, Sessions-Gene, minecraft-resolver) rides — none export
//     pulp_post_return, so none can be pulled onto the new path.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/tetratelabs/wazero/api"
)

// ---- host-side static-layout readers (the witcell-generated LIFT side) ----

func wcRdByte(mem api.Memory, a uint32) byte  { v, _ := mem.ReadByte(a); return v }
func wcRdU32(mem api.Memory, a uint32) uint32 { v, _ := mem.ReadUint32Le(a); return v }
func wcRdU64(mem api.Memory, a uint32) uint64 { v, _ := mem.ReadUint64Le(a); return v }

func wcStr(mem api.Memory, ptr, ln uint32) string {
	if ln == 0 {
		return ""
	}
	b, _ := mem.Read(ptr, ln)
	return string(b)
}
func wcStrSlot(mem api.Memory, a uint32) string {
	return wcStr(mem, wcRdU32(mem, a+0), wcRdU32(mem, a+4))
}
func wcListStr(mem api.Memory, a uint32) []string {
	base := wcRdU32(mem, a+0)
	n := wcRdU32(mem, a+4)
	out := make([]string, n)
	for i := uint32(0); i < n; i++ {
		out[i] = wcStrSlot(mem, base+i*8)
	}
	return out
}

type wcEnvVar struct{ Key, Value string }

func wcListEnv(mem api.Memory, a uint32) []wcEnvVar {
	base := wcRdU32(mem, a+0)
	n := wcRdU32(mem, a+4)
	out := make([]wcEnvVar, n)
	for i := uint32(0); i < n; i++ {
		slot := base + i*16
		out[i] = wcEnvVar{Key: wcStrSlot(mem, slot), Value: wcStrSlot(mem, slot+8)}
	}
	return out
}

type wcResp struct {
	URL                      string
	Plugins, Mods, Datapacks []string
	Cpu, Ram                 float64
	Env                      []wcEnvVar
	ErrArm                   string
	IsErr                    bool
}

// wcLift decodes result<ResolveResponse, string> at recPtr (align 8, size 64).
func wcLift(mem api.Memory, recPtr uint32) (wcResp, error) {
	switch wcRdByte(mem, recPtr+0) {
	case 0:
		return wcResp{
			URL:       wcStrSlot(mem, recPtr+8),
			Plugins:   wcListStr(mem, recPtr+16),
			Mods:      wcListStr(mem, recPtr+24),
			Datapacks: wcListStr(mem, recPtr+32),
			Cpu:       math.Float64frombits(wcRdU64(mem, recPtr+40)),
			Ram:       math.Float64frombits(wcRdU64(mem, recPtr+48)),
			Env:       wcListEnv(mem, recPtr+56),
		}, nil
	case 1:
		return wcResp{IsErr: true, ErrArm: wcStrSlot(mem, recPtr+8)}, nil
	default:
		return wcResp{}, errors.New("invalid result discriminant")
	}
}

func loadTestdataCell(t *testing.T, sourceDir, name string) (*Cell, context.Context) {
	t.Helper()
	wasmPath := BuildCell(t, sourceDir)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	spec := &manifest.CellSpec{
		SchemaVersion: manifest.CurrentSchemaVersion,
		Name:          name,
		Version:       "0.0.0-test",
		WASMPath:      wasmPath,
	}
	cell, err := Load(ctx, spec, nil, nil, logger)
	if err != nil {
		t.Fatalf("load %s cell: %v", name, err)
	}
	t.Cleanup(func() { _ = cell.Close(context.Background()) })
	if err := cell.Init(ctx, nil); err != nil {
		t.Fatalf("init %s cell: %v", name, err)
	}
	return cell, ctx
}

// probeAllocLive reads the cell's probe_alloc_live export (Fiber's pin count).
func probeAllocLive(t *testing.T, ctx context.Context, cell *Cell) uint32 {
	t.Helper()
	cell.mu.Lock()
	defer cell.mu.Unlock()
	fn := cell.module.ExportedFunction("probe_alloc_live")
	if fn == nil {
		t.Fatal("cell missing probe_alloc_live export")
	}
	res, err := fn.Call(ctx)
	if err != nil {
		t.Fatalf("probe_alloc_live: %v", err)
	}
	return uint32(res[0])
}

// TestWitcellCanonicalABI_NewPath drives the Fiber-backed witcell cell through
// the NEW CallTyped path and asserts a byte-correct lift + zero-leak tree-free.
func TestWitcellCanonicalABI_NewPath(t *testing.T) {
	cell, ctx := loadTestdataCell(t, "../../testdata/witcell-resolver", "witcell-resolver")

	if !cell.ExportsPostReturn() {
		t.Fatal("witcell cell must export pulp_post_return")
	}

	base := probeAllocLive(t, ctx, cell)

	// Flat canonical-ABI request (align 4, size 40): engine=Fabric(2), all
	// option<string> discriminants 0 (none). The response tree is rich
	// regardless, so this fully exercises the LIFT + tree-free.
	req := make([]byte, 40)
	req[0] = 2 // Fabric

	var got wcResp
	var liftedLen uint32
	err := cell.CallTyped(ctx, "resolve", req, func(mem api.Memory, respPtr, respLen uint32) error {
		liftedLen = respLen
		r, e := wcLift(mem, respPtr)
		got = r
		return e
	})
	if err != nil {
		t.Fatalf("CallTyped(resolve): %v", err)
	}
	if liftedLen != 64 {
		t.Errorf("respLen = %d, want 64 (result<> record size)", liftedLen)
	}
	if got.IsErr {
		t.Fatalf("unexpected err arm: %s", got.ErrArm)
	}
	if want := "https://cdn.example.net/fabric/1.21.4/server.jar"; got.URL != want {
		t.Errorf("URL = %q, want %q", got.URL, want)
	}
	if len(got.Plugins) != 3 {
		t.Errorf("Plugins = %v, want 3", got.Plugins)
	}
	if len(got.Mods) != 2 {
		t.Errorf("Mods = %v, want 2 (Fabric)", got.Mods)
	}
	if len(got.Datapacks) != 1 {
		t.Errorf("Datapacks = %v, want 1", got.Datapacks)
	}
	if got.Cpu != 1.5 {
		t.Errorf("Cpu = %v, want 1.5", got.Cpu)
	}
	if got.Ram != 4096 {
		t.Errorf("Ram = %v, want 4096 (Fabric)", got.Ram)
	}
	if len(got.Env) != 3 {
		t.Fatalf("Env = %v, want 3", got.Env)
	}
	foundVer := false
	for _, e := range got.Env {
		if e.Key == "MC_VERSION" && e.Value == "1.21.4" {
			foundVer = true
		}
	}
	if !foundVer {
		t.Errorf("Env missing MC_VERSION=1.21.4: %v", got.Env)
	}

	// ZERO LEAK: pulp_post_return walked + freed the whole tree, and CallTyped
	// freed its own name/args/out buffers. Pins must be back to baseline. Pulp's
	// single opaque free would have leaked every string/list sub-buffer here.
	after := probeAllocLive(t, ctx, cell)
	if after != base {
		t.Errorf("pin leak: base=%d after=%d (pulp_post_return must tree-free the whole response)", base, after)
	}
}

// TestWitcellGate_LegacyMsgpackUnchanged proves the additive graft leaves the
// opaque msgpack sibling path byte-for-byte: a cell that does NOT export
// pulp_post_return still serves a Provider via Call, and CallTyped refuses it.
func TestWitcellGate_LegacyMsgpackUnchanged(t *testing.T) {
	cell, ctx := loadTestdataCell(t, "../../testdata/fiber-msgpack", "fiber-msgpack")

	if cell.ExportsPostReturn() {
		t.Fatal("legacy msgpack cell must NOT export pulp_post_return")
	}

	// Gate: the new path refuses a cell without pulp_post_return.
	if err := cell.CallTyped(ctx, "echo.reverse", []byte{1, 2, 3}, nil); !errors.Is(err, ErrNoPostReturn) {
		t.Fatalf("CallTyped on no-post_return cell: got %v, want ErrNoPostReturn", err)
	}

	// Unchanged opaque path: the msgpack Provider still round-trips via Call.
	out, err := cell.Call(ctx, "echo.reverse", []byte{1, 2, 3, 4})
	if err != nil {
		t.Fatalf("Call(echo.reverse): %v", err)
	}
	if want := []byte{4, 3, 2, 1}; !bytes.Equal(out, want) {
		t.Errorf("Call returned %v, want %v (opaque msgpack path unchanged)", out, want)
	}
}
