// Package store persists the dabberz control plane in SQLite.
//
// SQLite is deliberate: v1 runs on a single large machine alongside the VMs it
// supervises, so a separate database server would add an operational dependency
// without buying anything. The driver is pure Go, so the control plane still
// builds as a static binary with no cgo.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/id"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrNotFound is returned when a lookup matches no row. Callers should compare
// with errors.Is rather than checking for sql.ErrNoRows.
var ErrNotFound = errors.New("store: not found")

// ErrConflict is returned when a write violates a uniqueness constraint.
var ErrConflict = errors.New("store: conflict")

// Store is the control plane's database handle.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the SQLite database at path and applies
// any outstanding migrations. Pass ":memory:" for an ephemeral store in tests.
func Open(ctx context.Context, path string) (*Store, error) {
	var dsn string
	if path == ":memory:" {
		// A shared cache keeps every pooled connection pointed at the same
		// in-memory database; without it each connection would get its own.
		// The database is given a unique name so that separate Open calls in
		// one process (parallel tests, most of all) stay isolated from each
		// other rather than all landing in the same shared database.
		dsn = "file:" + id.New("memdb") + "?mode=memory&cache=shared"
	} else {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := ensureDir(dir); err != nil {
				return nil, err
			}
		}
		dsn = "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// SQLite takes a single writer. Serialising through one connection avoids
	// SQLITE_BUSY entirely at the cost of write concurrency we do not need at
	// this scale.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: enable foreign keys: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying handle for callers that need raw access, such as
// integration tests and maintenance commands.
func (s *Store) DB() *sql.DB { return s.db }

// migrate applies every embedded migration that has not yet been recorded, in
// filename order, each inside its own transaction.
func (s *Store) migrate(ctx context.Context) error {
	const createTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
		name       TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`
	if _, err := s.db.ExecContext(ctx, createTable); err != nil {
		return fmt.Errorf("store: create migration table: %w", err)
	}

	applied, err := s.appliedMigrations(ctx)
	if err != nil {
		return err
	}

	entries, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("store: list migrations: %w", err)
	}
	sort.Strings(entries)

	for _, entry := range entries {
		name := filepath.Base(entry)
		if applied[name] {
			continue
		}
		body, err := migrationFS.ReadFile(entry)
		if err != nil {
			return fmt.Errorf("store: read migration %s: %w", name, err)
		}
		if err := s.applyMigration(ctx, name, string(body)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) appliedMigrations(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read applied migrations: %w", err)
	}
	defer rows.Close()

	applied := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: scan applied migration: %w", err)
		}
		applied[name] = true
	}
	return applied, rows.Err()
}

func (s *Store) applyMigration(ctx context.Context, name, body string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration %s: %w", name, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("store: apply migration %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)",
		name, formatTime(time.Now()),
	); err != nil {
		return fmt.Errorf("store: record migration %s: %w", name, err)
	}
	return tx.Commit()
}

const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// formatTime renders a timestamp for storage: UTC, fixed-width RFC3339, which
// sorts lexicographically in the same order as chronologically.
func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(v string) (time.Time, error) {
	// Parsing is lenient about the fractional width, so rows written by older
	// builds or by hand still load.
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: parse timestamp %q: %w", v, err)
	}
	return t.UTC(), nil
}

// nullTime renders an optional timestamp.
func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

func scanNullTime(v sql.NullString) (*time.Time, error) {
	if !v.Valid || v.String == "" {
		return nil, nil
	}
	t, err := parseTime(v.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// marshalJSON renders a value for a JSON column, storing NULL for nil.
func marshalJSON(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("store: encode json column: %w", err)
	}
	return string(raw), nil
}

// unmarshalJSON decodes a nullable JSON column into dst, leaving dst untouched
// when the column is NULL or empty.
func unmarshalJSON(col sql.NullString, dst any) error {
	if !col.Valid || col.String == "" || col.String == "null" {
		return nil
	}
	if err := json.Unmarshal([]byte(col.String), dst); err != nil {
		return fmt.Errorf("store: decode json column: %w", err)
	}
	return nil
}

// wrapErr translates driver-level errors into the package's sentinel errors.
func wrapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: %s: %w", op, ErrNotFound)
	}
	msg := err.Error()
	if strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "SQLITE_CONSTRAINT_UNIQUE") {
		return fmt.Errorf("store: %s: %w: %s", op, ErrConflict, msg)
	}
	return fmt.Errorf("store: %s: %w", op, err)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
