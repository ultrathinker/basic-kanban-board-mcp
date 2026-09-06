package auth

import (
	"encoding/base32"
	"strings"
	"sync"
	"time"
)

// TicketStore holds short-lived, single-use SSE connection tickets. The store
// lives in memory: the SSE ticket is tied to a freshly-authenticated request
// and consumed within seconds, so persistent storage buys nothing.
//
// Why a ticket at all: EventSource cannot set custom headers, and a bearer
// token in the URL lands in every reverse-proxy access log. We exchange the
// long-lived bearer for a one-time opaque value good for 60 s (PLAN §8).
type TicketStore struct {
	mu      sync.Mutex
	tickets map[string]ticket
	ttl     time.Duration
	now     func() time.Time
}

type ticket struct {
	tokenID   string
	createdAt time.Time
	used      bool
}

// NewTicketStore constructs a store with the given TTL and clock.
func NewTicketStore(ttl time.Duration, now func() time.Time) *TicketStore {
	if now == nil {
		now = time.Now
	}
	return &TicketStore{
		tickets: make(map[string]ticket),
		ttl:     ttl,
		now:     now,
	}
}

// Issue creates a ticket bound to the supplied token ID. The returned string
// is the only value the client will see.
func (s *TicketStore) Issue(tokenID string) (string, error) {
	var raw [24]byte
	if err := readRandom(raw[:]); err != nil {
		return "", err
	}
	id := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:]))

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(s.now())
	s.tickets[id] = ticket{tokenID: tokenID, createdAt: s.now()}
	return id, nil
}

// Consume atomically validates and removes a ticket. It returns the bound
// token ID on first use; a second use, an unknown value or an expired one
// all return ("", false). Tickets live for domain.TicketTTL and are bound to
// one token; the lookup is a map hit, not constant-time, which is fine
// because a successful consume deletes the ticket anyway so an attacker can
// never observe the timing of two consecutive hits on the same key.
func (s *TicketStore) Consume(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(s.now())
	t, ok := s.tickets[id]
	if !ok || t.used {
		return "", false
	}
	delete(s.tickets, id)
	return t.tokenID, true
}

// sweepLocked removes expired tickets. Caller holds s.mu.
func (s *TicketStore) sweepLocked(now time.Time) {
	for k, t := range s.tickets {
		if now.Sub(t.createdAt) > s.ttl {
			delete(s.tickets, k)
		}
	}
}

// Size is exported for tests / a future metrics endpoint.
func (s *TicketStore) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tickets)
}

// Sweep runs a pass and is safe to call from any goroutine.
func (s *TicketStore) Sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := len(s.tickets)
	s.sweepLocked(s.now())
	return before - len(s.tickets)
}
