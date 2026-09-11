package fusion_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/fusion"
)

func TestPackGoModuleDirectoryIsDeterministic(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example/packed\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package packed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	firstSource, first, err := fusion.PackGoModuleDirectory(root, "example/packed", "v1.0.0", "RegisterV2")
	if err != nil {
		t.Fatal(err)
	}
	secondSource, second, err := fusion.PackGoModuleDirectory(root, "example/packed", "v1.0.0", "RegisterV2")
	if err != nil {
		t.Fatal(err)
	}
	if firstSource != secondSource || string(first) != string(second) || firstSource.SHA256 == "" {
		t.Fatalf("packing is not deterministic: %+v %+v", firstSource, secondSource)
	}
}
