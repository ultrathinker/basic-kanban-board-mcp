package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// A container started twice against the same volume with KANBAN_ADMIN_TOKEN
// set must come up both times. The second Bootstrap sees an admin row that
// already exists; it must reuse it, not try to insert a second one and die on
// tokens_name_uniq. Getting this wrong is a crash loop on the documented
// Docker deployment, and only shows up on the *second* start — which is why
// nothing caught it until a reviewer read the code.
func TestBootstrap_SuppliedToken_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kanban.db")
	const supplied = "kbn_suppliedadmin000000000000000000000000"

	open := func() (store.Store, *Manager) {
		st, err := store.Open(context.Background(), store.Config{Path: path})
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		return st, NewManagerFromStore(st, nil, "", true, nil)
	}

	// First start: mints the supplied token.
	st1, m1 := open()
	if _, err := m1.Bootstrap(context.Background(), supplied); err != nil {
		st1.Close()
		t.Fatalf("first Bootstrap: %v", err)
	}
	st1.Close()

	// Second start against the same file — the container restart.
	st2, m2 := open()
	defer st2.Close()
	if _, err := m2.Bootstrap(context.Background(), supplied); err != nil {
		t.Fatalf("Bootstrap on restart failed: %v\n"+
			"A container with KANBAN_ADMIN_TOKEN set and a persistent volume "+
			"cannot start a second time.", err)
	}

	// And the supplied secret must still authenticate afterwards.
	tok, err := m2.VerifyToken(context.Background(), supplied)
	if err != nil {
		t.Fatalf("VerifyToken after restart: %v", err)
	}
	if tok == nil {
		t.Fatal("supplied admin token no longer authenticates after restart")
	}
}

// openBootstrapStore opens (or reopens) a database at path and returns it with
// a Manager wired to it. Reopening the same path is how these tests spell
// "the container restarted".
func openBootstrapStore(t *testing.T, path string) (store.Store, *Manager) {
	t.Helper()
	st, err := store.Open(context.Background(), store.Config{Path: path})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	return st, NewManagerFromStore(st, nil, "", true, nil)
}

func countTokens(t *testing.T, st store.Store) int {
	t.Helper()
	var n int
	if err := st.Read(context.Background(), func(tx store.Tx) error {
		c, err := st.Tokens().Count(tx)
		n = c
		return err
	}); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	return n
}

// Branch 1 of the restart decision: the environment and the database agree.
// The row is reused as it stands — no second row, and no secret handed back,
// because nothing was minted and the startup banner has nothing new to print.
func TestBootstrap_SuppliedToken_MatchingSecretReusesRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kanban.db")
	const supplied = "kbn_matchingsecret0000000000000000000000"

	st1, m1 := openBootstrapStore(t, path)
	first, err := m1.Bootstrap(context.Background(), supplied)
	if err != nil {
		st1.Close()
		t.Fatalf("first Bootstrap: %v", err)
	}
	if first != supplied {
		st1.Close()
		t.Fatalf("first Bootstrap returned %q, want the supplied secret", first)
	}
	st1.Close()

	st2, m2 := openBootstrapStore(t, path)
	defer st2.Close()
	second, err := m2.Bootstrap(context.Background(), supplied)
	if err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	if second != "" {
		t.Fatalf("second Bootstrap returned %q, want empty: nothing was minted, so the "+
			"banner must not reprint a credential the operator already has", second)
	}
	if n := countTokens(t, st2); n != 1 {
		t.Fatalf("token count = %d, want 1: the restart must not add a row", n)
	}
}

// Branch 2: the environment and the database disagree. Rewriting the stored
// hash from an env var would silently undo a deliberate rotation — the
// operator who rotates after a leak and later reboots would get the leaked
// secret back. So this refuses, and the stored credential is left alone.
func TestBootstrap_SuppliedToken_DifferentSecretRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kanban.db")
	const original = "kbn_originaladminsecret00000000000000000"
	const replacement = "kbn_replacementadminsecret000000000000000"

	st1, m1 := openBootstrapStore(t, path)
	if _, err := m1.Bootstrap(context.Background(), original); err != nil {
		st1.Close()
		t.Fatalf("first Bootstrap: %v", err)
	}
	st1.Close()

	st2, m2 := openBootstrapStore(t, path)
	defer st2.Close()
	got, err := m2.Bootstrap(context.Background(), replacement)
	if err == nil {
		t.Fatalf("Bootstrap with a different secret succeeded (returned %q); the stored "+
			"admin credential must not be replaced from the environment", got)
	}
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v (%T), want a domain.Forbidden so callers can branch on the code", err, err)
	}
	// The CLI prints Message, never Remediation, so the refusal has to say
	// what to do inside the message itself.
	if !strings.Contains(err.Error(), "KANBAN_ADMIN_TOKEN") {
		t.Fatalf("refusal %q does not name KANBAN_ADMIN_TOKEN; the operator cannot act on it", err)
	}

	// The database is untouched: the original still authenticates, the
	// replacement does not.
	tok, verr := m2.VerifyToken(context.Background(), original)
	if verr != nil || tok == nil {
		t.Fatalf("original secret stopped working after a refused bootstrap: (%v, %v)", tok, verr)
	}
	tok, verr = m2.VerifyToken(context.Background(), replacement)
	if verr != nil {
		t.Fatalf("VerifyToken(replacement): %v", verr)
	}
	if tok != nil {
		t.Fatal("the refused secret authenticates; the hash was rotated after all")
	}
	if n := countTokens(t, st2); n != 1 {
		t.Fatalf("token count = %d, want 1", n)
	}
}

// Branch 3: the admin row exists but was revoked. Reusing it would hand the
// operator a credential that cannot authenticate; un-revoking it would
// resurrect one somebody deliberately killed. Refuse and say so.
func TestBootstrap_SuppliedToken_RevokedRowRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kanban.db")
	const supplied = "kbn_revokedadminsecret000000000000000000"

	st1, m1 := openBootstrapStore(t, path)
	if _, err := m1.Bootstrap(context.Background(), supplied); err != nil {
		st1.Close()
		t.Fatalf("first Bootstrap: %v", err)
	}
	if err := st1.Write(context.Background(), func(tx store.Tx) error {
		return st1.Tokens().Revoke(tx, "admin")
	}); err != nil {
		st1.Close()
		t.Fatalf("revoke admin: %v", err)
	}
	st1.Close()

	st2, m2 := openBootstrapStore(t, path)
	defer st2.Close()
	if _, err := m2.Bootstrap(context.Background(), supplied); err == nil {
		t.Fatal("Bootstrap accepted a revoked admin row; the server would come up with " +
			"an admin token that cannot authenticate")
	} else if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v (%T), want a domain.Forbidden", err, err)
	} else if !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("refusal %q does not say the row is revoked", err)
	}
}

// A malformed KANBAN_ADMIN_TOKEN must be refused identically on the first
// start and on every later one: the shape is checked before the lookup, so
// the operator gets the same message whatever state the database is in.
func TestBootstrap_SuppliedToken_MalformedRefusedAfterBootstrap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kanban.db")
	const supplied = "kbn_wellformedadminsecret0000000000000000"

	st1, m1 := openBootstrapStore(t, path)
	if _, err := m1.Bootstrap(context.Background(), supplied); err != nil {
		st1.Close()
		t.Fatalf("first Bootstrap: %v", err)
	}
	st1.Close()

	st2, m2 := openBootstrapStore(t, path)
	defer st2.Close()
	_, err := m2.Bootstrap(context.Background(), "opaque-string-without-prefix")
	if err == nil {
		t.Fatal("Bootstrap accepted a secret without the kbn_ prefix")
	}
	var be *BootstrapError
	if !errors.As(err, &be) {
		t.Fatalf("error = %v (%T), want *BootstrapError as on a fresh database", err, err)
	}
}
