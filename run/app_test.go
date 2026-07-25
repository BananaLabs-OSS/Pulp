package run

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadManifestInputsRejectsMissingAndMixedCLIInputs(t *testing.T) {
	if _, _, err := loadManifestInputs("", nil); err == nil ||
		!strings.Contains(err.Error(), "one of -app or -manifest") {
		t.Fatalf("missing input error = %v", err)
	}
	if _, _, err := loadManifestInputs("pulp.app.toml", []string{"pulp.cell.toml"}); err == nil ||
		!strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("mixed input error = %v", err)
	}
}

func TestLoadManifestInputsPreservesRepeatedManifestPath(t *testing.T) {
	root := t.TempDir()
	provider := writeRunTestFile(t, root, "provider.toml", `
name = "provider"
version = "1"
`)
	consumer := writeRunTestFile(t, root, "consumer.toml", `
name = "consumer"
version = "1"
depends_on = ["provider"]
`)

	set, app, err := loadManifestInputs("", []string{consumer, provider})
	if err != nil {
		t.Fatalf("loadManifestInputs: %v", err)
	}
	if app != nil {
		t.Fatalf("legacy manifests returned app %#v", app)
	}
	if len(set.Order) != 2 || set.Order[0].Name != "provider" || set.Order[1].Name != "consumer" {
		t.Fatalf("order = [%s %s], want [provider consumer]", set.Order[0].Name, set.Order[1].Name)
	}
}

func TestLoadManifestInputsLoadsApplication(t *testing.T) {
	root := t.TempDir()
	writeRunTestFile(t, root, "lua.cell.toml", `
name = "lua-orchestrator"
version = "1"
`)
	script := `pulp.on("health", function() return true end)`
	writeRunTestFile(t, root, "app.lua", script)
	digest := sha256.Sum256([]byte(script))
	appPath := writeRunTestFile(t, root, "pulp.app.toml", fmt.Sprintf(`
name = "test-app"
version = "1"
cells = ["lua.cell.toml"]
[orchestrator]
manifest = "lua.cell.toml"
script = "app.lua"
sha256 = "%x"
`, digest))

	set, app, err := loadManifestInputs(appPath, nil)
	if err != nil {
		t.Fatalf("loadManifestInputs: %v", err)
	}
	if app == nil || app.Name != "test-app" || set.Lookup("lua-orchestrator") == nil {
		t.Fatalf("app = %#v, set = %#v", app, set)
	}
}

func writeRunTestFile(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}
