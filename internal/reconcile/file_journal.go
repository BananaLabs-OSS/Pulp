package reconcile

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// JournalEntry is an integrity-chained, append-only deployment record.
type JournalEntry struct {
	Sequence     uint64    `json:"sequence"`
	RecordedAt   time.Time `json:"recorded_at"`
	PreviousHash string    `json:"previous_hash,omitempty"`
	Record       Record    `json:"record"`
	Hash         string    `json:"hash"`
}

// FileJournal persists one JSON record per fsync. OpenFileJournal repairs only
// a torn final write; corruption in an otherwise complete record is fatal.
type FileJournal struct {
	mu       sync.Mutex
	file     *os.File
	entries  []JournalEntry
	lastHash string
}

func OpenFileJournal(path string) (*FileJournal, error) {
	if path == "" {
		return nil, errors.New("deployment journal path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create deployment journal directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	j := &FileJournal{file: f}
	if err = j.load(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return j, nil
}

func (j *FileJournal) load() error {
	if _, err := j.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReader(j.file)
	var offset int64
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) && len(line) != 0 {
			if e := j.file.Truncate(offset); e != nil {
				return e
			}
			break
		}
		if len(line) != 0 {
			var e JournalEntry
			if x := json.Unmarshal(line, &e); x != nil {
				return fmt.Errorf("corrupt deployment journal at sequence %d: %w", len(j.entries)+1, x)
			}
			if e.Sequence != uint64(len(j.entries)+1) || e.PreviousHash != j.lastHash || entryHash(e) != e.Hash {
				return fmt.Errorf("corrupt deployment journal integrity at sequence %d", e.Sequence)
			}
			j.entries = append(j.entries, e)
			j.lastHash = e.Hash
			offset += int64(len(line))
		}
		if err != nil {
			break
		}
	}
	_, err := j.file.Seek(0, io.SeekEnd)
	return err
}

func entryHash(e JournalEntry) string {
	e.Hash = ""
	b, _ := json.Marshal(e)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (j *FileJournal) Record(_ context.Context, r Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	e := JournalEntry{Sequence: uint64(len(j.entries) + 1), RecordedAt: time.Now().UTC(), PreviousHash: j.lastHash, Record: r}
	e.Hash = entryHash(e)
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err = j.file.Write(b); err != nil {
		return err
	}
	if err = j.file.Sync(); err != nil {
		return err
	}
	j.entries = append(j.entries, e)
	j.lastHash = e.Hash
	return nil
}
func (j *FileJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	return err
}
func (j *FileJournal) Entries() []JournalEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]JournalEntry(nil), j.entries...)
}

type Recovery struct {
	Incomplete    []JournalEntry
	LastCommitted *JournalEntry
}

// RecoveryState identifies transactions that did not durably commit or roll
// back. Hosts must restore their Previous graph before admitting traffic.
func (j *FileJournal) RecoveryState() Recovery {
	j.mu.Lock()
	defer j.mu.Unlock()
	terminal := map[string]bool{}
	var out Recovery
	for i := len(j.entries) - 1; i >= 0; i-- {
		e := j.entries[i]
		if e.Record.Status == StatusCommitted && out.LastCommitted == nil {
			x := e
			out.LastCommitted = &x
		}
		if e.Record.Status == StatusCommitted || e.Record.Status == StatusRolledBack {
			terminal[e.Record.TransactionID] = true
		}
	}
	seen := map[string]bool{}
	for i := len(j.entries) - 1; i >= 0; i-- {
		e := j.entries[i]
		id := e.Record.TransactionID
		if !terminal[id] && !seen[id] {
			out.Incomplete = append(out.Incomplete, e)
			seen[id] = true
		}
	}
	return out
}
