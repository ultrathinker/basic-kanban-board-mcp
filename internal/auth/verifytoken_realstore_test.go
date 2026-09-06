package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// These tests run against the REAL store rather than the in-package fake.
// The fake's TokenRepo.GetByHash returns (nil, nil) for an absent row, but the
// real one returns (nil, domain.NotFound) — see internal/store/scan-path
// scanToken. Anything that verifies a bearer token therefore behaves
// differently in production than it does under the fake, which is exactly the
// gap these tests exist to close.

func realStoreManager(t *testing.T) *Manager {
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

// An unknown but well-formed bearer token must be refused as forbidden, not
// reported as a server error. It is an ordinary bad credential.
func TestVerifyToken_UnknownSecret_IsNotAnError(t *testing.T) {
	m := realStoreManager(t)

	tok, err := m.VerifyToken(context.Background(), TokenPrefix+"deadbeefdeadbeefdeadbeef")
	if tok != nil {
		t.Fatalf("VerifyToken returned a token for an unknown secret: %+v", tok)
	}
	if err != nil {
		t.Fatalf("VerifyToken on an unknown secret returned an error (%v); want (nil, nil) "+
			"so RequireAuth can map it to 403", err)
	}
}

// End to end: the wire status for an unknown bearer token must be 403, never
// 500. A 500 tells every MCP client the server is broken when in fact the
// caller's credential is simply wrong.
func TestRequireAuth_UnknownBearer_Returns403(t *testing.T) {
	m := realStoreManager(t)

	h := m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+TokenPrefix+"deadbeefdeadbeefdeadbeef")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("unknown bearer token produced %d %q; want 403", rec.Code, rec.Body.String())
	}
}

// Guard the underlying store contract directly, so a future change to
// scanToken cannot silently reintroduce the problem.
func TestStoreGetByHash_AbsentRow_ContractIsExplicit(t *testing.T) {
	st, err := store.Open(context.Background(), store.Config{
		Path: filepath.Join(t.TempDir(), "kanban.db"),
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	var got *domain.Token
	var getErr error
	if err := st.Read(context.Background(), func(tx store.Tx) error {
		got, getErr = st.Tokens().GetByHash(tx, []byte("no-such-hash"))
		return nil
	}); err != nil {
		t.Fatalf("Read: %v", err)
	}

	t.Logf("real store GetByHash(absent) -> token=%v err=%v", got, getErr)
	if got != nil {
		t.Fatalf("expected no token, got %+v", got)
	}
	// Documenting the current behaviour: this is what the auth layer must cope
	// with. If this assertion ever flips, VerifyToken can be simplified.
	if getErr == nil {
		t.Log("GetByHash now returns (nil, nil) for an absent row — VerifyToken's " +
			"error passthrough is no longer a hazard")
	}
}

// A revoked token must verify as a bad credential, not a server error.
// `Token.Active()` is true when RevokedAt is nil; the real store returns the
// row regardless of revocation, so the auth layer's Active() check is what
// actually denies the request.
func TestVerifyToken_RevokedSecret_IsNotAnError(t *testing.T) {
	m := realStoreManager(t)
	ctx := context.Background()

	secret, err := m.MintAndStore(ctx, &domain.Token{
		ID: "revoked-1", Name: "revoke-me",
		Scopes: domain.Scopes{domain.ScopeWrite},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Tokens.Revoke(ctx, "revoke-me"); err != nil {
		t.Fatal(err)
	}

	tok, err := m.VerifyToken(ctx, secret)
	if tok != nil {
		t.Fatalf("revoked token verified as non-nil: %+v", tok)
	}
	if err != nil {
		t.Fatalf("revoked token returned err = %v; want (nil, nil)", err)
	}
}

func TestRequireAuth_RevokedBearer_Returns403(t *testing.T) {
	m := realStoreManager(t)
	ctx := context.Background()

	secret, _ := m.MintAndStore(ctx, &domain.Token{
		ID: "revoked-2", Name: "revoke-me-2",
		Scopes: domain.Scopes{domain.ScopeWrite},
	})
	if err := m.Tokens.Revoke(ctx, "revoke-me-2"); err != nil {
		t.Fatal(err)
	}

	h := m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("revoked bearer produced %d %q; want 403", rec.Code, rec.Body.String())
	}
}

// Malformed token (no kbn_ prefix) must already short-circuit at the
// shape check inside VerifyToken — never reaching the store. The wire
// status is 403 for symmetry with the unknown-secret case.
func TestRequireAuth_MalformedBearer_Returns403(t *testing.T) {
	m := realStoreManager(t)

	h := m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer not-a-kbn-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("malformed bearer produced %d %q; want 403", rec.Code, rec.Body.String())
	}
}

// Bearer-less request is the basic 401 path; it has always worked, the
// real-store test pins it down so the chain above stays observable.
func TestRequireAuth_NoCredentials_Returns401(t *testing.T) {
	m := realStoreManager(t)

	h := m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credentials produced %d %q; want 401", rec.Code, rec.Body.String())
	}
}

// Bootstrap on an EMPTY real store must produce a usable admin token.
// This is the regression test for round 4: mintAndStore used to omit the
// primary key, so every first boot of the server died at
// "id, name and hash are required". The fix is to generate the ID inside
// mintAndStore; this test runs against the real SQLite store and asserts
// both that Bootstrap returns a secret AND that the secret then verifies.
func TestBootstrap_EmptyStore_RealStoreRoundTrip(t *testing.T) {
	m := realStoreManager(t)
	ctx := context.Background()

	secret, err := m.Bootstrap(ctx, "")
	if err != nil {
		t.Fatalf("Bootstrap on empty real store failed: %v", err)
	}
	if !strings.HasPrefix(secret, TokenPrefix) {
		t.Fatalf("secret shape = %q, want %q", secret, TokenPrefix+"...")
	}

	tok, err := m.VerifyToken(ctx, secret)
	if err != nil {
		t.Fatalf("Bootstrap-generated secret does not verify: %v", err)
	}
	if tok == nil {
		t.Fatal("Bootstrap-generated secret verifies as nil")
	}
	if tok.Name != "admin" {
		t.Fatalf("name = %q, want admin", tok.Name)
	}
	if !tok.Scopes.Has(domain.ScopeAdmin) {
		t.Fatalf("scopes = %v, want admin", tok.Scopes)
	}
	if tok.ID == "" {
		t.Fatal("Bootstrap row has empty ID")
	}

	// A second Bootstrap on the same store must be a no-op (table is
	// non-empty): secret comes back empty.
	again, err := m.Bootstrap(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if again != "" {
		t.Fatalf("second Bootstrap returned %q, want empty", again)
	}
}

// Bootstrap with an operator-supplied secret on an empty store must also
// succeed and verify.
func TestBootstrap_SuppliedSecret_RealStoreRoundTrip(t *testing.T) {
	m := realStoreManager(t)
	ctx := context.Background()

	supplied := TokenPrefix + "bootstrap-supplied-secret-aaaaa-bbbb"
	got, err := m.Bootstrap(ctx, supplied)
	if err != nil {
		t.Fatal(err)
	}
	if got != supplied {
		t.Fatalf("returned %q, want %q", got, supplied)
	}
	tok, err := m.VerifyToken(ctx, supplied)
	if err != nil || tok == nil {
		t.Fatalf("supplied secret does not verify: (%v, %v)", tok, err)
	}
}
