// Package storage implements Pulp's storage primitives. v0.4 begins with
// a scoped filesystem (FS) that confines a plugin to a single root
// directory — all paths are relative, and traversal attempts (../,
// absolute paths, null bytes) are rejected before any syscall fires.
package storage

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// FS is a per-plugin scoped filesystem. Every path passed to its methods
// is resolved against Root; the result is required to remain underneath
// Root or the call fails. One FS per plugin — never share across plugins.
type FS struct {
	root   string
	logger *slog.Logger
}

// NewFS creates the root directory if missing and returns an FS rooted
// there. Root is resolved to an absolute path so later prefix checks
// are robust to relative calls or working-directory changes.
func NewFS(root string, logger *slog.Logger) (*FS, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create root: %w", err)
	}
	return &FS{root: abs, logger: logger}, nil
}

// Root returns the absolute root directory. Diagnostic only — plugins
// never see this path.
func (f *FS) Root() string { return f.root }

// Read returns the bytes at rel. rel must be a relative path below the
// root; absolute paths, "..", and null bytes are rejected.
func (f *FS) Read(rel string) ([]byte, error) {
	abs, err := f.resolve(rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(abs)
}

// Write writes data to rel, creating any missing parent directories
// under the root. Existing files are truncated.
func (f *FS) Write(rel string, data []byte) error {
	abs, err := f.resolve(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	return os.WriteFile(abs, data, 0o600)
}

// Delete removes rel. Missing files are reported as errors — callers
// that want "delete if exists" must handle os.IsNotExist themselves.
func (f *FS) Delete(rel string) error {
	abs, err := f.resolve(rel)
	if err != nil {
		return err
	}
	return os.Remove(abs)
}

// resolve turns a plugin-supplied relative path into an absolute path
// inside the root. Rejects absolute paths, null bytes, and anything
// that after cleaning escapes the root.
func (f *FS) resolve(rel string) (string, error) {
	if rel == "" {
		return "", errors.New("empty path")
	}
	if strings.ContainsRune(rel, 0) {
		return "", errors.New("null byte in path")
	}
	if strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute path %q not allowed", rel)
	}
	clean := filepath.Clean(filepath.Join(f.root, rel))
	rootWithSep := f.root + string(filepath.Separator)
	if clean != f.root && !strings.HasPrefix(clean+string(filepath.Separator), rootWithSep) {
		return "", fmt.Errorf("path %q escapes root", rel)
	}
	return clean, nil
}
