// Package store is calbridge's SQLite state (PLAN.md §6): sync tokens, the raw
// Google event cache, Google↔iCloud mappings with their state machine,
// per-instance RSVP state and an audit log. It uses modernc.org/sqlite so the
// binary stays CGO_ENABLED=0.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Sentinel errors.
var (
	ErrNotFound          = errors.New("store: not found")
	ErrConflict          = errors.New("store: unique constraint violated")
	ErrInvalidTransition = errors.New("store: invalid state transition")
	ErrSchemaTooNew      = errors.New("store: database schema is newer than this binary")
)

// dbtx is the subset of *sql.DB and *sql.Tx the queries need.
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Queries holds every typed query. Store and Tx embed it, so the same methods
// run either directly or inside a transaction.
type Queries struct {
	x   dbtx
	now func() time.Time
}

// Store is an open calbridge database.
type Store struct {
	Queries
	db   *sql.DB
	path string
}

// Tx is a write transaction; see Store.Tx.
type Tx struct {
	Queries
}

// Options tune Open. The zero value is fine.
type Options struct {
	// Now replaces time.Now for created_at/updated_at stamps (tests).
	Now func() time.Time
	// BusyTimeout is how long a writer waits for the lock (default 5s).
	BusyTimeout time.Duration
}

// Open opens (creating with mode 0600 if needed) the database at path and
// applies pending migrations.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty path")
	}
	if err := ensureFile(path); err != nil {
		return nil, err
	}
	busy := opts.BusyTimeout
	if busy <= 0 {
		busy = 5 * time.Second
	}
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "busy_timeout("+strconv.FormatInt(busy.Milliseconds(), 10)+")")
	// BEGIN IMMEDIATE: take the write lock up front so concurrent writers wait
	// on busy_timeout instead of failing with SQLITE_BUSY on lock upgrade.
	q.Set("_txlock", "immediate")
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + q.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	s := &Store{Queries: Queries{x: db, now: now}, db: db, path: path}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// ensureFile pre-creates the database with mode 0600 so SQLite (which gives
// -wal/-shm the main file's mode) never creates a world-readable file.
func ensureFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("store: create directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- path comes from operator config
	if err != nil {
		return fmt.Errorf("store: create %s: %w", path, err)
	}
	return f.Close()
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Tx runs fn in a write transaction. fn's error (or a panic) rolls back.
func (s *Store) Tx(ctx context.Context, fn func(tx *Tx) error) (err error) {
	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = sqlTx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = sqlTx.Rollback()
		}
	}()
	if err = fn(&Tx{Queries: Queries{x: sqlTx, now: s.now}}); err != nil {
		return err
	}
	if err = sqlTx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// SchemaVersion returns PRAGMA user_version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("store: read user_version: %w", err)
	}
	return v, nil
}

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		name := e.Name()
		num, _, ok := strings.Cut(name, "_")
		if !ok || !strings.HasSuffix(name, ".sql") {
			return nil, fmt.Errorf("store: bad migration name %q", name)
		}
		v, err := strconv.Atoi(num)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("store: bad migration number in %q", name)
		}
		b, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: name, sql: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("store: migrations must be numbered 1..n without gaps; got %s", m.name)
		}
	}
	return out, nil
}

// LatestSchemaVersion is the schema version this binary migrates to.
func LatestSchemaVersion() int {
	ms, err := loadMigrations()
	if err != nil {
		return 0
	}
	return len(ms)
}

func (s *Store) migrate(ctx context.Context) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	cur, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if cur > len(ms) {
		return fmt.Errorf("%w: database is at version %d, binary knows %d", ErrSchemaTooNew, cur, len(ms))
	}
	for _, m := range ms[cur:] {
		err := s.Tx(ctx, func(tx *Tx) error {
			if _, err := tx.x.ExecContext(ctx, m.sql); err != nil {
				return err
			}
			// PRAGMA does not take bind parameters; version is an int we parsed.
			_, err := tx.x.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version))
			return err
		})
		if err != nil {
			return fmt.Errorf("store: migration %s: %w", m.name, err)
		}
	}
	return nil
}

// --- value helpers -----------------------------------------------------------

// tsLayout is RFC 3339 with fixed-width nanoseconds in UTC, so stored
// timestamps sort lexically.
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

func (q *Queries) stamp() string { return q.now().UTC().Format(tsLayout) }

func fmtTime(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: t.UTC().Format(tsLayout), Valid: true}
}

func parseTime(ns sql.NullString) (time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, ns.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: bad timestamp %q: %w", ns.String, err)
	}
	return t.UTC(), nil
}

func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// wrapErr maps driver errors to sentinels.
func wrapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: %s: %w", op, ErrNotFound)
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return fmt.Errorf("store: %s: %w: %w", op, ErrConflict, err)
		}
	}
	return fmt.Errorf("store: %s: %w", op, err)
}
