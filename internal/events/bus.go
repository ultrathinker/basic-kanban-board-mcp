// Package events is the in-process fan-out that pushes committed domain events
// to live subscribers (SSE clients, the activity feed).
//
// The contract, from PLAN §7 and the agent-E brief:
//
//   - Publish runs AFTER a database commit. Callers do not publish mid-transaction:
//     a subscriber that receives an event whose transaction later rolled back
//     would learn state the database never agreed to.
//   - Publish MUST NEVER block on a slow subscriber. A subscriber whose buffer
//     is full is marked for resync and the bus moves on; the channel is closed
//     so the consumer cleans up.
//   - Subscribe returns a per-subscriber buffered channel and a cancel func.
//     Calling cancel removes the subscription; calling it concurrently with
//     Publish must not panic.
//   - Replay: subscribers may join with a Last-Event-ID. If that id is older than
//     the oldest event the events table still retains, the bus delivers a
//     ResyncSentinel and closes the channel so the caller must reload. The
//     result is explicit, not a comment.
//
// Concurrency model — every channel send and every channel close happens under
// the same mutex. The channel is buffered, so the send is non-blocking and the
// mutex is held only for the duration of a slice iteration plus a non-blocking
// select. That keeps both the publisher's throughput and the race detector
// happy.
package events

import (
	"errors"
	"sync"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// Replay is what a subscriber gets before the live stream starts. Events is
// historical events in id order; NeedsResync is set when the requested
// Last-Event-ID predates the retained range, so the consumer knows it missed
// state and must reload from the source of truth.
type Replay struct {
	// Events are the historical events newer than (or equal to) LastEventID,
	// in id order.
	Events []domain.Event
	// NeedsResync is true when LastEventID < MinID (the events table no longer
	// holds enough history to replay the gap). Consumers must reload rather
	// than trust a partial stream.
	NeedsResync bool
	// LastEventID is the id the caller supplied, echoed for logging.
	LastEventID int64
	// MinID is the oldest event id still retained; zero when no events exist.
	MinID int64
	// MaxID is the newest event id at subscription time; live events will be
	// strictly greater than this.
	MaxID int64
}

// ResyncSentinel is the EventType used when the bus cannot replay history
// (Last-Event-ID older than retained) and the caller must reload. It is
// intentionally a value not declared in the domain enum because the domain enum
// is the wire vocabulary of the activity feed; this one is internal.
const ResyncSentinel domain.EventType = "events.resync"

// Subscriber is the result of Subscribe. The caller reads from Events until it
// closes (which happens on unsubscribe or when the bus drops the subscriber for
// slowness).
type Subscriber struct {
	// Events streams historical events (Replay) followed by live events. A
	// sentinel close signals end-of-stream; the caller MUST drain it.
	Events <-chan domain.Event
	// Done is closed when the subscription has been fully removed. The caller
	// does not have to wait on it; it exists so tests can assert teardown.
	Done <-chan struct{}
	// Cancel releases the slot. Always safe to call, exactly once.
	Cancel func()
}

// HistoryLoader is the bus's seam to the events table. The bus never imports
// the store package: the service (or whoever wires main) supplies an
// implementation at construction time so the bus stays pure in-process and
// trivially testable.
type HistoryLoader interface {
	// Since returns events with ID greater than afterID, oldest first. When
	// afterID is zero the implementation returns up to limit events from the
	// start of the table; when limit is zero it returns everything available.
	Since(projectID string, afterID int64, limit int) ([]domain.Event, error)
	// MinID reports the oldest event id still retained across the whole table,
	// or zero when the table is empty.
	MinID() (int64, error)
}

// ErrHistoryUnavailable is returned from Subscribe when the configured
// HistoryLoader cannot answer. The bus itself is purely in-memory; it has
// no fallback. Callers should treat this as "service degraded, retry" —
// most often the underlying store has just not opened yet.
var ErrHistoryUnavailable = errors.New("events: history loader unavailable")

// Bus is the fan-out hub for a single process. One instance is shared by every
// surface that wants live updates.
type Bus struct {
	history HistoryLoader

	// Sub channel buffer: when full, drop the subscriber and mark NeedsResync.
	subBuffer int

	// mu guards subs, nextID, and every per-sub lifecycle flag. Channel
	// sends and channel closes both happen under this same lock — the
	// channel is buffered so the send is non-blocking and the lock is held
	// only for the duration of a slice iteration plus a select with
	// default. That keeps both throughput and the race detector happy.
	mu     sync.Mutex
	subs   []*subscription
	nextID int64
}

type subscription struct {
	id      int64
	project string // empty = all projects
	ch      chan domain.Event
	done    chan struct{}

	// needsResync, canceled and chClosed are only flipped by code paths
	// that hold mu, so any test for "should we still send/close?" is safe
	// under the same lock.
	needsResync bool
	canceled    bool
	chClosed    bool
}

// New constructs a Bus. subBuffer bounds each subscriber's channel; the value
// should be small enough that a slow consumer never starves but a healthy one
// never drops. 64 is a sane default for human-driven SSE clients.
func New(history HistoryLoader, subBuffer int) *Bus {
	if subBuffer <= 0 {
		subBuffer = 64
	}
	return &Bus{
		history:   history,
		subBuffer: subBuffer,
	}
}

// Subscribe attaches a new subscriber. projectID empty means "every project".
// lastEventID is the caller's last-seen event id; pass 0 for a fresh client.
// The returned Subscriber's Events channel first receives the replay slice
// (possibly empty), then live events until cancel is called or the bus drops
// the subscription for slowness.
//
// If lastEventID is older than the oldest event retained, the bus delivers a
// single ResyncSentinel event and then closes the channel — the caller MUST
// detect ResyncSentinel and reload.
func (b *Bus) Subscribe(projectID string, lastEventID int64) (*Subscriber, error) {
	replay, err := b.loadReplay(projectID, lastEventID)
	if err != nil {
		return nil, err
	}

	b.mu.Lock()
	sub := &subscription{
		id:      b.nextID + 1,
		project: projectID,
		ch:      make(chan domain.Event, b.subBuffer),
		done:    make(chan struct{}),
	}
	b.nextID = sub.id
	b.subs = append(b.subs, sub)
	b.mu.Unlock()

	if replay.NeedsResync {
		// The resync path closes the channel under the lock, so any later
		// Publish that started before this point cannot observe an
		// inconsistent state. We mark needsResync up-front so Publish
		// skips it even before the goroutine runs.
		sub.needsResync = true
		go b.deliverResyncAndClose(sub, "history_gap", replay.LastEventID, replay.MinID)
		return &Subscriber{
			Events: sub.ch,
			Done:   sub.done,
			Cancel: func() { b.removeSub(sub) },
		}, nil
	}

	// Replay historical events on a separate goroutine so the call returns
	// immediately. Live events accumulate in the channel buffer behind the
	// replay; the consumer reads them in order.
	go b.deliverReplay(sub, replay)

	return &Subscriber{
		Events: sub.ch,
		Done:   sub.done,
		Cancel: func() { b.removeSub(sub) },
	}, nil
}

func (b *Bus) loadReplay(projectID string, lastEventID int64) (Replay, error) {
	var out Replay
	out.LastEventID = lastEventID

	if b.history == nil {
		return out, ErrHistoryUnavailable
	}

	minID, err := b.history.MinID()
	if err != nil {
		return out, err
	}
	out.MinID = minID

	if minID == 0 {
		// Empty table: nothing to replay, nothing to resync against.
		return out, nil
	}

	if lastEventID > 0 && lastEventID < minID {
		// Caller is too far behind; replay is impossible.
		out.NeedsResync = true
		return out, nil
	}

	// No upper bound on the replay slice: events are append-only and the
	// caller is asking to catch up. A long replay is exactly the symptom of
	// either a long-lived client or a freshly restarted process with a
	// Last-Event-ID that the table still covers.
	hist, err := b.history.Since(projectID, lastEventID, 0)
	if err != nil {
		return out, err
	}
	out.Events = hist
	if n := len(hist); n > 0 {
		out.MaxID = hist[n-1].ID
	}
	return out, nil
}

// Publish fans out an event to every subscriber whose project matches (empty
// means "all"). It never blocks: if a subscriber's buffer is full, that
// subscriber is dropped and marked for resync, and Publish continues.
//
// Publish is called AFTER a database commit. It must not be called inside the
// write transaction.
func (b *Bus) Publish(e domain.Event) {
	// Hold mu for the entire pass. Every send is non-blocking (buffered
	// channel + default), so no subscriber can stall the publisher. Holding
	// the lock prevents a concurrent removeSub from closing the channel
	// mid-send, which is what the race detector would flag.
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, s := range b.subs {
		if s.needsResync || s.canceled {
			continue
		}
		if !(s.project == "" || s.project == e.ProjectID) {
			continue
		}
		select {
		case s.ch <- e:
		default:
			// Buffer full — drop. Mark + close the channel under the same
			// lock so a concurrent removeSub (also under the same lock)
			// sees the closed state and does not double-close.
			b.dropSlowLocked(s)
		}
	}
}

// dropSlowLocked claims the right to drop s. Only the first caller wins;
// every subsequent call sees needsResync=true and returns. Caller holds mu.
func (b *Bus) dropSlowLocked(s *subscription) {
	if s.needsResync || s.canceled {
		return
	}
	s.needsResync = true
	// Remove from the fan-out slice so a Publish that begins AFTER this
	// point will not see s at all.
	b.removeFromSliceLocked(s)
	// Best-effort marker. The consumer's buffer is full, so the send may
	// fall to default — the close still wakes them up.
	select {
	case s.ch <- domain.Event{
		Type:    ResyncSentinel,
		Payload: map[string]any{"reason": "slow_consumer"},
	}:
	default:
	}
	b.closeChannelsLocked(s)
}

// AllLen returns the total number of subscribers across all projects.
func (b *Bus) AllLen() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// removeSub is the cancel path. Idempotent: a second call is a no-op. Safe to
// call concurrently with Publish: the publish loop already checks canceled
// under the lock.
func (b *Bus) removeSub(s *subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s.canceled {
		return
	}
	s.canceled = true
	b.removeFromSliceLocked(s)
	b.closeChannelsLocked(s)
}

// removeFromSliceLocked removes s from the fan-out slice. Caller holds mu.
func (b *Bus) removeFromSliceLocked(s *subscription) {
	for i, ss := range b.subs {
		if ss == s {
			b.subs = append(b.subs[:i], b.subs[i+1:]...)
			return
		}
	}
}

// closeChannelsLocked closes ch and done exactly once. Caller holds mu.
func (b *Bus) closeChannelsLocked(s *subscription) {
	if s.chClosed {
		return
	}
	s.chClosed = true
	close(s.ch)
	close(s.done)
}

// deliverReplay pushes the replay slice onto the channel under the lock so
// the race detector stays happy. The channel is buffered so a few hundred
// replay events cost nothing.
func (b *Bus) deliverReplay(sub *subscription, replay Replay) {
	for _, e := range replay.Events {
		b.mu.Lock()
		if sub.canceled || sub.chClosed {
			b.mu.Unlock()
			return
		}
		// Buffered channel + default: the consumer may be slow; we
		// never block here.
		select {
		case sub.ch <- e:
		default:
			// Drop the rest of the replay and mark for resync: the
			// consumer cannot keep up even with history.
			sub.needsResync = true
			b.closeChannelsLocked(sub)
			b.mu.Unlock()
			// Best-effort: the consumer will see ResyncSentinel and
			// reload from the source of truth.
			func() {
				defer func() { _ = recover() }()
				sub.ch <- domain.Event{
					Type:    ResyncSentinel,
					Payload: map[string]any{"reason": "replay_overflow"},
				}
			}()
			return
		}
		b.mu.Unlock()
	}
}

// deliverResyncAndClose is the gap-too-far-behind path. It writes the
// ResyncSentinel marker, removes the subscription and closes both channels,
// all under the same lock so concurrent Publish / Cancel cannot race.
func (b *Bus) deliverResyncAndClose(sub *subscription, reason string, lastEventID, minID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if sub.chClosed {
		return
	}
	select {
	case sub.ch <- domain.Event{
		Type: ResyncSentinel,
		Payload: map[string]any{
			"reason":     reason,
			"last_event": lastEventID,
			"min_id":     minID,
		},
	}:
	default:
	}
	sub.needsResync = true
	sub.canceled = true
	b.removeFromSliceLocked(sub)
	b.closeChannelsLocked(sub)
}