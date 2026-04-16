package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pulp.plugin.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoad_Minimal(t *testing.T) {
	path := writeManifest(t, `
name = "hello"
version = "0.1.0"
`)
	spec, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if spec.Name != "hello" || spec.Version != "0.1.0" {
		t.Fatalf("identity wrong: %+v", spec)
	}
	if spec.Provides != nil || spec.Consumes != nil || spec.Capabilities != nil {
		t.Fatalf("empty lists should be nil: %+v", spec)
	}
	if spec.Config != nil {
		t.Fatalf("absent config should be nil: %+v", spec.Config)
	}
	if !strings.HasSuffix(spec.WASMPath, "plugin.wasm") {
		t.Fatalf("default wasm path wrong: %s", spec.WASMPath)
	}
}

func TestLoad_FullManifest(t *testing.T) {
	path := writeManifest(t, `
name = "ant-farm"
version = "0.2.0"
wasm = "ant-farm.wasm"

provides = ["game.tick", "game.state", "game.tick"]
consumes = ["identity.verify"]
capabilities = ["Transport.HTTP", "entropy"]
shared_memory_groups = ["core"]
dedicated_thread = true
snapshotable = true

[config]
world_size = 100
ant_count = 500
name = "hill-alpha"
`)
	spec, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := []struct {
		got, want string
	}{
		{spec.Name, "ant-farm"},
		{spec.Version, "0.2.0"},
	}
	for _, w := range want {
		if w.got != w.want {
			t.Errorf("got %q want %q", w.got, w.want)
		}
	}

	if len(spec.Provides) != 2 {
		t.Errorf("provides should dedupe: %v", spec.Provides)
	}
	if spec.Capabilities[0] != "transport.http" {
		t.Errorf("capabilities should be lowercased: %v", spec.Capabilities)
	}
	if !spec.DedicatedThread || !spec.Snapshotable {
		t.Errorf("bool flags not carried: %+v", spec)
	}
	if got, _ := spec.Config["world_size"].(int64); got != 100 {
		t.Errorf("config world_size = %v want 100", spec.Config["world_size"])
	}
	if !strings.HasSuffix(spec.WASMPath, "ant-farm.wasm") {
		t.Errorf("wasm path: %s", spec.WASMPath)
	}
}

func TestLoad_MissingRequired(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"no name", `version = "0.1.0"`, "name is required"},
		{"no version", `name = "x"`, "version is required"},
		{"blank name", `name = "  "` + "\n" + `version = "0.1"`, "name is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeManifest(t, c.body))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestLoad_UnknownField(t *testing.T) {
	path := writeManifest(t, `
name = "x"
version = "0.1"
typo_field = "oops"
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown manifest fields") {
		t.Fatalf("want unknown field error, got %v", err)
	}
	if !strings.Contains(err.Error(), "typo_field") {
		t.Fatalf("error should name the offending field: %v", err)
	}
}

func TestLoad_MalformedTOML(t *testing.T) {
	_, err := Load(writeManifest(t, `this is not toml =`))
	if err == nil || !strings.Contains(err.Error(), "parse manifest") {
		t.Fatalf("want parse error, got %v", err)
	}
}

func TestLoad_ReservedFederationFields(t *testing.T) {
	// Federation fields must parse cleanly (forward-compat) even though v0.2
	// does nothing with them. They belong to the schema; an older Pulp should
	// not reject manifests written for a newer Pulp.
	path := writeManifest(t, `
name = "x"
version = "0.1"
federated_callers = ["peer.example"]
federated_consumes = ["peer.example::search.query"]
migratable = true
`)
	if _, err := Load(path); err != nil {
		t.Fatalf("federation fields should parse: %v", err)
	}
}

func TestEncodeConfig_RoundTrip(t *testing.T) {
	in := map[string]any{
		"world_size": int64(100),
		"name":       "hill-alpha",
	}
	b, err := EncodeConfig(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("expected bytes")
	}
	var out map[string]any
	if err := msgpack.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["world_size"] != int64(100) || out["name"] != "hill-alpha" {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestEncodeConfig_EmptyReturnsNil(t *testing.T) {
	b, err := EncodeConfig(nil)
	if err != nil || b != nil {
		t.Fatalf("nil config should return (nil, nil), got (%v, %v)", b, err)
	}
	b, err = EncodeConfig(map[string]any{})
	if err != nil || b != nil {
		t.Fatalf("empty config should return (nil, nil), got (%v, %v)", b, err)
	}
}
