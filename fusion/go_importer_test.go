package fusion

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func proxyModuleZip(t *testing.T, module, version string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "module.zip")
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(file)
	for name, content := range map[string]string{
		module + "@" + version + "/go.mod":   "module " + module + "\n\ngo 1.25\n",
		module + "@" + version + "/value.go": "package value\n",
	} {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = entry.Write([]byte(content))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestGoDownloadLockSaveOpenAndArchive(t *testing.T) {
	zipPath := proxyModuleZip(t, "example/dependency", "v1.2.3")
	record := goDownload{Path: "example/dependency", Version: "v1.2.3", Sum: "h1:source", GoModSum: "h1:mod", Zip: zipPath}
	record.Origin.URL = "https://example.test/dependency"
	var input bytes.Buffer
	if err := json.NewEncoder(&input).Encode(record); err != nil {
		t.Fatal(err)
	}
	lock, err := lockGoDownloads(&input)
	if err != nil {
		t.Fatal(err)
	}
	if len(lock.Dependencies) != 1 || lock.Dependencies[0].Origin != record.Origin.URL || lock.Dependencies[0].GoSum != record.Sum {
		t.Fatalf("lock = %+v", lock)
	}
	directory := t.TempDir()
	if err := lock.Save(directory); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenGoSourceLock(directory)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := reopened.Archive(context.Background(), Source{ModulePath: record.Path})
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 2 || strings.Contains(zr.File[0].Name, "@v1.2.3/") {
		t.Fatalf("canonical archive entries = %+v", zr.File)
	}
	archivePath := filepath.Join(directory, "archives", lock.Dependencies[0].SHA256+".zip")
	if err := os.WriteFile(archivePath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenGoSourceLock(directory); err == nil || !strings.Contains(err.Error(), "failed verification") {
		t.Fatalf("tampered lock error = %v", err)
	}
}
