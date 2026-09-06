package events

import (
	"sync"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// hookHistory is a HistoryLoader that runs a callback from inside Since —
// that is, while Subscribe sits between registering the subscriber and
// putting the replay on its channel. Publishing from the hook lands an event
// in exactly that window every single run, so the ordering guarantee is
// tested deterministically instead of by racing goroutines and hoping the
// scheduler cooperates.
type hookHistory struct {
	mu     sync.Mutex
	events []domain.Event
	minID  int64

	fired       bool
	duringSince func()
}

func (h *hookHistory) MinID() (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.minID, nil
}

func (h *hookHistory) Since(projectID string, afterID int64, limit int) ([]domain.Event, error) {
	// Take the hook under the lock but call it outside: the hook publishes,
	// and may add to the history, so it must not re-enter h.mu.
	h.mu.Lock()
	hook := h.duringSince
	if hook != nil && !h.fired {
		h.fired = true
	} else {
		hook = nil
	}
	h.mu.Unlock()
	if hook != nil {
		hook()
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]domain.Event, 0, len(h.events))
	for _, e := range h.events {
		if e.ID <= afterID {
			continue
		}
		if projectID != "" && e.ProjectID != projectID {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (h *hookHistory) add(ids ...int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range ids {
		h.events = append(h.events, mkEvent(id, "BMB"))
	}
	if h.minID == 0 && len(h.events) > 0 {
		h.minID = h.events[0].ID
	}
}

// collectIDs reads exactly n events and returns their ids, failing the test
// if the channel closes or goes quiet first.
func collectIDs(t *testing.T, sub *Subscriber, n int, timeout time.Duration) []int64 {
	t.Helper()
	out := make([]int64, 0, n)
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case e, ok := <-sub.Events:
			if !ok {
				t.Fatalf("channel closed after %d/%d events (ids so far %v)", len(out), n, out)
			}
			if e.Type == ResyncSentinel {
				t.Fatalf("unexpected resync after %d/%d events (reason %v)", len(out), n, e.Payload)
			}
			out = append(out, e.ID)
		case <-deadline:
			t.Fatalf("timed out after %d/%d events (ids so far %v)", len(out), n, out)
		}
	}
	return out
}

// A live event published while the history read is still running must be
// delivered AFTER the replay it is newer than. Registering the subscriber and
// then handing it live events straight away puts event 4 in front of events
// 1-3; reading history before registering loses event 4 outright. Both leave
// the browser rendering a board that never converges until someone reloads.
func TestSubscribe_LiveEventDuringReplay_ArrivesAfterHistory(t *testing.T) {
	hist := &hookHistory{}
	hist.add(1, 2, 3)

	bus := New(hist, 8)
	hist.duringSince = func() { bus.Publish(mkEvent(4, "BMB")) }

	sub, err := bus.Subscribe("BMB", 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Cancel()

	got := collectIDs(t, sub, 4, time.Second)
	want := []int64{1, 2, 3, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delivery order %v, want %v (a live event overtook the replay)", got, want)
		}
	}
}

// The overlap case: an event that committed just before the history read and
// was published just after it is in the replay AND in the parked live queue.
// The subscriber must see it once — a replayed id repeated as a live event
// makes the stream non-monotonic just as surely as an inverted one.
func TestSubscribe_EventInBothReplayAndLive_DeliveredOnce(t *testing.T) {
	hist := &hookHistory{}
	hist.add(1, 2, 3)

	bus := New(hist, 8)
	hist.duringSince = func() {
		// Commit-then-publish, with the history snapshot taken in between.
		hist.add(4)
		bus.Publish(mkEvent(4, "BMB"))
	}

	sub, err := bus.Subscribe("BMB", 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Cancel()

	got := collectIDs(t, sub, 4, time.Second)
	want := []int64{1, 2, 3, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delivery order %v, want %v", got, want)
		}
	}
	select {
	case e, ok := <-sub.Events:
		if ok {
			t.Fatalf("extra event id=%d type=%q delivered; event 4 was duplicated", e.ID, e.Type)
		}
		t.Fatal("channel closed after a clean replay")
	case <-time.After(100 * time.Millisecond):
		// Nothing more: the duplicate was suppressed.
	}
}

// Parking live events during replay must stay bounded. A subscriber that
// takes more live events than its budget while the read is in flight is
// dropped and told to resync, exactly like one that overruns its channel —
// the queue must never become an unbounded memory sink.
func TestSubscribe_LiveFloodDuringReplay_DropsSubscriber(t *testing.T) {
	const buf = 4
	hist := &hookHistory{}
	hist.add(1, 2)

	bus := New(hist, buf)
	hist.duringSince = func() {
		for i := 0; i < buf+3; i++ {
			bus.Publish(mkEvent(int64(10+i), "BMB"))
		}
	}

	sub, err := bus.Subscribe("BMB", 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Cancel()

	sawSentinel := false
	deadline := time.After(time.Second)
	for !sawSentinel {
		select {
		case e, ok := <-sub.Events:
			if !ok {
				t.Fatal("channel closed without a resync marker")
			}
			if e.Type == ResyncSentinel {
				sawSentinel = true
				if reason, _ := e.Payload["reason"].(string); reason != "replay_backlog" {
					t.Fatalf("resync reason = %q, want replay_backlog", reason)
				}
			}
		case <-deadline:
			t.Fatal("flooded subscriber never received a resync marker")
		}
	}
	select {
	case _, ok := <-sub.Events:
		if ok {
			t.Fatal("expected the channel closed after the resync marker")
		}
	case <-time.After(time.Second):
		t.Fatal("channel never closed after the resync marker")
	}
	if n := bus.AllLen(); n != 0 {
		t.Fatalf("dropped subscriber still registered: AllLen = %d", n)
	}
}

// Dropping a slow subscriber happens in the middle of a fan-out pass, and it
// removes an element from the slice the pass is walking. Everyone else must
// still get the event, exactly once: a subscriber silently skipped shows a
// stale board forever, and one served twice has a stream whose ids no longer
// increase. This is the failure the monotonic stress test caught.
func TestPublish_DroppingASubscriberDoesNotSkipOrDuplicateOthers(t *testing.T) {
	hist := &fakeHistory{} // empty: no replay, straight to live
	bus := New(hist, 2)

	slow, err := bus.Subscribe("BMB", 0) // registered first, never drained
	if err != nil {
		t.Fatalf("subscribe slow: %v", err)
	}
	defer slow.Cancel()
	second, err := bus.Subscribe("BMB", 0)
	if err != nil {
		t.Fatalf("subscribe second: %v", err)
	}
	defer second.Cancel()
	third, err := bus.Subscribe("BMB", 0)
	if err != nil {
		t.Fatalf("subscribe third: %v", err)
	}
	defer third.Cancel()

	// Fill the slow subscriber's buffer while keeping the other two empty.
	bus.Publish(mkEvent(1, "BMB"))
	bus.Publish(mkEvent(2, "BMB"))
	for _, s := range []*Subscriber{second, third} {
		if got, complete, closed := drainN(s.Events, 2, time.Second); !complete || closed {
			t.Fatalf("priming drain: complete=%v closed=%v got=%v", complete, closed, got)
		}
	}

	// This one drops the slow subscriber mid-pass.
	bus.Publish(mkEvent(3, "BMB"))

	for i, s := range []*Subscriber{second, third} {
		select {
		case e, ok := <-s.Events:
			if !ok {
				t.Fatalf("subscriber %d: channel closed instead of receiving event 3", i)
			}
			if e.ID != 3 {
				t.Fatalf("subscriber %d: id=%d want 3", i, e.ID)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d never received event 3: the drop skipped it", i)
		}
		select {
		case e, ok := <-s.Events:
			if ok {
				t.Fatalf("subscriber %d received a second copy of id=%d", i, e.ID)
			}
			t.Fatalf("subscriber %d: channel closed unexpectedly", i)
		case <-time.After(50 * time.Millisecond):
			// Exactly one copy: correct.
		}
	}

	// And the slow one really was dropped.
	if n := bus.AllLen(); n != 2 {
		t.Fatalf("AllLen = %d, want 2 after the slow subscriber was dropped", n)
	}
}

// serialHistory models the production writer: the event is appended to the
// table and published while the same lock is held, so a history read either
// sees the row and its publish, or neither. That is what makes "strictly
// increasing ids at every subscriber" a property the bus can be held to.
type serialHistory struct {
	mu     sync.Mutex
	events []domain.Event
	minID  int64
}

func (s *serialHistory) MinID() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.minID, nil
}

func (s *serialHistory) Since(projectID string, afterID int64, limit int) ([]domain.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Event, 0, len(s.events))
	for _, e := range s.events {
		if e.ID <= afterID {
			continue
		}
		if projectID != "" && e.ProjectID != projectID {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *serialHistory) commitAndPublish(bus *Bus, e domain.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	if s.minID == 0 {
		s.minID = e.ID
	}
	bus.Publish(e)
}

// Subscribers joining a board that is being written to concurrently must
// never see an id go backwards or repeat, whichever point of the write stream
// they happen to join at.
func TestSubscribe_UnderConcurrentPublish_IDsStayMonotonic(t *testing.T) {
	hist := &serialHistory{}
	bus := New(hist, 64)

	const (
		writers     = 1
		eventCount  = 300
		subscribers = 8
	)

	stop := make(chan struct{})
	var pub sync.WaitGroup
	pub.Add(writers)
	go func() {
		defer pub.Done()
		for i := 1; i <= eventCount; i++ {
			hist.commitAndPublish(bus, mkEvent(int64(i), "BMB"))
		}
		close(stop)
	}()

	var subs sync.WaitGroup
	for i := 0; i < subscribers; i++ {
		subs.Add(1)
		go func(idx int) {
			defer subs.Done()
			sub, err := bus.Subscribe("BMB", 0)
			if err != nil {
				t.Errorf("subscriber %d: subscribe: %v", idx, err)
				return
			}
			defer sub.Cancel()

			var last int64
			idle := time.NewTimer(300 * time.Millisecond)
			defer idle.Stop()
			for {
				select {
				case e, ok := <-sub.Events:
					if !ok {
						return
					}
					if e.Type == ResyncSentinel {
						// A dropped subscriber is allowed; an out-of-order
						// one is not. Stop checking after the marker.
						return
					}
					if e.ID <= last {
						t.Errorf("subscriber %d: id %d after %d — stream is not monotonic", idx, e.ID, last)
						return
					}
					last = e.ID
					if !idle.Stop() {
						select {
						case <-idle.C:
						default:
						}
					}
					idle.Reset(300 * time.Millisecond)
				case <-idle.C:
					return
				}
			}
		}(i)
	}

	subs.Wait()
	<-stop
	pub.Wait()
}
