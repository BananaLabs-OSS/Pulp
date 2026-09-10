package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/registry"
)

func TestPublishResolveVerify(t *testing.T) {
	d := t.TempDir()
	repo := filepath.Join(d, "registry")
	blob := []byte("module bytes")
	blobPath := filepath.Join(d, "cell.wasm")
	if e := os.WriteFile(blobPath, blob, 0644); e != nil {
		t.Fatal(e)
	}
	manifest := `schema_version = 1
provides = ["echo.v1"]
consumes = []
capabilities = []
[module]
id = "io.test/echo"
version = "1.0.0"
[[artifacts]]
target = "wasm32-wasip1+pulp-v1"
digest = "` + registry.Sum(blob).String() + `"
size = 12
`
	manifestPath := filepath.Join(d, "pulp.module.toml")
	if e := os.WriteFile(manifestPath, []byte(manifest), 0644); e != nil {
		t.Fatal(e)
	}
	var out, errOut bytes.Buffer
	e := run(context.Background(), []string{"publish", "-registry", repo, "-manifest", manifestPath, "-blob", "wasm32-wasip1+pulp-v1=" + blobPath}, &out, &errOut)
	if e != nil {
		t.Fatalf("publish: %v (%s)", e, errOut.String())
	}
	if strings.TrimSpace(out.String()) != "io.test/echo@1.0.0" {
		t.Fatalf("publish output %q", out.String())
	}
	lockPath := filepath.Join(d, "pulp.lock")
	out.Reset()
	e = run(context.Background(), []string{"resolve", "-registry", repo, "-target", "wasm32-wasip1+pulp-v1", "-root", "io.test/echo@^1.0.0", "-out", lockPath}, &out, &errOut)
	if e != nil {
		t.Fatal(e)
	}
	lockBytes, e := os.ReadFile(lockPath)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(lockBytes, []byte(`"version": "1.0.0"`)) {
		t.Fatalf("lock: %s", lockBytes)
	}
	out.Reset()
	e = run(context.Background(), []string{"verify", "-registry", repo, "-lock", lockPath}, &out, &errOut)
	if e != nil {
		t.Fatal(e)
	}
	if out.String() != "verified\n" {
		t.Fatalf("verify output %q", out.String())
	}
}

func TestCLIRejectsMissingAndMalformedArguments(t *testing.T) {
	var out, errOut bytes.Buffer
	for _, args := range [][]string{{}, {"wat"}, {"publish"}, {"resolve", "-registry", t.TempDir(), "-target", "wasm"}, {"verify", "-registry", t.TempDir()}} {
		if e := run(context.Background(), args, &out, &errOut); e == nil {
			t.Fatalf("args %#v accepted", args)
		}
	}
}

func TestRegistryServerIsReadOnlyAndHealthy(t *testing.T) {
	store, err := registry.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler := registryServerHandler(store, nil, "")
	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || health.Body.String() != "ok\n" {
		t.Fatalf("health response: %d %q", health.Code, health.Body.String())
	}
	write := httptest.NewRecorder()
	handler.ServeHTTP(write, httptest.NewRequest(http.MethodPost, "/v1/releases", strings.NewReader("{}")))
	if write.Code >= http.StatusOK && write.Code < http.StatusMultipleChoices {
		t.Fatalf("write unexpectedly succeeded with status %d", write.Code)
	}
}

func TestRegistryServerPublishingRequiresScopedBearerCredential(t *testing.T) {
	store, err := registry.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	token := []byte("registry-test-publishing-token-at-least-32-bytes")
	handler := registryServerHandler(store, token, "bananalabs")
	payload := []byte("fused wasm")
	digest := registry.Sum(payload)
	version, _ := registry.ParseVersion("1.0.0")
	manifest, err := registry.ManifestTOML(registry.Manifest{
		SchemaVersion: 1, ID: "bananalabs/fused", Version: version,
		Artifacts: []registry.Artifact{{Target: "wasm", Digest: digest, DigestText: digest.String(), Size: int64(len(payload))}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(remotePublishRequest{Manifest: string(manifest), Blobs: map[string][]byte{digest.String(): payload}})
	if err != nil {
		t.Fatal(err)
	}
	for name, authorization := range map[string]struct {
		authorization string
		want          int
	}{
		"missing": {want: http.StatusUnauthorized},
		"wrong":   {authorization: "Bearer wrong", want: http.StatusUnauthorized},
		"valid":   {authorization: "Bearer " + string(token), want: http.StatusCreated},
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/releases", bytes.NewReader(body))
			request.Header.Set("Authorization", authorization.authorization)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != authorization.want {
				t.Fatalf("status = %d body=%q, want %d", response.Code, response.Body.String(), authorization.want)
			}
		})
	}
}

func TestPublishCellDerivesImmutableModule(t *testing.T) {
	root := t.TempDir()
	registryRoot := filepath.Join(root, "registry")
	wasm := []byte("cell wasm")
	wasmPath := filepath.Join(root, "engine.wasm")
	if err := os.WriteFile(wasmPath, wasm, 0o644); err != nil {
		t.Fatal(err)
	}
	cellPath := filepath.Join(root, "pulp.cell.toml")
	cell := `name="engine"
version="1.2.3"
wasm="engine.wasm"
provides=["engine.query.v1"]
consumes=[]
capabilities=["storage.sqlite"]
[execution]
mode="fusible"
group="state"
abi="pulp-member-v2"
`
	if err := os.WriteFile(cellPath, []byte(cell), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"publish-cell", "-registry", registryRoot, "-manifest", cellPath, "-module", "bananalabs/engine"}, &stdout, &stderr); err != nil {
		t.Fatalf("publish-cell: %v (%s)", err, stderr.String())
	}
	store, err := registry.OpenLocal(registryRoot)
	if err != nil {
		t.Fatal(err)
	}
	version, _ := registry.ParseVersion("1.2.3")
	manifest, err := store.Manifest(context.Background(), registry.ReleaseID{ID: "bananalabs/engine", Version: version})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.FusionABI != "pulp-member-v2" || manifest.Artifacts[0].Digest != registry.Sum(wasm) {
		t.Fatalf("derived manifest = %#v", manifest)
	}
}

func TestRefreshAppReplacesLegacyLuaAndWASMDigests(t *testing.T) {
	root := t.TempDir()
	write := func(name, value string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("module.wasm", "wasm bytes")
	write("module.cell.toml", "name = \"lua\"\nversion = \"1\"\nwasm = \"module.wasm\"\nwasm_sha256 = \"old\"\n")
	write("app.lua", "return true")
	appPath := filepath.Join(root, "pulp.app.toml")
	write("pulp.app.toml", "name = \"app\"\nversion = \"1\"\nrequire_wasm_sha256 = true\ncells = [\"module.cell.toml\"]\n[orchestrator]\nmanifest = \"module.cell.toml\"\nscript = \"app.lua\"\nsha256 = \"old\"\n")
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"refresh-app", "-manifest", appPath}, &out, &errOut); err != nil {
		t.Fatalf("refresh-app: %v (%s)", err, errOut.String())
	}
	app, _ := os.ReadFile(appPath)
	cell, _ := os.ReadFile(filepath.Join(root, "module.cell.toml"))
	if bytes.Contains(app, []byte(`sha256 = "old"`)) || bytes.Contains(cell, []byte(`wasm_sha256 = "old"`)) {
		t.Fatalf("digests not refreshed:\n%s\n%s", app, cell)
	}
}

func TestRefreshCommandReplacesArtifactDigest(t *testing.T) {
	d := t.TempDir()
	blobPath := filepath.Join(d, "cell.wasm")
	if err := os.WriteFile(blobPath, []byte("new bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(d, "pulp.module.toml")
	old := []byte("schema_version=1\nprovides=[]\nconsumes=[]\ncapabilities=[]\n[module]\nid=\"io.test/refresh\"\nversion=\"1.0.0\"\n[[artifacts]]\ntarget=\"wasm\"\ndigest=\"sha256:0000000000000000000000000000000000000000000000000000000000000000\"\nsize=0\n")
	if err := os.WriteFile(manifestPath, old, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"refresh", "-manifest", manifestPath, "-blob", "wasm=" + blobPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	wire, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.ParseManifest(wire)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Artifacts[0].Digest != registry.Sum([]byte("new bytes")) || manifest.Artifacts[0].Size != 9 {
		t.Fatalf("manifest not refreshed: %#v", manifest.Artifacts[0])
	}
}
