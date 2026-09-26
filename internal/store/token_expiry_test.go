package store

import (
	"context"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// KANB-60: migration 0010 adds tokens.expires_at. These tests pin the column
// through every read path and the in-place Reissue that recycles an expired
// executor key's row.

// "Is the latest" stopped being true at 0011; that the whole sequence
// applies up to the newest file is TestMigrations_NumberedWithoutGapsAndAllApply's
// job, so this only pins that 0010 is part of it.
func TestMigration0010_IsApplied(t *testing.T) {
	s := openTestStore(t)
	h, err := s.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Migration < 10 {
		t.Fatalf("schema migration = %d, want at least 10 (0010_token_expiry)", h.Migration)
	}
}

func TestTokenExpiry_RoundTripsThroughEveryRead(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	expires := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	withExpiry := &domain.Token{ID: "t-exp", Name: "exec-1", Hash: []byte("h-exp"),
		Scopes: domain.Scopes{domain.ScopeExecutor}, ProjectKeys: []string{"BMB"}, ExpiresAt: &expires}
	forever := &domain.Token{ID: "t-forever", Name: "writer", Hash: []byte("h-forever"),
		Scopes: domain.Scopes{domain.ScopeWrite}}
	if err := s.Write(ctx, func(tx Tx) error {
		if err := s.Tokens().Create(tx, withExpiry); err != nil {
			return err
		}
		return s.Tokens().Create(tx, forever)
	}); err != nil {
		t.Fatal(err)
	}

	check := func(label string, got *domain.Token, want *time.Time) {
		t.Helper()
		switch {
		case want == nil && got.ExpiresAt != nil:
			t.Errorf("%s: ExpiresAt = %v, want nil (never expires)", label, got.ExpiresAt)
		case want != nil && (got.ExpiresAt == nil || !got.ExpiresAt.Equal(*want)):
			t.Errorf("%s: ExpiresAt = %v, want %v", label, got.ExpiresAt, want)
		}
	}
	if err := s.Read(ctx, func(tx Tx) error {
		byName, err := s.Tokens().GetByName(tx, "exec-1")
		if err != nil {
			return err
		}
		check("GetByName", byName, &expires)
		byID, err := s.Tokens().GetByID(tx, "t-exp")
		if err != nil {
			return err
		}
		check("GetByID", byID, &expires)
		byHash, err := s.Tokens().GetByHash(tx, []byte("h-exp"))
		if err != nil {
			return err
		}
		check("GetByHash", byHash, &expires)
		all, err := s.Tokens().List(tx)
		if err != nil {
			return err
		}
		for _, tk := range all {
			if tk.ID == "t-exp" {
				check("List exec-1", tk, &expires)
			} else {
				check("List writer", tk, nil)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTokenReissue_KeepsRowReplacesLife(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	used := old.Add(-time.Hour)
	tok := &domain.Token{ID: "t-1", Name: "exec-1", Hash: []byte("old-hash"),
		Scopes: domain.Scopes{domain.ScopeExecutor}, ProjectKeys: []string{"BMB"}, ExpiresAt: &old, LastUsedAt: &used}
	if err := s.Write(ctx, func(tx Tx) error { return s.Tokens().Create(tx, tok) }); err != nil {
		t.Fatal(err)
	}

	fresh := old.Add(48 * time.Hour)
	if err := s.Write(ctx, func(tx Tx) error {
		return s.Tokens().Reissue(tx, &domain.Token{ID: "t-1", Hash: []byte("new-hash"),
			Scopes: domain.Scopes{domain.ScopeExecutor}, ProjectKeys: []string{"BMB", "OPS"}, ExpiresAt: &fresh})
	}); err != nil {
		t.Fatalf("Reissue: %v", err)
	}
	if err := s.Read(ctx, func(tx Tx) error {
		got, err := s.Tokens().GetByHash(tx, []byte("new-hash"))
		if err != nil {
			return err
		}
		if got.ID != "t-1" || got.Name != "exec-1" {
			t.Errorf("reissue changed identity: id=%s name=%s", got.ID, got.Name)
		}
		if got.ExpiresAt == nil || !got.ExpiresAt.Equal(fresh) {
			t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, fresh)
		}
		if len(got.ProjectKeys) != 2 || got.ProjectKeys[1] != "OPS" {
			t.Errorf("ProjectKeys = %v, want [BMB OPS]", got.ProjectKeys)
		}
		if got.LastUsedAt != nil {
			t.Errorf("LastUsedAt = %v, want nil: the new key has not been used", got.LastUsedAt)
		}
		if _, err := s.Tokens().GetByHash(tx, []byte("old-hash")); !isNotFoundErr(err) {
			t.Errorf("old hash still resolves: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A revoked row is never brought back.
	if err := s.Write(ctx, func(tx Tx) error { return s.Tokens().Revoke(tx, "exec-1") }); err != nil {
		t.Fatal(err)
	}
	err := s.Write(ctx, func(tx Tx) error {
		return s.Tokens().Reissue(tx, &domain.Token{ID: "t-1", Hash: []byte("third"), ExpiresAt: &fresh})
	})
	if e := domain.AsError(err); e == nil || e.Code != domain.CodeNotFound {
		t.Fatalf("Reissue of a revoked row: %v, want not_found", err)
	}
}

func isNotFoundErr(err error) bool {
	e := domain.AsError(err)
	return e != nil && e.Code == domain.CodeNotFound
}

// KANB-68: SetExpiry is the renewal of an executor key. Only the expiry
// moves — the hash, and so the secret the agent holds, stays — and a
// revoked row is not brought back.
func TestTokenSetExpiry_MovesOnlyTheExpiry(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	renewed := old.Add(48 * time.Hour)
	tok := &domain.Token{ID: "t-renew", Name: "exec-1", Hash: []byte("h-renew"),
		Scopes: domain.Scopes{domain.ScopeExecutor}, ProjectKeys: []string{"BMB"}, ExpiresAt: &old}
	dead := &domain.Token{ID: "t-dead", Name: "exec-dead", Hash: []byte("h-dead"),
		Scopes: domain.Scopes{domain.ScopeExecutor}, ProjectKeys: []string{"BMB"}, ExpiresAt: &old}
	if err := s.Write(ctx, func(tx Tx) error {
		for _, x := range []*domain.Token{tok, dead} {
			if err := s.Tokens().Create(tx, x); err != nil {
				return err
			}
		}
		if err := s.Tokens().Revoke(tx, "exec-dead"); err != nil {
			return err
		}
		return s.Tokens().SetExpiry(tx, tok.ID, renewed)
	}); err != nil {
		t.Fatal(err)
	}
	var got *domain.Token
	var deadErr error
	if err := s.Write(ctx, func(tx Tx) error {
		var err error
		if got, err = s.Tokens().GetByHash(tx, []byte("h-renew")); err != nil {
			return err
		}
		deadErr = s.Tokens().SetExpiry(tx, dead.ID, renewed)
		return nil
	}); err != nil {
		t.Fatalf("the old hash no longer resolves after SetExpiry: %v", err)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(renewed) || got.ID != tok.ID ||
		len(got.ProjectKeys) != 1 || len(got.Scopes) != 1 || got.Scopes[0] != domain.ScopeExecutor {
		t.Fatalf("after SetExpiry: %+v", got)
	}
	if e := domain.AsError(deadErr); e == nil || e.Code != domain.CodeNotFound {
		t.Fatalf("SetExpiry on a revoked row: %v, want not_found", deadErr)
	}
}
