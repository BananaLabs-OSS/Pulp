package manifest

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"
)

type identityFixture struct {
	appName, script, config, wasm string
	placements                    int
	fused                         bool
}

func writeIdentityFixture(t *testing.T, root string, f identityFixture) string {
	t.Helper()
	if f.appName == "" {
		f.appName = "identity-test"
	}
	if f.script == "" {
		f.script = "return true\n"
	}
	if f.config == "" {
		f.config = "mode = \"one\""
	}
	if f.wasm == "" {
		f.wasm = "engine-wasm-v1"
	}
	writeAppFile(t, root, "engine.wasm", f.wasm)
	engineDigest := sha256.Sum256([]byte(f.wasm))
	writeAppFile(t, root, "engine.cell.toml", fmt.Sprintf("name=\"engine\"\nversion=\"1\"\nwasm=\"engine.wasm\"\nwasm_sha256=\"%x\"\nprovides=[\"engine.v1\"]\n[config]\n%s\n", engineDigest, f.config))
	writeAppFile(t, root, "lua.wasm", "lua-wasm-v1")
	luaDigest := sha256.Sum256([]byte("lua-wasm-v1"))
	writeAppFile(t, root, "lua.cell.toml", fmt.Sprintf("name=\"lua\"\nversion=\"1\"\nwasm=\"lua.wasm\"\nwasm_sha256=\"%x\"\nprovides=[\"orchestrator.dispatch\"]\nconsumes=[\"engine.v1\"]\ndepends_on=[\"engine\"]\n", luaDigest))
	writeAppFile(t, root, "app.lua", f.script)
	scriptDigest := sha256.Sum256([]byte(f.script))
	extra := ""
	if f.placements > 0 {
		extra += fmt.Sprintf("[[cell_placements]]\ncell=\"engine\"\ncount=%d\n", f.placements)
	}
	if f.fused {
		writeAppFile(t, root, "fused.wasm", "fused-wasm-v1")
		fusedDigest := sha256.Sum256([]byte("fused-wasm-v1"))
		writeAppFile(t, root, "fused.cell.toml", fmt.Sprintf("name=\"fused\"\nversion=\"1\"\nwasm=\"fused.wasm\"\nwasm_sha256=\"%x\"\nprovides=[\"engine.v1\",\"orchestrator.dispatch\"]\n", fusedDigest))
		extra += "[[execution_units]]\nname=\"fused\"\nartifact=\"fused.cell.toml\"\nmembers=[\"engine\",\"lua\"]\n"
	}
	return writeAppFile(t, root, "pulp.app.toml", fmt.Sprintf("schema_version=1\nname=%q\nversion=\"1\"\nrequire_wasm_sha256=true\ncells=[\"engine.cell.toml\",\"lua.cell.toml\"]\n%s[orchestrator]\nmanifest=\"lua.cell.toml\"\nscript=\"app.lua\"\nsha256=\"%x\"\n", f.appName, extra, scriptDigest))
}

func fixtureDigest(t *testing.T, parent, name string, f identityFixture) string {
	t.Helper()
	app, err := LoadApp(writeIdentityFixture(t, filepath.Join(parent, name), f))
	if err != nil {
		t.Fatalf("LoadApp(%s): %v", name, err)
	}
	if len(app.CompositionSHA256) != 64 {
		t.Fatalf("digest = %q", app.CompositionSHA256)
	}
	return app.CompositionSHA256
}

func TestApplicationCompositionIdentityIsPathIndependentAndSemantic(t *testing.T) {
	parent := t.TempDir()
	base := identityFixture{}
	want := fixtureDigest(t, parent, "first/location", base)
	if got := fixtureDigest(t, parent, "unrelated/other/location", base); got != want {
		t.Fatalf("checkout path changed identity: %s != %s", got, want)
	}
	changes := []struct {
		name    string
		fixture identityFixture
	}{
		{"application", identityFixture{appName: "other"}},
		{"orchestration", identityFixture{script: "return false\n"}},
		{"cell-config", identityFixture{config: "mode = \"two\""}},
		{"cell-wasm", identityFixture{wasm: "engine-wasm-v2"}},
		{"placements", identityFixture{placements: 2}},
		{"fusion", identityFixture{fused: true}},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			if got := fixtureDigest(t, parent, "changed-"+change.name, change.fixture); got == want {
				t.Fatalf("%s change retained composition identity %s", change.name, got)
			}
		})
	}
}
