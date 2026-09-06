package events

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// fakeHistory is the in-memory HistoryLoader used by the tests. It mirrors
// what the SQLite EventRepo would answer, with no on-disk side effects.
type fakeHistory struct {
	mu       sync.Mutex
	events   []domain.Event
	minID    int64
	failNext atomic.Bool
}

func (f *fakeHistory) Since(projectID string, afterID int64, limit int) ([]domain.Event, error) {
	if f.failNext.Load() {
		return nil, errors.New("fake: store busy")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.Event, 0, len(f.events))
	for _, e := range f.events {
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

func (f *fakeHistory) MinID() (int64, error) {
	if f.failNext.Load() {
		return 0, errors.New("fake: store busy")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.minID, nil
}

func (f *fakeHistory) appendBatch(projectID string, n int) (first, last int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	base := int64(len(f.events)) + 1
	for i := 0; i < n; i++ {
		id := base + int64(i)
		f.events = append(f.events, domain.Event{
			ID:        id,
			TS:        time.Now().UTC(),
			Actor:     "tester",
			Type:      domain.EventTaskUpdated,
			ProjectID: projectID,
		})
	}
	if f.minID == 0 && len(f.events) > 0 {
		f.minID = f.events[0].ID
	}
	return base, base + int64(n) - 1
}

func mkEvent(id int64, projectID string) domain.Event {
	return domain.Event{
		ID:        id,
		TS:        time.Now().UTC(),
		Actor:     "tester",
		Type:      domain.EventTaskUpdated,
		ProjectID: projectID,
	}
}

// drainN reads up to n events from ch and returns what arrived and whether
// the channel closed before n was reached.
func drainN(ch <-chan domain.Event, n int, timeout time.Duration) ([]domain.Event, bool, bool) {
	out := make([]domain.Event, 0, n)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for i := 0; i < n; i++ {
		select {
		case e, ok := <-ch:
			if !ok {
				return out, false, true
			}
			out = append(out, e)
		case <-deadline.C:
			return out, false, false
		}
	}
	return out, true, false
}

// ---------------------------------------------------------------------------
// Subscribe + replay
// ---------------------------------------------------------------------------

func TestSubscribe_FreshClientGetsReplay(t *testing.T) {
	hist := &fakeHistory{}
	hist.appendBatch("BMB", 5)

	bus := New(hist, 8)
	sub, err := bus.Subscribe("BMB", 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Cancel()

	events, complete, closed := drainN(sub.Events, 5, time.Second)
	if !complete || closed {
		t.Fatalf("replay incomplete: complete=%v closed=%v got=%d", complete, closed, len(events))
	}
	for i, e := range events {
		wantID := int64(i + 1)
		if e.ID != wantID {
			t.Fatalf("event %d id=%d want %d", i, e.ID, wantID)
		}
	}
}

func TestSubscribe_ResyncWhenLastEventIDOlderThanRetained(t *testing.T) {
	hist := &fakeHistory{}
	// The events table retains ids 100..110.
	for i := 100; i <= 110; i++ {
		hist.events = append(hist.events, mkEvent(int64(i), "BMB"))
	}
	hist.minID = 100

	bus := New(hist, 8)
	sub, err := bus.Subscribe("BMB", 50)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Cancel()

	select {
	case e, ok := <-sub.Events:
		if !ok {
			t.Fatalf("expected resync marker, got channel closed")
		}
		if e.Type != ResyncSentinel {
			t.Fatalf("marker type=%q want %q", e.Type, ResyncSentinel)
		}
	case <-time.After(time.Second):
		t.Fatalf("no resync marker delivered")
	}
	select {
	case _, ok := <-sub.Events:
		if ok {
			t.Fatalf("expected channel closed after resync marker")
		}
	case <-time.After(time.Second):
		t.Fatalf("channel never closed after resync marker")
	}
}

func TestSubscribe_EmptyTable_NoResync(t *testing.T) {
	hist := &fakeHistory{}
	bus := New(hist, 8)
	sub, err := bus.Subscribe("BMB", 42)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Cancel()

	// No events, so the channel has nothing in it; Publish should work and
	// the consumer should see the new event.
	go func() {
		time.Sleep(10 * time.Millisecond)
		bus.Publish(mkEvent(1, "BMB"))
	}()
	select {
	case e := <-sub.Events:
		if e.ID != 1 {
			t.Fatalf("got id=%d want 1", e.ID)
		}
	case <-time.After(time.Second):
		t.Fatalf("no event delivered")
	}
}

func TestSubscribe_HistoryErrorPropagates(t *testing.T) {
	hist := &fakeHistory{}
	hist.failNext.Store(true)
	bus := New(hist, 8)
	if _, err := bus.Subscribe("BMB", 0); !errors.Is(err, ErrHistoryUnavailable) && err == nil {
		// The error is whatever the loader produced, not the sentinel —
		// but it must be non-nil so callers see something.
		t.Fatalf("expected non-nil error from subscribe on history failure")
	}
}

// ---------------------------------------------------------------------------
// Publish
// ---------------------------------------------------------------------------

func TestPublish_FansOutToAllProjectsAndMatching(t *testing.T) {
	hist := &fakeHistory{}
	bus := New(hist, 8)

	all, err := bus.Subscribe("", 0)
	if err != nil {
		t.Fatalf("subscribe all: %v", err)
	}
	defer all.Cancel()
	bmb, err := bus.Subscribe("BMB", 0)
	if err != nil {
		t.Fatalf("subscribe bmb: %v", err)
	}
	defer bmb.Cancel()
	other, err := bus.Subscribe("OTHER", 0)
	if err != nil {
		t.Fatalf("subscribe other: %v", err)
	}
	defer other.Cancel()

	bus.Publish(mkEvent(1, "BMB"))

	for _, c := range []<-chan domain.Event{all.Events, bmb.Events} {
		select {
		case e := <-c:
			if e.ID != 1 {
				t.Fatalf("got id=%d want 1", e.ID)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber did not receive event")
		}
	}
	// 'other' must NOT receive BMB events.
	select {
	case e := <-other.Events:
		t.Fatalf("OTHER subscriber received BMB event id=%d", e.ID)
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

func TestPublish_NeverBlocksOnSlowSubscriber(t *testing.T) {
	hist := &fakeHistory{}
	// Tight buffer so the subscriber fills up after just a couple of events.
	bus := New(hist, 2)
	slow, err := bus.Subscribe("BMB", 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer slow.Cancel()

	// Saturate the buffer; further sends should drop without blocking.
	for i := 1; i <= 2; i++ {
		bus.Publish(mkEvent(int64(i), "BMB"))
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 3; i <= 1000; i++ {
			bus.Publish(mkEvent(int64(i), "BMB"))
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Publish blocked on a slow subscriber")
	}

	// The slow subscriber must eventually observe the resync sentinel and
	// the channel closing.
	deadline := time.After(time.Second)
	for {
		select {
		case e, ok := <-slow.Events:
			if !ok {
				return
			}
			if e.Type == ResyncSentinel {
				return
			}
		case <-deadline:
			t.Fatalf("slow subscriber never received ResyncSentinel")
		}
	}
}

// ---------------------------------------------------------------------------
// Concurrency: 100 subscribers, publisher never blocks
// ---------------------------------------------------------------------------

func TestPublish_100ConcurrentSubscribers_NoBlock(t *testing.T) {
	hist := &fakeHistory{}
	bus := New(hist, 8)

	const subs = 100
	subscribers := make([]*Subscriber, subs)
	for i := 0; i < subs; i++ {
		s, err := bus.Subscribe("BMB", 0)
		if err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		subscribers[i] = s
	}
	t.Cleanup(func() {
		for _, s := range subscribers {
			s.Cancel()
		}
	})

	var received int64
	var wg sync.WaitGroup
	for i, s := range subscribers {
		wg.Add(1)
		go func(idx int, sub *Subscriber) {
			defer wg.Done()
			for {
				select {
				case _, ok := <-sub.Events:
					if !ok {
						return
					}
					atomic.AddInt64(&received, 1)
				case <-time.After(500 * time.Millisecond):
					return
				}
			}
		}(i, s)
	}

	// Publish 100 events; the publisher must finish well within a second.
	pubStart := time.Now()
	for i := 1; i <= 100; i++ {
		bus.Publish(mkEvent(int64(i), "BMB"))
	}
	pubDur := time.Since(pubStart)
	if pubDur > time.Second {
		t.Fatalf("publish 100 events took %v", pubDur)
	}

	wg.Wait()
	if got := atomic.LoadInt64(&received); got == 0 {
		t.Fatalf("no subscriber received anything")
	}
}

// ---------------------------------------------------------------------------
// Unsubscribe during publish must not panic.
// ---------------------------------------------------------------------------

func TestUnsubscribeDuringPublish_NoPanic(t *testing.T) {
	hist := &fakeHistory{}
	bus := New(hist, 8)

	const subs = 50
	subscribers := make([]*Subscriber, subs)
	for i := 0; i < subs; i++ {
		s, err := bus.Subscribe("BMB", 0)
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		subscribers[i] = s
	}

	stop := make(chan struct{})
	var pubWG sync.WaitGroup
	pubWG.Add(1)
	go func() {
		defer pubWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			bus.Publish(mkEvent(time.Now().UnixNano(), "BMB"))
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < subs; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			time.Sleep(time.Duration(idx) * time.Millisecond)
			subscribers[idx].Cancel()
		}(i)
	}
	wg.Wait()
	close(stop)
	pubWG.Wait()

	// After everyone cancelled, the bus must be drained.
	bus.mu.Lock()
	n := len(bus.subs)
	bus.mu.Unlock()
	if n != 0 {
		t.Fatalf("after cancel: %d subscribers still registered", n)
	}
}

// ---------------------------------------------------------------------------
// Unsubscribe is idempotent
// ---------------------------------------------------------------------------

func TestUnsubscribe_Idempotent(t *testing.T) {
	hist := &fakeHistory{}
	bus := New(hist, 8)
	sub, err := bus.Subscribe("BMB", 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	sub.Cancel()
	sub.Cancel() // must not panic on double-close
}

// ---------------------------------------------------------------------------
// Replay continues from a valid Last-Event-ID
// ---------------------------------------------------------------------------

func TestSubscribe_ResumeFromLastEventID(t *testing.T) {
	hist := &fakeHistory{}
	hist.appendBatch("BMB", 10)

	bus := New(hist, 8)
	sub, err := bus.Subscribe("BMB", 5)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Cancel()

	events, complete, _ := drainN(sub.Events, 5, time.Second)
	if !complete {
		t.Fatalf("replay incomplete: got %d", len(events))
	}
	for i, e := range events {
		want := int64(6 + i)
		if e.ID != want {
			t.Fatalf("event %d id=%d want %d", i, e.ID, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Table-driven: replay outcomes for a 4-case matrix
// ---------------------------------------------------------------------------

func TestReplayTable(t *testing.T) {
	type expect struct {
		resync     bool
		deliverIDs []int64
	}
	cases := []struct {
		name    string
		minID   int64
		events  []int64 // events stored in history
		lastID  int64   // caller-supplied last event id
		project string  // subscribe filter
		expect  expect
	}{
		{
			name:    "fresh-subscribe-all",
			minID:   1,
			events:  []int64{1, 2, 3},
			lastID:  0,
			project: "",
			expect:  expect{deliverIDs: []int64{1, 2, 3}},
		},
		{
			name:    "fresh-subscribe-resume",
			minID:   1,
			events:  []int64{1, 2, 3, 4, 5},
			lastID:  3,
			project: "",
			expect:  expect{deliverIDs: []int64{4, 5}},
		},
		{
			name:    "gap-too-far-behind",
			minID:   100,
			events:  []int64{100, 101, 102},
			lastID:  50,
			project: "",
			expect:  expect{resync: true},
		},
		{
			name:    "no-events-yet",
			minID:   0,
			events:  nil,
			lastID:  42,
			project: "",
			expect:  expect{deliverIDs: nil},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hist := &fakeHistory{minID: tc.minID}
			for _, id := range tc.events {
				hist.events = append(hist.events, mkEvent(id, "BMB"))
			}
			bus := New(hist, 8)
			sub, err := bus.Subscribe(tc.project, tc.lastID)
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			defer sub.Cancel()

			if tc.expect.resync {
				select {
				case e, ok := <-sub.Events:
					if !ok {
						t.Fatalf("expected resync marker, channel closed")
					}
					if e.Type != ResyncSentinel {
						t.Fatalf("expected ResyncSentinel, got %q", e.Type)
					}
				case <-time.After(time.Second):
					t.Fatalf("no resync marker")
				}
				// And the channel must close after.
				select {
				case _, ok := <-sub.Events:
					if ok {
						t.Fatalf("expected channel closed after resync")
					}
				case <-time.After(time.Second):
					t.Fatalf("channel not closed after resync")
				}
				return
			}

			got := make([]int64, 0, len(tc.expect.deliverIDs))
			deadline := time.After(500 * time.Millisecond)
			for len(got) < len(tc.expect.deliverIDs) {
				select {
				case e, ok := <-sub.Events:
					if !ok {
						t.Fatalf("channel closed early after %d/%d events", len(got), len(tc.expect.deliverIDs))
					}
					got = append(got, e.ID)
				case <-deadline:
					t.Fatalf("timed out after %d/%d events", len(got), len(tc.expect.deliverIDs))
				}
			}
			if len(got) != len(tc.expect.deliverIDs) {
				t.Fatalf("got %d events want %d", len(got), len(tc.expect.deliverIDs))
			}
			for i, id := range got {
				if id != tc.expect.deliverIDs[i] {
					t.Fatalf("event %d id=%d want %d", i, id, tc.expect.deliverIDs[i])
				}
			}
		})
	}
}
