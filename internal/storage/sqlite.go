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
//
// MaxOpenConns is pinned to 1 so that BEGIN/COMMIT issued by the plugin
// as normal Exec statements always land on the same connection. Without
// this the pool would spread statements across connections and
// transactions would silently no-op.
func NewSQLite(path string, logger *slog.Logger) (*SQLite, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
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

// ExecResult is what Exec returns: number of rows affected plus the
// auto-increment row ID created by an INSERT (0 if not applicable).
type ExecResult struct {
	RowsAffected int64 `msgpack:"rows_affected"`
	LastInsertID int64 `msgpack:"last_insert_id"`
}

// Exec runs a statement that is not expected to return rows and
// returns both RowsAffected and LastInsertId. Callers that only care
// about one can ignore the other.
func (s *SQLite) Exec(ctx context.Context, query string, args []any) (ExecResult, error) {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return ExecResult{}, err
	}
	var out ExecResult
	out.RowsAffected, _ = res.RowsAffected()
	out.LastInsertID, _ = res.LastInsertId()
	return out, nil
}

// QueryResult is the structured return value of Query: column names in
// declaration order plus the matching row values.
type QueryResult struct {
	Columns []string `msgpack:"columns"`
	Rows    [][]any  `msgpack:"rows"`
}

// Query runs a SELECT and returns the column names plus rows. Row
// values are ordered to match Columns. The encoder turns the
// QueryResult into MessagePack for delivery to the plugin.
func (s *SQLite) Query(ctx context.Context, query string, args []any) (QueryResult, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return QueryResult{}, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return QueryResult{}, err
	}

	result := QueryResult{Columns: cols}
	for rows.Next() {
		values := make([]any, len(cols))
		scan := make([]any, len(cols))
		for i := range values {
			scan[i] = &values[i]
		}
		if err := rows.Scan(scan...); err != nil {
			return QueryResult{}, err
		}
		result.Rows = append(result.Rows, values)
	}
	return result, rows.Err()
}
