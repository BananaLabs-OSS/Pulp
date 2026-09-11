package fusion_test

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/fusion"
	"github.com/BananaLabs-OSS/Pulp/registry"
)

type archives map[string][]byte

func (a archives) Archive(_ context.Context, source fusion.Source) ([]byte, error) {
	return append([]byte(nil), a[source.ModulePath]...), nil
}

type conformanceRunner struct{}

func (conformanceRunner) Run(_ context.Context, directory string, _ []string, _ string, _ ...string) error {
	// A valid empty Wasm module exercises the public hermetic build and registry
	// boundary without requiring a compiler in SDK consumer tests.
	return os.WriteFile(filepath.Join(directory, "fusion.wasm"), []byte{0, 'a', 's', 'm', 1, 0, 0, 0}, 0o600)
}

func sourceArchive(t *testing.T, module string) []byte {
	t.Helper()
	var output bytes.Buffer
	w := zip.NewWriter(&output)
	file, err := w.Create("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("module " + module + "\n\ngo 1.25\n"))
	file, err = w.Create("core/core.go")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("package core\nfunc RegisterV2([]byte) error { return nil }\n"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func boundSource(t *testing.T, all archives, module string) fusion.Source {
	t.Helper()
	blob := sourceArchive(t, module)
	all[module] = blob
	return fusion.Source{ModulePath: module, ImportPath: module + "/core", Version: "v1.0.0", SHA256: strings.TrimPrefix(registry.Sum(blob).String(), "sha256:"), Entrypoint: "RegisterV2"}
}

func TestPublicSDKBuildPublishResolveVerify(t *testing.T) {
	ctx := context.Background()
	all := archives{}
	alpha := boundSource(t, all, "example/alpha")
	beta := boundSource(t, all, "example/beta")
	fiber := boundSource(t, all, "github.com/BananaLabs-OSS/Fiber")
	thirdParty := boundSource(t, all, "example/third-party")

	result, err := fusion.Build(ctx, fusion.Request{
		Schema:    fusion.RecipeV2,
		Group:     "state",
		ABI:       fusion.MemberV2,
		GoVersion: "go1.25.6",
		Members: []fusion.Member{
			{Name: "alpha", Version: "1.0.0", Providers: []string{"alpha.v1"}, Capabilities: []string{"storage.sqlite"}, Snapshotable: true, Source: alpha},
			{Name: "beta", Version: "1.0.0", Providers: []string{"beta.v1"}, Capabilities: []string{"storage.sqlite"}, Snapshotable: true, Source: beta},
		},
		Dependencies: []fusion.Dependency{{ModulePath: thirdParty.ModulePath, Version: thirdParty.Version, SHA256: thirdParty.SHA256, Origin: "https://proxy.example.test", GoSum: "h1:source", GoModSum: "h1:mod"}},
		Toolchain:    fusion.Toolchain{FiberVersion: "v1.0.0", BuilderID: "fusion-sdk-conformance/v1", FiberSource: fiber, Runner: conformanceRunner{}},
		Archives:     all,
		TempRoot:     t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(result.GeneratedSource, []byte("RegisterV2")) || result.ArtifactSHA256 == "" || result.RecipeSHA256 == "" {
		t.Fatalf("incomplete public result: %+v", result)
	}

	publication, err := result.Publication("example/fused-state", "1.0.0", "wasm32-wasip1+pulp-v2")
	if err != nil {
		t.Fatal(err)
	}
	store, err := registry.OpenLocal(filepath.Join(t.TempDir(), "registry"))
	if err != nil {
		t.Fatal(err)
	}
	if err := publication.Publish(ctx, store); err != nil {
		t.Fatal(err)
	}
	lock, err := fusion.ResolveAndVerify(ctx, store, "example/fused-state", "1.0.0", "wasm32-wasip1+pulp-v2")
	if err != nil {
		t.Fatal(err)
	}
	if len(lock.Modules) != 1 || lock.Modules[0].ID != "example/fused-state" {
		t.Fatalf("lock = %#v", lock)
	}
}
