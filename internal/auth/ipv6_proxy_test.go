package auth

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

func TestParseIP_AddressShapes(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"2001:db8::1", "2001:db8::1"},
		{"[2001:db8::1]", "2001:db8::1"},
		{" 2001:db8::7 ", "2001:db8::7"},
		{"[::1]:8080", "::1"},
		{"10.0.0.5:1234", "10.0.0.5"},
		{"10.0.0.5", "10.0.0.5"},
		{"", ""},
		{"not-an-ip", ""},
		{"host.example:443", ""},
	}
	for _, c := range cases {
		got := parseIP(c.in)
		gotS := ""
		if got != nil {
			gotS = got.String()
		}
		if gotS != c.want {
			t.Errorf("parseIP(%q) = %q, want %q", c.in, gotS, c.want)
		}
	}
}

// ipv6ProxyManager trusts 2001:db8::/32, the network the proxy sits in, and
// answers over https only when the proxy says so.
func ipv6ProxyManager(t *testing.T) *Manager {
	t.Helper()
	m := newTestManager(t)
	_, n, err := net.ParseCIDR("2001:db8::/32")
	if err != nil {
		t.Fatal(err)
	}
	m.Trusted = []*net.IPNet{n}
	m.BaseURL = ""
	m.Insecure = false
	return m
}

func TestIPv6TrustedProxy_HTTPSGivesHostPrefixedSecureCookies(t *testing.T) {
	m := ipv6ProxyManager(t)
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "[2001:db8::1]:443"
	req.Header.Set("X-Forwarded-Proto", "https")

	if got := m.ForwardedProto(req); got != "https" {
		t.Fatalf("ForwardedProto = %q, want https from a trusted IPv6 proxy", got)
	}
	rw := httptest.NewRecorder()
	m.SetSessionCookie(rw, req, &domain.Session{ID: "sess-1", ExpiresAt: m.Now().Add(domain.SessionIdleTTL)})
	m.SetCSRFCookie(rw, req, "csrf-1")
	for _, base := range []string{DefaultCookiePolicy(false).Name, CSRFCookieName} {
		c := cookieIn(rw.Result().Cookies(), HostCookiePrefix+base)
		if c == nil {
			t.Fatalf("no %s%s cookie; got %v", HostCookiePrefix, base, rw.Result().Cookies())
		}
		if !c.Secure {
			t.Fatalf("%s is not Secure", c.Name)
		}
	}

	// The same header from an IPv6 peer outside the trusted network is noise.
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.RemoteAddr = "[2001:db9::1]:443"
	req2.Header.Set("X-Forwarded-Proto", "https")
	if DefaultCookiePolicy(m.cookieSecureBase()).Secure(req2, m) {
		t.Fatal("X-Forwarded-Proto from an untrusted IPv6 peer must not make cookies Secure")
	}
}

func TestIPv6TrustedProxy_ForwardedClientReachesRateLimit(t *testing.T) {
	m := ipv6ProxyManager(t)
	newReq := func(xff string) *http.Request {
		req := httptest.NewRequest("POST", "/login", nil)
		req.RemoteAddr = "[2001:db8::1]:443"
		req.Header.Set("X-Forwarded-For", xff)
		return req
	}

	for _, c := range []struct{ xff, want string }{
		{"2001:db9::7", "2001:db9::7"},
		{"[2001:db9::7]:51000", "2001:db9::7"},
		{"2001:db9::7, 2001:db8::2", "2001:db9::7"}, // 2001:db8::2 is a trusted hop
		{"198.51.100.4, 2001:db8::2", "198.51.100.4"},
	} {
		if got := m.ClientIP(newReq(c.xff)); got != c.want {
			t.Errorf("ClientIP with X-Forwarded-For %q = %q, want %q", c.xff, got, c.want)
		}
	}

	// Two IPv6 clients sharing a first group must not share a login bucket:
	// the key is the whole address.
	h := m.LoginRateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	hit := func(xff string) int {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, newReq(xff))
		return rw.Code
	}
	for i := 0; i < domain.RateLimitLoginPerIPMin; i++ {
		if code := hit("2001:db9::7"); code != http.StatusNoContent {
			t.Fatalf("attempt %d from 2001:db9::7: status %d, want 204", i+1, code)
		}
	}
	if code := hit("2001:db9::7"); code != http.StatusTooManyRequests {
		t.Fatalf("attempt over the limit: status %d, want 429", code)
	}
	if code := hit("2001:db9::8"); code != http.StatusNoContent {
		t.Fatalf("a different IPv6 client was throttled by its neighbour's bucket: status %d", code)
	}
}

func cookieIn(cs []*http.Cookie, name string) *http.Cookie {
	for _, c := range cs {
		if strings.EqualFold(c.Name, name) {
			return c
		}
	}
	return nil
}
