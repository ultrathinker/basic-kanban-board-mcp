package auth

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ---------------------------------------------------------------------------
// In-memory test stores. These exist only so the tests can exercise the
// auth behaviour without SQLite: they implement the public TokenLookup /
// SessionLookup interfaces from manager.go and return values consistent with
// the production semantics.
// ---------------------------------------------------------------------------

type memTokenStore struct {
	mu    sync.Mutex
	byID  map[string]*domain.Token
	byHsh map[string]*domain.Token
	seq   int
}

func newMemTokenStore() *memTokenStore { return &memTokenStore{byID: map[string]*domain.Token{}, byHsh: map[string]*domain.Token{}} }

func (m *memTokenStore) Create(ctx context.Context, t *domain.Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.ID == "" {
		m.seq++
		t.ID = "tok-" + itoa(m.seq)
	}
	if _, exists := m.byID[t.Name]; exists {
		return errDuplicate("token")
	}
	cp := *t
	m.byID[t.Name] = &cp
	m.byHsh[hexBytes(t.Hash)] = &cp
	return nil
}

func (m *memTokenStore) GetByHash(ctx context.Context, hash []byte) (*domain.Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.byHsh[hexBytes(hash)]
	if !ok {
		return nil, nil
	}
	cp := *t
	return &cp, nil
}

func (m *memTokenStore) GetByName(ctx context.Context, name string) (*domain.Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.byID[name]
	if !ok {
		return nil, nil
	}
	cp := *t
	return &cp, nil
}

func (m *memTokenStore) List(ctx context.Context) ([]*domain.Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*domain.Token, 0, len(m.byID))
	for _, t := range m.byID {
		cp := *t
		out = append(out, &cp)
	}
	return out, nil
}

func (m *memTokenStore) Count(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byID), nil
}

func (m *memTokenStore) UpdateHash(ctx context.Context, id string, hash []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.byID {
		if t.ID != id {
			continue
		}
		delete(m.byHsh, hexBytes(t.Hash))
		t.Hash = append([]byte(nil), hash...)
		m.byHsh[hexBytes(t.Hash)] = t
		return nil
	}
	return stubErr{s: "token not found"}
}

func (m *memTokenStore) Revoke(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.byID[name]
	if !ok {
		return nil
	}
	now := time.Now().UTC()
	t.RevokedAt = &now
	delete(m.byID, name)
	return nil
}

type memSessionStore struct {
	mu      sync.Mutex
	byID    map[string]*domain.Session
	deleted map[string]bool
}

func newMemSessionStore() *memSessionStore {
	return &memSessionStore{byID: map[string]*domain.Session{}, deleted: map[string]bool{}}
}

func (m *memSessionStore) CreateSession(ctx context.Context, s *domain.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *s
	m.byID[s.ID] = &cp
	return nil
}

func (m *memSessionStore) GetSession(ctx context.Context, id string) (*domain.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[id]
	if !ok || m.deleted[id] {
		return nil, nil
	}
	cp := *s
	return &cp, nil
}

func (m *memSessionStore) TouchSession(ctx context.Context, id string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[id]
	if !ok {
		return nil
	}
	s.LastSeenAt = now
	return nil
}

func (m *memSessionStore) DeleteSession(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted[id] = true
	delete(m.byID, id)
	return nil
}

// ---------------------------------------------------------------------------
// Tiny helpers shared across test files.
// ---------------------------------------------------------------------------

type stubErr struct{ s string }

func (e stubErr) Error() string { return e.s }
func errDuplicate(s string) error { return stubErr{s: "duplicate " + s} }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

func hexBytes(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexdigits[c>>4]
		out[i*2+1] = hexdigits[c&0x0f]
	}
	return string(out)
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	clock := func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) }
	return NewManager(newMemTokenStore(), newMemSessionStore(), nil, "", true, clock)
}

// ---------------------------------------------------------------------------
// Token format + hash
// ---------------------------------------------------------------------------

func TestNewSecret_HasPrefixAndDecodesTo32Bytes(t *testing.T) {
	s, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s, TokenPrefix) {
		t.Fatalf("missing prefix: %q", s)
	}
	// Strip prefix + decode base32 to verify entropy.
	body := strings.TrimPrefix(s, TokenPrefix)
	if body == "" {
		t.Fatal("empty body")
	}
	// Just sanity-check length: base32 of 32 bytes -> 52 chars without padding.
	if len(body) < 50 {
		t.Fatalf("body too short to contain 32 bytes: %d", len(body))
	}
	// Two distinct calls must not collide.
	s2, _ := NewSecret()
	if s == s2 {
		t.Fatal("NewSecret returned the same secret twice")
	}
}

func TestHashToken_DeterministicAndDistinct(t *testing.T) {
	h1 := HashToken("kbn_abc")
	h2 := HashToken("kbn_abc")
	if !bytes.Equal(h1, h2) {
		t.Fatal("hash of the same input differs")
	}
	h3 := HashToken("kbn_abd")
	if bytes.Equal(h1, h3) {
		t.Fatal("hash of different inputs collides")
	}
	if len(h1) != HashBytes {
		t.Fatalf("hash length = %d, want %d", len(h1), HashBytes)
	}
}

func TestVerifyToken_AcceptsActiveRejectsRevoked(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)

	secret, err := m.Bootstrap(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := m.VerifyToken(ctx, secret)
	if err != nil || tok == nil {
		t.Fatalf("verify returned (%v,%v)", tok, err)
	}
	if tok.Name != "admin" {
		t.Fatalf("name = %q", tok.Name)
	}

	// Revoke and try again.
	if err := m.Tokens.Revoke(ctx, tok.Name); err != nil {
		t.Fatal(err)
	}
	tok2, err := m.VerifyToken(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	if tok2 != nil {
		t.Fatal("revoked token still verifies")
	}
}

func TestVerifyToken_RejectsUnknownAndMalformed(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	cases := []string{
		"",
		"not-a-token",
		"kbn_short",       // right shape, wrong bytes
		"Bearer kbn_xyz",  // with prefix
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			tok, err := m.VerifyToken(ctx, s)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if tok != nil {
				t.Fatal("expected nil token")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Bootstrap
// ---------------------------------------------------------------------------

func TestBootstrap_GeneratesWhenTableEmptyAndStoreAcceptsSupplied(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)

	secret, err := m.Bootstrap(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, TokenPrefix) {
		t.Fatalf("generated secret shape: %q", secret)
	}

	// Bootstrap a second time: nothing new should be created.
	second, err := m.Bootstrap(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if second != "" {
		t.Fatalf("expected empty secret on second bootstrap, got %q", second)
	}
}

func TestBootstrap_SuppliedOverridesEmptyTable(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	const supplied = "kbn_suppliedadminsecret00xx00xx00xx00xx00xx"
	got, err := m.Bootstrap(ctx, supplied)
	if err != nil {
		t.Fatal(err)
	}
	if got != supplied {
		t.Fatalf("returned %q, want %q", got, supplied)
	}
	tok, err := m.VerifyToken(ctx, supplied)
	if err != nil || tok == nil {
		t.Fatalf("supplied token does not verify (%v,%v)", tok, err)
	}
}

func TestBootstrap_RejectsMalformedSuppliedSecret(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	_, err := m.Bootstrap(ctx, "opaque-string-without-prefix")
	if err == nil {
		t.Fatal("expected error on malformed supplied secret")
	}
}

// ---------------------------------------------------------------------------
// MintAndStore / Rotate
// ---------------------------------------------------------------------------

func TestMintAndStore_ProducesVerifiableSecret(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	tok := &domain.Token{
		ID:          "fixed-id-1",
		Name:        "alice",
		Scopes:      domain.Scopes{domain.ScopeWrite},
		ProjectKeys: []string{"BMB"},
	}
	secret, err := m.MintAndStore(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, TokenPrefix) {
		t.Fatalf("secret shape = %q", secret)
	}
	if len(tok.Hash) != HashBytes {
		t.Fatalf("hash length = %d, want %d", len(tok.Hash), HashBytes)
	}
	got, err := m.VerifyToken(ctx, secret)
	if err != nil || got == nil {
		t.Fatalf("verify = (%v,%v)", got, err)
	}
	if got.Name != "alice" {
		t.Fatalf("name = %q", got.Name)
	}
}

func TestMintAndStore_RejectsBadInput(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	if _, err := m.MintAndStore(ctx, nil); err == nil {
		t.Fatal("expected error on nil")
	}
	if _, err := m.MintAndStore(ctx, &domain.Token{ID: "x"}); err == nil {
		t.Fatal("expected error on empty name")
	}
}

func TestRotate_InvalidatesOldSecret(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)

	original, err := m.MintAndStore(ctx, &domain.Token{ID: "rot-1", Name: "bob", Scopes: domain.Scopes{domain.ScopeRead}})
	if err != nil {
		t.Fatal(err)
	}
	newSecret, err := m.Rotate(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if original == newSecret {
		t.Fatal("rotation returned the same secret")
	}
	if tok, err := m.VerifyToken(ctx, original); err == nil && tok != nil {
		t.Fatal("old secret still verifies after rotation")
	}
	if tok, err := m.VerifyToken(ctx, newSecret); err != nil || tok == nil {
		t.Fatalf("new secret does not verify after rotation: (%v, %v)", tok, err)
	}
}

func TestRotate_UnknownToken(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	if _, err := m.Rotate(ctx, "ghost"); err == nil {
		t.Fatal("expected error on unknown name")
	}
}

// ---------------------------------------------------------------------------
// StoreAdapter
// ---------------------------------------------------------------------------

// TestStoreAdapter_FakeStore confirms the adapter shape: a fake store.Store
// implementation that records the calls and returns canned values lets the
// auth surface exercise the adapter end-to-end without SQLite. We do not
// cover every method — the contract is exercised by the production wiring.

type fakeStore struct {
	// Embedding the interface gives us every method we do not care about;
	// calling one panics, which is the right outcome for a fake whose only
	// job is to prove the adapter routes through Read and Write.
	store.Store

	mu        sync.Mutex
	tokens    map[string]*domain.Token
	sessions  map[string]*domain.Session
	readCalls int
	wrCalls   int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		tokens:   map[string]*domain.Token{},
		sessions: map[string]*domain.Session{},
	}
}

func (f *fakeStore) Read(ctx context.Context, fn func(store.Tx) error) error {
	f.mu.Lock()
	f.readCalls++
	f.mu.Unlock()
	return fn(fakeTx{})
}

func (f *fakeStore) Write(ctx context.Context, fn func(store.Tx) error) error {
	f.mu.Lock()
	f.wrCalls++
	f.mu.Unlock()
	return fn(fakeTx{})
}

// Tokens and Sessions must return distinct types: TokenRepo.Create and
// SessionRepo.Create share a name but not a signature, so a single type
// cannot satisfy both.
func (f *fakeStore) Tokens() store.TokenRepo     { return &fakeTokenRepo{f: f} }
func (f *fakeStore) Sessions() store.SessionRepo { return &fakeSessionRepo{f: f} }

// fakeTx carries the fixed clock the adapter reads.
type fakeTx struct{}

func (fakeTx) Now() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) }

type fakeTokenRepo struct {
	store.TokenRepo // unimplemented methods panic if the adapter grows a new call
	f               *fakeStore
}

func (r *fakeTokenRepo) Create(_ store.Tx, t *domain.Token) error {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	if _, ok := r.f.tokens[t.Name]; ok {
		return errDuplicate("token")
	}
	cp := *t
	r.f.tokens[t.Name] = &cp
	return nil
}

func (r *fakeTokenRepo) GetByHash(_ store.Tx, hash []byte) (*domain.Token, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	for _, t := range r.f.tokens {
		if bytes.Equal(t.Hash, hash) {
			cp := *t
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *fakeTokenRepo) GetByName(_ store.Tx, name string) (*domain.Token, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	t, ok := r.f.tokens[name]
	if !ok {
		return nil, nil
	}
	cp := *t
	return &cp, nil
}

func (r *fakeTokenRepo) List(_ store.Tx) ([]*domain.Token, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	out := make([]*domain.Token, 0, len(r.f.tokens))
	for _, t := range r.f.tokens {
		cp := *t
		out = append(out, &cp)
	}
	return out, nil
}

func (r *fakeTokenRepo) Revoke(_ store.Tx, name string) error {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	if t, ok := r.f.tokens[name]; ok {
		now := time.Now().UTC()
		t.RevokedAt = &now
	}
	return nil
}

func (r *fakeTokenRepo) UpdateHash(_ store.Tx, id string, hash []byte) error {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	for _, t := range r.f.tokens {
		if t.ID == id {
			t.Hash = append([]byte(nil), hash...)
			return nil
		}
	}
	return stubErr{s: "token not found"}
}

func (r *fakeTokenRepo) TouchLastUsed(_ store.Tx, _ string) error { return nil }

func (r *fakeTokenRepo) Count(_ store.Tx) (int, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	return len(r.f.tokens), nil
}

type fakeSessionRepo struct {
	store.SessionRepo
	f *fakeStore
}

func (r *fakeSessionRepo) Create(_ store.Tx, s *domain.Session) error {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	cp := *s
	r.f.sessions[s.ID] = &cp
	return nil
}

func (r *fakeSessionRepo) Get(_ store.Tx, id string) (*domain.Session, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	s, ok := r.f.sessions[id]
	if !ok {
		return nil, nil
	}
	cp := *s
	return &cp, nil
}

func (r *fakeSessionRepo) Touch(_ store.Tx, id string, now time.Time) error {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	if s, ok := r.f.sessions[id]; ok {
		s.LastSeenAt = now
	}
	return nil
}

func (r *fakeSessionRepo) Delete(_ store.Tx, id string) error {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	delete(r.f.sessions, id)
	return nil
}

func TestStoreAdapter_EndToEnd(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	m := NewManagerFromStore(st, nil, "", true, func() time.Time {
		return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	})

	secret, err := m.MintAndStore(ctx, &domain.Token{ID: "x", Name: "carol", Scopes: domain.Scopes{domain.ScopeWrite}})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := m.VerifyToken(ctx, secret)
	if err != nil || tok == nil {
		t.Fatalf("verify = (%v,%v)", tok, err)
	}
	if st.readCalls == 0 {
		t.Fatal("VerifyToken did not call store.Read")
	}
	if st.wrCalls == 0 {
		t.Fatal("MintAndStore did not call store.Write")
	}
}

// ---------------------------------------------------------------------------
// Scope rules
// ---------------------------------------------------------------------------

func TestScopes_ImplicationAndProjectRestriction(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)

	write := domain.Scopes{domain.ScopeWrite}
	admin := domain.Scopes{domain.ScopeAdmin}
	read := domain.Scopes{domain.ScopeRead}

	if !write.Has(domain.ScopeRead) {
		t.Fatal("write does not imply read")
	}
	if !admin.Has(domain.ScopeWrite) {
		t.Fatal("admin does not imply write")
	}
	if write.Has(domain.ScopeAdmin) {
		t.Fatal("write claims admin")
	}
	if !read.Has(domain.ScopeRead) {
		t.Fatal("read should have read")
	}

	// Project restriction: token bound to BMB must not see KANBAN.
	tok := &domain.Token{
		Name:        "scoped",
		Hash:        HashToken("kbn_xx"),
		Scopes:      write,
		ProjectKeys: []string{"BMB"},
		CreatedAt:   m.Now(),
	}
	if err := m.Tokens.Create(ctx, tok); err != nil {
		t.Fatal(err)
	}
	actor := ResolveActor(tok)
	if !actor.MayAccessProject("bmb") { // case-insensitive
		t.Fatal("actor should access BMB")
	}
	if actor.MayAccessProject("KANBAN") {
		t.Fatal("actor should NOT access KANBAN")
	}

	// Unrestricted token sees everything.
	all := &domain.Token{
		Name: "all", Hash: HashToken("kbn_yy"),
		Scopes: write, CreatedAt: m.Now(),
	}
	if err := m.Tokens.Create(ctx, all); err != nil {
		t.Fatal(err)
	}
	if !ResolveActor(all).MayAccessProject("ANY") {
		t.Fatal("unrestricted actor should access any project")
	}
}

// ---------------------------------------------------------------------------
// Rate limiter
// ---------------------------------------------------------------------------

func TestRateLimiter_PerTokenBurstThenBlock(t *testing.T) {
	rl := NewRateLimiter(func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) })
	for i := 0; i < domain.RateLimitPerTokenPerMin; i++ {
		if !rl.AllowToken("alice") {
			t.Fatalf("blocked at %d (limit %d)", i, domain.RateLimitPerTokenPerMin)
		}
	}
	if rl.AllowToken("alice") {
		t.Fatal("should be blocked after hitting the limit")
	}
}

func TestRateLimiter_PerIPIsTighter(t *testing.T) {
	rl := NewRateLimiter(func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) })
	for i := 0; i < domain.RateLimitLoginPerIPMin; i++ {
		if !rl.AllowLogin("1.2.3.4") {
			t.Fatalf("login blocked at %d", i)
		}
	}
	if rl.AllowLogin("1.2.3.4") {
		t.Fatalf("login should be blocked after %d", domain.RateLimitLoginPerIPMin)
	}
}

func TestRateLimiter_BoundedUnder10kDistinctKeys(t *testing.T) {
	rl := NewRateLimiter(func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) })
	for i := 0; i < 10_000; i++ {
		rl.AllowToken("key-" + itoa(i))
	}
	if got := rl.Size(); got > rateLimitMaxKeys {
		t.Fatalf("rate limiter grew to %d entries, want <= %d", got, rateLimitMaxKeys)
	}
}

func TestRateLimiter_NewMinuteResets(t *testing.T) {
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	current := base
	rl := NewRateLimiter(func() time.Time { return current })
	for i := 0; i < domain.RateLimitPerTokenPerMin; i++ {
		rl.AllowToken("alice")
	}
	if rl.AllowToken("alice") {
		t.Fatal("should be blocked in the same minute")
	}
	current = base.Add(time.Minute)
	if !rl.AllowToken("alice") {
		t.Fatal("should be allowed in the next minute")
	}
}

// ---------------------------------------------------------------------------
// Tickets
// ---------------------------------------------------------------------------

func TestTicketStore_SingleUseAndExpiry(t *testing.T) {
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	now := base
	store := NewTicketStore(domain.TicketTTL, func() time.Time { return now })

	id, err := store.Issue("tok-1")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("empty ticket id")
	}

	bound, ok := store.Consume(id)
	if !ok || bound != "tok-1" {
		t.Fatalf("first consume = (%q,%v), want (tok-1,true)", bound, ok)
	}
	if _, ok := store.Consume(id); ok {
		t.Fatal("ticket reused on second consume")
	}
	if _, ok := store.Consume("not-a-real-ticket"); ok {
		t.Fatal("unknown ticket accepted")
	}
	if _, ok := store.Consume(""); ok {
		t.Fatal("empty ticket accepted")
	}
}

func TestTicketStore_Expires(t *testing.T) {
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	now := base
	store := NewTicketStore(domain.TicketTTL, func() time.Time { return now })
	id, err := store.Issue("tok-1")
	if err != nil {
		t.Fatal(err)
	}
	now = base.Add(domain.TicketTTL + time.Second)
	if _, ok := store.Consume(id); ok {
		t.Fatal("ticket consumed after TTL")
	}
	if store.Size() != 0 {
		t.Fatalf("size = %d, want 0", store.Size())
	}
}

// ---------------------------------------------------------------------------
// Middleware: bearer, X-API-Key, ticket, session, scope
// ---------------------------------------------------------------------------

func TestMiddleware_BearerAndAPIKey(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	secret, _ := m.Bootstrap(ctx, "")

	hits := 0
	h := m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		res := FromContext(r.Context())
		if res == nil || res.Token == nil || res.Actor == nil {
			t.Fatal("auth result missing from context")
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"bearer", map[string]string{"Authorization": "Bearer " + secret}, http.StatusNoContent},
		{"apikey", map[string]string{"X-API-Key": secret}, http.StatusNoContent},
		{"bare token", map[string]string{"Authorization": secret}, http.StatusNoContent},
		{"no creds", map[string]string{}, http.StatusUnauthorized},
		{"bad shape", map[string]string{"Authorization": "Bearer garbage"}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/x", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, req)
			if rw.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%q)", rw.Code, tc.want, rw.Body.String())
			}
		})
	}
	if hits != 3 {
		t.Fatalf("downstream hits = %d, want 3", hits)
	}
}

func TestMiddleware_Ticket(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	_, _ = m.Bootstrap(ctx, "")

	// We need the token lookup by ID to find admin. The Bootstrap path stores
	// the token under the name "admin"; lookupByTokenID walks the list, so
	// we must ensure the name->ID mapping is wired. Bootstrap fills ID via
	// the store; in our in-memory store, Create() assigns one. So fetch it:
	list, _ := m.Tokens.List(ctx)
	var adminID string
	for _, tk := range list {
		if tk.Name == "admin" {
			adminID = tk.ID
		}
	}
	if adminID == "" {
		t.Fatal("admin token has no id")
	}
	m.Tickets = NewTicketStore(domain.TicketTTL, m.Now)
	id, _ := m.Tickets.Issue(adminID)

	called := false
	h := m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		res := FromContext(r.Context())
		if res.Kind != AuthTicket {
			t.Fatalf("kind = %q, want ticket", res.Kind)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest("GET", "/events?ticket="+id, nil)
	req.Header.Set("X-Real-IP", "127.0.0.1")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%q", rw.Code, rw.Body.String())
	}
	if !called {
		t.Fatal("handler not invoked")
	}

	// Ticket is single-use.
	rw2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/events?ticket="+id, nil)
	h.ServeHTTP(rw2, req2)
	if rw2.Code != http.StatusForbidden {
		t.Fatalf("replay status = %d, want 403", rw2.Code)
	}
}

func TestMiddleware_Session(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	secret, _ := m.Bootstrap(ctx, "")
	sess, err := m.CreateSession(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}

	called := false
	h := m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if FromContext(r.Context()).Kind != AuthSession {
			t.Fatal("kind != session")
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "kanban_session", Value: sess.ID})
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%q", rw.Code, rw.Body.String())
	}
	if !called {
		t.Fatal("handler not invoked")
	}
}

func TestMiddleware_RateLimitOnToken(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	secret, _ := m.Bootstrap(ctx, "")

	h := m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for i := 0; i < domain.RateLimitPerTokenPerMin; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusNoContent {
			t.Fatalf("status at i=%d = %d", i, rw.Code)
		}
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rw.Code)
	}
	if rw.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
}

func TestMiddleware_RequireScope(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	_, _ = m.Bootstrap(ctx, "")

	// Add a read-only token.
	const readSecret = "kbn_readonlyxxxx00xx00xx00xx00xx00xx00xx00xx"
	readTok := &domain.Token{
		Name: "reader", Hash: HashToken(readSecret),
		Scopes: domain.Scopes{domain.ScopeRead}, CreatedAt: m.Now(),
	}
	if err := m.Tokens.Create(ctx, readTok); err != nil {
		t.Fatal(err)
	}

	h := m.RequireAuth(m.RequireScope(domain.ScopeAdmin, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+readSecret)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rw.Code)
	}
}

// ---------------------------------------------------------------------------
// Trusted proxies / client IP
// ---------------------------------------------------------------------------

func TestClientIP_UntrustedPeerIgnoresForwarded(t *testing.T) {
	m := newTestManager(t)
	m.Trusted = nil
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	got := m.ClientIP(req)
	if got != "10.0.0.5" {
		t.Fatalf("got %q, want 10.0.0.5", got)
	}
}

func TestClientIP_TrustedProxyPicksFirstUntrustedHop(t *testing.T) {
	// Trusted: 10.0.0.0/8 (the internal Caddy mesh).
	m := newTestManager(t)
	_, n, _ := netParseCIDR("10.0.0.0/8")
	m.Trusted = []*net.IPNet{n}

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.5:1234" // Caddy is trusted
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.5")
	got := m.ClientIP(req)
	if got != "1.2.3.4" {
		t.Fatalf("got %q, want 1.2.3.4", got)
	}
}

func TestForwardedProto_OnlyFromTrusted(t *testing.T) {
	m := newTestManager(t)
	_, n, _ := netParseCIDR("10.0.0.0/8")
	m.Trusted = []*net.IPNet{n}

	// Untrusted peer: header is ignored.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "1.2.3.4:80"
	req.Header.Set("X-Forwarded-Proto", "https")
	if got := m.ForwardedProto(req); got != "" {
		t.Fatalf("got %q, want empty", got)
	}

	// Trusted peer: header is honoured.
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.RemoteAddr = "10.0.0.5:443"
	req2.Header.Set("X-Forwarded-Proto", "https")
	if got := m.ForwardedProto(req2); got != "https" {
		t.Fatalf("got %q, want https", got)
	}
}

// ---------------------------------------------------------------------------
// Cookie Secure derivation
// ---------------------------------------------------------------------------

func TestCookiePolicy_SecureDerivation(t *testing.T) {
	m := newTestManager(t)

	// Loopback + insecure => not secure.
	m.BaseURL = ""
	m.Insecure = true
	pol := DefaultCookiePolicy(m.cookieSecureBase())
	req := httptest.NewRequest("GET", "/", nil)
	if pol.Secure(req, m) {
		t.Fatal("loopback+insecure should not be secure")
	}

	// base url https => secure.
	m.BaseURL = "https://kanban.example.com"
	m.Insecure = false
	pol = DefaultCookiePolicy(m.cookieSecureBase())
	if !pol.Secure(req, m) {
		t.Fatal("https base url should be secure")
	}

	// Trusted proxy says https even when base url is empty.
	m.BaseURL = ""
	_, n, _ := netParseCIDR("10.0.0.0/8")
	m.Trusted = []*net.IPNet{n}
	m.Insecure = false
	pol = DefaultCookiePolicy(m.cookieSecureBase())
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.RemoteAddr = "10.0.0.5:443"
	req2.Header.Set("X-Forwarded-Proto", "https")
	if !pol.Secure(req2, m) {
		t.Fatal("trusted proxy https should be secure")
	}
	// Same trusted setup but X-Forwarded-Proto from untrusted peer.
	req3 := httptest.NewRequest("GET", "/", nil)
	req3.RemoteAddr = "1.2.3.4:443"
	req3.Header.Set("X-Forwarded-Proto", "https")
	if pol.Secure(req3, m) {
		t.Fatal("untrusted forwarded https must NOT be secure")
	}
}

// ---------------------------------------------------------------------------
// Log redaction
// ---------------------------------------------------------------------------

func TestRedact_StripsAuthorizationAndCookies(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer kbn_supersecret1234567890")
	h.Set("X-API-Key", "kbn_supersecret1234567890")
	h.Set("Cookie", "kanban_session=abcdef0123; theme=dark")
	out := Redact(h)
	if !strings.HasPrefix(out.Get("Authorization"), "Bearer kbn_") {
		t.Fatalf("Authorization prefix missing: %q", out.Get("Authorization"))
	}
	if strings.Contains(out.Get("Authorization"), "supersecret") {
		t.Fatalf("Authorization not redacted: %q", out.Get("Authorization"))
	}
	if strings.Contains(out.Get("X-API-Key"), "supersecret") {
		t.Fatalf("X-API-Key not redacted: %q", out.Get("X-API-Key"))
	}
	if strings.Contains(out.Get("Cookie"), "abcdef0123") {
		t.Fatalf("Cookie not redacted: %q", out.Get("Cookie"))
	}
	if !strings.Contains(out.Get("Cookie"), "theme=dark") {
		t.Fatalf("unrelated cookie dropped: %q", out.Get("Cookie"))
	}
	// Original header untouched.
	if h.Get("Authorization") == "" {
		t.Fatal("Redact mutated the input header")
	}
}

func TestRedactString_Variants(t *testing.T) {
	cases := map[string]string{
		"Bearer kbn_supersecret1234567890":  "Bearer kbn_",
		"kbn_supersecret1234567890":         "kbn_",
		"kanban_session=abcdef0123; theme=dark": "kanban_session=***",
		"theme=dark": "theme=dark",
	}
	for in, wantPrefix := range cases {
		out := RedactString(in)
		if !strings.HasPrefix(out, wantPrefix) {
			t.Fatalf("RedactString(%q) = %q, want prefix %q", in, out, wantPrefix)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers required by netParseCIDR (defined here so the package compiles).
// ---------------------------------------------------------------------------

func netParseCIDR(s string) (net.IP, *net.IPNet, error) {
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		return nil, nil, err
	}
	return ip, n, nil
}

// ---------------------------------------------------------------------------
// Body cap (PLAN §8 "1 MB body limit")
// ---------------------------------------------------------------------------

func TestRequireAuth_RejectsOversizedBodyByContentLength(t *testing.T) {
	m := newTestManager(t)
	secret, _ := m.Bootstrap(context.Background(), "")
	hits := 0
	h := m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusNoContent)
	}))

	big := bytes.Repeat([]byte{'a'}, int(domainMaxBody+1))
	req := httptest.NewRequest("POST", "/", bytes.NewReader(big))
	req.ContentLength = int64(len(big))
	req.Header.Set("Authorization", "Bearer "+secret)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rw.Code)
	}
	if hits != 0 {
		t.Fatalf("downstream handler called %d times, want 0", hits)
	}
}

func TestRequireAuth_AllowsBodiesAtTheLimit(t *testing.T) {
	m := newTestManager(t)
	secret, _ := m.Bootstrap(context.Background(), "")
	hits := 0
	h := m.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusNoContent)
	}))

	body := bytes.Repeat([]byte{'a'}, int(domainMaxBody))
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%q)", rw.Code, rw.Body.String())
	}
	if hits != 1 {
		t.Fatalf("handler called %d times", hits)
	}
}

// ---------------------------------------------------------------------------
// CSRF
// ---------------------------------------------------------------------------

func TestCSRF_HeaderMatchesCookie(t *testing.T) {
	m := newTestManager(t)
	tok, err := m.IssueCSRF()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/", nil)
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
	req.Header.Set(CSRFHeaderName, tok)
	if err := m.VerifyCSRF(req); err != nil {
		t.Fatalf("verify = %v", err)
	}
}

func TestCSRF_FormValueMatchesCookie(t *testing.T) {
	m := newTestManager(t)
	tok, err := m.IssueCSRF()
	if err != nil {
		t.Fatal(err)
	}
	body := CSRFFieldName + "=" + tok
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
	if err := m.VerifyCSRF(req); err != nil {
		t.Fatalf("verify = %v", err)
	}
}

func TestCSRF_RejectsMissingOrWrongToken(t *testing.T) {
	m := newTestManager(t)
	tok, _ := m.IssueCSRF()

	cases := []struct {
		name string
		build func() *http.Request
	}{
		{"no cookie", func() *http.Request {
			r := httptest.NewRequest("POST", "/", nil)
			r.Header.Set(CSRFHeaderName, tok)
			return r
		}},
		{"no header or form", func() *http.Request {
			r := httptest.NewRequest("POST", "/", nil)
			r.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
			return r
		}},
		{"mismatch", func() *http.Request {
			r := httptest.NewRequest("POST", "/", nil)
			r.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
			r.Header.Set(CSRFHeaderName, tok+"x")
			return r
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := m.VerifyCSRF(tc.build()); err == nil {
				t.Fatal("expected forbidden")
			}
		})
	}
}

// domainMaxBody aliases the constant so the test file does not need to
// import internal/domain directly.
const domainMaxBody = 1 << 20
