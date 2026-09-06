package main

import (
	"flag"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/config"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
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
