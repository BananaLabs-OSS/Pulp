package run

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/internal/reconcile"
)

func TestDeploymentRecoveryIncludesRollbackRevision(t *testing.T) {
	history, err := OpenDeploymentHistory(filepath.Join(t.TempDir(), "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	if err := history.journal.Record(context.Background(), reconcile.Record{TransactionID: "tx", Status: reconcile.StatusCommitted, Previous: reconcile.GraphDescriptor{Revision: "previous"}, Desired: reconcile.GraphDescriptor{Revision: "current"}}); err != nil {
		t.Fatal(err)
	}
	got := history.Recovery()
	if got.LastCommittedGraphRevision != "current" || got.RollbackGraphRevision != "previous" {
		t.Fatalf("recovery = %+v", got)
	}
}
