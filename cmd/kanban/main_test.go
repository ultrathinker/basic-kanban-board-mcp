package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/config"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// envMap builds an EnvLookup from a plain map. Tests use it so they never
// touch the real process environment.
func envMap(m map[string]string) config.EnvLookup {
	return func(name string) string { return m[name] }
}

// buildServe is a small helper that runs config.Build + config.Validate for
// the given argv slice. It returns the typed refusal error so callers can
// assert on the code (refusal message can be reworded without breaking
// the suite).
func buildServe(t *testing.T, args ...string) (config.Config, error) {
	t.Helper()
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfg, _, err := config.Build(config.Inputs{
		FS:   fs,
		Env:  envMap(nil),
		Args: args,
		Now:  time.Now,
	})
	if err != nil {
		return cfg, err
	}
	if err := config.Validate(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func TestServe_LoopbackNoAuth(t *testing.T) {
	cfg, err := buildServe(t, "--addr=127.0.0.1:8080", "--auth=off")
	if err != nil {
		t.Fatalf("loopback+auth=off should be accepted: %v", err)
	}
	if cfg.Auth != config.AuthOff {
		t.Fatalf("auth=%v want off", cfg.Auth)
	}
}

func TestServe_NonLoopbackAuthOffRefused(t *testing.T) {
	_, err := buildServe(t, "--addr=0.0.0.0:8080", "--auth=off")
	r := config.AsRefusal(err)
	if r == nil || r.Code != config.RefuseAuthOffRemote {
		t.Fatalf("expected RefuseAuthOffRemote, got %v", err)
	}
	if r.Flag != "auth" {
		t.Fatalf("expected fix-it flag 'auth', got %q", r.Flag)
	}
}

func TestServe_NonLoopbackHTTPRequiresInsecureFlag(t *testing.T) {
	_, err := buildServe(t, "--addr=0.0.0.0:8080", "--base-url=http://example.com")
	r := config.AsRefusal(err)
	if r == nil || r.Code != config.RefuseInsecureHTTP {
		t.Fatalf("expected RefuseInsecureHTTP, got %v", err)
	}
	if r.Flag != "insecure-http" {
		t.Fatalf("expected fix-it flag 'insecure-http', got %q", r.Flag)
	}

	// With the flag it must pass.
	_, err = buildServe(t, "--addr=0.0.0.0:8080", "--base-url=http://example.com", "--insecure-http")
	if err != nil {
		t.Fatalf("--insecure-http should let it through: %v", err)
	}
}

func TestServe_HTTPSAccepted(t *testing.T) {
	cfg, err := buildServe(t, "--addr=0.0.0.0:8080", "--base-url=https://kanban.example.com")
	if err != nil {
		t.Fatalf("https base-url should be accepted on non-loopback: %v", err)
	}
	if cfg.BaseURL != "https://kanban.example.com" {
		t.Fatalf("BaseURL=%q", cfg.BaseURL)
	}
}

func TestServe_AuthInvalid(t *testing.T) {
	_, err := buildServe(t, "--addr=127.0.0.1:8080", "--auth=maybe")
	if err == nil {
		t.Fatalf("invalid auth value must error")
	}
}

func TestServe_TrustedProxiesSplit(t *testing.T) {
	cfg, err := buildServe(t,
		"--addr=127.0.0.1:8080",
		"--trusted-proxies=10.0.0.0/8, 192.168.0.0/16 ,",
	)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(cfg.TrustedProxies) != 2 {
		t.Fatalf("got %d proxies, want 2", len(cfg.TrustedProxies))
	}
}

func TestServe_DefaultsApplied(t *testing.T) {
	cfg, err := buildServe(t)
	if err != nil {
		t.Fatalf("defaults must pass validation: %v", err)
	}
	if cfg.Addr != config.DefaultAddr {
		t.Fatalf("Addr=%q want %q", cfg.Addr, config.DefaultAddr)
	}
	if cfg.Auth != config.AuthRequired {
		t.Fatalf("Auth=%v want required", cfg.Auth)
	}
}

func TestServe_LoopbackShortFormRefuses(t *testing.T) {
	// ":8080" binds every interface — config.IsLoopbackHost treats it as
	// remote, so --auth=off must be refused.
	_, err := buildServe(t, "--addr=:8080", "--auth=off")
	r := config.AsRefusal(err)
	if r == nil || r.Code != config.RefuseAuthOffRemote {
		t.Fatalf("expected RefuseAuthOffRemote for :8080, got %v", err)
	}
}

func TestRenderAgentConfig_AllClients(t *testing.T) {
	cases := []string{"claude", "codex", "cursor", "generic", ""}
	for _, c := range cases {
		out := renderAgentConfig(c, "http://127.0.0.1:8080", "kbn_test")
		if out == "" {
			t.Fatalf("client %q returned empty output", c)
		}
		if !strings.Contains(out, "127.0.0.1:8080") && !strings.Contains(out, "url") {
			t.Fatalf("client %q output missing URL: %s", c, out)
		}
	}
}

func TestRenderAgentConfig_UnknownClient(t *testing.T) {
	out := renderAgentConfig("weird-codename", "http://x", "")
	if out != "" {
		t.Fatalf("unknown client should return empty, got %s", out)
	}
}

func TestRenderAgentConfig_NoTokenPlaceholder(t *testing.T) {
	out := renderAgentConfig("claude", "http://x", "")
	if !strings.Contains(out, "paste-token-here") {
		t.Fatalf("missing token placeholder: %s", out)
	}
}

func TestNormalizeName(t *testing.T) {
	if _, err := normalizeName("  "); err == nil {
		t.Fatalf("empty must error")
	}
	if _, err := normalizeName("a b"); err == nil {
		t.Fatalf("whitespace must error")
	}
	got, err := normalizeName("claude@rog")
	if err != nil || got != "claude@rog" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestIsScope(t *testing.T) {
	for _, v := range []string{"read", "write", "admin"} {
		if !isScope(v) {
			t.Fatalf("%q should be a valid scope", v)
		}
	}
	if isScope("super") {
		t.Fatalf("super should not be a valid scope")
	}
}

func TestFormatScopes(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, "-"},
		{[]string{"admin"}, "admin"},
		{[]string{"admin", "read"}, "admin,read"},
	}
	for _, tc := range cases {
		ds := make(domain.Scopes, len(tc.in))
		for i, s := range tc.in {
			ds[i] = domain.Scope(s)
		}
		out := formatScopes(ds)
		if out != tc.want {
			t.Fatalf("in=%v out=%q want %q", tc.in, out, tc.want)
		}
	}
}

func TestUsage_ContainsAllSubcommands(t *testing.T) {
	b := &strings.Builder{}
	usage(b)
	out := b.String()
	for _, sub := range []string{"serve", "token", "agent-config", "agent-md",
		"export", "import", "backup", "task", "doctor", "healthcheck",
		"mcp", "migrate", "demo", "version"} {
		if !strings.Contains(out, sub) {
			t.Fatalf("usage missing %q", sub)
		}
	}
}

func TestRunVersion(t *testing.T) {
	b := &strings.Builder{}
	runVersion(b)
	if !strings.HasPrefix(b.String(), "kanban ") {
		t.Fatalf("unexpected version output: %s", b.String())
	}
}

func TestRunAgentMD_NonEmpty(t *testing.T) {
	b := &strings.Builder{}
	if err := runAgentMD(b); err != nil {
		t.Fatalf("agent-md: %v", err)
	}
	if !strings.Contains(b.String(), "board_get") || !strings.Contains(b.String(), "task_next") {
		t.Fatalf("agent-md missing expected content")
	}
}

func TestRunToken_NoSubcommand(t *testing.T) {
	if err := runToken(nil); err == nil {
		t.Fatalf("expected error")
	}
	if err := runToken([]string{"bogus"}); err == nil {
		t.Fatalf("expected error")
	}
}

func TestRunTask_NoSubcommand(t *testing.T) {
	if err := runTask(nil); err == nil {
		t.Fatalf("expected error")
	}
}

func TestRunTask_Purge_NoKeys(t *testing.T) {
	if err := taskPurge([]string{"--yes"}); err == nil {
		t.Fatalf("expected error for empty key list")
	}
}

func TestRunMCP_NonStdioRefused(t *testing.T) {
	if err := runMCP([]string{"--stdio=false"}); err == nil {
		t.Fatalf("non-stdio must refuse")
	}
}

func TestRunServe_ExitCodeIsOneOnRefusal(t *testing.T) {
	// runServe should NOT call os.Exit itself; it should return the typed
	// refusal error so the calling `exit(err)` handler prints it.
	err := runServe([]string{"--addr=0.0.0.0:8080", "--auth=off"})
	if err == nil {
		t.Fatalf("expected refusal error")
	}
	if r := config.AsRefusal(err); r == nil || r.Code != config.RefuseAuthOffRemote {
		t.Fatalf("expected RefuseAuthOffRemote, got %v", err)
	}
}

func TestRunHealthcheck_ProbeOverride(t *testing.T) {
	prev := probeHealthz
	probeHealthz = func(addr string, timeout time.Duration) error { return nil }
	t.Cleanup(func() { probeHealthz = prev })

	if err := runHealthcheck(nil); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// Compile-time checks that our flag sets are constructable without panicking.
func TestFlagConstructors(t *testing.T) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.String("addr", "127.0.0.1:8080", "")
	fs.String("data", "./data", "")
	fs.String("base-url", "", "")
	fs.String("auth", "required", "")
	fs.Bool("insecure-http", false, "")
	fs.String("trusted-proxies", "", "")
	fs.Bool("demo", false, "")
}

// TestPathImport keeps filepath / io imports alive even when vet is run on
// this file in isolation (none are used directly after the rewrite).
func TestPathImport(t *testing.T) {
	_ = io.Discard
}

// ---------------------------------------------------------------------------
// Store round-trip tests.
//
// These run the actual CLI commands against a real SQLite store in a temp
// directory and assert the round-trip semantics. The previous round of
// reviews caught a broken token rotate because no test exercised it against
// a real store — `go test ./cmd/...` was green while the command could never
// succeed. The tests below make that impossible to regress.
//
// We override the openStore seam to point at a temp-dir SQLite file, then
// capture stdout to read the secret the command prints. The secret is never
// reused elsewhere; once the test prints its single use, it is gone.
// ---------------------------------------------------------------------------

// withTempStore swaps openStore with one that uses a per-test temp dir,
// opens the store at the standard path, and returns a teardown. Tests must
// defer the returned func.
func withTempStore(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	prev := openStore
	openStore = func(ctx context.Context, dataDir string) (store.Store, error) {
		return store.Open(ctx, store.Config{Path: filepath.Join(dir, "kanban.db")})
	}
	return dir, func() { openStore = prev }
}

// captureStdout redirects os.Stdout for the duration of fn, returning the
// captured output. The redirect is restored even if fn panics.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fnErr := fn()
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out, fnErr
}

// secretRegex extracts a token secret from the create / rotate stdout. The
// create command prints "Secret: …"; rotate prints "New secret: …". Any
// stronger parser would couple the test to copy we own.
var secretRegex = regexp.MustCompile(`(?m)^(?:Secret|New secret):\s+(kbn_[A-Za-z0-9]+)\s*$`)

func mustExtractSecret(t *testing.T, out string) string {
	t.Helper()
	m := secretRegex.FindStringSubmatch(out)
	if len(m) < 2 {
		t.Fatalf("no Secret: / New secret: line in stdout:\n%s", out)
	}
	return m[1]
}

// openTempStore opens a fresh store at dataDir/kanban.db. Every caller MUST
// defer Close: on Windows the writer pool keeps a file handle that blocks
// t.TempDir cleanup.
func openTempStore(t *testing.T, dataDir string) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), store.Config{Path: filepath.Join(dataDir, "kanban.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

// readTokenByName fetches the persisted token row inside a single read tx.
// It opens one store, runs the read, and closes — no handle leak across the
// t.TempDir cleanup boundary.
func readTokenByName(t *testing.T, dataDir, name string) *domain.Token {
	t.Helper()
	st := openTempStore(t, dataDir)
	defer st.Close()
	var out *domain.Token
	err := st.Read(context.Background(), func(tx store.Tx) error {
		tk, err := st.Tokens().GetByName(tx, name)
		out = tk
		return err
	})
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	if out == nil {
		t.Fatalf("token %q not found", name)
	}
	return out
}

// tokenAuthenticate verifies whether a candidate secret resolves to a live
// token. It treats both "token not found" and "any other lookup error" as
// a non-authenticating secret: the auth layer's contract is binary — either
// the secret works or it does not — so this matches the caller's question
// rather than the underlying error type.
func tokenAuthenticate(t *testing.T, dataDir, secret string) bool {
	t.Helper()
	st := openTempStore(t, dataDir)
	defer st.Close()
	mgr := auth.NewManagerFromStore(st, nil, "", false, time.Now)
	tok, err := mgr.VerifyToken(context.Background(), secret)
	if err != nil {
		// not_found on the row is the same outcome as "unknown token" for
		// the caller: the secret does not authenticate. Anything else is a
		// real failure (DB down, hash mismatch via store corruption).
		var de *domain.Error
		if errorsAs(err, &de) && de.Code == domain.CodeNotFound {
			return false
		}
		t.Fatalf("verify: %v", err)
	}
	return tok != nil
}

// TestTokenCLI_RoundTrip_CreateRotateRevoke is the regression test for the
// rotate defect: it creates a token, rotates it, asserts the same row was
// updated (id and name preserved), and that the old secret no longer
// authenticates while the new one does. Then it revokes the token and
// confirms authentication fails.
func TestTokenCLI_RoundTrip_CreateRotateRevoke(t *testing.T) {
	dir, restore := withTempStore(t)
	defer restore()

	// Capture stdout for the create command and extract the first secret.
	createOut, err := captureStdout(t, func() error {
		return runToken([]string{"create", "--name", "test-cli", "--scope", "write",
			"--data", dir})
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	original := mustExtractSecret(t, createOut)
	originalRow := readTokenByName(t, dir, "test-cli")
	if !tokenAuthenticate(t, dir, original) {
		t.Fatalf("original secret does not authenticate right after create")
	}

	// Rotate. The new secret must be different from the original, the row
	// must keep its id and name, and the new secret must authenticate while
	// the old one must not.
	rotateOut, err := captureStdout(t, func() error {
		return runToken([]string{"rotate", "test-cli", "--data", dir})
	})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	rotated := mustExtractSecret(t, rotateOut)
	if rotated == original {
		t.Fatalf("rotated secret is the same as the original")
	}

	rotatedRow := readTokenByName(t, dir, "test-cli")
	if rotatedRow.ID != originalRow.ID {
		t.Fatalf("rotation changed token id: %q -> %q", originalRow.ID, rotatedRow.ID)
	}
	if rotatedRow.Name != originalRow.Name {
		t.Fatalf("rotation changed token name: %q -> %q", originalRow.Name, rotatedRow.Name)
	}
	if !rotatedRow.CreatedAt.Equal(originalRow.CreatedAt) {
		t.Fatalf("rotation changed token created_at: %v -> %v",
			originalRow.CreatedAt, rotatedRow.CreatedAt)
	}
	if bytesEqual(rotatedRow.Scopes, originalRow.Scopes) == false {
		t.Fatalf("rotation changed token scopes")
	}

	if tokenAuthenticate(t, dir, original) {
		t.Fatalf("old secret still authenticates after rotate")
	}
	if !tokenAuthenticate(t, dir, rotated) {
		t.Fatalf("rotated secret does not authenticate")
	}

	// Revoke and confirm both secrets fail.
	if err := runToken([]string{"revoke", "test-cli", "--data", dir}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if tokenAuthenticate(t, dir, original) {
		t.Fatalf("old secret authenticates after revoke")
	}
	if tokenAuthenticate(t, dir, rotated) {
		t.Fatalf("rotated secret authenticates after revoke")
	}
}

// bytesEqual reports whether two scope slices are identical.
func bytesEqual(a, b domain.Scopes) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestTokenCLI_RotateMissingToken asserts the friendly error path: rotating
// a token that does not exist returns a not-found error rather than
// silently succeeding.
func TestTokenCLI_RotateMissingToken(t *testing.T) {
	dir, restore := withTempStore(t)
	defer restore()

	err := runToken([]string{"rotate", "no-such-token", "--data", dir})
	if err == nil {
		t.Fatalf("expected error for missing token")
	}
	var de *domain.Error
	if !errorsAs(err, &de) {
		t.Fatalf("expected *domain.Error, got %T: %v", err, err)
	}
	if de.Code != domain.CodeNotFound {
		t.Fatalf("expected not_found, got %q", de.Code)
	}
}

// TestTokenCLI_RotateTwice asserts successive rotations each succeed and
// the older secret never re-authenticates. Catches a class of bugs where
// rotation only works the first time because of state leaks in the
// underlying repo.
func TestTokenCLI_RotateTwice(t *testing.T) {
	dir, restore := withTempStore(t)
	defer restore()

	out1, err := captureStdout(t, func() error {
		return runToken([]string{"create", "--name", "twice", "--scope", "write",
			"--data", dir})
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	s1 := mustExtractSecret(t, out1)
	id1 := readTokenByName(t, dir, "twice").ID

	out2, err := captureStdout(t, func() error {
		return runToken([]string{"rotate", "twice", "--data", dir})
	})
	if err != nil {
		t.Fatalf("rotate #1: %v", err)
	}
	s2 := mustExtractSecret(t, out2)
	id2 := readTokenByName(t, dir, "twice").ID
	if s1 == s2 || id1 != id2 {
		t.Fatalf("first rotation: secret identical=%v, id changed=%v", s1 == s2, id1 != id2)
	}

	out3, err := captureStdout(t, func() error {
		return runToken([]string{"rotate", "twice", "--data", dir})
	})
	if err != nil {
		t.Fatalf("rotate #2: %v", err)
	}
	s3 := mustExtractSecret(t, out3)
	id3 := readTokenByName(t, dir, "twice").ID
	if s3 == s2 || id2 != id3 {
		t.Fatalf("second rotation: secret identical=%v, id changed=%v", s3 == s2, id2 != id3)
	}

	if tokenAuthenticate(t, dir, s1) {
		t.Fatalf("original secret authenticates after two rotations")
	}
	if tokenAuthenticate(t, dir, s2) {
		t.Fatalf("first rotated secret still authenticates")
	}
	if !tokenAuthenticate(t, dir, s3) {
		t.Fatalf("most-recent secret does not authenticate")
	}
}

// errorsAs is a thin shim so the test does not import "errors" directly
// (which is otherwise unused after the rewrite).
func errorsAs(err error, target **domain.Error) bool {
	for err != nil {
		if de, ok := err.(*domain.Error); ok {
			*target = de
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
