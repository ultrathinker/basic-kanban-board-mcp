package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// KANB-60: a token that carries an expiry stops authenticating at that
// instant on EVERY credential path — bearer, browser session and SSE ticket —
// against the real store, so the stored expires_at column is what decides.

// movableClock is a test clock the expiry tests step past a key's deadline.
type movableClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *movableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *movableClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// expiringKey mints a token that expires one hour after the clock's start.
func expiringKey(t *testing.T) (*Manager, *movableClock, string, time.Time) {
	t.Helper()
	st, err := store.Open(context.Background(), store.Config{Path: filepath.Join(t.TempDir(), "kanban.db")})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	clock := &movableClock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
	m := NewManagerFromStore(st, nil, "", true, clock.Now)
	expires := clock.Now().Add(time.Hour)
	secret, err := m.MintAndStore(context.Background(), &domain.Token{
		ID: "exec-id", Name: "exec-1",
		Scopes:      domain.Scopes{domain.ScopeExecutor},
		ProjectKeys: []string{"BMB"},
		ExpiresAt:   &expires,
	})
	if err != nil {
		t.Fatalf("MintAndStore: %v", err)
	}
	return m, clock, secret, expires
}

func TestVerifyToken_ExpiredKeyIsRefused(t *testing.T) {
	m, clock, secret, expires := expiringKey(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"well before expiry", expires.Add(-time.Hour + time.Second), true},
		{"one second before expiry", expires.Add(-time.Second), true},
		{"at the expiry instant", expires, false},
		{"after expiry", expires.Add(time.Minute), false},
	} {
		clock.Set(tc.at)
		tok, err := m.VerifyToken(ctx, secret)
		if err != nil {
			t.Fatalf("%s: VerifyToken error %v; an expired key is a bad credential, not a fault", tc.name, err)
		}
		if got := tok != nil; got != tc.want {
			t.Errorf("%s: authenticated=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRequireAuth_ExpiredKeyRefusedOnEveryPath(t *testing.T) {
	m, clock, secret, expires := expiringKey(t)
	ctx := context.Background()
	h := m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	serve := func(req *http.Request) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// While the key is live: a session and a ticket are obtained.
	clock.Set(expires.Add(-30 * time.Minute))
	sess, err := m.CreateSession(ctx, secret)
	if err != nil {
		t.Fatalf("CreateSession while live: %v", err)
	}
	cookieRec := httptest.NewRecorder()
	m.SetSessionCookie(cookieRec, httptest.NewRequest(http.MethodGet, "/", nil), sess)
	cookies := cookieRec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie written")
	}

	bearer := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		r.Header.Set("Authorization", "Bearer "+secret)
		return r
	}
	withSession := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, c := range cookies {
			r.AddCookie(c)
		}
		return r
	}
	if code := serve(bearer()); code != http.StatusOK {
		t.Fatalf("live bearer: %d, want 200", code)
	}
	if code := serve(withSession()); code != http.StatusOK {
		t.Fatalf("live session: %d, want 200", code)
	}

	// The ticket is taken moments before the deadline, so it is still
	// inside its own one-minute life when the key dies.
	clock.Set(expires.Add(-5 * time.Second))
	ticket, err := m.Tickets.Issue("exec-id")
	if err != nil {
		t.Fatalf("Issue ticket: %v", err)
	}

	// Past the deadline nothing the key obtained still opens the door.
	clock.Set(expires.Add(time.Second))
	if code := serve(bearer()); code != http.StatusForbidden {
		t.Errorf("expired bearer: %d, want 403", code)
	}
	if code := serve(withSession()); code != http.StatusForbidden {
		t.Errorf("session of an expired key: %d, want 403", code)
	}
	if code := serve(httptest.NewRequest(http.MethodGet, "/events?ticket="+ticket, nil)); code != http.StatusForbidden {
		t.Errorf("ticket of an expired key: %d, want 403", code)
	}
	if _, err := m.CreateSession(ctx, secret); err == nil {
		t.Error("an expired key opened a new browser session")
	}
}
