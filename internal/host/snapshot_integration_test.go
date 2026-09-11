package host

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/internal/manifest"
)

func TestCellSnapshotRestoreABI(t *testing.T) {
	wasm := BuildCell(t, "testdata/snapshot-cell")
	cell, err := Load(context.Background(), &manifest.CellSpec{Name: "snapshot-fixture", WASMPath: wasm}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cell.Close(context.Background()) })
	if !cell.HasSnapshotABI() {
		t.Fatal("snapshot ABI was not detected")
	}
	if err := cell.Init(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	got, err := cell.Snapshot(context.Background())
	if err != nil || string(got) != "initial" {
		t.Fatalf("initial snapshot=%q err=%v", got, err)
	}
	if err := cell.Restore(context.Background(), []byte("migrated")); err != nil {
		t.Fatal(err)
	}
	got, err = cell.Snapshot(context.Background())
	if err != nil || string(got) != "migrated" {
		t.Fatalf("migrated snapshot=%q err=%v", got, err)
	}
	if err := cell.Restore(context.Background(), []byte("restore-error")); err == nil || !strings.Contains(err.Error(), "fixture restore failed") {
		t.Fatalf("restore diagnostic error=%v", err)
	}
	if err := cell.Restore(context.Background(), []byte("snapshot-error")); err != nil {
		t.Fatal(err)
	}
	if _, err := cell.Snapshot(context.Background()); err == nil || !strings.Contains(err.Error(), "fixture snapshot failed") {
		t.Fatalf("snapshot diagnostic error=%v", err)
	}
}
