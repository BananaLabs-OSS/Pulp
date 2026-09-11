package fusion

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/BananaLabs-OSS/Pulp/registry"
)

const (
	maxSourceFiles = 10000
	maxSourceBytes = 512 << 20
)

// SourceArchives supplies immutable source archives. Implementations must
// return the bytes identified by source.SourceSHA256; materializeSourceArchive
// independently verifies that promise before extraction.
type SourceArchives interface {
	Archive(context.Context, Source) ([]byte, error)
}

// RegistrySourceArchives reads source archives from a content-addressed store.
type RegistrySourceArchives struct{ Store registry.Store }

func (r RegistrySourceArchives) Archive(ctx context.Context, source Source) ([]byte, error) {
	if r.Store == nil {
		return nil, fmt.Errorf("registry source store is required")
	}
	digest, err := registry.ParseDigest("sha256:" + source.SourceSHA256)
	if err != nil {
		return nil, err
	}
	reader, err := r.Store.Blob(ctx, digest)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(io.LimitReader(reader, maxSourceBytes+1))
}

func materializeSourceArchive(destination string, source Source, archive []byte) error {
	want, err := registry.ParseDigest("sha256:" + source.SourceSHA256)
	if err != nil {
		return err
	}
	if int64(len(archive)) > maxSourceBytes || registry.Sum(archive) != want {
		return fmt.Errorf("source archive for %q failed digest or size verification", source.ModulePath)
	}
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return fmt.Errorf("source archive for %q is not a valid zip: %w", source.ModulePath, err)
	}
	if len(zr.File) == 0 || len(zr.File) > maxSourceFiles {
		return fmt.Errorf("source archive for %q has invalid file count", source.ModulePath)
	}
	seen := map[string]bool{}
	var total uint64
	for _, file := range zr.File {
		if strings.Contains(file.Name, "\\") {
			return fmt.Errorf("source archive contains unsafe path %q", file.Name)
		}
		name := strings.ReplaceAll(file.Name, "\\", "/")
		clean := path.Clean(name)
		if name == "" || clean == "." || path.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, "../") || clean != strings.TrimSuffix(name, "/") {
			return fmt.Errorf("source archive contains unsafe path %q", file.Name)
		}
		if seen[clean] {
			return fmt.Errorf("source archive contains duplicate path %q", clean)
		}
		seen[clean] = true
		mode := file.Mode()
		if mode&os.ModeSymlink != 0 || (!mode.IsRegular() && !mode.IsDir()) {
			return fmt.Errorf("source archive contains unsupported file %q", clean)
		}
		if mode.IsDir() {
			continue
		}
		total += file.UncompressedSize64
		if total > maxSourceBytes {
			return fmt.Errorf("source archive expands beyond size limit")
		}
		target := filepath.Join(destination, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := file.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		var copied int64
		if err == nil {
			copied, err = io.Copy(out, io.LimitReader(in, int64(file.UncompressedSize64)+1))
			if err == nil && copied != int64(file.UncompressedSize64) {
				err = fmt.Errorf("source archive file %q size mismatch", clean)
			}
		}
		closeOut := error(nil)
		if out != nil {
			closeOut = out.Close()
		}
		closeIn := in.Close()
		if err != nil {
			return err
		}
		if closeOut != nil {
			return closeOut
		}
		if closeIn != nil {
			return closeIn
		}
	}
	moduleBytes, err := os.ReadFile(filepath.Join(destination, "go.mod"))
	if err != nil {
		return fmt.Errorf("source archive for %q has no root go.mod", source.ModulePath)
	}
	if modulePath(moduleBytes) != source.ModulePath {
		return fmt.Errorf("source archive module is %q, expected %q", modulePath(moduleBytes), source.ModulePath)
	}
	return nil
}

func modulePath(goMod []byte) string {
	for _, line := range strings.Split(string(goMod), "\n") {
		fields := strings.Fields(strings.TrimSpace(strings.SplitN(line, "//", 2)[0]))
		if len(fields) == 2 && fields[0] == "module" {
			return strings.Trim(fields[1], "\"")
		}
	}
	return ""
}
