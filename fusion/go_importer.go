package fusion

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BananaLabs-OSS/Pulp/registry"
)

const GoSourceLockV1 = "pulp.go-source-lock.v1"

// GoImporter is the network-capable resolution phase. Its output is immutable
// and can subsequently be consumed by Build while GOPROXY is disabled.
type GoImporter struct {
	GoCommand string
	Proxy     string
	TempRoot  string
}

type GoSourceLock struct {
	Schema       string       `json:"schema"`
	Dependencies []Dependency `json:"dependencies"`
	archives     map[string][]byte
}

type persistedGoSourceLock struct {
	Schema       string       `json:"schema"`
	Dependencies []Dependency `json:"dependencies"`
}

type goDownload struct {
	Path, Version, Sum, GoModSum, Zip, GoMod string
	Origin                                   struct{ URL string }
	Error                                    *struct{ Err string }
}

// Import resolves all modules named by go.mod/go.sum through the Go tool,
// canonicalizes their source archives, and binds both Go and SHA-256 digests.
func (i GoImporter) Import(ctx context.Context, goMod, goSum []byte) (*GoSourceLock, error) {
	directory, err := os.MkdirTemp(i.TempRoot, "pulp-go-import-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	if err := os.WriteFile(filepath.Join(directory, "go.mod"), goMod, 0o600); err != nil {
		return nil, err
	}
	if len(goSum) > 0 {
		if err := os.WriteFile(filepath.Join(directory, "go.sum"), goSum, 0o600); err != nil {
			return nil, err
		}
	}
	command := strings.TrimSpace(i.GoCommand)
	if command == "" {
		command = "go"
	}
	cmd := exec.CommandContext(ctx, command, "mod", "download", "-json", "all")
	cmd.Dir = directory
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOENV=off", "GOMODCACHE="+filepath.Join(directory, "modcache"))
	if strings.TrimSpace(i.Proxy) != "" {
		cmd.Env = append(cmd.Env, "GOPROXY="+i.Proxy)
	}
	output, err := cmd.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("resolve Go modules: %w: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("resolve Go modules: %w", err)
	}
	lock, err := lockGoDownloads(bytes.NewReader(output))
	if err != nil {
		return nil, err
	}
	origin := strings.TrimSpace(i.Proxy)
	if origin == "" {
		origin = "go-toolchain:GOPROXY"
	}
	for index := range lock.Dependencies {
		if lock.Dependencies[index].Origin == "" {
			lock.Dependencies[index].Origin = origin
		}
	}
	return lock, nil
}

func lockGoDownloads(input io.Reader) (*GoSourceLock, error) {
	lock := &GoSourceLock{Schema: GoSourceLockV1, archives: map[string][]byte{}}
	decoder := json.NewDecoder(input)
	for {
		var downloaded goDownload
		if err := decoder.Decode(&downloaded); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode Go module download: %w", err)
		}
		if downloaded.Error != nil {
			return nil, fmt.Errorf("download %s@%s: %s", downloaded.Path, downloaded.Version, downloaded.Error.Err)
		}
		if downloaded.Path == "" || downloaded.Version == "" || downloaded.Zip == "" || downloaded.Sum == "" {
			return nil, fmt.Errorf("Go module download is incomplete for %s@%s", downloaded.Path, downloaded.Version)
		}
		archive, err := canonicalGoModuleZip(downloaded.Zip, downloaded.GoMod)
		if err != nil {
			return nil, fmt.Errorf("archive %s@%s: %w", downloaded.Path, downloaded.Version, err)
		}
		digest := strings.TrimPrefix(registry.Sum(archive).String(), "sha256:")
		lock.Dependencies = append(lock.Dependencies, Dependency{ModulePath: downloaded.Path, Version: downloaded.Version, SHA256: digest, Origin: downloaded.Origin.URL, GoSum: downloaded.Sum, GoModSum: downloaded.GoModSum})
		lock.archives[downloaded.Path] = archive
	}
	sort.Slice(lock.Dependencies, func(a, b int) bool { return lock.Dependencies[a].ModulePath < lock.Dependencies[b].ModulePath })
	return lock, nil
}

// Archive implements ArchiveProvider for imported third-party dependencies.
func (l *GoSourceLock) Archive(_ context.Context, source Source) ([]byte, error) {
	if l == nil {
		return nil, fmt.Errorf("Go source lock is nil")
	}
	archive, ok := l.archives[source.ModulePath]
	if !ok {
		return nil, fmt.Errorf("module %q is absent from Go source lock", source.ModulePath)
	}
	return append([]byte(nil), archive...), nil
}

// Save writes a portable lock plus content-addressed archives. Existing
// identical blobs are reused and conflicting blobs are rejected.
func (l *GoSourceLock) Save(directory string) error {
	if l == nil || l.Schema != GoSourceLockV1 {
		return fmt.Errorf("invalid Go source lock")
	}
	if err := os.MkdirAll(filepath.Join(directory, "archives"), 0o755); err != nil {
		return err
	}
	for _, dependency := range l.Dependencies {
		if err := validatePublicDependency(dependency); err != nil {
			return err
		}
		archive := l.archives[dependency.ModulePath]
		if strings.TrimPrefix(registry.Sum(archive).String(), "sha256:") != dependency.SHA256 {
			return fmt.Errorf("archive for %q does not match source lock", dependency.ModulePath)
		}
		filename := filepath.Join(directory, "archives", dependency.SHA256+".zip")
		if existing, err := os.ReadFile(filename); err == nil {
			if !bytes.Equal(existing, archive) {
				return fmt.Errorf("archive path %q contains conflicting bytes", filename)
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.WriteFile(filename, archive, 0o600); err != nil {
			return err
		}
	}
	wire, err := json.MarshalIndent(persistedGoSourceLock{Schema: l.Schema, Dependencies: l.Dependencies}, "", "  ")
	if err != nil {
		return err
	}
	wire = append(wire, '\n')
	return os.WriteFile(filepath.Join(directory, "pulp.go-sources.lock.json"), wire, 0o600)
}

// OpenGoSourceLock reopens a saved lock and verifies every archive immediately.
func OpenGoSourceLock(directory string) (*GoSourceLock, error) {
	wire, err := os.ReadFile(filepath.Join(directory, "pulp.go-sources.lock.json"))
	if err != nil {
		return nil, err
	}
	var persisted persistedGoSourceLock
	if err := json.Unmarshal(wire, &persisted); err != nil {
		return nil, err
	}
	if persisted.Schema != GoSourceLockV1 {
		return nil, fmt.Errorf("unsupported Go source lock schema %q", persisted.Schema)
	}
	lock := &GoSourceLock{Schema: persisted.Schema, Dependencies: persisted.Dependencies, archives: map[string][]byte{}}
	seen := map[string]bool{}
	for _, dependency := range lock.Dependencies {
		if err := validatePublicDependency(dependency); err != nil {
			return nil, err
		}
		if seen[dependency.ModulePath] {
			return nil, fmt.Errorf("duplicate locked Go module %q", dependency.ModulePath)
		}
		seen[dependency.ModulePath] = true
		archive, err := os.ReadFile(filepath.Join(directory, "archives", dependency.SHA256+".zip"))
		if err != nil {
			return nil, err
		}
		if strings.TrimPrefix(registry.Sum(archive).String(), "sha256:") != dependency.SHA256 {
			return nil, fmt.Errorf("locked archive for %q failed verification", dependency.ModulePath)
		}
		lock.archives[dependency.ModulePath] = archive
	}
	return lock, nil
}

func validatePublicDependency(dependency Dependency) error {
	if strings.TrimSpace(dependency.ModulePath) == "" || strings.TrimSpace(dependency.Version) == "" {
		return fmt.Errorf("locked Go module path and version are required")
	}
	if _, err := registry.ParseDigest("sha256:" + dependency.SHA256); err != nil {
		return fmt.Errorf("locked Go module %q digest: %w", dependency.ModulePath, err)
	}
	return nil
}

// ArchiveProviders combines direct/registry sources with an imported lock.
type ArchiveProviders []ArchiveProvider

func (p ArchiveProviders) Archive(ctx context.Context, source Source) ([]byte, error) {
	var failures []string
	for _, provider := range p {
		if provider == nil {
			continue
		}
		archive, err := provider.Archive(ctx, source)
		if err == nil && len(archive) > 0 {
			return archive, nil
		}
		if err != nil {
			failures = append(failures, err.Error())
		}
	}
	return nil, fmt.Errorf("source archive %q not found: %s", source.ModulePath, strings.Join(failures, "; "))
}

func canonicalGoModuleZip(filename, goModFilename string) ([]byte, error) {
	reader, err := zip.OpenReader(filename)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	type entry struct {
		name string
		mode os.FileMode
		data []byte
	}
	entries := make([]entry, 0, len(reader.File))
	seen := map[string]bool{}
	var total int64
	for _, file := range reader.File {
		name := file.Name
		if at := strings.LastIndexByte(name, '@'); at >= 0 {
			if slash := strings.IndexByte(name[at:], '/'); slash >= 0 {
				name = name[at+slash+1:]
			}
		}
		original := name
		name = path.Clean(name)
		if file.FileInfo().IsDir() {
			continue
		}
		if name == "." || path.IsAbs(name) || strings.HasPrefix(name, "../") || name != original || strings.Contains(name, "\\") {
			return nil, fmt.Errorf("unsafe module path %q", file.Name)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate module path %q", name)
		}
		seen[name] = true
		if file.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("symlink %q is unsupported", file.Name)
		}
		opened, err := file.Open()
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(opened, (512<<20)+1))
		closeErr := opened.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		total += int64(len(data))
		if total > 512<<20 {
			return nil, fmt.Errorf("module archive expands beyond size limit")
		}
		entries = append(entries, entry{name: name, mode: file.Mode(), data: data})
	}
	if !seen["go.mod"] {
		if strings.TrimSpace(goModFilename) == "" {
			return nil, fmt.Errorf("module archive and download metadata have no go.mod")
		}
		goMod, err := os.ReadFile(goModFilename)
		if err != nil {
			return nil, fmt.Errorf("read downloaded go.mod: %w", err)
		}
		entries = append(entries, entry{name: "go.mod", mode: 0o644, data: goMod})
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].name < entries[b].name })
	var output bytes.Buffer
	w := zip.NewWriter(&output)
	for _, item := range entries {
		header := &zip.FileHeader{Name: item.name, Method: zip.Deflate}
		header.SetMode(item.mode.Perm())
		header.SetModTime(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC))
		file, err := w.CreateHeader(header)
		if err != nil {
			return nil, err
		}
		if _, err := file.Write(item.data); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
