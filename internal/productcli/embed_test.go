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
