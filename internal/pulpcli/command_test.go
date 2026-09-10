package pulpcli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/internal/reconcile"
)

func TestIsCommandDoesNotCaptureRuntimeFlags(t *testing.T) {
	if IsCommand([]string{"-app", "pulp.app.toml"}) || IsCommand([]string{"ctl", "status"}) {
		t.Fatal("runtime invocation captured as package command")
	}
	if !IsCommand([]string{"sync"}) {
		t.Fatal("sync not recognized")
	}
	if !IsCommand([]string{"inspect-app"}) {
		t.Fatal("inspect-app not recognized")
	}
}

func TestInspectAppEmitsRuntimeDependencyPlan(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("root.cell.toml", "name=\"root\"\nversion=\"1\"\nprovides=[\"root.v1\"]\n")
	write("peer.cell.toml", "name=\"peer\"\nversion=\"1\"\n")
	write("lua.cell.toml", "name=\"lua\"\nversion=\"1\"\nconsumes=[\"root.v1\"]\ndepends_on=[\"root\",\"peer\"]\n")
	script := []byte("return true\n")
	write("app.lua", string(script))
	digest := sha256.Sum256(script)
	write("pulp.app.toml", fmt.Sprintf("schema_version=1\nname=\"test\"\nversion=\"1\"\ncells=[\"root.cell.toml\",\"peer.cell.toml\",\"lua.cell.toml\"]\n[orchestrator]\nmanifest=\"lua.cell.toml\"\nscript=\"app.lua\"\nsha256=\"%x\"\n", digest))

	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), []string{"inspect-app", "-manifest", filepath.Join(dir, "pulp.app.toml"), "-json"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	got := stdout.String()
	for _, want := range []string{
		`"schema":"pulp.application-dependency-plan/v1"`,
		`"levels":[["root","peer"],["lua"]]`,
		`"start_order":["root","peer","lua"]`,
		`"stop_order":["lua","peer","root"]`,
		`"dependencies":["root","peer"]`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("plan JSON %q does not contain %q", got, want)
		}
	}
}

func TestRefreshApp(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "app.lua")
	manifest := filepath.Join(dir, "pulp.app.toml")
	if err := os.WriteFile(script, []byte("return true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wire := "schema_version = 1\nname = \"test\"\nversion = \"1.0.0\"\n\n[orchestrator]\ncell = \"lua\"\nscript = \"app.lua\"\nsha256 = \"" + strings.Repeat("0", 64) + "\"\n"
	if err := os.WriteFile(manifest, []byte(wire), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), []string{"refresh-app", "-manifest", manifest}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(updated), strings.Repeat("0", 64)) {
		t.Fatal("digest was not refreshed")
	}
	if !strings.Contains(stdout.String(), manifest) {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRollbackRequiresHistory(t *testing.T) {
	err := Run(context.Background(), []string{"rollback"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "-history is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestRollbackInspectsJournalAndApplyFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployments.jsonl")
	journal, err := reconcile.OpenFileJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	err = journal.Record(context.Background(), reconcile.Record{TransactionID: "tx", Status: reconcile.StatusCommitted, Previous: reconcile.GraphDescriptor{Revision: "old"}, Desired: reconcile.GraphDescriptor{Revision: "new"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := Run(context.Background(), []string{"rollback", "-history", path}, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, "last committed: new") || !strings.Contains(got, "rollback target: old") {
		t.Fatalf("stdout = %q", got)
	}
	err = Run(context.Background(), []string{"rollback", "-history", path, "-apply"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "transport is not configured") {
		t.Fatalf("apply error = %v", err)
	}
}
