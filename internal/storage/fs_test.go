package storage

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func newFS(t *testing.T) *FS {
	t.Helper()
	fs, err := NewFS(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fs
}

func TestFS_ReadWrite(t *testing.T) {
	fs := newFS(t)
	if err := fs.Write("token.txt", []byte("secret")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := fs.Read("token.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "secret" {
		t.Errorf("Read = %q, want secret", got)
	}
}

func TestFS_CreatesParentDirs(t *testing.T) {
	fs := newFS(t)
	if err := fs.Write("nested/dir/token.txt", []byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := fs.Read("nested/dir/token.txt")
	if err != nil || string(got) != "x" {
		t.Errorf("nested read: got=%q err=%v", got, err)
	}
}

func TestFS_Delete(t *testing.T) {
	fs := newFS(t)
	if err := fs.Write("x.txt", []byte("gone")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := fs.Delete("x.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := fs.Read("x.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Read after Delete err = %v, want ErrNotExist", err)
	}
}

func TestFS_RejectsEscape(t *testing.T) {
	fs := newFS(t)
	cases := []string{
		"../outside.txt",
		"a/../../outside.txt",
		"/absolute/path",
		"with\x00null",
		"",
	}
	for _, p := range cases {
		if _, err := fs.Read(p); err == nil {
			t.Errorf("Read(%q) expected error", p)
		}
		if err := fs.Write(p, []byte("x")); err == nil {
			t.Errorf("Write(%q) expected error", p)
		}
	}
}

func TestFS_EscapeAttemptDoesNotReachDisk(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "outside.txt")

	fs, err := NewFS(root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if err := fs.Write("../outside.txt", []byte("gotcha")); err == nil {
		t.Error("expected error for escape write")
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("escape write reached disk")
	}
}
