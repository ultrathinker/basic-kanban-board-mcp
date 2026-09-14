package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestMigrations_NumberedWithoutGapsAndAllApply guards the one thing
// parallel development can break here that no single branch can see: two
// tracks each adding "the next" migration and picking the same number. 0006
// came from one branch and 0007/0008 from another, and nothing in either
// branch's own test suite could notice a collision.
//
// So this asserts the sequence itself (file N is version N, no gap, no
// duplicate) and then proves the whole set applies to a brand-new file, once
// each, leaving the recorded schema version equal to the highest file.
func TestMigrations_NumberedWithoutGapsAndAllApply(t *testing.T) {
	ctx := context.Background()
	files, err := collectMigrations()
	if err != nil {
		t.Fatalf("collectMigrations: %v", err)
	}
	for i, f := range files {
		if f.version != i+1 {
			t.Fatalf("migration %d is numbered %d - the sequence has a gap or a duplicate", i, f.version)
		}
	}
	s, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "m.db")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	want := files[len(files)-1].version
	if got := migrationVersion(s.(*sqlStore), ctx); got != want {
		t.Fatalf("schema version = %d, want %d", got, want)
	}
	var applied int
	if err := s.Read(ctx, func(tx Tx) error {
		return tx.(*txWrap).tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied)
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	if applied != len(files) {
		t.Fatalf("applied rows = %d, want %d", applied, len(files))
	}
}
