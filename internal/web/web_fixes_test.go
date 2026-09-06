package web

// Tests for the three defects the 2026-09-06 independent review filed against
// this package (#6 malformed SSE frames, #10 missing backups directory, #13
// the activity snapshot's idle-window wait). Each test pins the fix's wire- or
// filesystem-level behaviour, not just an internal shape.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/events"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/templates"
)

// fixesAdminSecret must match the secret newTestManager bootstraps, so a
// session minted from it authenticates as the admin token.
const fixesAdminSecret = "kbn_testadmin00xx00xx00xx00xx00xx00xx00xx"

// newWebWithDeps is newTestWeb plus a mutation hook, for tests that need to
// wire (or deliberately unwire) optional Deps like Backup, BackupDir and
// History.
func newWebWithDeps(t *testing.T, mutate func(*Deps)) *Web {
	t.Helper()
	tpl, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New: %v", err)
	}
	d := Deps{
		Service:   stubService{},
		Auth:      newTestManager(t),
		Bus:       events.New(newEmptyHistory(), 4),
		Templates: tpl,
		BaseURL:   "http://127.0.0.1:0",
	}
	mutate(&d)
	return New(d)
}

// ---------------------------------------------------------------------------
// Shared fakes
// ---------------------------------------------------------------------------

// sliceHistory is a deterministic events.HistoryLoader: events kept
// oldest-first, filtered by project, honouring the Since limit semantics
// (limit zero = everything). It serves both the Deps.History seam and the
// events.Bus constructor, like the real store-backed adapter does.
type sliceHistory struct {
	evs []domain.Event
}

func (h sliceHistory) Since(projectID string, afterID int64, limit int) ([]domain.Event, error) {
	var out []domain.Event
	for _, e := range h.evs {
		if e.ID <= afterID {
			continue
		}
		if projectID != "" && e.ProjectID != projectID {
			continue
		}
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, e)
	}
	return out, nil
}

func (h sliceHistory) MinID() (int64, error) {
	if len(h.evs) == 0 {
		return 0, nil
	}
	return h.evs[0].ID, nil
}

// newSliceHistory builds n events for projectID with ids 1..n and distinct,
// collision-free actor names ("who-01".."who-60") so index-based ordering
// assertions on rendered HTML cannot trip on substrings.
func newSliceHistory(n int, projectID string) sliceHistory {
	evs := make([]domain.Event, 0, n)
	for i := 1; i <= n; i++ {
		evs = append(evs, domain.Event{
			ID:        int64(i),
			TS:        time.Date(2026, 9, 6, 10, i, 0, 0, time.UTC),
			Actor:     fmt.Sprintf("who-%02d", i),
			Type:      domain.EventTaskMoved,
			ProjectID: projectID,
			Payload:   map[string]any{"key": "BMB-1", "title": "T"},
		})
	}
	return sliceHistory{evs: evs}
}

// errHistory always fails, for the fail-loud path.
type errHistory struct{}

func (errHistory) Since(string, int64, int) ([]domain.Event, error) {
	return nil, errors.New("history backend unavailable")
}

// stubProjectLookup resolves every key to one fixed internal id.
type stubProjectLookup struct{ id string }

func (p stubProjectLookup) ProjectIDForKey(context.Context, string) (string, bool, error) {
	return p.id, true, nil
}

// ---------------------------------------------------------------------------
// #6 — SSE frames parse the way the W3C spec requires
// ---------------------------------------------------------------------------

// sseFrame is one parsed SSE frame: the accumulated field values plus the
// raw per-field counts, so tests can assert "exactly one event: line" — the
// thing the spec's last-write-wins event-type buffer makes critical.
type sseFrame struct {
	id         string
	event      string
	data       string
	eventLines int
	dataLines  int
}

// parseSSEFrames splits a raw byte stream the way the SSE spec's frame layer
// does: frames are terminated by a blank line, so a stream that does not end
// with one is malformed by definition.
func parseSSEFrames(t *testing.T, raw string) []sseFrame {
	t.Helper()
	if !strings.HasSuffix(raw, "\n\n") {
		t.Fatalf("raw SSE bytes must end with a blank-line frame terminator; got tail %q", trim(raw, 120))
	}
	blocks := strings.Split(strings.TrimSuffix(raw, "\n\n"), "\n\n")
	if len(blocks) == 0 || (len(blocks) == 1 && blocks[0] == "") {
		t.Fatalf("no frames in stream: %q", raw)
	}
	frames := make([]sseFrame, 0, len(blocks))
	for _, b := range blocks {
		var f sseFrame
		var data []string
		for _, line := range strings.Split(b, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				f.eventLines++
				f.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				f.dataLines++
				data = append(data, strings.TrimPrefix(line, "data:"))
			case strings.HasPrefix(line, "id:"):
				f.id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			}
		}
		f.data = strings.Join(data, "\n")
		frames = append(frames, f)
	}
	return frames
}

// assertSpecCompliantFrame pins the invariants the W3C parser imposes: one
// event type per frame (several event: lines collapse to the last, so a
// listener on any other name never fires) and at least one data: line.
func assertSpecCompliantFrame(t *testing.T, f sseFrame, wantEvent string) {
	t.Helper()
	if f.eventLines != 1 {
		t.Fatalf("frame has %d event: lines, want exactly 1 (spec: last one wins, others are unreachable): %q", f.eventLines, f.event)
	}
	if f.event != wantEvent {
		t.Fatalf("frame event = %q, want %q", f.event, wantEvent)
	}
	if f.dataLines < 1 {
		t.Fatalf("frame %q has no data: line", f.event)
	}
}

func TestWriteSSEEvent_OneEventNamePerFrame(t *testing.T) {
	cases := []struct {
		name      string
		ev        domain.Event
		wantEvent string
		wantID    string
	}{
		{
			name:      "domain event with id",
			ev:        domain.Event{ID: 42, TS: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC), Actor: "claude@rog", Type: domain.EventTaskMoved, ProjectID: "p1", Payload: map[string]any{"key": "BMB-1"}},
			wantEvent: sseChangeEvent,
			wantID:    "42",
		},
		{
			name:      "domain event without id",
			ev:        domain.Event{Type: domain.EventProjectCreated, ProjectID: "p1"},
			wantEvent: sseChangeEvent,
			wantID:    "",
		},
		{
			name:      "resync sentinel keeps its own name",
			ev:        domain.Event{Type: events.ResyncSentinel, Payload: map[string]any{"reason": "history_gap"}},
			wantEvent: sseResyncEvent,
			wantID:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rw := httptest.NewRecorder()
			writeSSEEvent(rw, tc.ev)

			frames := parseSSEFrames(t, rw.Body.String())
			if len(frames) != 1 {
				t.Fatalf("got %d frames, want 1", len(frames))
			}
			f := frames[0]
			assertSpecCompliantFrame(t, f, tc.wantEvent)
			if f.id != tc.wantID {
				t.Fatalf("frame id = %q, want %q", f.id, tc.wantID)
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(f.data), &payload); err != nil {
				t.Fatalf("data is not JSON: %v; data=%q", err, f.data)
			}
			// The concrete domain type must ride inside the payload: with a
			// single canonical event name, the payload is the only place a
			// client can learn what actually happened.
			if got, _ := payload["Type"].(string); got != string(tc.ev.Type) {
				t.Fatalf("payload Type = %q, want %q (payload=%q)", got, tc.ev.Type, f.data)
			}
		})
	}
}

// TestSSE_StreamFramesAreSpecCompliant drives the real handler end to end:
// connect, receive the connected frame, publish a domain event on the bus,
// receive the change frame — then spec-parse every byte the server sent.
func TestSSE_StreamFramesAreSpecCompliant(t *testing.T) {
	w := newTestWeb(t)
	srv := httptest.NewServer(w.Handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+fixesAdminSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()

	type chunk struct {
		data string
		err  error
	}
	chunks := make(chan chunk, 16)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				chunks <- chunk{data: string(buf[:n])}
			}
			if rerr != nil {
				chunks <- chunk{err: rerr}
				return
			}
		}
	}()

	// Phase 1: the connected frame proves the subscription is registered
	// (handleEvents subscribes before writing it), so a publish cannot race
	// past the fan-out.
	var acc bytes.Buffer
	waitFor := func(needle string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			if strings.Contains(acc.String(), needle) && strings.HasSuffix(acc.String(), "\n\n") {
				return
			}
			select {
			case c := <-chunks:
				if c.err != nil {
					t.Fatalf("read: %v (after %q)", c.err, acc.String())
				}
				acc.WriteString(c.data)
			case <-deadline:
				t.Fatalf("did not receive %q within 5s; got %q", needle, acc.String())
			}
		}
	}
	waitFor("event: connected")

	// Phase 2: a live domain event must arrive as exactly one spec-clean
	// frame named for the canonical change signal.
	w.d.Bus.Publish(domain.Event{
		ID: 7, TS: time.Date(2026, 9, 6, 12, 0, 1, 0, time.UTC),
		Actor: "claude@rog", Type: domain.EventTaskMoved, ProjectID: "p1",
		Payload: map[string]any{"key": "BMB-1"},
	})
	waitFor("event: " + sseChangeEvent)

	frames := parseSSEFrames(t, acc.String())
	if len(frames) < 2 {
		t.Fatalf("got %d frames, want at least connected + change", len(frames))
	}
	assertSpecCompliantFrame(t, frames[0], "connected")
	var change *sseFrame
	for i := range frames {
		if frames[i].event == sseChangeEvent {
			change = &frames[i]
		}
	}
	if change == nil {
		t.Fatalf("no %q frame in stream: %q", sseChangeEvent, acc.String())
	}
	assertSpecCompliantFrame(t, *change, sseChangeEvent)
	if change.id != "7" {
		t.Fatalf("change frame id = %q, want 7 (Last-Event-ID resume depends on it)", change.id)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(change.data), &payload); err != nil {
		t.Fatalf("change data is not JSON: %v; data=%q", err, change.data)
	}
	if got, _ := payload["Type"].(string); got != string(domain.EventTaskMoved) {
		t.Fatalf("change payload Type = %q, want %q", got, domain.EventTaskMoved)
	}
}

// ---------------------------------------------------------------------------
// #10 — backup works on a clean install
// ---------------------------------------------------------------------------

// postBackupWithSession issues the admin backup POST exactly as the browser
// would: session cookie plus matching CSRF cookie and form field.
func postBackupWithSession(t *testing.T, w *Web, withCSRF bool) *httptest.ResponseRecorder {
	t.Helper()
	sess := sessionFor(t, w.d.Auth, fixesAdminSecret)
	body := ""
	var csrfCookie *http.Cookie
	if withCSRF {
		csrf, err := w.d.Auth.IssueCSRF()
		if err != nil {
			t.Fatalf("IssueCSRF: %v", err)
		}
		body = "csrf_token=" + csrf
		csrfCookie = &http.Cookie{Name: auth.CSRFCookieName, Value: csrf}
	}
	req := httptest.NewRequest("POST", "/admin/backup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionCookie(sess))
	if csrfCookie != nil {
		req.AddCookie(csrfCookie)
	}
	rw := httptest.NewRecorder()
	w.Handler().ServeHTTP(rw, req)
	return rw
}

func TestAdminBackup_CreatesBackupDirOnFreshInstall(t *testing.T) {
	// A clean install: the data dir exists (the composition root creates it)
	// but the backups directory inside it never has.
	root := t.TempDir()
	backupDir := filepath.Join(root, "backups")
	if _, err := os.Stat(backupDir); !os.IsNotExist(err) {
		t.Fatalf("precondition: %q must not exist yet, stat err=%v", backupDir, err)
	}

	var gotDest string
	w := newWebWithDeps(t, func(d *Deps) {
		d.BackupDir = func() string { return backupDir }
		d.Backup = func(_ context.Context, dest string) error {
			gotDest = dest
			// Mirrors VACUUM INTO, which cannot open a file in a missing
			// directory: without the MkdirAll this write fails and the test
			// fails with it.
			return os.WriteFile(dest, []byte("sqlite-bytes"), 0o600)
		}
	})

	rw := postBackupWithSession(t, w, true)
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%q", rw.Code, trim(rw.Body.String(), 200))
	}
	if loc := rw.Header().Get("Location"); loc != "/admin" {
		t.Fatalf("Location = %q, want /admin", loc)
	}
	if info, err := os.Stat(backupDir); err != nil || !info.IsDir() {
		t.Fatalf("backup dir was not created: stat err=%v", err)
	}
	if !strings.HasPrefix(gotDest, backupDir) || !strings.HasSuffix(gotDest, ".db") {
		t.Fatalf("backup dest = %q, want inside %q ending in .db", gotDest, backupDir)
	}
	got, err := os.ReadFile(gotDest)
	if err != nil {
		t.Fatalf("backup file missing after 303: %v", err)
	}
	if string(got) != "sqlite-bytes" {
		t.Fatalf("backup file content = %q", got)
	}
}

// TestAdminBackup_GetIsRejected pins the already-fixed half of review #10:
// the disk-writing route answers POST only, so no foreign <img> tag can
// reach it, and a plain GET gets the method rejection.
func TestAdminBackup_GetIsRejected(t *testing.T) {
	w := newWebWithDeps(t, func(d *Deps) {
		d.BackupDir = func() string { return t.TempDir() }
		d.Backup = func(context.Context, string) error { return errors.New("must not run") }
	})
	sess := sessionFor(t, w.d.Auth, fixesAdminSecret)
	rw := do(w, "GET", "/admin/backup", nil, sessionCookie(sess))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /admin/backup status = %d, want 405", rw.Code)
	}
}

// TestAdminBackup_StillRequiresCSRF is the second half of that pin: a valid
// session without a CSRF token must not reach the backup.
func TestAdminBackup_StillRequiresCSRF(t *testing.T) {
	w := newWebWithDeps(t, func(d *Deps) {
		d.BackupDir = func() string { return t.TempDir() }
		d.Backup = func(context.Context, string) error { return errors.New("must not run") }
	})
	rw := postBackupWithSession(t, w, false)
	if rw.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (CSRF gate must stay shut)", rw.Code)
	}
}

// ---------------------------------------------------------------------------
// #13 — the activity snapshot reads history, not a timed drain
// ---------------------------------------------------------------------------

// TestSnapshotEvents_HistoryPathDoesNotTouchBus proves the wired path never
// subscribes: the Bus is nil, so any drain attempt would fail the test
// instead of silently costing 150ms.
func TestSnapshotEvents_HistoryPathDoesNotTouchBus(t *testing.T) {
	w := newWebWithDeps(t, func(d *Deps) {
		d.History = newSliceHistory(60, "p1")
		d.Bus = nil
	})
	evs, err := w.snapshotEvents("p1", 50)
	if err != nil {
		t.Fatalf("snapshotEvents: %v", err)
	}
	if len(evs) != 50 {
		t.Fatalf("got %d events, want 50 (capped)", len(evs))
	}
	if evs[0].ID != 60 || evs[49].ID != 11 {
		t.Fatalf("events not newest-first/capped: first=%d last=%d, want first=60 last=11", evs[0].ID, evs[49].ID)
	}
	// Project filtering is the loader's contract; the handler passes the
	// internal project id through unchanged.
	other, err := w.snapshotEvents("other-project", 50)
	if err != nil {
		t.Fatalf("snapshotEvents(other): %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("got %d events for a foreign project, want 0", len(other))
	}
}

// TestSnapshotEvents_HistoryErrorPropagates pins fail-loud: a broken history
// read surfaces as an error, never as a confident empty feed.
func TestSnapshotEvents_HistoryErrorPropagates(t *testing.T) {
	w := newWebWithDeps(t, func(d *Deps) { d.History = errHistory{} })
	if _, err := w.snapshotEvents("p1", 50); err == nil {
		t.Fatal("snapshotEvents swallowed the history error")
	}
}

// TestSnapshotEvents_FallbackDrainsBusWhenHistoryUnwired keeps the
// compatibility net honest: with no History wired, the old bus-drain still
// returns the project's events (this is the one test that pays the ~150ms
// idle window, by design).
func TestSnapshotEvents_FallbackDrainsBusWhenHistoryUnwired(t *testing.T) {
	w := newWebWithDeps(t, func(d *Deps) {
		d.Bus = events.New(newSliceHistory(3, "p1"), 8)
	})
	evs, err := w.snapshotEvents("p1", 50)
	if err != nil {
		t.Fatalf("snapshotEvents: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3", len(evs))
	}
	if evs[0].ID != 3 || evs[2].ID != 1 {
		t.Fatalf("fallback not newest-first: got [%d %d %d], want [3 2 1]", evs[0].ID, evs[1].ID, evs[2].ID)
	}
}

// TestActivityPage_RendersHistoryNewestFirstCapped drives the full page: the
// 50 newest events render newest-first and the 51st stays off the page.
func TestActivityPage_RendersHistoryNewestFirstCapped(t *testing.T) {
	w := newWebWithDeps(t, func(d *Deps) {
		d.History = newSliceHistory(60, "p1")
		d.ProjectLookup = stubProjectLookup{id: "p1"}
	})
	sess := sessionFor(t, w.d.Auth, fixesAdminSecret)
	rw := do(w, "GET", "/p/BMB/activity", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rw.Code, trim(rw.Body.String(), 200))
	}
	body := rw.Body.String()
	for _, want := range []string{"who-60</span>", "who-11</span>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("activity page missing newest/cap-bound actor %q", want)
		}
	}
	if strings.Contains(body, "who-10</span>") {
		t.Fatal("activity page rendered the 51st-newest event; cap is 50")
	}
	if strings.Index(body, "who-60</span>") > strings.Index(body, "who-11</span>") {
		t.Fatal("activity page is not newest-first")
	}
}
