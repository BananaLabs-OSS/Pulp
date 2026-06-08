package host

// Full-fidelity proof that the projx projectional-editor engine runs as a real
// Pulp cell — loaded through the actual host (Load/Init/Call), not a bespoke
// wazero harness. The cell declares NO capabilities (parsing + dst rewriting are
// pure in-memory compute), so unlike the Sessions-Gene harness this needs no DB,
// no capability stubs, no seeding — just BuildCell -> Load -> Init -> Call.
//
// Nicely self-referential: it mutates Pulp's own internal/safe/safe.go by
// driving the cell's "editor.mutate" over pulp_on_call, and asserts the edit
// (reorder two funcs + rename a symbol) round-tripped losslessly — comments
// preserved, code rename complete — across the WASM boundary, inside Pulp's host.
//
// NO projx import: the cell's wire contract is two msgpack structs, mirrored
// here (importing projx would add it to Pulp's go.mod for no reason).

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
	"github.com/vmihailenco/msgpack/v5"
)

func projxCellSourceDir() string {
	// Pulp/internal/host -> ../../../projx/cell
	return filepath.Join("..", "..", "..", "projx", "cell")
}

type projxMutateReq struct {
	Src   []byte `msgpack:"src"`
	Old   string `msgpack:"old"`
	New   string `msgpack:"new"`
	FuncA string `msgpack:"func_a"`
	FuncB string `msgpack:"func_b"`
}

type projxMutateResp struct {
	Source         []byte `msgpack:"source"`
	Lossless       bool   `msgpack:"lossless"`
	Reordered      bool   `msgpack:"reordered"`
	RenamedIdents  int    `msgpack:"renamed_idents"`
	CommentsBefore int    `msgpack:"comments_before"`
	CommentsAfter  int    `msgpack:"comments_after"`
	CodeRefsOld    int    `msgpack:"code_refs_old"`
	ProseOld       int    `msgpack:"prose_old"`
}

type projxProjectReq struct {
	Name string `msgpack:"name"`
	Src  []byte `msgpack:"src"`
	Func string `msgpack:"func"`
}

type projxProjectResp struct {
	Nodes   int `msgpack:"nodes"`
	Control int `msgpack:"control"`
	Depth   int `msgpack:"depth"`
}

func loadProjxCell(t *testing.T) *Cell {
	t.Helper()
	wasmPath := BuildCell(t, projxCellSourceDir())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	spec := &manifest.CellSpec{
		SchemaVersion: manifest.CurrentSchemaVersion,
		Name:          "projx",
		Version:       "0.0.0-test",
		// No Capabilities: the cell makes no host calls.
		WASMPath: wasmPath,
	}
	cfgBytes, err := manifest.EncodeConfig(nil)
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cell, err := Load(ctx, spec, NewRegistry(), nil, logger)
	if err != nil {
		cancel()
		t.Fatalf("load projx cell: %v", err)
	}
	if err := cell.Init(ctx, cfgBytes); err != nil {
		_ = cell.Close(context.Background())
		cancel()
		t.Fatalf("init projx cell: %v", err)
	}
	t.Cleanup(func() {
		_ = cell.Shutdown(context.Background())
		_ = cell.Close(context.Background())
		cancel()
	})
	return cell
}

func TestProjxCell_MutateLosslessInRealHost(t *testing.T) {
	cell := loadProjxCell(t)

	src, err := os.ReadFile(filepath.Join("..", "safe", "safe.go"))
	if err != nil {
		t.Fatalf("read safe.go: %v", err)
	}

	args, err := msgpack.Marshal(projxMutateReq{
		Src: src, Old: "ErrExtensionPanic", New: "PanicError", FuncA: "HostFunc", FuncB: "RecoverHost",
	})
	if err != nil {
		t.Fatalf("marshal mutate req: %v", err)
	}

	out, err := cell.Call(context.Background(), "editor.mutate", args)
	if err != nil {
		t.Fatalf("cell.Call(editor.mutate): %v", err)
	}
	var resp projxMutateResp
	if err := msgpack.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal mutate resp: %v", err)
	}

	if !resp.Lossless {
		t.Fatalf("mutation NOT lossless: %+v", resp)
	}
	if !resp.Reordered {
		t.Fatal("reorder not applied")
	}
	if resp.RenamedIdents != 3 {
		t.Fatalf("renamed idents: want 3, got %d", resp.RenamedIdents)
	}
	if resp.CommentsBefore != resp.CommentsAfter {
		t.Fatalf("comments dropped across mutation: %d -> %d", resp.CommentsBefore, resp.CommentsAfter)
	}
	if resp.CodeRefsOld != 0 {
		t.Fatalf("old symbol still referenced in CODE after rename: %d", resp.CodeRefsOld)
	}
	if len(resp.Source) == 0 {
		t.Fatal("no mutated source returned")
	}

	t.Logf("real-host editor.mutate OK: lossless=%v reordered=%v renamed=%d comments=%d->%d codeRefsOld=%d proseOld=%d bytes=%d",
		resp.Lossless, resp.Reordered, resp.RenamedIdents, resp.CommentsBefore, resp.CommentsAfter,
		resp.CodeRefsOld, resp.ProseOld, len(resp.Source))
}

func TestProjxCell_ProjectReadSideInRealHost(t *testing.T) {
	cell := loadProjxCell(t)

	src, err := os.ReadFile(filepath.Join("..", "safe", "safe.go"))
	if err != nil {
		t.Fatalf("read safe.go: %v", err)
	}
	args, err := msgpack.Marshal(projxProjectReq{Name: "safe.go", Src: src, Func: "HostFunc"})
	if err != nil {
		t.Fatalf("marshal project req: %v", err)
	}

	out, err := cell.Call(context.Background(), "editor.project", args)
	if err != nil {
		t.Fatalf("cell.Call(editor.project): %v", err)
	}
	var resp projxProjectResp
	if err := msgpack.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal project resp: %v", err)
	}
	if resp.Nodes < 1 {
		t.Fatalf("projection returned no nodes: %+v", resp)
	}
	t.Logf("real-host editor.project HostFunc: nodes=%d control=%d depth=%d", resp.Nodes, resp.Control, resp.Depth)
}
