package config

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// Refusal is the kind of startup refusal encountered. Surfacing a typed code
// (rather than a free-form error) means the CLI can render a stable help line
// and tests can assert on the specific rule that fired.
type Refusal string

const (
	RefuseAuthOffRemote    Refusal = "auth_off_remote"
	RefuseInsecureHTTP     Refusal = "insecure_http_missing"
	RefuseMissingAuthValue Refusal = "missing_auth_value"
)

// RefusalError is the typed error returned by Validate. Code is machine
// readable; Message is what the operator reads; Flag names the switch that
// would resolve the refusal when there is one.
type RefusalError struct {
	Code    Refusal
	Message string
	Flag    string // empty when there is no single fix-it flag
}

func (e *RefusalError) Error() string {
	if e.Flag == "" {
		return fmt.Sprintf("refusing to start: %s", e.Message)
	}
	return fmt.Sprintf("refusing to start: %s. Pass --%s to override.", e.Message, e.Flag)
}

func (e *RefusalError) Is(target error) bool {
	t, ok := target.(*RefusalError)
	return ok && t.Code == e.Code
}

// AsRefusal extracts a RefusalError from any error chain.
func AsRefusal(err error) *RefusalError {
	var r *RefusalError
	if errors.As(err, &r) {
		return r
	}
	return nil
}

// Validate enforces PLAN §8 refusals on a built Config. Each rule is its own
// pure function so tests can target them in isolation; the orchestrator below
// just composes them.
func Validate(c Config) error {
	if err := validateAuthOffOnLoopback(c); err != nil {
		return err
	}
	if err := validateTLSForRemote(c); err != nil {
		return err
	}
	return nil
}

// validateAuthOffOnLoopback refuses `--auth=off` when the listener is reachable
// from off the box. The bind-all form `:8080` (no host) binds every interface
// even on a laptop on café Wi-Fi, so it is treated as remote.
func validateAuthOffOnLoopback(c Config) error {
	if c.Auth != AuthOff {
		return nil
	}
	if IsLoopbackHost(c.Host) {
		return nil
	}
	return &RefusalError{
		Code: RefuseAuthOffRemote,
		Message: fmt.Sprintf(
			"--auth=off is only allowed on loopback listeners; %q is reachable from the network. "+
				"Auth off + remote bind = anyone on the network can read and mutate your board.",
			c.Host),
		Flag: "auth",
	}
}

// validateTLSForRemote refuses a remote HTTP listener unless either the public
// base URL is https:// or the operator explicitly opts in with
// --insecure-http. A bearer token in cleartext is no auth at all.
func validateTLSForRemote(c Config) error {
	if IsLoopbackHost(c.Host) {
		return nil
	}
	if c.InsecureHTTP {
		return nil
	}
	scheme, err := BaseURLTLS(c.BaseURL)
	if err != nil {
		return &RefusalError{
			Code: RefuseInsecureHTTP,
			Message: fmt.Sprintf("--base-url: %s. Either set base-url to https://… or pass --insecure-http. The two-minute Caddy path is in docs/DEPLOY.md.", err.Error()),
			Flag: "insecure-http",
		}
	}
	if scheme == "https" {
		return nil
	}
	return &RefusalError{
		Code: RefuseInsecureHTTP,
		Message: fmt.Sprintf(
			"listener %s is reachable from the network and --base-url is %q (scheme %s). "+
				"A bearer token over plain HTTP is not protected. "+
				"Put a TLS terminator in front (the two-minute Caddy path is in docs/DEPLOY.md) or pass --insecure-http to acknowledge the risk.",
			c.Addr, c.BaseURL, schemeOrPlain(scheme)),
		Flag: "insecure-http",
	}
}

func schemeOrPlain(scheme string) string {
	if scheme == "" {
		return "<empty>"
	}
	return scheme
}

// CIDRContains reports whether addr (an IP or host:port) falls inside any of
// the supplied CIDRs. Used by auth/rate-limit code to decide whether to honour
// X-Forwarded-For from a peer.
func CIDRContains(cidrs []*net.IPNet, addr string) bool {
	ip := parseAddrToIP(addr)
	if ip == nil {
		return false
	}
	for _, n := range cidrs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// parseAddrToIP accepts either a bare IP or a host:port pair and returns the
// IP. It returns nil when the address is not parseable, so callers can use the
// helper as a filter.
func parseAddrToIP(s string) net.IP {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if h, _, ok := strings.Cut(s, ":"); ok {
		s = h
	}
	// Strip brackets around IPv6 literals (e.g. "[::1]:8080" -> "::1").
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	return net.ParseIP(s)
}
