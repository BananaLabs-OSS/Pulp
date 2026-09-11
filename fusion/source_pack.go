package fusion

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BananaLabs-OSS/Pulp/registry"
)

// PackGoModuleDirectory creates the deterministic root-layout source archive
// consumed by Build. Repository metadata is excluded; symlinks and special
// files fail closed so the archive never depends on host filesystem behavior.
func PackGoModuleDirectory(directory, importPath, version, entrypoint string) (Source, []byte, error) {
	root, err := filepath.Abs(directory)
	if err != nil {
		return Source{}, nil, err
	}
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return Source{}, nil, fmt.Errorf("read module go.mod: %w", err)
	}
	modulePath := parseGoModulePath(goMod)
	if modulePath == "" {
		return Source{}, nil, fmt.Errorf("module go.mod has no module directive")
	}
	type packedFile struct {
		name string
		mode fs.FileMode
		data []byte
	}
	var files []packedFile
	var total int64
	err = filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported source entry %q", relative)
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		total += int64(len(data))
		if total > 512<<20 {
			return fmt.Errorf("module source exceeds size limit")
		}
		files = append(files, packedFile{name: filepath.ToSlash(relative), mode: info.Mode(), data: data})
		return nil
	})
	if err != nil {
		return Source{}, nil, err
	}
	sort.Slice(files, func(a, b int) bool { return files[a].name < files[b].name })
	var output bytes.Buffer
	w := zip.NewWriter(&output)
	for _, item := range files {
		header := &zip.FileHeader{Name: item.name, Method: zip.Deflate}
		header.SetMode(item.mode.Perm())
		header.SetModTime(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC))
		file, err := w.CreateHeader(header)
		if err != nil {
			return Source{}, nil, err
		}
		if _, err := file.Write(item.data); err != nil {
			return Source{}, nil, err
		}
	}
	if err := w.Close(); err != nil {
		return Source{}, nil, err
	}
	archive := output.Bytes()
	digest := strings.TrimPrefix(registry.Sum(archive).String(), "sha256:")
	return Source{ModulePath: modulePath, ImportPath: importPath, Version: version, SHA256: digest, Entrypoint: entrypoint}, archive, nil
}

func parseGoModulePath(goMod []byte) string {
	for _, line := range strings.Split(string(goMod), "\n") {
		fields := strings.Fields(strings.SplitN(line, "//", 2)[0])
		if len(fields) == 2 && fields[0] == "module" {
			return strings.Trim(fields[1], "\"")
		}
	}
	return ""
}
