package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWarmCompilesDiscoveredModulesIntoPersistentCache(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "nested", "cell.wasm")
	if err := os.MkdirAll(filepath.Dir(module), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(module, []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignore.txt"), []byte("not wasm"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(t.TempDir(), "cache")
	if err := warm(context.Background(), cache, []string{root}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("persistent compilation cache is empty")
	}
}

func TestWasmPathsRejectsEmptyInputTree(t *testing.T) {
	if _, err := wasmPaths([]string{t.TempDir()}); err == nil {
		t.Fatal("empty input tree was accepted")
	}
}
