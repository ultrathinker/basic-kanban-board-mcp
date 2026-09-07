// Package config turns flags and KANBAN_* environment variables into a
// validated Config and enforces the startup refusals that prevent the most
// common security mistakes (PLAN §8, §10).
//
// The parsing is split deliberately: Build() produces a Config from arbitrary
// inputs (a flag set and an env lookup function) so tests can exercise every
// refusal without touching the process environment. Load() is the thin wrapper
// that reads the real environment and os.Args for the serve command.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// AuthMode captures whether the server requires bearer/session auth.
// "required" is the only safe default for any non-loopback listener; "off" is
// refused at startup unless the listener is loopback.
type AuthMode string

const (
	AuthRequired AuthMode = "required"
	AuthOff      AuthMode = "off"
)

func (a AuthMode) Valid() bool {
	return a == AuthRequired || a == AuthOff
}

// LogFormat selects the human vs JSON logger format.
type LogFormat string

const (
	LogText LogFormat = "text"
	LogJSON LogFormat = "json"
)

func (f LogFormat) Valid() bool { return f == LogText || f == LogJSON }

// LogLevel is a textual level (debug, info, warn, error). It is parsed but
// interpreted by the logger setup later; config only validates the spelling so
// misconfiguration is caught at startup.
type LogLevel string

const (
	LogDebug LogLevel = "debug"
	LogInfo  LogLevel = "info"
	LogWarn  LogLevel = "warn"
	LogError LogLevel = "error"
)

func (l LogLevel) Valid() bool {
	switch l {
	case LogDebug, LogInfo, LogWarn, LogError:
		return true
	}
	return false
}

// Defaults applied when neither flag nor env is set. The Docker image
// entrypoint overrides --addr to 0.0.0.0:8080 because a loopback bind inside a
// container is unreachable through -p (PLAN §8 footgun).
const (
	DefaultAddr        = "127.0.0.1:8080"
	DefaultData        = "./data"
	DefaultBaseURL     = ""
	DefaultAuth        = AuthRequired
	DefaultLogLevel    = LogInfo
	DefaultLogFormat   = LogText
	DefaultClaimTTL    = time.Hour
	DefaultTrustedSeed = ""
)

// EnvLookup returns the value of name, or "" when unset. Production code
// passes os.Getenv; tests pass a map-backed function.
type EnvLookup func(name string) string

// OSEnv is the production EnvLookup.
func OSEnv(name string) string { return os.Getenv(name) }

// envKey is the env-var name for a flag. We keep this mapping close to the
// flag declaration so adding a flag also makes its env var discoverable.
func envKey(flagName string) string {
	return "KANBAN_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// Config is the validated runtime configuration. It is passed by value to the
// rest of the program; once Validate() succeeds the fields are immutable for
// the lifetime of the process.
type Config struct {
	// Addr is the listen address passed to http.Server. It is already split
	// into host/port here so predicate checks do not duplicate the parsing.
	Addr    string
	Host    string
	Port    string
	DataDir string

	// BaseURL is the externally-visible origin used to build redirect URLs and
	// decide cookie Secure. Empty means "same as request scheme/host".
	BaseURL string

	Auth         AuthMode
	InsecureHTTP bool

	// TrustedProxies is the pre-parsed CIDR allow-list for X-Forwarded-*. Any
	// spoofed header from outside the set is dropped before reaching the auth
	// or rate-limit code paths.
	TrustedProxies []*net.IPNet

	LogLevel  LogLevel
	LogFormat LogFormat

	Demo       bool
	AdminToken string // resolved from --admin-token / KANBAN_ADMIN_TOKEN[_FILE]

	ClaimTTL time.Duration
}

// ListenHost returns the host portion of Addr, lowercased. It is a small
// convenience so callers do not have to reason about the split.
func (c Config) ListenHost() string { return strings.ToLower(c.Host) }

// Summary is the shape kanban doctor prints: it never contains the bootstrap
// secret, the bind address is rendered with the redacted-IP form, and the
// trusted-proxy list is a count.
type Summary struct {
	Addr           string
	Host           string
	Port           string
	DataDir        string
	BaseURL        string
	Auth           AuthMode
	TLSRequired    bool
	InsecureHTTP   bool
	TrustedProxies int
	LogLevel       LogLevel
	LogFormat      LogFormat
	Demo           bool
	ClaimTTL       time.Duration
}

// Summarize returns the doctor-facing view of the resolved Config. Bootstrap
// secrets are deliberately omitted; they were shown exactly once at startup.
func (c Config) Summarize() Summary {
	return Summary{
		Addr:           c.Addr,
		Host:           c.Host,
		Port:           c.Port,
		DataDir:        c.DataDir,
		BaseURL:        c.BaseURL,
		Auth:           c.Auth,
		TLSRequired:    c.tlsRequired(),
		InsecureHTTP:   c.InsecureHTTP,
		TrustedProxies: len(c.TrustedProxies),
		LogLevel:       c.LogLevel,
		LogFormat:      c.LogFormat,
		Demo:           c.Demo,
		ClaimTTL:       c.ClaimTTL,
	}
}

// tlsRequired reports whether a remote client must use TLS. Loopback is exempt
// because the operator is the only one who can reach it; the explicit
// --insecure-http flag is the only other escape.
func (c Config) tlsRequired() bool {
	if c.InsecureHTTP {
		return false
	}
	return !IsLoopbackHost(c.Host)
}

// Inputs collects every input Build() needs. fs and env are mandatory because
// Build has no implicit dependency on the process state.
type Inputs struct {
	// FS is the flag set to declare on. Tests pass a fresh FlagSet so each
	// case is independent.
	FS *flag.FlagSet
	// Env resolves a KANBAN_* variable to its string value.
	Env EnvLookup
	// Args is the positional argv after the subcommand name (i.e. what
	// "kanban serve" sees after stripping "serve").
	Args []string
	// AdminTokenFilePath, when non-empty, is the path to a file whose contents
	// replace the KANBAN_ADMIN_TOKEN value. Mirrors the Kubernetes/Docker
	// _FILE convention.
	AdminTokenFilePath string
	// Now lets tests inject a clock; Load passes time.Now.
	Now func() time.Time
}

// Build constructs a Config from fs/env/args. It returns the parsed Config and
// the flag.FlagSet so callers can read remaining flags (e.g. --help).
//
// Build does NOT enforce the startup refusals — call Validate on the result.
// This split lets tests assert individual refusal conditions without going
// through every layer.
func Build(in Inputs) (Config, *flag.FlagSet, error) {
	if in.FS == nil {
		return Config{}, nil, errors.New("config: flag set is required")
	}
	if in.Env == nil {
		in.Env = OSEnv
	}
	now := in.Now
	if now == nil {
		now = time.Now
	}

	var (
		addr      = in.FS.String("addr", DefaultAddr, "Listen address (host:port). Loopback unless --insecure-http is set for remote binds.")
		data      = in.FS.String("data", DefaultData, "Directory for the SQLite database.")
		baseURL   = in.FS.String("base-url", "", "Public origin, e.g. https://kanban.example.com. Used for redirects and cookie Secure.")
		authFlag  = in.FS.String("auth", string(DefaultAuth), "Auth mode: 'required' (default) or 'off'. --auth=off is refused unless the listener is loopback.")
		insecure  = in.FS.Bool("insecure-http", false, "Allow a non-loopback HTTP listener. Required when base-url is not https:// and the listener is reachable remotely.")
		proxies   = in.FS.String("trusted-proxies", "", "Comma-separated CIDRs allowed to set X-Forwarded-* (e.g. 10.0.0.0/8,127.0.0.1/32).")
		logLevel  = in.FS.String("log-level", string(DefaultLogLevel), "Log level: debug|info|warn|error.")
		logFormat = in.FS.String("log-format", string(DefaultLogFormat), "Log format: text|json.")
		demo      = in.FS.Bool("demo", true, "Seed one labeled sample project on a brand-new (empty) database so a fresh node opens on a populated board. No-op once any project exists. Pass --demo=false (or KANBAN_DEMO=false) for a clean board.")
		adminTok  = in.FS.String("admin-token", "", "Bootstrap admin token value. Prefer KANBAN_ADMIN_TOKEN env or _FILE path. If neither is set, one is auto-generated and printed once.")
		adminFile = in.FS.String("admin-token-file", "", "Path to a file containing the bootstrap admin token.")
		claimTTL  = in.FS.Duration("claim-ttl", DefaultClaimTTL, "Default claim TTL, clamped to ["+domain.ClaimTTLMin.String()+", "+domain.ClaimTTLMax.String()+"].")
	)
	if err := in.FS.Parse(in.Args); err != nil {
		return Config{}, in.FS, err
	}

	// Flags win over env, env wins over defaults. The flag default above is
	// the same as the package default, so we resolve env via the flag name.
	resolveString(in.FS, "addr", addr, in.Env)
	resolveString(in.FS, "data", data, in.Env)
	resolveString(in.FS, "base-url", baseURL, in.Env)
	resolveString(in.FS, "auth", authFlag, in.Env)
	resolveString(in.FS, "log-level", logLevel, in.Env)
	resolveString(in.FS, "log-format", logFormat, in.Env)
	resolveString(in.FS, "trusted-proxies", proxies, in.Env)
	resolveString(in.FS, "admin-token", adminTok, in.Env)
	resolveString(in.FS, "admin-token-file", adminFile, in.Env)
	resolveBool(in.FS, "insecure-http", insecure, in.Env)
	resolveBool(in.FS, "demo", demo, in.Env)
	resolveDuration(in.FS, "claim-ttl", claimTTL, in.Env)

	host, port, err := splitHostPort(*addr)
	if err != nil {
		return Config{}, in.FS, fmt.Errorf("config: --addr: %w", err)
	}

	auth := AuthMode(strings.ToLower(strings.TrimSpace(*authFlag)))
	if !auth.Valid() {
		return Config{}, in.FS, fmt.Errorf("config: --auth must be 'required' or 'off', got %q", *authFlag)
	}

	ll := LogLevel(strings.ToLower(strings.TrimSpace(*logLevel)))
	if !ll.Valid() {
		return Config{}, in.FS, fmt.Errorf("config: --log-level must be debug|info|warn|error, got %q", *logLevel)
	}
	lf := LogFormat(strings.ToLower(strings.TrimSpace(*logFormat)))
	if !lf.Valid() {
		return Config{}, in.FS, fmt.Errorf("config: --log-format must be text|json, got %q", *logFormat)
	}

	cidrs, err := parseCIDRs(*proxies)
	if err != nil {
		return Config{}, in.FS, err
	}

	ttl := *claimTTL
	if ttl <= 0 {
		ttl = DefaultClaimTTL
	}
	ttl = domain.ClampClaimTTL(ttl)

	adminValue := strings.TrimSpace(*adminTok)
	// Admin token file resolution. Precedence: explicit Inputs.AdminTokenFilePath
	// > KANBAN_ADMIN_TOKEN_FILE env > --admin-token flag > KANBAN_ADMIN_TOKEN env.
	// We must read the file env via in.Env so tests can stay hermetic.
	var adminFilePath string
	setFile := false
	in.FS.Visit(func(f *flag.Flag) {
		if f.Name == "admin-token-file" {
			setFile = true
		}
	})
	if in.AdminTokenFilePath != "" {
		adminFilePath = in.AdminTokenFilePath
	} else if !setFile {
		if v := strings.TrimSpace(in.Env(envKey("admin-token-file"))); v != "" {
			adminFilePath = v
		}
	} else {
		adminFilePath = *adminFile
	}
	if adminFilePath != "" {
		fileVal, err := readTrimmedFile(adminFilePath)
		if err != nil {
			return Config{}, in.FS, fmt.Errorf("config: --admin-token-file: %w", err)
		}
		adminValue = fileVal
	} else {
		envVal := strings.TrimSpace(in.Env(envKey("admin-token")))
		adminValue = mergeAdminEnv(adminValue, envVal, in.FS)
	}

	cfg := Config{
		Addr:           *addr,
		Host:           host,
		Port:           port,
		DataDir:        *data,
		BaseURL:        strings.TrimSpace(*baseURL),
		Auth:           auth,
		InsecureHTTP:   *insecure,
		TrustedProxies: cidrs,
		LogLevel:       ll,
		LogFormat:      lf,
		Demo:           *demo,
		AdminToken:     adminValue,
		ClaimTTL:       ttl,
	}
	return cfg, in.FS, nil
}

// mergeAdminEnv applies the KANBAN_ADMIN_TOKEN env value when the flag was
// not set on the command line. We honour env -> default ordering but never
// let env overwrite an explicit flag (PLAN §10: "Flags win over env").
func mergeAdminEnv(flagVal, envVal string, fs *flag.FlagSet) string {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "admin-token" {
			set = true
		}
	})
	if set {
		return flagVal
	}
	return envVal
}

// resolveString applies the flag -> env precedence for a string flag.
func resolveString(fs *flag.FlagSet, name string, dst *string, env EnvLookup) {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	if set {
		return
	}
	if v, ok := envMaybe(env(envKey(name))); ok {
		*dst = v
	}
}

func resolveBool(fs *flag.FlagSet, name string, dst *bool, env EnvLookup) {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	if set {
		return
	}
	if v, ok := envMaybeBool(env(envKey(name))); ok {
		*dst = v
	}
}

func resolveDuration(fs *flag.FlagSet, name string, dst *time.Duration, env EnvLookup) {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	if set {
		return
	}
	if v, ok := envMaybeDuration(env(envKey(name))); ok {
		*dst = v
	}
}

// envMaybe returns (value, true) when raw is non-empty, trimming whitespace.
// The bool lets callers distinguish "unset" from "set to empty string".
func envMaybe(raw string) (string, bool) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return "", false
	}
	return t, true
}

// envMaybeBool accepts the same spellings the flag package would accept:
// "1", "t", "true", "yes" / "0", "f", "false", "no" (case-insensitive).
func envMaybeBool(raw string) (bool, bool) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return false, false
	}
	switch strings.ToLower(t) {
	case "1", "t", "true", "yes", "on":
		return true, true
	case "0", "f", "false", "no", "off":
		return false, true
	}
	return false, false
}

func envMaybeDuration(raw string) (time.Duration, bool) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return 0, false
	}
	d, err := time.ParseDuration(t)
	if err != nil {
		return 0, false
	}
	return d, true
}

// parseCIDRs splits a comma-separated list, validating every entry. We
// require every entry to be a CIDR (or a single IP, normalised to /32 / /128):
// accepting only-IPs silently has bitten us in past projects where operators
// typo an interface address.
func parseCIDRs(s string) ([]*net.IPNet, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]*net.IPNet, 0, len(parts))
	for _, p := range parts {
		entry := strings.TrimSpace(p)
		if entry == "" {
			continue
		}
		if !strings.Contains(entry, "/") {
			ip := net.ParseIP(entry)
			if ip == nil {
				return nil, fmt.Errorf("config: --trusted-proxies: %q is not an IP or CIDR", entry)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, &net.IPNet{IP: ip.Mask(net.CIDRMask(bits, bits)), Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, n, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("config: --trusted-proxies: %q is not a valid CIDR: %w", entry, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// splitHostPort accepts both ":8080" (all interfaces) and "127.0.0.1:8080",
// and reports the loopback status alongside. IPv6 must be bracketed.
func splitHostPort(addr string) (host, port string, err error) {
	h, p, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		// ":8080" is a Go quirk: net.SplitHostPort refuses it because it is
		// ambiguous (host="", port="8080"). We special-case it because the
		// short form is exactly the footgun PLAN §8 calls out.
		if strings.HasPrefix(addr, ":") {
			return "", strings.TrimPrefix(addr, ":"), nil
		}
		return "", "", splitErr
	}
	return h, p, nil
}

// readTrimmedFile reads a small text file and trims surrounding whitespace.
// We deliberately do not allow inline comments or escape processing — the
// file's purpose is "give me one secret".
func readTrimmedFile(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 — operator-provided path.
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// IsLoopbackHost reports whether a host resolves to a loopback address.
// It accepts the textual shortcuts (`localhost`, `127.0.0.1`, `::1`) and any
// other IP that is in the loopback range, because Caddy and similar reverse
// proxies can present themselves under any address inside the loopback block.
func IsLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	switch strings.ToLower(host) {
	case "localhost", "ip6-localhost", "ip6-loopback":
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// BaseURLTLS reports the scheme of the public base URL. Empty string means
// "could not determine / no base URL configured".
func BaseURLTLS(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("config: --base-url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("config: --base-url scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("config: --base-url must include a host")
	}
	return u.Scheme, nil
}
