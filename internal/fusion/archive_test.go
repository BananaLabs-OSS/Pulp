package fusion

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/registry"
)

func customZip(t *testing.T, entries map[string]string, symlink string) []byte {
	t.Helper()
	var output bytes.Buffer
	w := zip.NewWriter(&output)
	for name, body := range entries {
		var f interface{ Write([]byte) (int, error) }
		var err error
		if name == symlink {
			h := &zip.FileHeader{Name: name, Method: zip.Store}
			h.SetMode(os.ModeSymlink | 0o777)
			f, err = w.CreateHeader(h)
		} else {
			f, err = w.Create(name)
		}
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte(body))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func archiveSource(module string, blob []byte) Source {
	return Source{ModulePath: module, ImportPath: module + "/core", Version: "v1.0.0", SourceSHA256: strings.TrimPrefix(registry.Sum(blob).String(), "sha256:"), Entrypoint: "Register"}
}

func TestMaterializeSourceArchiveRejectsTraversalSymlinkAndWrongModule(t *testing.T) {
	tests := []struct {
		name             string
		blob             []byte
		module, contains string
	}{
		{"traversal", customZip(t, map[string]string{"go.mod": "module example/good\n", "../escape": "bad"}, ""), "example/good", "unsafe path"},
		{"symlink", customZip(t, map[string]string{"go.mod": "module example/good\n", "link": "outside"}, "link"), "example/good", "unsupported file"},
		{"module", customZip(t, map[string]string{"go.mod": "module example/wrong\n"}, ""), "example/good", "expected"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := materializeSourceArchive(t.TempDir(), archiveSource(test.module, test.blob), test.blob)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestMaterializeSourceArchiveRejectsDigestMismatch(t *testing.T) {
	blob := customZip(t, map[string]string{"go.mod": "module example/good\n"}, "")
	source := archiveSource("example/good", blob)
	source.SourceSHA256 = strings.Repeat("0", 64)
	if err := materializeSourceArchive(t.TempDir(), source, blob); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("error = %v", err)
	}
}

type inspectingRunner struct {
	fakeRunner
	sourceModules []string
}

func (r *inspectingRunner) Run(ctx context.Context, directory string, environment []string, command string, arguments ...string) error {
	entries, _ := os.ReadDir(filepath.Join(directory, "sources"))
	for _, entry := range entries {
		contents, _ := os.ReadFile(filepath.Join(directory, "sources", entry.Name(), "go.mod"))
		r.sourceModules = append(r.sourceModules, modulePath(contents))
	}
	return r.fakeRunner.Run(ctx, directory, environment, command, arguments...)
}

func TestBuilderUsesOnlyVerifiedLocalReplacements(t *testing.T) {
	runner := &inspectingRunner{fakeRunner: fakeRunner{wasm: []byte("wasm")}}
	recipe, builder := hermeticFixture(t, builderRecipe(), runner, Toolchain{FiberVersion: "v0.4.0", BuilderID: "hermetic/v1", Environment: []string{"GOPROXY=https://untrusted.invalid"}})
	result, err := builder.Build(context.Background(), recipe)
	if err != nil {
		t.Fatal(err)
	}
	module := string(result.GoModule)
	for _, path := range []string{"example/alpha", "example/zeta", "github.com/BananaLabs-OSS/Fiber"} {
		if !strings.Contains(module, path+" => ./sources/") {
			t.Fatalf("missing local replacement for %s:\n%s", path, module)
		}
	}
	if !containsEnvironment(runner.environment, "GOPROXY=off") || !containsEnvironment(runner.environment, "GOENV=off") {
		t.Fatalf("environment = %#v", runner.environment)
	}
	if len(runner.sourceModules) != 3 {
		t.Fatalf("materialized modules = %#v", runner.sourceModules)
	}
}
