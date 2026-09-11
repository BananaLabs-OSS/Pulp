package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestFileJournalPersistsRecoversAndRepairsTornTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "history.jsonl")
	j, err := OpenFileJournal(p)
	if err != nil {
		t.Fatal(err)
	}
	started := Record{TransactionID: "tx1", Stage: StagePrepare, Status: StatusStarted, Desired: GraphDescriptor{Revision: "next"}}
	if err = j.Record(context.Background(), started); err != nil {
		t.Fatal(err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"sequence":2`)
	_ = f.Close()
	j, err = OpenFileJournal(p)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	r := j.RecoveryState()
	if len(r.Incomplete) != 1 || r.Incomplete[0].Record.TransactionID != "tx1" {
		t.Fatalf("recovery=%+v", r)
	}
	if err = j.Record(context.Background(), Record{TransactionID: "tx1", Stage: StagePrepare, Status: StatusRolledBack}); err != nil {
		t.Fatal(err)
	}
	if got := j.RecoveryState(); len(got.Incomplete) != 0 {
		t.Fatalf("still incomplete: %+v", got)
	}
}

func TestFileJournalConcurrentRecordsRemainValid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "history.jsonl")
	j, e := OpenFileJournal(p)
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := j.Record(context.Background(), Record{TransactionID: "tx", Stage: StageHealth, Status: StatusCompleted}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	_ = j.Close()
	j, e = OpenFileJournal(p)
	if e != nil {
		t.Fatal(e)
	}
	defer j.Close()
	if len(j.Entries()) != 20 {
		t.Fatalf("entries=%d", len(j.Entries()))
	}
}

func TestFileJournalRejectsIntegrityCorruption(t *testing.T) {
	p := filepath.Join(t.TempDir(), "history.jsonl")
	j, _ := OpenFileJournal(p)
	_ = j.Record(context.Background(), Record{TransactionID: "tx", Status: StatusStarted})
	_ = j.Close()
	b, _ := os.ReadFile(p)
	for i := range b {
		if b[i] == 't' {
			b[i] = 'x'
			break
		}
	}
	_ = os.WriteFile(p, b, 0o600)
	if j, e := OpenFileJournal(p); e == nil {
		j.Close()
		t.Fatal("accepted corrupt history")
	}
}
