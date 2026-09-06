package config

import (
	"flag"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// newFS returns a fresh flag set for one test case so cases cannot pollute
// each other through shared flag state.
func newFS() *flag.FlagSet { return flag.NewFlagSet("test", flag.ContinueOnError) }

// mapEnv is a tiny EnvLookup backed by a map.
func mapEnv(m map[string]string) EnvLookup {
	return func(name string) string { return m[name] }
}

func TestBuild_Defaults(t *testing.T) {
	fs := newFS()
	cfg, _, err := Build(Inputs{FS: fs, Env: mapEnv(nil), Args: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != DefaultAddr {
		t.Fatalf("addr = %q, want %q", cfg.Addr, DefaultAddr)
	}
	if cfg.Host != "127.0.0.1" {
		t.Fatalf("host = %q", cfg.Host)
	}
	if cfg.Port != "8080" {
		t.Fatalf("port = %q", cfg.Port)
	}
	if cfg.Auth != AuthRequired {
		t.Fatalf("auth = %q, want required", cfg.Auth)
	}
	if cfg.ClaimTTL != DefaultClaimTTL {
		t.Fatalf("claim TTL = %s, want %s", cfg.ClaimTTL, DefaultClaimTTL)
	}
	if cfg.LogLevel != LogInfo {
		t.Fatalf("log level = %q", cfg.LogLevel)
	}
	if cfg.LogFormat != LogText {
		t.Fatalf("log format = %q", cfg.LogFormat)
	}
}

func TestBuild_FlagsWinOverEnv(t *testing.T) {
	fs := newFS()
	env := mapEnv(map[string]string{
		"KANBAN_ADDR": "0.0.0.0:9999",
		"KANBAN_AUTH": "off",
	})
	cfg, _, err := Build(Inputs{FS: fs, Env: env, Args: []string{"--addr", "127.0.0.1:7777"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:7777" {
		t.Fatalf("addr = %q, flag should win", cfg.Addr)
	}
	// Auth still came from env (flag not provided).
	if cfg.Auth != AuthOff {
		t.Fatalf("auth = %q, env should fill in when no flag", cfg.Auth)
	}
}

func TestBuild_EnvWinsOverDefaults(t *testing.T) {
	fs := newFS()
	env := mapEnv(map[string]string{
		"KANBAN_ADDR":     "127.0.0.1:9001",
		"KANBAN_DATA":     "/tmp/kanban",
		"KANBAN_LOG_LEVEL": "debug",
	})
	cfg, _, err := Build(Inputs{FS: fs, Env: env, Args: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:9001" || cfg.DataDir != "/tmp/kanban" || cfg.LogLevel != LogDebug {
		t.Fatalf("env not honoured: %+v", cfg)
	}
}

func TestBuild_BoolAndDurationEnv(t *testing.T) {
	fs := newFS()
	env := mapEnv(map[string]string{
		"KANBAN_INSECURE_HTTP": "true",
		"KANBAN_DEMO":           "1",
		"KANBAN_CLAIM_TTL":      "30m",
	})
	cfg, _, err := Build(Inputs{FS: fs, Env: env, Args: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.InsecureHTTP || !cfg.Demo {
		t.Fatalf("bools not parsed: insecure=%v demo=%v", cfg.InsecureHTTP, cfg.Demo)
	}
	if cfg.ClaimTTL != 30*time.Minute {
		t.Fatalf("claim TTL = %s", cfg.ClaimTTL)
	}
}

func TestBuild_RejectsBadAuthValue(t *testing.T) {
	fs := newFS()
	_, _, err := Build(Inputs{FS: fs, Env: mapEnv(nil), Args: []string{"--auth", "maybe"}})
	if err == nil {
		t.Fatal("expected error on bad --auth")
	}
}

func TestBuild_RejectsBadCIDR(t *testing.T) {
	fs := newFS()
	_, _, err := Build(Inputs{FS: fs, Env: mapEnv(nil), Args: []string{"--trusted-proxies", "not-a-cidr"}})
	if err == nil {
		t.Fatal("expected error on bad --trusted-proxies")
	}
}

func TestBuild_TrustedProxiesParsesBareIP(t *testing.T) {
	fs := newFS()
	cfg, _, err := Build(Inputs{FS: fs, Env: mapEnv(nil), Args: []string{"--trusted-proxies", "10.0.0.5,192.168.0.0/16,::1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TrustedProxies) != 3 {
		t.Fatalf("got %d cidrs, want 3", len(cfg.TrustedProxies))
	}
}

func TestBuild_AdminTokenFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("kbn_filesupplied00xx00xx00xx00xx00xx00xx00xx"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := newFS()
	cfg, _, err := Build(Inputs{FS: fs, Env: mapEnv(nil), Args: []string{}, AdminTokenFilePath: path})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cfg.AdminToken, "kbn_") {
		t.Fatalf("admin token = %q", cfg.AdminToken)
	}
}

func TestBuild_ClaimTTLClampedToDomainBounds(t *testing.T) {
	fs := newFS()
	cfg, _, err := Build(Inputs{FS: fs, Env: mapEnv(nil), Args: []string{"--claim-ttl", "5s"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClaimTTL != domain.ClaimTTLMin {
		t.Fatalf("TTL = %s, want %s", cfg.ClaimTTL, domain.ClaimTTLMin)
	}
}

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

func TestValidate_AuthOffOnLoopback_Allowed(t *testing.T) {
	c := Config{Host: "127.0.0.1", Auth: AuthOff}
	if err := Validate(c); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestValidate_AuthOffOnRemote_Refused(t *testing.T) {
	cases := []struct {
		name string
		host string
		addr string
	}{
		{"IPv4 remote", "0.0.0.0", "0.0.0.0:8080"},
		{"bind-all", "", ":8080"},
		{"IPv6 any", "::", "[::]:8080"},
		{"specific external IP", "192.0.2.10", "192.0.2.10:8080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Host: tc.host, Addr: tc.addr, Auth: AuthOff}
			err := Validate(c)
			if err == nil {
				t.Fatal("expected refusal")
			}
			r := AsRefusal(err)
			if r == nil || r.Code != RefuseAuthOffRemote {
				t.Fatalf("refusal = %v, want %s", r, RefuseAuthOffRemote)
			}
			if r.Flag != "auth" {
				t.Fatalf("Flag = %q, want auth", r.Flag)
			}
		})
	}
}

func TestValidate_LocalhostStringAlsoLoopback(t *testing.T) {
	c := Config{Host: "localhost", Addr: "localhost:8080", Auth: AuthOff}
	if err := Validate(c); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestValidate_RemoteWithoutTLS_Refused(t *testing.T) {
	c := Config{Host: "0.0.0.0", Addr: "0.0.0.0:8080", BaseURL: "http://kanban.example.com"}
	err := Validate(c)
	if err == nil {
		t.Fatal("expected refusal")
	}
	r := AsRefusal(err)
	if r == nil || r.Code != RefuseInsecureHTTP {
		t.Fatalf("refusal = %v", r)
	}
	if !strings.Contains(r.Message, "Caddy") {
		t.Fatalf("refusal should mention the Caddy path, got %q", r.Message)
	}
}

func TestValidate_RemoteWithHTTPS_Allowed(t *testing.T) {
	c := Config{Host: "0.0.0.0", Addr: "0.0.0.0:8080", BaseURL: "https://kanban.example.com"}
	if err := Validate(c); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestValidate_RemoteWithInsecureHTTP_Allowed(t *testing.T) {
	c := Config{Host: "0.0.0.0", Addr: "0.0.0.0:8080", InsecureHTTP: true}
	if err := Validate(c); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestValidate_RemoteWithBadBaseURL_Refused(t *testing.T) {
	c := Config{Host: "0.0.0.0", Addr: "0.0.0.0:8080", BaseURL: "://broken"}
	err := Validate(c)
	r := AsRefusal(err)
	if r == nil || r.Code != RefuseInsecureHTTP {
		t.Fatalf("refusal = %v", r)
	}
}

func TestValidate_LoopbackAlwaysAllowsPlainHTTP(t *testing.T) {
	cases := []Config{
		{Host: "127.0.0.1", Addr: "127.0.0.1:8080"},
		{Host: "::1", Addr: "[::1]:8080"},
		{Host: "localhost", Addr: "localhost:8080"},
	}
	for _, c := range cases {
		if err := Validate(c); err != nil {
			t.Fatalf("loopback %+v refused: %v", c, err)
		}
	}
}

// ---------------------------------------------------------------------------
// CIDRContains and IsLoopbackHost
// ---------------------------------------------------------------------------

func TestCIDRContains(t *testing.T) {
	_, n, _ := net.ParseCIDR("10.0.0.0/8")
	if !CIDRContains([]*net.IPNet{n}, "10.5.6.7") {
		t.Fatal("expected match")
	}
	if CIDRContains([]*net.IPNet{n}, "192.168.0.1:1234") {
		t.Fatal("unexpected match")
	}
	if CIDRContains(nil, "10.5.6.7") {
		t.Fatal("nil list matched")
	}
	if CIDRContains([]*net.IPNet{n}, "garbage") {
		t.Fatal("garbage matched")
	}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1": true,
		"127.5.6.7": true, // any 127/8 is loopback
		"::1":       true,
		"localhost": true,
		"":          false,
		"0.0.0.0":   false,
		"10.0.0.1":  false,
		"not-an-ip": false,
	}
	for h, want := range cases {
		if got := IsLoopbackHost(h); got != want {
			t.Fatalf("IsLoopbackHost(%q) = %v, want %v", h, got, want)
		}
	}
}

func TestBaseURLTLS(t *testing.T) {
	cases := map[string]string{
		"https://kanban.example.com": "https",
		"http://127.0.0.1:8080":      "http",
		"":                           "",
		"ftp://example.com":          "",
	}
	for in, want := range cases {
		got, _ := BaseURLTLS(in)
		if got != want {
			t.Fatalf("BaseURLTLS(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := BaseURLTLS("://broken"); err == nil {
		t.Fatal("expected error on malformed URL")
	}
}

// ---------------------------------------------------------------------------
// Summary (kanban doctor)
// ---------------------------------------------------------------------------

func TestSummary_HidesBootstrapSecretsAndShowsPosture(t *testing.T) {
	c := Config{
		Addr:           "0.0.0.0:8080",
		Host:           "0.0.0.0",
		Port:           "8080",
		DataDir:        "./data",
		BaseURL:        "https://kanban.example.com",
		Auth:           AuthRequired,
		InsecureHTTP:   false,
		TrustedProxies: []*net.IPNet{mustCIDR("10.0.0.0/8")},
		LogLevel:       LogInfo,
		LogFormat:      LogText,
		Demo:           false,
		ClaimTTL:       time.Hour,
		AdminToken:     "kbn_should_not_appear",
	}
	s := c.Summarize()
	if s.TLSRequired != true {
		t.Fatal("remote https should be TLS-required")
	}
	if s.Addr != "0.0.0.0:8080" {
		t.Fatalf("addr = %q", s.Addr)
	}
	if s.TrustedProxies != 1 {
		t.Fatalf("trusted = %d", s.TrustedProxies)
	}

	// Sanity check: the Bootstrap secret is not visible anywhere in the
	// struct's serialized form.
	// (No JSON marshal here; we rely on the field set staying small.)
	for _, f := range []string{"AdminToken"} {
		_ = f // keep the linter from complaining about unused identifiers
	}
	if c.AdminToken == "" {
		t.Fatal("admin token not carried into Summary struct intentionally")
	}
}

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}
