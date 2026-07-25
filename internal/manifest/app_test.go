package manifest

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAppResolvesInjectsAndOrders(t *testing.T) {
	root := t.TempDir()
	writeAppFile(t, root, "cells/sessions/pulp.cell.toml", `
name = "sessions"
version = "1.0.0"
provides = ["sessions"]
`)
	writeAppFile(t, root, "cells/lua/pulp.cell.toml", `
name = "lua-orchestrator"
version = "1.0.0"
provides = ["orchestrator"]
consumes = ["sessions"]
depends_on = ["sessions"]
[config]
timeout_ms = 5000
script = "old inline script"
`)
	writeAppFile(t, root, "cells/evolution/pulp.cell.toml", `
name = "evolution"
version = "1.0.0"
consumes = ["orchestrator"]
depends_on = ["lua-orchestrator"]
`)
	script := `pulp.on("health", function() return { ok = true } end)`
	writeAppFile(t, root, "evolution.lua", script)
	digest := sha256.Sum256([]byte(script))
	appPath := writeAppFile(t, root, "pulp.app.toml", fmt.Sprintf(`
schema_version = 1
name = "evolution"
version = "2026.7.25"
cells = [
  "cells/sessions/pulp.cell.toml",
  "cells/lua/pulp.cell.toml",
  "cells/evolution/pulp.cell.toml",
]

[orchestrator]
manifest = "cells/lua/pulp.cell.toml"
script = "evolution.lua"
sha256 = "%x"
`, digest))

	app, err := LoadApp(appPath)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if app.Name != "evolution" || app.Version != "2026.7.25" {
		t.Fatalf("identity = %q %q", app.Name, app.Version)
	}
	if app.OrchestratorCell != "lua-orchestrator" {
		t.Fatalf("orchestrator = %q", app.OrchestratorCell)
	}
	orchestrator := app.Cells.Lookup("lua-orchestrator")
	if orchestrator == nil {
		t.Fatal("orchestrator cell missing")
	}
	if got := orchestrator.Config["script"]; got != script {
		t.Fatalf("injected script = %#v", got)
	}
	if got := orchestrator.Config["timeout_ms"]; got != int64(5000) {
		t.Fatalf("existing config lost: timeout_ms = %#v", got)
	}
	order := app.Cells.Order
	if len(order) != 3 ||
		order[0].Name != "sessions" ||
		order[1].Name != "lua-orchestrator" ||
		order[2].Name != "evolution" {
		t.Fatalf("boot order = %#v", appCellNames(order))
	}
	for _, path := range append(app.CellManifestPaths, app.OrchestrationScript) {
		if !filepath.IsAbs(path) {
			t.Fatalf("path is not absolute: %q", path)
		}
	}
}

func TestLoadAppRejectsScriptSHA256Mismatch(t *testing.T) {
	root := t.TempDir()
	writeAppFile(t, root, "lua.cell.toml", `
name = "lua-orchestrator"
version = "1.0.0"
`)
	writeAppFile(t, root, "app.lua", `return true`)
	appPath := writeAppFile(t, root, "pulp.app.toml", fmt.Sprintf(`
name = "test"
version = "1"
cells = ["lua.cell.toml"]
[orchestrator]
manifest = "lua.cell.toml"
script = "app.lua"
sha256 = "%s"
`, strings.Repeat("0", 64)))

	if _, err := LoadApp(appPath); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("LoadApp error = %v", err)
	}
}

func TestLoadAppVerifiesPinnedWASMAndCanRequireEveryPin(t *testing.T) {
	root := t.TempDir()
	wasm := []byte("first independently versioned package")
	writeAppFile(t, root, "cell.wasm", string(wasm))
	digest := sha256.Sum256(wasm)
	writeAppFile(t, root, "cell.toml", fmt.Sprintf("\nname = \"lua-orchestrator\"\nversion = \"1\"\nwasm = \"cell.wasm\"\nwasm_sha256 = \"%x\"\n", digest))
	script := "return true"
	writeAppFile(t, root, "app.lua", script)
	scriptDigest := sha256.Sum256([]byte(script))
	appPath := writeAppFile(t, root, "pulp.app.toml", fmt.Sprintf("\nname = \"pinned\"\nversion = \"1\"\nrequire_wasm_sha256 = true\ncells = [\"cell.toml\"]\n[orchestrator]\nmanifest = \"cell.toml\"\nscript = \"app.lua\"\nsha256 = \"%x\"\n", scriptDigest))
	app, err := LoadApp(appPath)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if !app.RequireWASMSHA256 || app.Cells.Lookup("lua-orchestrator").WASMSHA256 != fmt.Sprintf("%x", digest) {
		t.Fatalf("app pinning metadata = %#v", app)
	}

	writeAppFile(t, root, "cell.wasm", "tampered package")
	if _, err := LoadApp(appPath); err == nil || !strings.Contains(err.Error(), "WASM SHA-256 mismatch") {
		t.Fatalf("LoadApp tampered WASM error = %v", err)
	}

	writeAppFile(t, root, "cell.toml", "name = \"lua-orchestrator\"\nversion = \"1\"\nwasm = \"cell.wasm\"\n")
	if _, err := LoadApp(appPath); err == nil || !strings.Contains(err.Error(), "wasm_sha256 is required") {
		t.Fatalf("LoadApp missing required pin error = %v", err)
	}
}

func TestLoadAppExpandsRepeatedCellPlacementsWithScopedAliases(t *testing.T) {
	root := t.TempDir()
	writeAppFile(t, root, "player.cell.toml", `
name = "player-manager"
version = "1"
provides = ["player.tick.v1"]
[config]
mode = "template"
`)
	writeAppFile(t, root, "lua.cell.toml", `
name = "lua-orchestrator"
version = "1"
provides = ["orchestrator.dispatch"]
`)
	script := `return true`
	writeAppFile(t, root, "app.lua", script)
	digest := sha256.Sum256([]byte(script))
	appPath := writeAppFile(t, root, "pulp.app.toml", fmt.Sprintf(`
name = "players"
version = "1"
cells = ["player.cell.toml", "lua.cell.toml"]

[[cell_placements]]
cell = "player-manager"
count = 2
aliases = ["b1", "b2"]
[cell_placements.config]
mode = "placement"

[orchestrator]
manifest = "lua.cell.toml"
script = "app.lua"
sha256 = "%x"
`, digest))

	app, err := LoadApp(appPath)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if len(app.Placements) != 3 {
		t.Fatalf("placements = %d, want 3", len(app.Placements))
	}
	got := map[string]CellPlacement{}
	for _, placement := range app.Placements {
		got[placement.Address] = placement
	}
	for _, address := range []string{"player-manager@b1", "player-manager@b2", "lua-orchestrator"} {
		if _, ok := got[address]; !ok {
			t.Fatalf("missing placement %q from %#v", address, got)
		}
	}
	if got["player-manager@b1"].InstanceID != "b1" || got["player-manager@b2"].InstanceID != "b2" {
		t.Fatalf("player placement instances = %#v", got)
	}
	if mode := got["player-manager@b1"].Spec.Config["mode"]; mode != "placement" {
		t.Fatalf("placement config override = %#v, want placement", mode)
	}
}

func TestLoadAppRejectsAmbiguousPlacementAliases(t *testing.T) {
	root := t.TempDir()
	writeAppFile(t, root, "cell.toml", "name = \"worker\"\nversion = \"1\"\n")
	writeAppFile(t, root, "app.lua", "return true")
	digest := sha256.Sum256([]byte("return true"))
	appPath := writeAppFile(t, root, "pulp.app.toml", fmt.Sprintf(`
name = "test"
version = "1"
cells = ["cell.toml"]
[[cell_placements]]
cell = "worker"
instances = ["blue", "blue"]
[orchestrator]
manifest = "cell.toml"
script = "app.lua"
sha256 = "%x"
`, digest))
	if _, err := LoadApp(appPath); err == nil || !strings.Contains(err.Error(), "duplicate instance") {
		t.Fatalf("LoadApp error = %v, want duplicate instance", err)
	}
}

func TestLoadAppRejectsMissingOrDuplicateCells(t *testing.T) {
	root := t.TempDir()
	writeAppFile(t, root, "lua.cell.toml", `
name = "lua-orchestrator"
version = "1.0.0"
`)
	writeAppFile(t, root, "other.cell.toml", `
name = "other"
version = "1.0.0"
`)
	script := `return true`
	writeAppFile(t, root, "app.lua", script)
	digest := sha256.Sum256([]byte(script))

	tests := []struct {
		name  string
		cells string
		orch  string
		want  string
	}{
		{
			name:  "orchestrator-omitted",
			cells: `["other.cell.toml"]`,
			orch:  "lua.cell.toml",
			want:  "is not listed in app cells",
		},
		{
			name:  "duplicate-manifest-path",
			cells: `["lua.cell.toml", "lua.cell.toml"]`,
			orch:  "lua.cell.toml",
			want:  "duplicate cell manifest path",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			appPath := writeAppFile(t, root, test.name+".toml", fmt.Sprintf(`
name = "test"
version = "1"
cells = %s
[orchestrator]
manifest = %q
script = "app.lua"
sha256 = "%x"
`, test.cells, test.orch, digest))
			if _, err := LoadApp(appPath); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadApp error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadAppRejectsDuplicateCellNames(t *testing.T) {
	root := t.TempDir()
	writeAppFile(t, root, "one.cell.toml", `
name = "same"
version = "1.0.0"
`)
	writeAppFile(t, root, "two.cell.toml", `
name = "same"
version = "2.0.0"
`)
	script := `return true`
	writeAppFile(t, root, "app.lua", script)
	digest := sha256.Sum256([]byte(script))
	appPath := writeAppFile(t, root, "pulp.app.toml", fmt.Sprintf(`
name = "test"
version = "1"
cells = ["one.cell.toml", "two.cell.toml"]
[orchestrator]
manifest = "one.cell.toml"
script = "app.lua"
sha256 = "%x"
`, digest))

	if _, err := LoadApp(appPath); err == nil || !strings.Contains(err.Error(), `duplicate cell name "same"`) {
		t.Fatalf("LoadApp error = %v", err)
	}
}

func TestLoadAppRejectsMissingCellManifest(t *testing.T) {
	root := t.TempDir()
	script := `return true`
	writeAppFile(t, root, "app.lua", script)
	digest := sha256.Sum256([]byte(script))
	appPath := writeAppFile(t, root, "pulp.app.toml", fmt.Sprintf(`
name = "test"
version = "1"
cells = ["missing.cell.toml"]
[orchestrator]
manifest = "missing.cell.toml"
script = "app.lua"
sha256 = "%x"
`, digest))

	if _, err := LoadApp(appPath); err == nil || !strings.Contains(err.Error(), "read manifest") {
		t.Fatalf("LoadApp error = %v", err)
	}
}

func TestLoadAppRequiresRelativePathsAndSHA256(t *testing.T) {
	root := t.TempDir()
	cellPath := writeAppFile(t, root, "lua.cell.toml", `
name = "lua-orchestrator"
version = "1.0.0"
`)
	writeAppFile(t, root, "app.lua", `return true`)

	appPath := writeAppFile(t, root, "absolute.toml", fmt.Sprintf(`
name = "test"
version = "1"
cells = [%q]
[orchestrator]
manifest = "lua.cell.toml"
script = "app.lua"
sha256 = ""
`, cellPath))
	if _, err := LoadApp(appPath); err == nil || !strings.Contains(err.Error(), "must be relative") {
		t.Fatalf("absolute path error = %v", err)
	}

	appPath = writeAppFile(t, root, "missing-sha.toml", `
name = "test"
version = "1"
cells = ["lua.cell.toml"]
[orchestrator]
manifest = "lua.cell.toml"
script = "app.lua"
`)
	if _, err := LoadApp(appPath); err == nil || !strings.Contains(err.Error(), "sha256 is required") {
		t.Fatalf("missing SHA error = %v", err)
	}
}

func writeAppFile(t *testing.T, root, relative, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func appCellNames(specs []*CellSpec) []string {
	names := make([]string, len(specs))
	for i, spec := range specs {
		names[i] = spec.Name
	}
	return names
}
