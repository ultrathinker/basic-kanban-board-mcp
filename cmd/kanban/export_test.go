package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
			ID:        "pm-2",
			Project:   "RKND",
			Task:      keys[0],
			Assessor:  "alex",
			Percent:   40,
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
				ID: m.ID, ProjectID: projID, TaskID: tid, Assessor: m.Assessor, Percent: m.Percent, CreatedAt: m.CreatedAt,
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
