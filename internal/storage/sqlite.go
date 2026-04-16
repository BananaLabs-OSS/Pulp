package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// SQLite is a per-plugin SQLite database handle. The underlying
// database lives at a fixed on-disk path the host chooses; all access
// goes through Exec and Query, which the capability wires into host
// imports.
type SQLite struct {
	db     *sql.DB
	path   string
	logger *slog.Logger
}

// NewSQLite opens (creating if absent) a SQLite database at path. The
// parent directory is created as needed. The returned handle owns the
// connection pool — call Close to release it.
func NewSQLite(path string, logger *slog.Logger) (*SQLite, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return &SQLite{db: db, path: path, logger: logger}, nil
}

// Path returns the absolute DB file path. Diagnostic only.
func (s *SQLite) Path() string { return s.path }

// Close releases the connection pool.
func (s *SQLite) Close() error { return s.db.Close() }

// Exec runs a statement that is not expected to return rows. Returns
// the number of rows affected when the driver can report it; zero
// otherwise.
func (s *SQLite) Exec(ctx context.Context, query string, args []any) (int64, error) {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Query runs a SELECT and returns rows as [][]any. Each row is a slice
// of column values in declaration order — the encoder turns these into
// MessagePack for delivery to the plugin.
func (s *SQLite) Query(ctx context.Context, query string, args []any) ([][]any, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var result [][]any
	for rows.Next() {
		values := make([]any, len(cols))
		scan := make([]any, len(cols))
		for i := range values {
			scan[i] = &values[i]
		}
		if err := rows.Scan(scan...); err != nil {
			return nil, err
		}
		result = append(result, values)
	}
	return result, rows.Err()
}
