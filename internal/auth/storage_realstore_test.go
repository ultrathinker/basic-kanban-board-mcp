package auth

// Real-store coverage for every Manager method that reads or writes a token
// or session. The in-package fakes were hiding constraint violations (round
// 4 found mintAndStore omitting the primary key because the fake
// auto-filled it) and softer-than-real error semantics (round 3 found
// GetByHash returning (nil, nil) where the real store returns NotFound).
// These tests pin the storage path against the real SQLite engine so any
// future regression in the wiring between Manager and store is caught at
// `go test` time, not by the operator on a fresh boot.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// realManager is the helper that opens a fresh SQLite database in
// t.TempDir() and wires it through NewManagerFromStore. We deliberately do
// NOT use NewManager-from-fake for any storage-touching test: round 4's
// failure mode was a fake that hid a real-store constraint.
func realManager(t *testing.T) *Manager {
	t.Helper()
	st, err := store.Open(context.Background(), store.Config{
		Path: filepath.Join(t.TempDir(), "kanban.db"),
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewManagerFromStore(st, nil, "", true, nil)
}

// ---------------------------------------------------------------------------
// Token writes
// ---------------------------------------------------------------------------

func TestMintAndStore_RealStore_ProducesVerifiableSecret(t *testing.T) {
	ctx := context.Background()
	m := realManager(t)

	secret, err := m.MintAndStore(ctx, &domain.Token{
		ID:          "tok-alice",
		Name:        "alice",
		Scopes:      domain.Scopes{domain.ScopeWrite},
		ProjectKeys: []string{"BMB"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := m.VerifyToken(ctx, secret)
	if err != nil || tok == nil {
		t.Fatalf("verify = (%v,%v)", tok, err)
	}
	if tok.Name != "alice" || !tok.Scopes.Has(domain.ScopeWrite) {
		t.Fatalf("token shape wrong: %+v", tok)
	}
}

func TestMintAndStore_RealStore_DuplicateNameFails(t *testing.T) {
	ctx := context.Background()
	m := realManager(t)

	if _, err := m.MintAndStore(ctx, &domain.Token{ID: "tok-x", Name: "dup", Scopes: domain.Scopes{domain.ScopeWrite}}); err != nil {
		t.Fatal(err)
	}
	// tokens_name_uniq must reject the second mint.
	if _, err := m.MintAndStore(ctx, &domain.Token{ID: "tok-y", Name: "dup", Scopes: domain.Scopes{domain.ScopeWrite}}); err == nil {
		t.Fatal("expected unique-name error on second mint")
	}
}

// ---------------------------------------------------------------------------
// Token rotation — regression for the round-1 bug where Rotate called Create
// and collided with the UNIQUE indexes.
// ---------------------------------------------------------------------------

func TestRotate_RealStore_ReplacesHashInPlace(t *testing.T) {
	ctx := context.Background()
	m := realManager(t)

	origSecret, err := m.MintAndStore(ctx, &domain.Token{ID: "tok-r", Name: "rot-real", Scopes: domain.Scopes{domain.ScopeRead}})
	if err != nil {
		t.Fatal(err)
	}
	newSecret, err := m.Rotate(ctx, "rot-real")
	if err != nil {
		t.Fatal(err)
	}
	if origSecret == newSecret {
		t.Fatal("rotation returned the same secret")
	}
	// Old secret is dead.
	if _, err := m.VerifyToken(ctx, origSecret); err != nil {
		t.Fatalf("old secret errored (expected nil,nil): %v", err)
	}
	if tok, _ := m.VerifyToken(ctx, origSecret); tok != nil {
		t.Fatalf("old secret still verifies: %+v", tok)
	}
	// New secret lives.
	if tok, err := m.VerifyToken(ctx, newSecret); err != nil || tok == nil {
		t.Fatalf("new secret does not verify: (%v,%v)", tok, err)
	}
}

func TestRotate_RealStore_UnknownToken(t *testing.T) {
	ctx := context.Background()
	m := realManager(t)
	_, err := m.Rotate(ctx, "does-not-exist")
	if err == nil {
		t.Fatal("expected error on unknown token name")
	}
	// Manager.Rotate must surface "no such token" as a *domain.Error with
	// CodeNotFound. CLI callers (cmd/kanban/main_test.go) and any future
	// service-layer handler branch on the code — a package-private
	// *BootstrapError would force every caller to unwrap.
	de := domain.AsError(err)
	if de == nil {
		t.Fatalf("Rotate returned %T (%v); want *domain.Error", err, err)
	}
	if de.Code != domain.CodeNotFound {
		t.Fatalf("Rotate code = %q; want not_found", de.Code)
	}
}

func TestRotate_RealStore_RevokedTokenRefused(t *testing.T) {
	ctx := context.Background()
	m := realManager(t)
	if _, err := m.MintAndStore(ctx, &domain.Token{ID: "tok-rev", Name: "will-be-revoked", Scopes: domain.Scopes{domain.ScopeRead}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Tokens.Revoke(ctx, "will-be-revoked"); err != nil {
		t.Fatal(err)
	}
	_, err := m.Rotate(ctx, "will-be-revoked")
	if err == nil {
		t.Fatal("expected rotation of revoked token to error")
	}
	// A revoked token exists, so the failure is a permission denial, not
	// a not-found. The CLI test only pins the not-found case; this is the
	// auth layer's own contract.
	de := domain.AsError(err)
	if de == nil {
		t.Fatalf("Rotate returned %T (%v); want *domain.Error", err, err)
	}
	if de.Code != domain.CodeForbidden {
		t.Fatalf("Rotate code = %q; want forbidden", de.Code)
	}
}

// ---------------------------------------------------------------------------
// Token reads
// ---------------------------------------------------------------------------

func TestList_RealStore_ReturnsAllActiveAndRevoked(t *testing.T) {
	ctx := context.Background()
	m := realManager(t)
	for i, name := range []string{"a", "b", "c"} {
		if _, err := m.MintAndStore(ctx, &domain.Token{
			ID: "tok-" + string(rune('a'+i)), Name: name,
			Scopes: domain.Scopes{domain.ScopeWrite},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Tokens.Revoke(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	all, err := m.Tokens.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("List returned %d tokens, want 3", len(all))
	}
}

func TestCount_RealStore_MatchesInsertions(t *testing.T) {
	ctx := context.Background()
	m := realManager(t)
	if n, _ := m.Tokens.Count(ctx); n != 0 {
		t.Fatalf("count on empty store = %d, want 0", n)
	}
	for _, name := range []string{"x", "y"} {
		if _, err := m.MintAndStore(ctx, &domain.Token{
			ID: "tok-" + name, Name: name,
			Scopes: domain.Scopes{domain.ScopeWrite},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := m.Tokens.Count(ctx); n != 2 {
		t.Fatalf("count after two inserts = %d, want 2", n)
	}
}

func TestGetByName_RealStore_ReturnsNotFoundForAbsentRow(t *testing.T) {
	ctx := context.Background()
	m := realManager(t)
	tok, err := m.Tokens.GetByName(ctx, "nope")
	if tok != nil {
		t.Fatalf("GetByName returned non-nil token: %+v", tok)
	}
	if e := domain.AsError(err); e == nil || e.Code != domain.CodeNotFound {
		t.Fatalf("GetByName err = %v, want CodeNotFound", err)
	}
}

func TestRevoke_RealStore_FlipsActive(t *testing.T) {
	ctx := context.Background()
	m := realManager(t)
	secret, _ := m.MintAndStore(ctx, &domain.Token{ID: "tok-k", Name: "k", Scopes: domain.Scopes{domain.ScopeRead}})
	if err := m.Tokens.Revoke(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if tok, _ := m.VerifyToken(ctx, secret); tok != nil {
		t.Fatal("revoked token still verifies")
	}
}

// ---------------------------------------------------------------------------
// Sessions — exercise the full session path end-to-end on a real store so
// the auth surface cannot drift away from the real schema.
// ---------------------------------------------------------------------------

func TestCreateSession_RealStore_PersistsAndReadsBack(t *testing.T) {
	ctx := context.Background()
	m := realManager(t)
	// Mint a real token first so CreateSession has something to verify.
	secret, err := m.MintAndStore(ctx, &domain.Token{ID: "tok-sess", Name: "session-user", Scopes: domain.Scopes{domain.ScopeWrite}})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := m.CreateSession(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	if sess == nil || sess.ID == "" {
		t.Fatal("session has no id")
	}
	got, err := m.Sessions.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != sess.ID {
		t.Fatalf("GetSession returned %+v", got)
	}
}

func TestSessions_RealStore_TouchUpdatesLastSeen(t *testing.T) {
	ctx := context.Background()
	m := realManager(t)
	secret, err := m.MintAndStore(ctx, &domain.Token{ID: "tok-touch", Name: "touch-user", Scopes: domain.Scopes{domain.ScopeWrite}})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := m.CreateSession(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	before := sess.LastSeenAt
	if err := m.Sessions.TouchSession(ctx, sess.ID, before.Add(1)); err != nil {
		t.Fatal(err)
	}
	got, err := m.Sessions.GetSession(ctx, sess.ID)
	if err != nil || got == nil {
		t.Fatalf("GetSession after Touch: (%v, %v)", got, err)
	}
}

func TestSessions_RealStore_DeleteRemovesRow(t *testing.T) {
	ctx := context.Background()
	m := realManager(t)
	secret, err := m.MintAndStore(ctx, &domain.Token{ID: "tok-del", Name: "delete-user", Scopes: domain.Scopes{domain.ScopeWrite}})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := m.CreateSession(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Sessions.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Sessions.GetSession(ctx, sess.ID); got != nil {
		t.Fatalf("session survived delete: %+v", got)
	}
}

func TestSessions_RealStore_AbsentRowReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	m := realManager(t)
	got, err := m.Sessions.GetSession(ctx, "no-such-session")
	if got != nil {
		t.Fatalf("GetSession returned %+v for absent id", got)
	}
	if e := domain.AsError(err); e == nil || e.Code != domain.CodeNotFound {
		t.Fatalf("GetSession err = %v, want CodeNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Wire-level: end-to-end bearer round-trip through RequireAuth.
// ---------------------------------------------------------------------------

func TestRequireAuth_RealStore_HappyPathReturns200(t *testing.T) {
	ctx := context.Background()
	m := realManager(t)
	secret, _ := m.Bootstrap(ctx, "")

	hits := 0
	h := m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if hits != 1 {
		t.Fatalf("handler called %d times, want 1", hits)
	}
}
