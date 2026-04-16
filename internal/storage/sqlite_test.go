package storage

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

func newSQLite(t *testing.T) *SQLite {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := NewSQLite(dbPath, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSQLite_CreateTableInsertSelect(t *testing.T) {
	db := newSQLite(t)
	ctx := context.Background()

	if _, err := db.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO users (name) VALUES (?), (?)`, []any{"alice", "bob"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rows, err := db.Query(ctx, `SELECT id, name FROM users ORDER BY id`, nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if name, ok := rows[0][1].(string); !ok || name != "alice" {
		t.Errorf("row[0] name = %v, want alice", rows[0][1])
	}
	if name, ok := rows[1][1].(string); !ok || name != "bob" {
		t.Errorf("row[1] name = %v, want bob", rows[1][1])
	}
}

func TestSQLite_Persistence(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persist.db")
	ctx := context.Background()

	db1, err := NewSQLite(dbPath, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	if _, err := db1.Exec(ctx, `CREATE TABLE kv (k TEXT, v TEXT)`, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db1.Exec(ctx, `INSERT INTO kv VALUES (?, ?)`, []any{"x", "1"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_ = db1.Close()

	db2, err := NewSQLite(dbPath, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	defer db2.Close()
	rows, err := db2.Query(ctx, `SELECT v FROM kv WHERE k = ?`, []any{"x"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 || rows[0][0].(string) != "1" {
		t.Errorf("rows = %+v, want [[1]]", rows)
	}
}

func TestSQLite_ErrorOnBadSQL(t *testing.T) {
	db := newSQLite(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `NOT VALID SQL`, nil); err == nil {
		t.Error("expected error from invalid SQL")
	}
}
