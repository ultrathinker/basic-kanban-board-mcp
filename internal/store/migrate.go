package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// runMigrations is invoked from Open. It runs every *.sql file under
// migrations/ in lex order inside the writer, recording the version number
// in schema_migrations. The runner is idempotent: a second Open re-records
// the same versions and skips already-applied files.
//
// The migration runner lives in this file rather than migrations/0001_init.sql
// because it has to create its own tracking table; the SQL files themselves
// must be pure DDL+DML, no PRAGMA-side-effects we don't want to repeat.
func (s *sqlStore) runMigrations(ctx context.Context) error {
	// Create the tracking table outside of a write transaction: there is no
	// other writer contending with us at Open (the process owns the file).
	if _, err := s.writer.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	files, err := collectMigrations()
	if err != nil {
		return err
	}
	for _, m := range files {
		var seen bool
		err := s.writer.QueryRowContext(ctx,
			"SELECT 1 FROM schema_migrations WHERE version = ?", m.version,
		).Scan(&seen)
		switch {
		case err == sql.ErrNoRows:
			// Not applied yet.
		case err != nil:
			return fmt.Errorf("store: check migration %d: %w", m.version, err)
		case seen:
			continue
		}
		if err := s.applyOneMigration(ctx, m); err != nil {
			return fmt.Errorf("store: apply migration %s: %w", m.name, err)
		}
		s.mu.Lock()
		s.migration = m.version
		s.mu.Unlock()
	}
	return nil
}

// migration is one file from the embed.FS.
type migration struct {
	version int
	name    string // "0001_init"
	body    string
}

func collectMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations dir: %w", err)
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		ver, base, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}
		body, err := fs.ReadFile(migrationsFS, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: ver, name: base, body: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// parseMigrationName expects "NNNN_*.sql" with N a positive integer. The
// prefix is the version number recorded in schema_migrations.
func parseMigrationName(name string) (int, string, error) {
	base := strings.TrimSuffix(name, ".sql")
	i := strings.IndexByte(base, '_')
	if i <= 0 {
		return 0, "", fmt.Errorf("store: migration %q does not start with NNNN_", name)
	}
	n, err := strconv.Atoi(base[:i])
	if err != nil || n <= 0 {
		return 0, "", fmt.Errorf("store: migration %q has invalid version prefix", name)
	}
	return n, base[i+1:], nil
}

// applyOneMigration runs the SQL body inside a write transaction and
// records the version. Splitting the DDL into multiple statements in a
// single tx is fine: modernc executes them one by one on the same
// connection.
func (s *sqlStore) applyOneMigration(ctx context.Context, m migration) error {
	return s.Write(ctx, func(t Tx) error {
		tx := t.(*txWrap).tx
		if _, err := tx.ExecContext(ctx, m.body); err != nil {
			return fmt.Errorf("execute: %w", err)
		}
		now := t.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO schema_migrations(version, name, applied_at) VALUES(?,?,?)",
			m.version, m.name, now,
		); err != nil {
			return fmt.Errorf("record: %w", err)
		}
		return nil
	})
}
