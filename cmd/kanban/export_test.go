package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// seedBoardWithProgressAndChat creates a project with two tasks and a
// specific progress / chat history, then writes each row through the
// store directly so the test owns every CreatedAt. The store is the only
// layer that honours a caller-supplied timestamp on these tables —
// service.ProgressSet / service.ChatAdd stamp now() — which is exactly
// the rule KANB-29 keeps in place after import.
func seedBoardWithProgressAndChat(t *testing.T, dataDir string) (exportedProgress []exportProgressMark, exportedChat []exportChatMessage) {
	t.Helper()
	ctx := context.Background()
	st, err := openStore(ctx, dataDir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer st.Close()

	svc := service.New(st, nil)
	actor := service.Actor{
		Name:   "tester",
		Scopes: domain.Scopes{domain.ScopeAdmin, domain.ScopeWrite, domain.ScopeRead},
	}

	// Create the project: a fixed, deterministic shape so the test does
	// not depend on the demo board's content.
	strictDone := true
	enforce := true
	desc := "round-trip"
	if _, err := svc.ProjectUpsert(ctx, actor, service.ProjectUpsertInput{
		Mode:        service.UpsertCreate,
		Key:         "RKND",
		Name:        "Round-trip kind",
		Description: &desc,
		Columns: []service.ColumnSpec{
			{Name: "Backlog", Kind: domain.KindBacklog},
			{Name: "Doing", Kind: domain.KindActive, WIPLimit: intPtrLocal(3)},
			{Name: "Done", Kind: domain.KindDone},
		},
		Settings: &service.ProjectSettings{
			StrictDone:          &strictDone,
			EnforceDependencies: &enforce,
		},
	}); err != nil {
		t.Fatalf("ProjectUpsert: %v", err)
	}

	// Two tasks; deterministic keys.
	created, err := svc.TaskCreate(ctx, actor, service.TaskCreateInput{
		Tasks: []service.NewTask{
			{ProjectKey: "RKND", Column: "Backlog", Title: "first", Type: domain.TypeTask, Acceptance: []string{"written"}},
			{ProjectKey: "RKND", Column: "Backlog", Title: "second", Type: domain.TypeTask},
		},
	})
	if err != nil {
		t.Fatalf("TaskCreate: %v", err)
	}
	if len(created.Tasks) != 2 {
		t.Fatalf("created %d tasks, want 2", len(created.Tasks))
	}
	keys := []string{created.Tasks[0].Key, created.Tasks[1].Key}

	// Three progress marks with hand-picked timestamps so we can assert
	// them byte-for-byte after the round-trip. The store is the only way
	// to land a non-now() CreatedAt — exactly what import must also do.
	task1ID, task2ID := created.Tasks[0].ID, created.Tasks[1].ID
	projID := created.Tasks[0].ProjectID
	exportedProgress = []exportProgressMark{
		{
			ID:        "pm-1",
			Project:   "RKND",
			Assessor:  "alex",
			Percent:   20,
			CreatedAt: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
		},
		{
			// The only mark carrying a forecast. Owner decision 6: the
			// forecast is an absolute date-time, versioned, and visible on
			// the chart — so losing it on transfer is a data loss, not a
			// cosmetic one. The other two marks deliberately carry none, so
			// the same test also pins that an ABSENT forecast imports as
			// absent rather than as a zero date.
			ID:        "pm-2",
			Project:   "RKND",
			Task:      keys[0],
			Assessor:  "alex",
			Percent:   40,
			ETA:       ptrTime(time.Date(2026, 8, 15, 18, 0, 0, 0, time.UTC)),
			CreatedAt: time.Date(2026, 6, 1, 9, 30, 0, 0, time.UTC),
		},
		{
			ID:        "pm-3",
			Project:   "RKND",
			Task:      keys[1],
			Assessor:  "bea",
			Percent:   75,
			CreatedAt: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		},
	}
	exportedChat = []exportChatMessage{
		{
			ID:        "chat-1",
			Project:   "RKND",
			Author:    "alex",
			Body:      "started the first task",
			CreatedAt: time.Date(2026, 6, 1, 9, 5, 0, 0, time.UTC),
		},
		{
			ID:        "chat-2",
			Project:   "RKND",
			Author:    "bea",
			Body:      "second task is more nuanced than expected",
			CreatedAt: time.Date(2026, 6, 1, 9, 50, 0, 0, time.UTC),
		},
	}

	if err := st.Write(ctx, func(tx store.Tx) error {
		for _, m := range exportedProgress {
			var tid *string
			if m.Task != "" {
				if m.Task == keys[0] {
					tid = &task1ID
				} else {
					tid = &task2ID
				}
			}
			mark := &domain.ProgressMark{
				ID: m.ID, ProjectID: projID, TaskID: tid, Assessor: m.Assessor, Percent: m.Percent, ETA: m.ETA, CreatedAt: m.CreatedAt,
			}
			if err := st.Progress().Add(tx, mark); err != nil {
				return err
			}
		}
		for _, m := range exportedChat {
			msg := &domain.ChatMessage{
				ID: m.ID, ProjectID: projID, Author: m.Author, Body: m.Body, CreatedAt: m.CreatedAt,
			}
			if err := st.Chat().Add(tx, msg); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed dated rows: %v", err)
	}
	return exportedProgress, exportedChat
}

func intPtrLocal(v int) *int { return &v }

// fetchProgressAndChat reads the destination board's append-only tables
// directly so the round-trip test can compare timestamps against the
// seeded values without going through a service whose summary view would
// aggregate or limit them.
func fetchProgressAndChat(t *testing.T, dataDir string) (progress []domain.ProgressMark, chat []domain.ChatMessage) {
	t.Helper()
	ctx := context.Background()
	st, err := openStore(ctx, dataDir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer st.Close()
	if err := st.Read(ctx, func(tx store.Tx) error {
		p, err := st.Projects().GetByKey(tx, "RKND")
		if err != nil {
			return err
		}
		marks, err := st.Progress().History(tx, p.ID, nil)
		if err != nil {
			return err
		}
		// Task-scoped marks too — we don't yet know the task ids on the
		// destination, so re-read through BoardGet to fetch their keys.
		board, err := service.New(st, nil).BoardGet(ctx, service.Actor{
			Name:   "tester",
			Scopes: domain.Scopes{domain.ScopeAdmin, domain.ScopeWrite, domain.ScopeRead},
		}, service.BoardGetInput{ProjectKey: "RKND", View: service.ViewTasks})
		if err != nil {
			return err
		}
		for _, bp := range board.Projects {
			for _, col := range bp.Columns {
				for _, tv := range col.Tasks {
					task, err := st.Tasks().GetByKey(tx, tv.Key)
					if err != nil {
						return err
					}
					tm, err := st.Progress().History(tx, p.ID, &task.ID)
					if err != nil {
						return err
					}
					marks = append(marks, tm...)
				}
			}
		}
		progress = marks

		msgs, err := st.Chat().List(tx, store.ChatFilter{ProjectID: &p.ID, Limit: 100})
		if err != nil {
			return err
		}
		chat = msgs
		return nil
	}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	return progress, chat
}

// TestExportImport_RoundTripWithProgressAndChat is KANB-29's headline
// guarantee: a board that has progress marks and chat messages round-trips
// through export -> import with the values intact AND the original
// timestamps preserved, so the post-import chart looks like the source
// chart and not a vertical wall.
func TestExportImport_RoundTripWithProgressAndChat(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	wantProgress, wantChat := seedBoardWithProgressAndChat(t, dir1)

	exportFile := filepath.Join(t.TempDir(), "export.json")
	if err := runExport([]string{"--data", dir1, "--out", exportFile}); err != nil {
		t.Fatalf("runExport: %v", err)
	}

	raw, err := os.ReadFile(exportFile)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	var doc exportDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse export: %v", err)
	}
	if got := len(doc.ProgressMarks); got != len(wantProgress) {
		t.Errorf("export progress_marks = %d, want %d", got, len(wantProgress))
	}
	if got := len(doc.ChatMessages); got != len(wantChat) {
		t.Errorf("export chat_messages = %d, want %d", got, len(wantChat))
	}

	if err := runImport([]string{"--data", dir2, "--in", exportFile}); err != nil {
		t.Fatalf("runImport: %v", err)
	}

	gotProgress, gotChat := fetchProgressAndChat(t, dir2)

	if len(gotProgress) != len(wantProgress) {
		t.Fatalf("imported progress count = %d, want %d", len(gotProgress), len(wantProgress))
	}
	if len(gotChat) != len(wantChat) {
		t.Fatalf("imported chat count = %d, want %d", len(gotChat), len(wantChat))
	}

	// Compare every mark's assessor / percent / created_at — not just
	// the counts. The point of the round-trip is to be byte-precise on
	// the values the chart cares about.
	progressByID := make(map[string]domain.ProgressMark, len(gotProgress))
	for _, m := range gotProgress {
		progressByID[m.ID] = m
	}
	for _, want := range wantProgress {
		got, ok := progressByID[want.ID]
		if !ok {
			t.Errorf("missing imported mark %s", want.ID)
			continue
		}
		if got.Assessor != want.Assessor {
			t.Errorf("mark %s assessor = %q, want %q", want.ID, got.Assessor, want.Assessor)
		}
		if got.Percent != want.Percent {
			t.Errorf("mark %s percent = %d, want %d", want.ID, got.Percent, want.Percent)
		}
		if !got.CreatedAt.Equal(want.CreatedAt) {
			t.Errorf("mark %s created_at = %s, want %s", want.ID, got.CreatedAt, want.CreatedAt)
		}
		// KANB-29 criterion 2 names the forecast alongside author, percent
		// and time. Both directions matter: a forecast that vanishes is a
		// loss, and a forecast conjured out of nil is a lie the chart would
		// happily draw.
		switch {
		case want.ETA == nil && got.ETA != nil:
			t.Errorf("mark %s eta = %s, want none — an absent forecast must import as absent, not as a date", want.ID, got.ETA)
		case want.ETA != nil && got.ETA == nil:
			t.Errorf("mark %s eta is missing, want %s", want.ID, want.ETA)
		case want.ETA != nil && got.ETA != nil && !got.ETA.Equal(*want.ETA):
			t.Errorf("mark %s eta = %s, want %s", want.ID, got.ETA, want.ETA)
		}
	}

	// Same for chat: author / body / timestamp must survive.
	chatByID := make(map[string]domain.ChatMessage, len(gotChat))
	for _, m := range gotChat {
		chatByID[m.ID] = m
	}
	for _, want := range wantChat {
		got, ok := chatByID[want.ID]
		if !ok {
			t.Errorf("missing imported chat %s", want.ID)
			continue
		}
		if got.Author != want.Author {
			t.Errorf("chat %s author = %q, want %q", want.ID, got.Author, want.Author)
		}
		if got.Body != want.Body {
			t.Errorf("chat %s body = %q, want %q", want.ID, got.Body, want.Body)
		}
		if !got.CreatedAt.Equal(want.CreatedAt) {
			t.Errorf("chat %s created_at = %s, want %s", want.ID, got.CreatedAt, want.CreatedAt)
		}
	}
}

// TestExportImport_BackwardCompatibleOldShape makes sure an export file
// that only carries the legacy service.Board shape (no progress_marks,
// no chat_messages) still imports cleanly: the import side treats the
// extra slices as optional and the round-trip of just the board part
// must keep working.
func TestExportImport_BackwardCompatibleOldShape(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	if err := runDemo([]string{"--data", dir1}); err != nil {
		t.Fatalf("runDemo: %v", err)
	}

	// Mimic an export from a build that pre-dates KANB-29: only the
	// `projects` field, no progress_marks / chat_messages at all.
	svc := service.New(mustOpenStore(t, dir1), nil)
	actor := service.Actor{
		Name:   "tester",
		Scopes: domain.Scopes{domain.ScopeAdmin, domain.ScopeWrite, domain.ScopeRead},
	}
	board, err := svc.BoardGet(context.Background(), actor, service.BoardGetInput{
		View: service.ViewTasks, DoneLimit: domain.MaxDoneLimit,
	})
	if err != nil {
		t.Fatalf("BoardGet: %v", err)
	}
	raw, err := json.Marshal(board)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	oldFile := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(oldFile, raw, 0o644); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	if err := runImport([]string{"--data", dir2, "--in", oldFile}); err != nil {
		t.Fatalf("runImport legacy: %v", err)
	}
}

// TestImportRejectsDanglingHistoryBeforeCreatingProjects pins the preflight
// that prevents a malformed history row from stranding a half-imported board.
func TestImportRejectsDanglingHistoryBeforeCreatingProjects(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	seedBoardWithProgressAndChat(t, source)
	exportFile := filepath.Join(t.TempDir(), "export.json")
	if err := runExport([]string{"--data", source, "--out", exportFile}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exportFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc exportDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc.ProgressMarks = append(doc.ProgressMarks, exportProgressMark{ID: "bad", Project: "NOPE"})
	broken, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	brokenFile := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(brokenFile, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runImport([]string{"--data", destination, "--in", brokenFile}); err == nil {
		t.Fatal("dangling mark import succeeded")
	}
	st := mustOpenStore(t, destination)
	actor := service.Actor{Name: "test", Scopes: domain.Scopes{domain.ScopeRead}}
	board, err := service.New(st, nil).BoardGet(context.Background(), actor, service.BoardGetInput{View: service.ViewSummary})
	if err != nil {
		t.Fatal(err)
	}
	if len(board.Projects) != 0 {
		t.Fatalf("projects after rejected import = %d, want 0", len(board.Projects))
	}
}

// mustOpenStore is a thin helper that wraps openStore and fails the test
// on error — used in tests that need a store handle inline without the
// full deferred Close dance.
func mustOpenStore(t *testing.T, dir string) store.Store {
	t.Helper()
	st, err := openStore(context.Background(), dir)
	if err != nil {
		t.Fatalf("openStore %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// ptrTime is the one-liner the forecast fixture needs: domain.ProgressMark
// models "no forecast" as a nil *time.Time, so the round-trip test has to
// be able to write both a real forecast and the absence of one.
func ptrTime(t time.Time) *time.Time { return &t }

// floatPtrLocal is the float64 counterpart of intPtrLocal, needed for the
// journal round-trip fixture's estimate-change entry.
func floatPtrLocal(v float64) *float64 { return &v }

// canonicalizeJournal turns raw store.TaskHistoryEntry rows into the same
// re-keyed shape exportJournalEntry uses (task KEYS, column NAMES), via
// caller-supplied lookups. It is a second, independent implementation of
// the re-keying collectJournal does — written directly against the store
// rather than reusing collectJournal — so the round-trip test is not just
// checking collectJournal's output against itself.
func canonicalizeJournal(entries []store.TaskHistoryEntry, projectKey string, taskKeyOf func(string) string, colNameOf func(*string) string) []exportJournalEntry {
	out := make([]exportJournalEntry, 0, len(entries))
	for _, e := range entries {
		je := exportJournalEntry{
			Project:       projectKey,
			Actor:         e.Actor,
			Kind:          string(e.Kind),
			TS:            e.TS,
			Reconstructed: e.Reconstructed,
			Archived:      e.Archived,
			FromColumn:    colNameOf(e.FromColumn),
			ToColumn:      colNameOf(e.ToColumn),
			OldEstimate:   e.OldEstimate,
			NewEstimate:   e.NewEstimate,
		}
		if e.TaskID != nil {
			je.Task = taskKeyOf(*e.TaskID)
		}
		if e.FromKind != nil {
			je.FromKind = string(*e.FromKind)
		}
		if e.ToKind != nil {
			je.ToKind = string(*e.ToKind)
		}
		if e.OldParent != nil {
			je.OldParent = taskKeyOf(*e.OldParent)
		}
		if e.NewParent != nil {
			je.NewParent = taskKeyOf(*e.NewParent)
		}
		out = append(out, je)
	}
	return out
}

// journalSortKey orders journal entries for comparison: kind + timestamp is
// unique across every fixture entry this test writes (every timestamp
// below is distinct on purpose), so sorting both sides by it lines up the
// pairs a field-by-field comparison needs.
func journalSortKey(e exportJournalEntry) string {
	return e.Kind + "@" + e.TS.UTC().Format(time.RFC3339Nano) + "@" + e.Task
}

func sortJournal(entries []exportJournalEntry) {
	sort.Slice(entries, func(i, j int) bool { return journalSortKey(entries[i]) < journalSortKey(entries[j]) })
}

func floatPtrEqualLocal(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func boolPtrEqualLocal(a, b *bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// assertJournalEntriesEqual compares two aligned (same length, same sort
// order) journal slices field by field, the way the progress/chat
// comparisons above do -- not by count, per KANB-32's own warning.
func assertJournalEntriesEqual(t *testing.T, label string, got, want []exportJournalEntry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: entry count = %d, want %d\ngot:  %+v\nwant: %+v", label, len(got), len(want), got, want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Project != w.Project {
			t.Errorf("%s[%d] project = %q, want %q", label, i, g.Project, w.Project)
		}
		if g.Task != w.Task {
			t.Errorf("%s[%d] (%s) task = %q, want %q", label, i, w.Kind, g.Task, w.Task)
		}
		if g.Actor != w.Actor {
			t.Errorf("%s[%d] (%s) actor = %q, want %q", label, i, w.Kind, g.Actor, w.Actor)
		}
		if g.Kind != w.Kind {
			t.Errorf("%s[%d] kind = %q, want %q", label, i, g.Kind, w.Kind)
		}
		if !g.TS.Equal(w.TS) {
			t.Errorf("%s[%d] (%s) ts = %s, want %s", label, i, w.Kind, g.TS, w.TS)
		}
		if g.Reconstructed != w.Reconstructed {
			t.Errorf("%s[%d] (%s) reconstructed = %v, want %v", label, i, w.Kind, g.Reconstructed, w.Reconstructed)
		}
		if !boolPtrEqualLocal(g.Archived, w.Archived) {
			t.Errorf("%s[%d] (%s) archived = %v, want %v", label, i, w.Kind, g.Archived, w.Archived)
		}
		if g.FromColumn != w.FromColumn {
			t.Errorf("%s[%d] (%s) from_column = %q, want %q", label, i, w.Kind, g.FromColumn, w.FromColumn)
		}
		if g.FromKind != w.FromKind {
			t.Errorf("%s[%d] (%s) from_kind = %q, want %q", label, i, w.Kind, g.FromKind, w.FromKind)
		}
		if g.ToColumn != w.ToColumn {
			t.Errorf("%s[%d] (%s) to_column = %q, want %q", label, i, w.Kind, g.ToColumn, w.ToColumn)
		}
		if g.ToKind != w.ToKind {
			t.Errorf("%s[%d] (%s) to_kind = %q, want %q", label, i, w.Kind, g.ToKind, w.ToKind)
		}
		if !floatPtrEqualLocal(g.OldEstimate, w.OldEstimate) {
			t.Errorf("%s[%d] (%s) old_estimate = %v, want %v", label, i, w.Kind, g.OldEstimate, w.OldEstimate)
		}
		if !floatPtrEqualLocal(g.NewEstimate, w.NewEstimate) {
			t.Errorf("%s[%d] (%s) new_estimate = %v, want %v", label, i, w.Kind, g.NewEstimate, w.NewEstimate)
		}
		if g.OldParent != w.OldParent {
			t.Errorf("%s[%d] (%s) old_parent = %q, want %q", label, i, w.Kind, g.OldParent, w.OldParent)
		}
		if g.NewParent != w.NewParent {
			t.Errorf("%s[%d] (%s) new_parent = %q, want %q", label, i, w.Kind, g.NewParent, w.NewParent)
		}
	}
}

// TestExportImport_RoundTripWithJournal is KANB-32's headline guarantee,
// mirroring TestExportImport_RoundTripWithProgressAndChat for the task
// lifecycle journal (KANB-30): every kind of journal entry this fixture
// writes round-trips through export -> import with its fields AND its
// ORIGINAL timestamp intact, field by field rather than by count, and the
// journal's origin instant (history_starts_at) travels with it instead of
// becoming the moment of import.
func TestExportImport_RoundTripWithJournal(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	tMoved := base.Add(1 * time.Minute)
	tEstimate := base.Add(2 * time.Minute)
	tParent := base.Add(3 * time.Minute)
	tArchived := base.Add(4 * time.Minute)
	tRestored := base.Add(5 * time.Minute)
	tColumnKind := base.Add(6 * time.Minute)

	var (
		projID             string
		task1ID, task2ID   string
		task1Key, task2Key string
		backlogID, doingID string
		sourceEntries      []store.TaskHistoryEntry
		sourceOrigin       time.Time
	)
	func() {
		st, err := openStore(ctx, dir1)
		if err != nil {
			t.Fatalf("openStore: %v", err)
		}
		defer st.Close()
		svc := service.New(st, nil)
		actor := service.Actor{
			Name:   "tester",
			Scopes: domain.Scopes{domain.ScopeAdmin, domain.ScopeWrite, domain.ScopeRead},
		}

		if _, err := svc.ProjectUpsert(ctx, actor, service.ProjectUpsertInput{
			Mode: service.UpsertCreate, Key: "RKJR", Name: "Journal round-trip",
			Columns: []service.ColumnSpec{
				{Name: "Backlog", Kind: domain.KindBacklog},
				{Name: "Doing", Kind: domain.KindActive, WIPLimit: intPtrLocal(5)},
				{Name: "Done", Kind: domain.KindDone},
			},
		}); err != nil {
			t.Fatalf("ProjectUpsert: %v", err)
		}

		created, err := svc.TaskCreate(ctx, actor, service.TaskCreateInput{
			Tasks: []service.NewTask{
				{ProjectKey: "RKJR", Column: "Backlog", Title: "parent", Type: domain.TypeTask, Estimate: floatPtrLocal(3)},
				{ProjectKey: "RKJR", Column: "Backlog", Title: "child", Type: domain.TypeTask},
			},
		})
		if err != nil {
			t.Fatalf("TaskCreate: %v", err)
		}
		task1ID, task1Key = created.Tasks[0].ID, created.Tasks[0].Key
		task2ID, task2Key = created.Tasks[1].ID, created.Tasks[1].Key
		projID = created.Tasks[0].ProjectID

		activeKind := domain.KindActive
		doneKind := domain.KindDone
		backlogKind := domain.KindBacklog
		trueVal, falseVal := true, false
		est3, est5 := 3.0, 5.0

		if err := st.Write(ctx, func(tx store.Tx) error {
			backlog, err := st.Columns().GetByName(tx, projID, "Backlog")
			if err != nil {
				return err
			}
			backlogID = backlog.ID
			doing, err := st.Columns().GetByName(tx, projID, "Doing")
			if err != nil {
				return err
			}
			doingID = doing.ID

			// One hand-written entry of every kind that carries a value,
			// at strictly increasing, hand-picked timestamps. Direct store
			// writes are the only way to control TS -- the mutation paths
			// that normally record these stamp tx.Now() -- the same
			// technique the progress/chat fixtures above use.
			fixtures := []*store.TaskHistoryEntry{
				{
					TS: tMoved, Actor: "tester", ProjectID: projID, TaskID: &task1ID,
					Kind:       store.HistoryMoved,
					FromColumn: &backlogID, FromKind: &backlogKind,
					ToColumn: &doingID, ToKind: &activeKind,
				},
				{
					TS: tEstimate, Actor: "tester", ProjectID: projID, TaskID: &task1ID,
					Kind: store.HistoryEstimate, OldEstimate: &est3, NewEstimate: &est5,
				},
				{
					TS: tParent, Actor: "tester", ProjectID: projID, TaskID: &task2ID,
					Kind: store.HistoryParent, NewParent: &task1ID,
				},
				{
					TS: tArchived, Actor: "tester", ProjectID: projID, TaskID: &task1ID,
					Kind: store.HistoryArchived, Archived: &trueVal,
				},
				{
					TS: tRestored, Actor: "tester", ProjectID: projID, TaskID: &task1ID,
					Kind: store.HistoryRestored, Archived: &falseVal,
				},
				{
					TS: tColumnKind, Actor: "tester", ProjectID: projID,
					Kind:       store.HistoryColumnKind,
					FromColumn: &doingID, FromKind: &activeKind,
					ToColumn: &doingID, ToKind: &doneKind,
				},
			}
			for _, e := range fixtures {
				if err := st.TaskHistory().Append(tx, e); err != nil {
					return fmt.Errorf("append fixture %s: %w", e.Kind, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("seed journal: %v", err)
		}

		if err := st.Read(ctx, func(tx store.Tx) error {
			entries, err := st.TaskHistory().ListByProject(tx, projID)
			if err != nil {
				return err
			}
			sourceEntries = entries
			origin, err := st.TaskHistory().Origin(tx)
			if err != nil {
				return err
			}
			sourceOrigin = origin
			return nil
		}); err != nil {
			t.Fatalf("read back source journal: %v", err)
		}
	}()

	// 8 entries: the 2 auto-recorded 'created' rows (one per task, an
	// unavoidable side effect of TaskCreate) plus the 6 fixtures above.
	if len(sourceEntries) != 8 {
		t.Fatalf("seeded journal has %d entries, want 8: %+v", len(sourceEntries), sourceEntries)
	}

	sourceTaskKeyOf := func(id string) string {
		switch id {
		case task1ID:
			return task1Key
		case task2ID:
			return task2Key
		default:
			return ""
		}
	}
	sourceColNameOf := func(id *string) string {
		if id == nil {
			return ""
		}
		switch *id {
		case backlogID:
			return "Backlog"
		case doingID:
			return "Doing"
		default:
			return ""
		}
	}
	want := canonicalizeJournal(sourceEntries, "RKJR", sourceTaskKeyOf, sourceColNameOf)
	sortJournal(want)

	// --- export ---
	exportFile := filepath.Join(t.TempDir(), "export.json")
	if err := runExport([]string{"--data", dir1, "--out", exportFile}); err != nil {
		t.Fatalf("runExport: %v", err)
	}
	raw, err := os.ReadFile(exportFile)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	var doc exportDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse export: %v", err)
	}
	if !doc.HistoryStartsAt.Equal(sourceOrigin) {
		t.Errorf("export history_starts_at = %s, want %s (the source's own origin)", doc.HistoryStartsAt, sourceOrigin)
	}
	gotExported := append([]exportJournalEntry(nil), doc.Journal...)
	sortJournal(gotExported)
	assertJournalEntriesEqual(t, "export", gotExported, want)

	// --- import ---
	if err := runImport([]string{"--data", dir2, "--in", exportFile}); err != nil {
		t.Fatalf("runImport: %v", err)
	}

	var (
		gotImported    []exportJournalEntry
		importedOrigin time.Time
	)
	st2, err := openStore(ctx, dir2)
	if err != nil {
		t.Fatalf("openStore dir2: %v", err)
	}
	defer st2.Close()
	if err := st2.Read(ctx, func(tx store.Tx) error {
		p, err := st2.Projects().GetByKey(tx, "RKJR")
		if err != nil {
			return err
		}
		t1, err := st2.Tasks().GetByKey(tx, task1Key)
		if err != nil {
			return err
		}
		t2, err := st2.Tasks().GetByKey(tx, task2Key)
		if err != nil {
			return err
		}
		backlog, err := st2.Columns().GetByName(tx, p.ID, "Backlog")
		if err != nil {
			return err
		}
		doing, err := st2.Columns().GetByName(tx, p.ID, "Doing")
		if err != nil {
			return err
		}
		destTaskKeyOf := func(id string) string {
			switch id {
			case t1.ID:
				return task1Key
			case t2.ID:
				return task2Key
			default:
				return ""
			}
		}
		destColNameOf := func(id *string) string {
			if id == nil {
				return ""
			}
			switch *id {
			case backlog.ID:
				return "Backlog"
			case doing.ID:
				return "Doing"
			default:
				return ""
			}
		}
		entries, err := st2.TaskHistory().ListByProject(tx, p.ID)
		if err != nil {
			return err
		}
		gotImported = canonicalizeJournal(entries, "RKJR", destTaskKeyOf, destColNameOf)
		origin, err := st2.TaskHistory().Origin(tx)
		if err != nil {
			return err
		}
		importedOrigin = origin
		return nil
	}); err != nil {
		t.Fatalf("read imported journal: %v", err)
	}

	sortJournal(gotImported)
	assertJournalEntriesEqual(t, "import", gotImported, want)

	// The whole point of KANB-32's history_starts_at: the destination must
	// NOT report that its journal became trustworthy on import day. It
	// must carry the source's own origin, unchanged.
	if !importedOrigin.Equal(sourceOrigin) {
		t.Errorf("imported origin = %s, want %s (the source's origin, not the moment of import)", importedOrigin, sourceOrigin)
	}
	if importedOrigin.After(base) {
		t.Errorf("imported origin = %s is after the fixture's own base time %s -- it looks like the import instant leaked in", importedOrigin, base)
	}
}

// captureStderr redirects os.Stderr for the duration of fn, returning
// whatever it wrote. Mirrors captureStdout in main_test.go — export's
// truncation warning deliberately goes to stderr rather than stdout, since
// stdout carries the document itself and has to stay pipeable straight into
// `kanban import`.
func captureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fnErr := fn()
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out, fnErr
}

// fetchAllChat reads every chat message of one project directly from the
// store, in one page. Store-layer ChatFilter.Limit carries no upper clamp
// (that discipline lives in the service layer, for the UI's sake), so a
// generous literal limit is enough to read the whole feed back for
// comparison without re-implementing the paging loop the CLI itself uses.
func fetchAllChat(t *testing.T, dataDir, projectKey string) []domain.ChatMessage {
	t.Helper()
	ctx := context.Background()
	st, err := openStore(ctx, dataDir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer st.Close()
	var out []domain.ChatMessage
	if err := st.Read(ctx, func(tx store.Tx) error {
		p, err := st.Projects().GetByKey(tx, projectKey)
		if err != nil {
			return err
		}
		msgs, err := st.Chat().List(tx, store.ChatFilter{ProjectID: &p.ID, Limit: 10000})
		out = msgs
		return err
	}); err != nil {
		t.Fatalf("fetch chat: %v", err)
	}
	return out
}

// TestExportImport_CLIChatPagingBeyondOnePage is KANB-29's CLI-path
// guarantee, mirroring internal/web's TestProjectExportRoute_CarriesTheWholeDocument:
// a board with more chat than collectChat's 100-message page size must
// round-trip in full through `kanban export` / `kanban import`.
//
// Before this test existed, a review canary broke collectChat's paging loop
// (an unconditional `return nil` after the first page) and the WHOLE `go
// test ./...` suite stayed green — the round-trip test in this file only
// ever seeded two chat messages, well under one page, so it could not see a
// pagination bug. This test seeds 143 messages (deliberately more than the
// 100-message page) so a broken loop fails loudly, and it checks the OLDEST
// message specifically: a truncating export keeps the newest page and drops
// exactly the history nobody is watching for.
func TestExportImport_CLIChatPagingBeyondOnePage(t *testing.T) {
	const chatBeyondOnePage = 143
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	ctx := context.Background()

	func() {
		st, err := openStore(ctx, dir1)
		if err != nil {
			t.Fatalf("openStore: %v", err)
		}
		defer st.Close()
		svc := service.New(st, nil)
		actor := service.Actor{
			Name:   "tester",
			Scopes: domain.Scopes{domain.ScopeAdmin, domain.ScopeWrite, domain.ScopeRead},
		}
		if _, err := svc.ProjectUpsert(ctx, actor, service.ProjectUpsertInput{
			Mode: service.UpsertCreate, Key: "RKCH", Name: "Chat paging",
			Columns: []service.ColumnSpec{
				{Name: "Backlog", Kind: domain.KindBacklog},
				{Name: "Doing", Kind: domain.KindActive},
				{Name: "Done", Kind: domain.KindDone},
			},
		}); err != nil {
			t.Fatalf("ProjectUpsert: %v", err)
		}
	}()

	// Re-open for a single write transaction that seeds every message with
	// a caller-chosen, strictly ascending CreatedAt so "oldest" is
	// unambiguous — the store honours a supplied timestamp, service.ChatAdd
	// would stamp now() for all of them and erase the ordering this test
	// depends on.
	st, err := openStore(ctx, dir1)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	var projID string
	if err := st.Read(ctx, func(tx store.Tx) error {
		p, err := st.Projects().GetByKey(tx, "RKCH")
		if err != nil {
			return err
		}
		projID = p.ID
		return nil
	}); err != nil {
		t.Fatalf("lookup project: %v", err)
	}
	oldestBody := "thought number 0"
	if err := st.Write(ctx, func(tx store.Tx) error {
		for i := 0; i < chatBeyondOnePage; i++ {
			if err := st.Chat().Add(tx, &domain.ChatMessage{
				ID:        fmt.Sprintf("chat-%03d", i),
				ProjectID: projID,
				Author:    "alex",
				Body:      fmt.Sprintf("thought number %d", i),
				CreatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute),
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	st.Close()

	exportFile := filepath.Join(t.TempDir(), "export.json")
	if err := runExport([]string{"--data", dir1, "--out", exportFile}); err != nil {
		t.Fatalf("runExport: %v", err)
	}
	raw, err := os.ReadFile(exportFile)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	var doc exportDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse export: %v", err)
	}
	if got := len(doc.ChatMessages); got != chatBeyondOnePage {
		t.Fatalf("export chat_messages = %d, want %d — the CLI export truncated the feed instead of paging to the end", got, chatBeyondOnePage)
	}
	var sawOldest bool
	for _, m := range doc.ChatMessages {
		if m.Body == oldestBody {
			sawOldest = true
		}
	}
	if !sawOldest {
		t.Fatalf("the OLDEST chat message (%q) is missing from the export — this is what a broken paging loop looks like", oldestBody)
	}

	if err := runImport([]string{"--data", dir2, "--in", exportFile}); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	gotChat := fetchAllChat(t, dir2, "RKCH")
	if len(gotChat) != chatBeyondOnePage {
		t.Fatalf("imported chat count = %d, want %d", len(gotChat), chatBeyondOnePage)
	}
}

// TestExportBoard_DoneColumnTruncationIsNamed is KANB-29 defect #2: a done
// column with more live tasks than domain.MaxDoneLimit (200) must not
// export short in silence. board_get's own cap is a deliberate,
// service-layer invariant this command does not try to bypass — but the gap
// has to be named, both in the document (`truncated`) and on the command's
// stderr, so an operator moving a big board to another machine finds out
// about the loss instead of discovering it later as a shorter history than
// they remember.
func TestExportBoard_DoneColumnTruncationIsNamed(t *testing.T) {
	doneCount := domain.MaxDoneLimit + 5
	dir1 := t.TempDir()
	ctx := context.Background()

	st, err := openStore(ctx, dir1)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	svc := service.New(st, nil)
	actor := service.Actor{
		Name:   "tester",
		Scopes: domain.Scopes{domain.ScopeAdmin, domain.ScopeWrite, domain.ScopeRead},
	}
	if _, err := svc.ProjectUpsert(ctx, actor, service.ProjectUpsertInput{
		Mode: service.UpsertCreate, Key: "RKDN", Name: "Done overflow",
		Columns: []service.ColumnSpec{
			{Name: "Backlog", Kind: domain.KindBacklog},
			{Name: "Doing", Kind: domain.KindActive},
			{Name: "Done", Kind: domain.KindDone},
		},
	}); err != nil {
		t.Fatalf("ProjectUpsert: %v", err)
	}

	created := 0
	for created < doneCount {
		batch := doneCount - created
		if batch > domain.MaxBatchTasks {
			batch = domain.MaxBatchTasks
		}
		tasks := make([]service.NewTask, batch)
		for i := range tasks {
			tasks[i] = service.NewTask{
				ProjectKey: "RKDN", Column: "Done",
				Title: fmt.Sprintf("done task %d", created+i),
				Type:  domain.TypeTask,
			}
		}
		if _, err := svc.TaskCreate(ctx, actor, service.TaskCreateInput{Tasks: tasks}); err != nil {
			t.Fatalf("TaskCreate batch at %d: %v", created, err)
		}
		created += batch
	}
	st.Close()

	var doc *exportDocument
	stderr, err := captureStderr(t, func() error {
		st, err := openStore(ctx, dir1)
		if err != nil {
			return err
		}
		defer st.Close()
		doc, err = exportBoard(ctx, service.New(st, nil), st, actor, "RKDN")
		if err != nil {
			return err
		}
		for _, tn := range doc.Truncated {
			fmt.Fprintf(os.Stderr, "warning: project %s exports only %d of %d done tasks\n", tn.Project, tn.Included, tn.Total)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("exportBoard: %v", err)
	}

	if len(doc.Truncated) != 1 {
		t.Fatalf("truncated notices = %d, want 1 (RKDN/Done); doc.Truncated = %+v", len(doc.Truncated), doc.Truncated)
	}
	tn := doc.Truncated[0]
	if tn.Project != "RKDN" {
		t.Errorf("truncation notice = %+v, want project RKDN", tn)
	}
	if tn.Total != doneCount {
		t.Errorf("truncation total = %d, want %d (the true unarchived count)", tn.Total, doneCount)
	}
	if tn.Included != domain.MaxDoneLimit {
		t.Errorf("truncation included = %d, want %d (board_get's own cap)", tn.Included, domain.MaxDoneLimit)
	}
	if tn.Included >= tn.Total {
		t.Errorf("truncation notice claims included (%d) >= total (%d); that is not a truncation", tn.Included, tn.Total)
	}

	if !bytes.Contains([]byte(stderr), []byte("RKDN")) {
		t.Errorf("stderr did not name the truncated project; got %q", stderr)
	}
	wantFrag := fmt.Sprintf("%d of %d", domain.MaxDoneLimit, doneCount)
	if !bytes.Contains([]byte(stderr), []byte(wantFrag)) {
		t.Errorf("stderr does not name the numbers involved (%q); got %q", wantFrag, stderr)
	}

	// The document itself must carry the same fact — not only the command's
	// stderr — so a document handed to `kanban import` on its own (stderr
	// discarded, e.g. by a shell pipeline) still lets an operator find out.
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"truncated"`)) {
		t.Errorf("export document has no \"truncated\" field: %s", raw)
	}
}
