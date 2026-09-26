package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// KANB-67: migration 0011 adds projects.idle_after_seconds. These tests pin
// the column through Create, Update and every read path, and that "not set"
// is stored as NULL rather than as a 0 that a later reader might take for a
// real threshold.

// rawIdleAfter reads the column directly, bypassing scanProject, so a test
// can tell NULL from a stored value.
func rawIdleAfter(t *testing.T, s Store, id string) sql.NullInt64 {
	t.Helper()
	var v sql.NullInt64
	if err := s.Read(context.Background(), func(tx Tx) error {
		return tx.(*txWrap).tx.QueryRowContext(context.Background(),
			"SELECT idle_after_seconds FROM projects WHERE id = ?", id).Scan(&v)
	}); err != nil {
		t.Fatal(err)
	}
	return v
}

// idleAfterEverywhere returns the value each read path reports for p.
func idleAfterEverywhere(t *testing.T, s Store, p *domain.Project) map[string]int {
	t.Helper()
	got := map[string]int{}
	if err := s.Read(context.Background(), func(tx Tx) error {
		byKey, err := s.Projects().GetByKey(tx, p.Key)
		if err != nil {
			return err
		}
		got["GetByKey"] = byKey.IdleAfterSeconds
		byID, err := s.Projects().GetByID(tx, p.ID)
		if err != nil {
			return err
		}
		got["GetByID"] = byID.IdleAfterSeconds
		all, err := s.Projects().List(tx, true)
		if err != nil {
			return err
		}
		for _, lp := range all {
			if lp.ID == p.ID {
				got["List"] = lp.IdleAfterSeconds
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestProjectIdleAfter_RoundTripsThroughCreateUpdateAndReads(t *testing.T) {
	s := openTestStore(t)
	p, _ := seedProject(t, s)

	if v := rawIdleAfter(t, s, p.ID); v.Valid {
		t.Fatalf("a project created without the setting stored %d, want NULL", v.Int64)
	}
	for path, v := range idleAfterEverywhere(t, s, p) {
		if v != 0 {
			t.Errorf("%s: IdleAfterSeconds = %d on an unset project, want 0", path, v)
		}
	}

	update := func(n int) {
		t.Helper()
		if err := s.Write(context.Background(), func(tx Tx) error {
			cur, err := s.Projects().GetByID(tx, p.ID)
			if err != nil {
				return err
			}
			cur.IdleAfterSeconds = n
			return s.Projects().Update(tx, cur, nil)
		}); err != nil {
			t.Fatal(err)
		}
	}

	update(172800)
	if v := rawIdleAfter(t, s, p.ID); !v.Valid || v.Int64 != 172800 {
		t.Fatalf("after Update the column holds %+v, want 172800", v)
	}
	for path, v := range idleAfterEverywhere(t, s, p) {
		if v != 172800 {
			t.Errorf("%s: IdleAfterSeconds = %d, want 172800", path, v)
		}
	}

	update(0)
	if v := rawIdleAfter(t, s, p.ID); v.Valid {
		t.Fatalf("clearing the setting stored %d, want NULL", v.Int64)
	}
}

func TestProjectIdleAfter_CreateStoresAGivenValue(t *testing.T) {
	s := openTestStore(t)
	p := &domain.Project{ID: "p-idle", Key: "IDLE", Name: "Idle", IdleAfterSeconds: 3 * 3600}
	if err := s.Write(context.Background(), func(tx Tx) error {
		return s.Projects().Create(tx, p)
	}); err != nil {
		t.Fatal(err)
	}
	for path, v := range idleAfterEverywhere(t, s, p) {
		if v != 3*3600 {
			t.Errorf("%s: IdleAfterSeconds = %d, want %d", path, v, 3*3600)
		}
	}
}
