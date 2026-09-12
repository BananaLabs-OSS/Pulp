package productcli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveProductIsDeterministic(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "cells"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cells", "engine.wasm"), []byte("wasm"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := archiveProduct(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := archiveProduct(root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("identical product trees produced different archives")
	}
}

func TestArchiveProductRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("outside", filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := archiveProduct(root); err == nil {
		t.Fatal("symlink was accepted")
	}
}

func TestActivatePackageReplacesStaleOutput(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "product")
	stage := filepath.Join(root, "stage")
	if err := os.MkdirAll(output, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "stale"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "current"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := activatePackage(stage, output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(output, "current")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(output, "stale")); !os.IsNotExist(err) {
		t.Fatalf("stale output survived: %v", err)
	}
}
