package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/events"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/templates"
)

// ---------------------------------------------------------------------------
// KANB-70: an export is the owner's backup. Whatever surface wrote it, it
// must restore to the same board, and an import that cannot complete must
// write nothing.
// ---------------------------------------------------------------------------

var kanb70Admin = service.Actor{
	Name:   "cli-admin",
	Scopes: domain.Scopes{domain.ScopeAdmin, domain.ScopeWrite, domain.ScopeRead},
}

func kanb70Open(t *testing.T, dir string) (store.Store, service.Service) {
	t.Helper()
	st, err := openStore(context.Background(), dir)
	if err != nil {
		t.Fatalf("openStore(%s): %v", dir, err)
	}
	return st, service.New(st, nil)
}

// kanb70Token plants a token with a chosen id, so two boards can share an
// identity the way a restore onto a board with the same keys does.
func kanb70Token(t *testing.T, st store.Store, id, name string, scopes ...domain.Scope) {
	t.Helper()
	sum := sha256.Sum256([]byte(id))
	if err := st.Write(context.Background(), func(tx store.Tx) error {
		return st.Tokens().Create(tx, &domain.Token{ID: id, Name: name, Hash: sum[:], Scopes: scopes})
	}); err != nil {
		t.Fatalf("create token %s: %v", name, err)
	}
}

func kanb70Project(t *testing.T, svc service.Service, key string, settings *service.ProjectSettings) {
	t.Helper()
	if _, err := svc.ProjectUpsert(context.Background(), kanb70Admin, service.ProjectUpsertInput{
		Mode: service.UpsertCreate, Key: key, Name: "Project " + key,
		Columns: []service.ColumnSpec{
			{Name: "Backlog", Kind: domain.KindBacklog},
			{Name: "Doing", Kind: domain.KindActive},
			{Name: "Done", Kind: domain.KindDone},
		},
		Settings: settings,
	}); err != nil {
		t.Fatalf("ProjectUpsert %s: %v", key, err)
	}
}

func kanb70Tasks(t *testing.T, svc service.Service, key, column string, titles ...string) []domain.TaskView {
	t.Helper()
	var out []domain.TaskView
	for len(titles) > 0 {
		n := min(len(titles), domain.MaxBatchTasks)
		batch := make([]service.NewTask, n)
		for i, title := range titles[:n] {
			batch[i] = service.NewTask{ProjectKey: key, Column: column, Title: title, Type: domain.TypeTask}
		}
		titles = titles[n:]
		res, err := svc.TaskCreate(context.Background(), kanb70Admin, service.TaskCreateInput{Tasks: batch})
		if err != nil {
			t.Fatalf("TaskCreate in %s: %v", key, err)
		}
		out = append(out, res.Tasks...)
	}
	return out
}

func kanb70Patch(t *testing.T, svc service.Service, p service.TaskPatch) *domain.TaskView {
	t.Helper()
	res, err := svc.TaskUpdate(context.Background(), kanb70Admin, service.TaskUpdateInput{Patches: []service.TaskPatch{p}})
	if err != nil {
		t.Fatalf("TaskUpdate %s: %v", p.Key, err)
	}
	if !res.Items[0].OK {
		t.Fatalf("TaskUpdate %s: %v", p.Key, res.Items[0].Err)
	}
	return res.Items[0].Task
}

func kanb70Chat(t *testing.T, svc service.Service, a service.Actor, in service.ChatAddInput) *domain.ChatMessage {
	t.Helper()
	m, err := svc.ChatAdd(context.Background(), a, in)
	if err != nil {
		t.Fatalf("ChatAdd %q: %v", in.Body, err)
	}
	return m
}

func kanb70Feed(t *testing.T, st store.Store, key string) []domain.ChatMessage {
	t.Helper()
	var out []domain.ChatMessage
	if err := st.Read(context.Background(), func(tx store.Tx) error {
		p, err := st.Projects().GetByKey(tx, key)
		if err != nil {
			return err
		}
		out, err = st.Chat().List(tx, store.ChatFilter{ProjectID: &p.ID, Ascending: true, Limit: 100000})
		return err
	}); err != nil {
		t.Fatalf("read feed of %s: %v", key, err)
	}
	return out
}

// kanb70Counts is everything an import could leave behind.
func kanb70Counts(t *testing.T, st store.Store) (projects, tasks, messages int) {
	t.Helper()
	if err := st.Read(context.Background(), func(tx store.Tx) error {
		ps, err := st.Projects().List(tx, true)
		if err != nil {
			return err
		}
		ts, err := st.Tasks().List(tx, store.TaskFilter{IncludeArchived: true, IncludeDone: true})
		if err != nil {
			return err
		}
		ms, err := st.Chat().List(tx, store.ChatFilter{Ascending: true, Limit: 100000})
		if err != nil {
			return err
		}
		projects, tasks, messages = len(ps), len(ts), len(ms)
		return nil
	}); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return
}

func stamp(tm *time.Time) string {
	if tm == nil {
		return "-"
	}
	return tm.UTC().Format(time.RFC3339Nano)
}

// cardFacts is what "the same card" means for a backup: where it is, what it
// says, and every value idle time and the history charts are computed from.
type cardFacts struct {
	Column, Title, Parent                                        string
	Version                                                      int
	Created, Updated, Entered, Started, Done, Archived, Assignee string
	Blocks                                                       string
	Acceptance                                                   string
}

func factsOf(doc *service.ExportDocument) map[string]cardFacts {
	idToKey := map[string]string{}
	var all []domain.TaskView
	for _, bp := range doc.Projects {
		for _, c := range bp.Columns {
			all = append(all, c.Tasks...)
		}
	}
	for _, at := range doc.ArchivedTasks {
		all = append(all, at.Task)
	}
	for _, tv := range all {
		idToKey[tv.ID] = tv.Key
	}
	out := map[string]cardFacts{}
	for _, tv := range all {
		f := cardFacts{
			Column: tv.ColumnName, Title: tv.Title, Version: tv.Version,
			Created: stamp(&tv.CreatedAt), Updated: stamp(&tv.UpdatedAt), Entered: stamp(&tv.ColumnEnteredAt),
			Started: stamp(tv.StartedAt), Done: stamp(tv.DoneAt), Archived: stamp(tv.ArchivedAt),
			Blocks: strings.Join(tv.Blocks, ","),
		}
		if tv.ParentID != nil {
			f.Parent = idToKey[*tv.ParentID]
		}
		if tv.Assignee != nil {
			f.Assignee = *tv.Assignee
		}
		acc, _ := json.Marshal(tv.Acceptance)
		f.Acceptance = string(acc)
		out[tv.Key] = f
	}
	return out
}

// sameStream fails with the first differing element of two history streams.
func sameStream[T any](t *testing.T, what string, got, want []T) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d entries after the round trip, want %d", what, len(got), len(want))
		return
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("%s[%d] = %+v, want %+v", what, i, got[i], want[i])
			return
		}
	}
}

type noHistory struct{}

func (noHistory) Since(string, int64, int) ([]domain.Event, error) { return nil, nil }
func (noHistory) MinID() (int64, error)                            { return 0, nil }

// webExportBody downloads /p/{key}/export through the real web handler,
// signed in with a bootstrapped admin session, exactly as the browser does.
func webExportBody(t *testing.T, st store.Store, svc service.Service, key string) []byte {
	t.Helper()
	ctx := context.Background()
	const secret = "kbn_kanb70admin0xx00xx00xx00xx00xx00xx00x"
	mgr := auth.NewManagerFromStore(st, nil, "", true, time.Now)
	if _, err := mgr.Bootstrap(ctx, secret); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	sess, err := mgr.CreateSession(ctx, secret)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	tpl, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New: %v", err)
	}
	w := web.New(web.Deps{
		Service: svc, Auth: mgr, Bus: events.New(noHistory{}, 4), Templates: tpl, BaseURL: "http://127.0.0.1:0",
	})
	csrf := mgr.CSRFTokenForSession(sess.ID)
	form := url.Values{"csrf_token": {csrf}}
	req := httptest.NewRequest("POST", "/p/"+key+"/export", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "kanban_session", Value: sess.ID})
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: csrf})
	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("web export: status %d, body %.300s", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

// TestWebExport_RestoresThroughKanbanImport is the first acceptance
// criterion of KANB-70: a project downloaded from the web Export button —
// with more done cards than board_get returns, notes, a lifecycle journal,
// an archived card and a feed — goes through `kanban import` and comes back
// as the same project. Before, the download carried a "truncated" field the
// importer refused and lacked notes, the journal and archived cards.
func TestWebExport_RestoresThroughKanbanImport(t *testing.T) {
	ctx := context.Background()
	dir1, dir2 := t.TempDir(), t.TempDir()
	st, svc := kanb70Open(t, dir1)

	kanb70Project(t, svc, "WEB", nil)
	titles := make([]string, domain.MaxDoneLimit+5)
	for i := range titles {
		titles[i] = fmt.Sprintf("done %d", i)
	}
	kanb70Tasks(t, svc, "WEB", "Done", titles...)
	live := kanb70Tasks(t, svc, "WEB", "Backlog", "live card", "archived card")
	kanb70Patch(t, svc, service.TaskPatch{Key: live[0].Key, Note: "first note"})
	kanb70Patch(t, svc, service.TaskPatch{Key: live[0].Key, Column: "Doing", IfVersion: &live[0].Version})
	kanb70Patch(t, svc, service.TaskPatch{Key: live[0].Key, Note: "second note"})
	kanb70Patch(t, svc, service.TaskPatch{Key: live[1].Key, Note: "note on a card about to be archived"})
	if res, err := svc.TaskRemove(ctx, kanb70Admin, service.TaskRemoveInput{Items: []service.RemoveItem{{Key: live[1].Key}}}); err != nil || !res.Items[0].OK {
		t.Fatalf("TaskRemove: %v %+v", err, res)
	}
	q := kanb70Chat(t, svc, kanb70Admin, service.ChatAddInput{ProjectKey: "WEB", Body: "how is it going?"})
	kanb70Chat(t, svc, kanb70Admin, service.ChatAddInput{ProjectKey: "WEB", Body: "fine", ReplyTo: q.ID})

	body := webExportBody(t, st, svc, "WEB")
	source, err := svc.Export(ctx, kanb70Admin, service.ExportInput{ProjectKey: "WEB"})
	if err != nil {
		t.Fatalf("source Export: %v", err)
	}
	st.Close()

	file := filepath.Join(t.TempDir(), "web-export.json")
	if err := os.WriteFile(file, body, 0o644); err != nil {
		t.Fatalf("write download: %v", err)
	}
	if err := runImport([]string{"--data", dir2, "--in", file}); err != nil {
		t.Fatalf("kanban import of the web download: %v", err)
	}

	st2, svc2 := kanb70Open(t, dir2)
	defer st2.Close()
	restored, err := svc2.Export(ctx, kanb70Admin, service.ExportInput{ProjectKey: "WEB"})
	if err != nil {
		t.Fatalf("restored Export: %v", err)
	}

	want, got := factsOf(source), factsOf(restored)
	if len(got) != len(want) || len(want) != domain.MaxDoneLimit+7 {
		t.Fatalf("restored %d cards, source had %d (want %d)", len(got), len(want), domain.MaxDoneLimit+7)
	}
	for key, w := range want {
		if g := got[key]; g != w {
			t.Errorf("card %s after the round trip:\n got %+v\nwant %+v", key, g, w)
		}
	}
	if len(restored.ArchivedTasks) != 1 || restored.ArchivedTasks[0].Task.Key != live[1].Key {
		t.Errorf("archived cards after the round trip = %+v, want only %s", restored.ArchivedTasks, live[1].Key)
	}
	sameStream(t, "notes", restored.Notes, source.Notes)
	sameStream(t, "journal", restored.Journal, source.Journal)
	if len(source.Journal) == 0 || len(source.Notes) != 3 {
		t.Fatalf("fixture too thin: journal %d, notes %d", len(source.Journal), len(source.Notes))
	}
	clearSeq := func(ms []service.ExportChatMessage) []service.ExportChatMessage {
		out := append([]service.ExportChatMessage(nil), ms...)
		for i := range out {
			out[i].Seq = 0
		}
		return out
	}
	sameStream(t, "chat", clearSeq(restored.ChatMessages), clearSeq(source.ChatMessages))
	if restored.NextTaskSeq["WEB"] != source.NextTaskSeq["WEB"] {
		t.Errorf("next task number = %d, want %d", restored.NextTaskSeq["WEB"], source.NextTaskSeq["WEB"])
	}
}

// TestExportImport_CommandAndReplySurviveInFeedOrder is the second criterion:
// an addressed command and the reply to it keep every protocol field and
// their place in the feed, the restored command is still accepted by its
// executor, an already-accepted command stays accepted, and a sender's
// idempotency key still recognises its own retry.
func TestExportImport_CommandAndReplySurviveInFeedOrder(t *testing.T) {
	ctx := context.Background()
	dir1, dir2 := t.TempDir(), t.TempDir()
	boss := service.Actor{TokenID: "tok-boss", Name: "boss", Scopes: domain.Scopes{domain.ScopeAdmin, domain.ScopeWrite, domain.ScopeRead}}
	worker := service.Actor{TokenID: "tok-worker", Name: "worker", Scopes: domain.Scopes{domain.ScopeWrite, domain.ScopeRead}}

	st, svc := kanb70Open(t, dir1)
	kanb70Token(t, st, boss.TokenID, boss.Name, boss.Scopes...)
	kanb70Token(t, st, worker.TokenID, worker.Name, worker.Scopes...)
	kanb70Project(t, svc, "CMD", nil)

	kanb70Chat(t, svc, boss, service.ChatAddInput{ProjectKey: "CMD", Body: "kick-off"})
	cmd1 := kanb70Chat(t, svc, boss, service.ChatAddInput{
		ProjectKey: "CMD", Kind: "command", Recipient: worker.TokenID, Body: "build the thing", IdempotencyKey: "cmd-1",
	})
	kanb70Chat(t, svc, worker, service.ChatAddInput{ProjectKey: "CMD", Body: "on it", ReplyTo: cmd1.ID, IdempotencyKey: "ack-1"})
	cmd2 := kanb70Chat(t, svc, boss, service.ChatAddInput{
		ProjectKey: "CMD", Kind: "command", Recipient: worker.TokenID, Body: "and the other thing", IdempotencyKey: "cmd-2",
	})
	batch2 := []service.NewTask{{ProjectKey: "CMD", Title: "the other thing", Type: domain.TypeTask}}
	if _, err := svc.TaskCreate(ctx, worker, service.TaskCreateInput{SourceMessage: cmd2.ID, Tasks: batch2}); err != nil {
		t.Fatalf("accept cmd2 on the source: %v", err)
	}
	kanb70Chat(t, svc, boss, service.ChatAddInput{ProjectKey: "CMD", Body: "wrap-up"})
	sourceFeed := kanb70Feed(t, st, "CMD")
	st.Close()

	file := filepath.Join(t.TempDir(), "export.json")
	if err := runExport([]string{"--data", dir1, "--out", file}); err != nil {
		t.Fatalf("runExport: %v", err)
	}
	st2, svc2 := kanb70Open(t, dir2)
	defer st2.Close()
	kanb70Token(t, st2, boss.TokenID, boss.Name, boss.Scopes...)
	kanb70Token(t, st2, worker.TokenID, worker.Name, worker.Scopes...)
	if err := runImport([]string{"--data", dir2, "--in", file}); err != nil {
		t.Fatalf("runImport: %v", err)
	}

	restoredFeed := kanb70Feed(t, st2, "CMD")
	norm := func(ms []domain.ChatMessage) []domain.ChatMessage {
		out := append([]domain.ChatMessage(nil), ms...)
		for i := range out {
			out[i].Seq, out[i].ProjectID = 0, ""
			out[i].CreatedAt = out[i].CreatedAt.UTC()
		}
		return out
	}
	sameStream(t, "feed", norm(restoredFeed), norm(sourceFeed))
	if t.Failed() {
		return
	}
	if restoredFeed[1].Kind != domain.MessageCommand || restoredFeed[1].ResolvedExecutor != worker.TokenID ||
		restoredFeed[2].ReplyToID != cmd1.ID || restoredFeed[2].AuthorTokenID != worker.TokenID {
		t.Fatalf("fixture lost its protocol fields on the source already: %+v", restoredFeed[1:3])
	}

	res, err := svc2.TaskCreate(ctx, worker, service.TaskCreateInput{
		SourceMessage: cmd1.ID, Tasks: []service.NewTask{{ProjectKey: "CMD", Title: "the thing", Type: domain.TypeTask}},
	})
	if err != nil {
		t.Fatalf("the executor cannot accept the restored command: %v", err)
	}
	if res.AlreadyAccepted {
		t.Fatalf("an unaccepted command came back as already accepted")
	}
	res, err = svc2.TaskCreate(ctx, worker, service.TaskCreateInput{SourceMessage: cmd2.ID, Tasks: batch2})
	if err != nil {
		t.Fatalf("replaying the accepted command: %v", err)
	}
	if !res.AlreadyAccepted {
		t.Fatalf("a command accepted on the source could be accepted a second time after the restore")
	}

	again, err := svc2.ChatAdd(ctx, boss, service.ChatAddInput{
		ProjectKey: "CMD", Kind: "command", Recipient: worker.TokenID, Body: "build the thing", IdempotencyKey: "cmd-1",
	})
	if err != nil {
		t.Fatalf("retrying cmd-1 after the restore: %v", err)
	}
	if again.ID != cmd1.ID {
		t.Fatalf("the sender's retry of cmd-1 posted a new message %s instead of returning %s", again.ID, cmd1.ID)
	}
}

// TestImport_RefusesAnExistingProjectAndWritesNothing is the third
// criterion: a file with projects [AAA, BBB] against a board that already has
// BBB is refused up front, names BBB, and leaves the board exactly as it was
// — AAA included, which the old per-project import created before failing.
func TestImport_RefusesAnExistingProjectAndWritesNothing(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	st, svc := kanb70Open(t, dir1)
	for _, key := range []string{"AAA", "BBB"} {
		kanb70Project(t, svc, key, nil)
		kanb70Tasks(t, svc, key, "Backlog", key+" card")
		kanb70Chat(t, svc, kanb70Admin, service.ChatAddInput{ProjectKey: key, Body: key + " message"})
	}
	st.Close()
	file := filepath.Join(t.TempDir(), "export.json")
	if err := runExport([]string{"--data", dir1, "--out", file}); err != nil {
		t.Fatalf("runExport: %v", err)
	}

	st2, svc2 := kanb70Open(t, dir2)
	defer st2.Close()
	kanb70Project(t, svc2, "BBB", nil)
	kanb70Tasks(t, svc2, "BBB", "Backlog", "the destination's own card")
	kanb70Chat(t, svc2, kanb70Admin, service.ChatAddInput{ProjectKey: "BBB", Body: "the destination's own message"})
	p0, t0, m0 := kanb70Counts(t, st2)

	err := runImport([]string{"--data", dir2, "--in", file})
	if err == nil {
		t.Fatal("import into a board that already has BBB succeeded")
	}
	if !strings.Contains(err.Error(), "already has 1 of the export's projects: BBB (") || strings.Contains(err.Error(), "AAA (") {
		t.Errorf("the refusal must come before any write and name exactly the existing project BBB: %v", err)
	}
	if p, tk, m := kanb70Counts(t, st2); p != p0 || tk != t0 || m != m0 {
		t.Errorf("a refused import changed the board: projects %d->%d, tasks %d->%d, messages %d->%d", p0, p, t0, tk, m0, m)
	}
}

// TestImport_LateFailureRollsBackEverything pins the transaction: the file
// is valid up to its very last step (a journal row naming a card the file
// does not contain), so projects, cards, notes and messages have all been
// written when the failure hits — and none of them may survive it.
func TestImport_LateFailureRollsBackEverything(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	st, svc := kanb70Open(t, dir1)
	kanb70Project(t, svc, "LATE", nil)
	cards := kanb70Tasks(t, svc, "LATE", "Backlog", "one", "two")
	kanb70Patch(t, svc, service.TaskPatch{Key: cards[0].Key, Note: "a note"})
	kanb70Chat(t, svc, kanb70Admin, service.ChatAddInput{ProjectKey: "LATE", Body: "a message"})
	st.Close()

	file := filepath.Join(t.TempDir(), "export.json")
	if err := runExport([]string{"--data", dir1, "--out", file}); err != nil {
		t.Fatalf("runExport: %v", err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var doc exportDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc.Journal = append(doc.Journal, exportJournalEntry{
		Project: "LATE", Task: "LATE-999", Actor: "ghost", Kind: "created", TS: time.Now().UTC(),
	})
	raw, _ = json.Marshal(doc)
	if err := os.WriteFile(file, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	err = runImport([]string{"--data", dir2, "--in", file})
	if err == nil || !strings.Contains(err.Error(), "LATE-999") {
		t.Fatalf("import with a dangling journal row: err = %v, want a refusal naming LATE-999", err)
	}
	st2, _ := kanb70Open(t, dir2)
	defer st2.Close()
	if p, tk, m := kanb70Counts(t, st2); p != 0 || tk != 0 || m != 0 {
		t.Errorf("a failed import left projects=%d tasks=%d messages=%d behind", p, tk, m)
	}
}

// TestExportImport_FocusCoordinatorDatesAndVersion covers the two restore
// gaps found in review: focus and coordinator were dropped, and every card
// came back with the import instant as its dates and version 1, so idle
// time and the attention line lied on the first read. The export file's
// card dates are moved into the past before the import, so a restore that
// stamps "now" cannot pass by landing in the same millisecond.
func TestExportImport_FocusCoordinatorDatesAndVersion(t *testing.T) {
	ctx := context.Background()
	dir1, dir2 := t.TempDir(), t.TempDir()
	st, svc := kanb70Open(t, dir1)
	kanb70Token(t, st, "tok-coord", "coord", domain.ScopeWrite, domain.ScopeRead)
	kanb70Token(t, st, "tok-gone", "gone", domain.ScopeWrite, domain.ScopeRead)
	coord, gone := "tok-coord", "tok-gone"
	kanb70Project(t, svc, "FOC", &service.ProjectSettings{Coordinator: &coord})
	kanb70Project(t, svc, "GON", &service.ProjectSettings{Coordinator: &gone})

	cards := kanb70Tasks(t, svc, "FOC", "Backlog", "focus", "shipped", "edited", "archived")
	focus := true
	kanb70Patch(t, svc, service.TaskPatch{Key: cards[0].Key, Focus: &focus})
	moved := kanb70Patch(t, svc, service.TaskPatch{Key: cards[1].Key, Column: "Doing", IfVersion: &cards[1].Version})
	kanb70Patch(t, svc, service.TaskPatch{Key: cards[1].Key, Column: "Done", IfVersion: &moved.Version})
	v := cards[2].Version
	for i := 0; i < 3; i++ {
		title := fmt.Sprintf("edited %d", i)
		v = kanb70Patch(t, svc, service.TaskPatch{Key: cards[2].Key, IfVersion: &v, Title: &title}).Version
	}
	if res, err := svc.TaskRemove(ctx, kanb70Admin, service.TaskRemoveInput{Items: []service.RemoveItem{{Key: cards[3].Key}}}); err != nil || !res.Items[0].OK {
		t.Fatalf("TaskRemove: %v %+v", err, res)
	}
	st.Close()

	file := filepath.Join(t.TempDir(), "export.json")
	if err := runExport([]string{"--data", dir1, "--out", file}); err != nil {
		t.Fatalf("runExport: %v", err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := decodeExportDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	past := func(tm *time.Time, h int) {
		if tm != nil && !tm.IsZero() {
			*tm = base.Add(time.Duration(h) * time.Hour)
		}
	}
	plant := func(tv *domain.TaskView, i int) {
		past(&tv.CreatedAt, 10*i)
		past(&tv.ColumnEnteredAt, 10*i+2)
		past(tv.StartedAt, 10*i+1)
		past(tv.DoneAt, 10*i+2)
		past(tv.ArchivedAt, 10*i+4)
		past(&tv.UpdatedAt, 10*i+5)
		tv.Version += 10
	}
	for pi := range doc.Projects {
		for ci := range doc.Projects[pi].Columns {
			for ti := range doc.Projects[pi].Columns[ci].Tasks {
				plant(&doc.Projects[pi].Columns[ci].Tasks[ti], ci*10+ti)
			}
		}
	}
	for i := range doc.ArchivedTasks {
		plant(&doc.ArchivedTasks[i].Task, 90+i)
	}
	want := factsOf(&doc)

	st2, svc2 := kanb70Open(t, dir2)
	defer st2.Close()
	kanb70Token(t, st2, "tok-coord", "coord", domain.ScopeWrite, domain.ScopeRead)
	raw, _ = json.Marshal(doc)
	res, err := importBoard(ctx, svc2, kanb70Admin, raw)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	restored, err := svc2.Export(ctx, kanb70Admin, service.ExportInput{})
	if err != nil {
		t.Fatalf("restored Export: %v", err)
	}
	got := factsOf(restored)
	if len(got) != len(want) {
		t.Fatalf("restored %d cards, want %d", len(got), len(want))
	}
	for key, w := range want {
		if g := got[key]; g != w {
			t.Errorf("card %s:\n got %+v\nwant %+v", key, g, w)
		}
	}
	if w := want[cards[1].Key]; w.Started == "-" || w.Done == "-" || want[cards[3].Key].Archived == "-" {
		t.Fatalf("fixture lost its lifecycle stamps: %+v / %+v", w, want[cards[3].Key])
	}

	projects := map[string]service.BoardProject{}
	for _, bp := range restored.Projects {
		projects[bp.Key] = bp
	}
	if f := projects["FOC"]; f.FocusKey != cards[0].Key {
		t.Errorf("FOC focus = %q, want %s", f.FocusKey, cards[0].Key)
	}
	if c := projects["FOC"].Coordinator; c == nil || c.TokenID != coord {
		t.Errorf("FOC coordinator = %+v, want %s", c, coord)
	}
	if c := projects["GON"].Coordinator; c != nil {
		t.Errorf("GON coordinator = %+v, want none: its token does not exist on the destination", c)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "coordinator gone was not restored") {
		t.Errorf("warnings = %q, want one naming the coordinator of GON that could not be restored", res.Warnings)
	}
}
