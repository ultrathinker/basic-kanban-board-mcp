package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// progressBase anchors every test timestamp. Offsets are whole seconds so the
// millisecond storage format keeps the intended order.
var progressBase = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func strPtr(s string) *string { return &s }

// newMark builds a mark with a fixed timestamp so tests control the order.
func newMark(id, projectID string, taskID *string, assessor string, percent int) *domain.ProgressMark {
	return &domain.ProgressMark{
		ID:        id,
		ProjectID: projectID,
		TaskID:    taskID,
		Assessor:  assessor,
		Percent:   percent,
		CreatedAt: progressBase,
	}
}

func addMark(t *testing.T, s Store, m *domain.ProgressMark) {
	t.Helper()
	if err := s.Write(context.Background(), func(tx Tx) error {
		return s.Progress().Add(tx, m)
	}); err != nil {
		t.Fatalf("add progress mark %+v: %v", m, err)
	}
}

func progressHistory(t *testing.T, s Store, projectID string, taskID *string) []domain.ProgressMark {
	t.Helper()
	var out []domain.ProgressMark
	err := s.Read(context.Background(), func(tx Tx) error {
		var err error
		out, err = s.Progress().History(tx, projectID, taskID)
		return err
	})
	if err != nil {
		t.Fatalf("progress history: %v", err)
	}
	return out
}

func progressLatest(t *testing.T, s Store, projectID string, taskID *string) []domain.ProgressMark {
	t.Helper()
	var out []domain.ProgressMark
	err := s.Read(context.Background(), func(tx Tx) error {
		var err error
		out, err = s.Progress().LatestByAssessor(tx, projectID, taskID)
		return err
	})
	if err != nil {
		t.Fatalf("progress latest: %v", err)
	}
	return out
}

func deleteTrack(t *testing.T, s Store, projectID string, taskID *string, assessor string) int64 {
	t.Helper()
	var n int64
	err := s.Write(context.Background(), func(tx Tx) error {
		var err error
		n, err = s.Progress().DeleteTrack(tx, projectID, taskID, assessor)
		return err
	})
	if err != nil {
		t.Fatalf("delete track %q: %v", assessor, err)
	}
	return n
}

// countRows runs a raw scalar COUNT over the store. Chat has no repository in
// this task, so its assertions go through raw SQL on purpose: the table is
// pinned here, not through a layer that does not exist yet.
func countRows(t *testing.T, s Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.Read(context.Background(), func(tx Tx) error {
		return tx.(*txWrap).tx.QueryRowContext(context.Background(), query, args...).Scan(&n)
	}); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// insertChatRow writes one chat_messages row directly: the chat repository is
// a separate task, but the cascade and pruner guarantees cover this table too.
func insertChatRow(t *testing.T, s Store, id, projectID, author, body string, at time.Time) {
	t.Helper()
	err := s.Write(context.Background(), func(tx Tx) error {
		_, err := tx.(*txWrap).tx.ExecContext(context.Background(), `
			INSERT INTO chat_messages(id, project_id, author, body, created_at)
			VALUES (?,?,?,?,?)`,
			id, projectID, author, body, formatTime(at))
		return err
	})
	if err != nil {
		t.Fatalf("insert chat message %s: %v", id, err)
	}
}

// tableColumns returns the column names of a table in declaration order.
func tableColumns(t *testing.T, s Store, table string) []string {
	t.Helper()
	var out []string
	err := s.Read(context.Background(), func(tx Tx) error {
		rows, err := tx.(*txWrap).tx.QueryContext(context.Background(),
			"SELECT name FROM pragma_table_info(?) ORDER BY cid", table)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			out = append(out, name)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("table info for %s: %v", table, err)
	}
	return out
}

// ---------------------------------------------------------------------------
// 1. Migration applies on an empty database and on one that already has data;
// reopening records nothing new and breaks nothing.
// ---------------------------------------------------------------------------

func TestMigration0005_ProgressAndChat(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kanban.db")
	ctx := context.Background()
	open := func() Store {
		t.Helper()
		s, err := Open(ctx, Config{Path: dbPath, ReadPoolSize: 4})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return s
	}

	// Fresh file: both tables exist, version 5 is recorded, columns are exact.
	s1 := open()
	if got := migrationVersion(s1.(*sqlStore), ctx); got != 5 {
		t.Fatalf("migration version after fresh Open = %d, want 5", got)
	}
	wantCols := []string{"id", "project_id", "task_id", "assessor", "percent", "eta", "created_at"}
	if got := tableColumns(t, s1, "progress_marks"); !reflect.DeepEqual(got, wantCols) {
		t.Fatalf("progress_marks columns = %v, want %v", got, wantCols)
	}
	if got := tableColumns(t, s1, "chat_messages"); len(got) == 0 {
		t.Fatal("chat_messages does not exist after migration")
	}

	// Leave data behind so the reopen runs over a populated database.
	p, cols := seedProject(t, s1)
	task := seedTask(t, s1, p, cols["Backlog"], "seeded", "tester")
	addMark(t, s1, newMark("pm-1", p.ID, &task.ID, "alpha", 40))
	insertChatRow(t, s1, "cm-1", p.ID, "alpha", "hello", progressBase)
	if err := s1.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	s2 := open()
	defer func() { _ = s2.Close() }()
	if got := migrationVersion(s2.(*sqlStore), ctx); got != 5 {
		t.Fatalf("migration version after reopen = %d, want 5", got)
	}
	// Reopening must not re-apply anything: exactly five recorded migrations.
	var applied int
	if err := s2.Read(ctx, func(tx Tx) error {
		return tx.(*txWrap).tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM schema_migrations").Scan(&applied)
	}); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if applied != 5 {
		t.Fatalf("schema_migrations rows = %d, want 5", applied)
	}
	// The data written before the reopen is still there, and new writes work.
	hist := progressHistory(t, s2, p.ID, &task.ID)
	if len(hist) != 1 || hist[0].ID != "pm-1" || hist[0].Percent != 40 {
		t.Fatalf("history after reopen = %+v, want [pm-1/40]", hist)
	}
	if got := countRows(t, s2, "SELECT COUNT(*) FROM chat_messages WHERE id = 'cm-1'"); got != 1 {
		t.Fatalf("chat message survived reopen = %d rows, want 1", got)
	}
	addMark(t, s2, newMark("pm-2", p.ID, &task.ID, "beta", 60))
	if got := len(progressHistory(t, s2, p.ID, &task.ID)); got != 2 {
		t.Fatalf("history after post-reopen write = %d rows, want 2", got)
	}
}

// ---------------------------------------------------------------------------
// 2. The percent CHECK really rejects out-of-range values at the database
// level — including when a future caller skips every Go-side check.
// ---------------------------------------------------------------------------

func TestProgressMarks_PercentCheckRejectsOutOfRange(t *testing.T) {
	ts := openTestStore(t)
	p, _ := seedProject(t, ts)
	ctx := context.Background()

	// Raw INSERT, bypassing the repository: the schema is the backstop.
	rawInsert := func(percent int) error {
		return ts.Write(ctx, func(tx Tx) error {
			_, err := tx.(*txWrap).tx.ExecContext(ctx, `
				INSERT INTO progress_marks(id, project_id, task_id, assessor, percent, eta, created_at)
				VALUES (?,?,?,?,?,?,?)`,
				"raw-"+strconv.Itoa(percent), p.ID, nil, "check-test", percent, nil, formatTime(progressBase))
			return err
		})
	}
	for _, bad := range []int{-1, 101, 1000} {
		err := rawInsert(bad)
		if err == nil {
			t.Fatalf("INSERT percent=%d: expected a CHECK violation, got nil", bad)
		}
		if !IsCheckViolation(err) {
			t.Fatalf("INSERT percent=%d: error is not a CHECK violation: %v", bad, err)
		}
	}
	for _, good := range []int{0, 100} {
		if err := rawInsert(good); err != nil {
			t.Fatalf("INSERT percent=%d at the boundary: %v", good, err)
		}
	}

	// Through the repository the same constraint surfaces as a validation
	// error naming the field.
	err := ts.Write(ctx, func(tx Tx) error {
		return ts.Progress().Add(tx, newMark("pm-bad", p.ID, nil, "alpha", 101))
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Add percent=101: want a domain validation error, got %v", err)
	}
	if !strings.Contains(err.Error(), "percent") {
		t.Fatalf("Add percent=101: error should name percent, got %v", err)
	}
	// And the boundary values go through the repository too.
	addMark(t, ts, newMark("pm-zero", p.ID, nil, "alpha", 0))
	addMark(t, ts, newMark("pm-max", p.ID, nil, "alpha", 100))
}

// ---------------------------------------------------------------------------
// 3. Deleting a project cascades to its progress marks and chat messages.
// ---------------------------------------------------------------------------

func TestProgressCascade_ProjectDeleteRemovesMarksAndChat(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	task := seedTask(t, ts, p, cols["Backlog"], "task", "tester")

	addMark(t, ts, newMark("pm-project", p.ID, nil, "alpha", 10))
	addMark(t, ts, newMark("pm-task", p.ID, &task.ID, "alpha", 50))
	insertChatRow(t, ts, "cm-1", p.ID, "alpha", "before delete", progressBase)

	if err := ts.Write(context.Background(), func(tx Tx) error {
		return ts.Projects().Delete(tx, p.ID)
	}); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if got := countRows(t, ts, "SELECT COUNT(*) FROM progress_marks"); got != 0 {
		t.Fatalf("progress_marks rows after project delete = %d, want 0", got)
	}
	if got := countRows(t, ts, "SELECT COUNT(*) FROM chat_messages"); got != 0 {
		t.Fatalf("chat_messages rows after project delete = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// 4. Deleting a task cascades to ITS marks but leaves the project-level marks
// (task_id IS NULL) and other tasks' marks alone.
// ---------------------------------------------------------------------------

func TestProgressCascade_TaskDeleteKeepsProjectMarks(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	taskA := seedTask(t, ts, p, cols["Backlog"], "A", "tester")
	taskB := seedTask(t, ts, p, cols["Backlog"], "B", "tester")

	addMark(t, ts, newMark("pm-a1", p.ID, &taskA.ID, "alpha", 10))
	addMark(t, ts, newMark("pm-a2", p.ID, &taskA.ID, "alpha", 20))
	addMark(t, ts, newMark("pm-b1", p.ID, &taskB.ID, "beta", 30))
	addMark(t, ts, newMark("pm-p1", p.ID, nil, "alpha", 40))
	addMark(t, ts, newMark("pm-p2", p.ID, nil, "beta", 50))

	if err := ts.Write(context.Background(), func(tx Tx) error {
		return ts.Tasks().Delete(tx, taskA.ID)
	}); err != nil {
		t.Fatalf("delete task A: %v", err)
	}
	if got := countRows(t, ts, "SELECT COUNT(*) FROM progress_marks WHERE task_id = ?", taskA.ID); got != 0 {
		t.Fatalf("task A marks survived the task delete: %d rows, want 0", got)
	}
	if got := countRows(t, ts, "SELECT COUNT(*) FROM progress_marks WHERE task_id IS NULL"); got != 2 {
		t.Fatalf("project-level marks after task delete = %d, want 2", got)
	}
	if got := countRows(t, ts, "SELECT COUNT(*) FROM progress_marks WHERE task_id = ?", taskB.ID); got != 1 {
		t.Fatalf("task B marks after deleting task A = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// 5. The events pruner deletes events rows only: progress marks and chat
// messages, even decades old, are permanent.
// ---------------------------------------------------------------------------

func TestEventsPrune_SparesProgressAndChat(t *testing.T) {
	ts := openTestStore(t)
	p, _ := seedProject(t, ts)
	ctx := context.Background()
	old := progressBase.Add(-90 * 24 * time.Hour) // far beyond the 30-day window

	// An event old enough to be pruned.
	if err := ts.Write(ctx, func(tx Tx) error {
		return ts.Events().Append(tx, &domain.Event{
			TS: old, Actor: "tester", Type: domain.EventNoteAdded, ProjectID: p.ID,
		})
	}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	// Progress and chat rows even older than that event.
	addMark(t, ts, &domain.ProgressMark{
		ID: "pm-old", ProjectID: p.ID, Assessor: "alpha", Percent: 25, CreatedAt: old,
	})
	insertChatRow(t, ts, "cm-old", p.ID, "alpha", "old message", old)

	var pruned int64
	if err := ts.Write(ctx, func(tx Tx) error {
		var err error
		pruned, err = ts.Events().Prune(tx, progressBase)
		return err
	}); err != nil {
		t.Fatalf("prune events: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned %d events, want 1", pruned)
	}
	if got := countRows(t, ts, "SELECT COUNT(*) FROM events"); got != 0 {
		t.Fatalf("events after prune = %d, want 0", got)
	}
	if got := countRows(t, ts, "SELECT COUNT(*) FROM progress_marks"); got != 1 {
		t.Fatalf("progress_marks after the events prune = %d, want 1: the pruner must not touch progress", got)
	}
	if got := countRows(t, ts, "SELECT COUNT(*) FROM chat_messages"); got != 1 {
		t.Fatalf("chat_messages after the events prune = %d, want 1: the pruner must not touch chat", got)
	}
	// The surviving mark still reads back intact through the repository.
	hist := progressHistory(t, ts, p.ID, nil)
	if len(hist) != 1 || hist[0].ID != "pm-old" || hist[0].Percent != 25 {
		t.Fatalf("progress history after prune = %+v, want [pm-old/25]", hist)
	}
}

// ---------------------------------------------------------------------------
// 6. LatestByAssessor returns exactly one row per assessor — the newest — and
// DeleteTrack removes one track's whole history without touching any other.
// ---------------------------------------------------------------------------

func TestProgressMarks_LatestByAssessorAndDeleteTrack(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	task := seedTask(t, ts, p, cols["Backlog"], "task", "tester")

	// alpha: three marks over time; beta: two; gamma: one project-level mark.
	// Timestamps are distinct on purpose: the ordering assertions below mean
	// nothing if every mark carries the same instant.
	mk := func(id, assessor string, percent int, at time.Time, taskID *string) {
		addMark(t, ts, &domain.ProgressMark{
			ID: id, ProjectID: p.ID, TaskID: taskID, Assessor: assessor,
			Percent: percent, CreatedAt: at,
		})
	}
	mk("pm-a1", "alpha", 10, progressBase, &task.ID)
	mk("pm-a2", "alpha", 50, progressBase.Add(time.Second), &task.ID)
	mk("pm-a3", "alpha", 80, progressBase.Add(2*time.Second), &task.ID)
	mk("pm-b1", "beta", 30, progressBase, &task.ID)
	mk("pm-b2", "beta", 60, progressBase.Add(3*time.Second), &task.ID)
	mk("pm-g1", "gamma", 15, progressBase, nil)

	// Latest in the task scope: one row per assessor, each the newest.
	latest := progressLatest(t, ts, p.ID, &task.ID)
	if len(latest) != 2 {
		t.Fatalf("latest in task scope = %d rows, want 2 (one per assessor)", len(latest))
	}
	if latest[0].Assessor != "alpha" || latest[1].Assessor != "beta" {
		t.Fatalf("latest rows are not sorted by assessor: %s, %s", latest[0].Assessor, latest[1].Assessor)
	}
	byAssessor := map[string]int{}
	for _, m := range latest {
		byAssessor[m.Assessor] = m.Percent
	}
	if byAssessor["alpha"] != 80 || byAssessor["beta"] != 60 {
		t.Fatalf("latest per assessor = %v, want alpha=80 beta=60 (the newest marks)", byAssessor)
	}

	// The project scope sees only project-level marks, task tracks excluded.
	projLatest := progressLatest(t, ts, p.ID, nil)
	if len(projLatest) != 1 || projLatest[0].Assessor != "gamma" || projLatest[0].Percent != 15 {
		t.Fatalf("latest in project scope = %+v, want [gamma/15]", projLatest)
	}

	// Full history of the task scope, oldest first.
	hist := progressHistory(t, ts, p.ID, &task.ID)
	if len(hist) != 5 {
		t.Fatalf("task history = %d rows, want 5", len(hist))
	}
	for i := 1; i < len(hist); i++ {
		if hist[i-1].CreatedAt.After(hist[i].CreatedAt) {
			t.Fatalf("history not ascending at %d: %v then %v", i, hist[i-1].CreatedAt, hist[i].CreatedAt)
		}
	}
	if hist[0].ID != "pm-a1" || hist[len(hist)-1].ID != "pm-b2" {
		t.Fatalf("history endpoints = %s..%s, want pm-a1..pm-b2", hist[0].ID, hist[len(hist)-1].ID)
	}

	// Deleting alpha's track removes its whole history and nothing else.
	if removed := deleteTrack(t, ts, p.ID, &task.ID, "alpha"); removed != 3 {
		t.Fatalf("DeleteTrack removed %d rows, want 3 (alpha's whole history)", removed)
	}
	if got := countRows(t, ts, "SELECT COUNT(*) FROM progress_marks WHERE task_id = ? AND assessor = 'alpha'", task.ID); got != 0 {
		t.Fatalf("alpha's track has %d rows left, want 0", got)
	}
	latest = progressLatest(t, ts, p.ID, &task.ID)
	if len(latest) != 1 || latest[0].Assessor != "beta" || latest[0].Percent != 60 {
		t.Fatalf("latest after track delete = %+v, want [beta/60]", latest)
	}
	if got := countRows(t, ts, "SELECT COUNT(*) FROM progress_marks WHERE task_id IS NULL"); got != 1 {
		t.Fatalf("project-level marks after a task track delete = %d, want 1", got)
	}
	// An unknown assessor is a clean no-op, not an error.
	if got := deleteTrack(t, ts, p.ID, &task.ID, "nobody"); got != 0 {
		t.Fatalf("deleting an unknown track removed %d rows, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// 7. Add validates its inputs, maps constraint failures to domain errors,
// defaults CreatedAt to the database clock and round-trips ETA.
// ---------------------------------------------------------------------------
func TestProgressAdd_Validation(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	task := seedTask(t, ts, p, cols["Backlog"], "task", "tester")
	ctx := context.Background()

	// Required fields (and a nil mark) are refused.
	for _, m := range []*domain.ProgressMark{
		nil,
		{ProjectID: p.ID, Assessor: "alpha"},
		{ID: "pm-x", Assessor: "alpha"},
		{ID: "pm-x", ProjectID: p.ID},
	} {
		err := ts.Write(ctx, func(tx Tx) error { return ts.Progress().Add(tx, m) })
		if err == nil {
			t.Fatalf("Add(%v): want an error, got nil", m)
		}
		if m != nil && !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("Add(%+v): want a validation error, got %v", m, err)
		}
	}

	// An unknown project or task is not_found, not a raw driver error.
	err := ts.Write(ctx, func(tx Tx) error {
		return ts.Progress().Add(tx, newMark("pm-nop", "no-such-project", nil, "alpha", 1))
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Add with unknown project: want not_found, got %v", err)
	}
	err = ts.Write(ctx, func(tx Tx) error {
		return ts.Progress().Add(tx, newMark("pm-not", p.ID, strPtr("no-such-task"), "alpha", 1))
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Add with unknown task: want not_found, got %v", err)
	}

	// A zero CreatedAt takes the database clock.
	m := &domain.ProgressMark{ID: "pm-clock", ProjectID: p.ID, TaskID: &task.ID, Assessor: "alpha", Percent: 7}
	addMark(t, ts, m)
	if m.CreatedAt.IsZero() {
		t.Fatal("Add left CreatedAt zero; the database clock should have filled it")
	}

	// ETA round-trips through storage.
	eta := progressBase.Add(48 * time.Hour)
	addMark(t, ts, &domain.ProgressMark{
		ID: "pm-eta", ProjectID: p.ID, TaskID: &task.ID, Assessor: "alpha", Percent: 9, ETA: &eta, CreatedAt: progressBase,
	})
	hist := progressHistory(t, ts, p.ID, &task.ID)
	var got *domain.ProgressMark
	for i := range hist {
		if hist[i].ID == "pm-eta" {
			got = &hist[i]
		}
	}
	if got == nil || got.ETA == nil || !got.ETA.Equal(eta) {
		t.Fatalf("ETA did not round-trip: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// 8. LatestByTask returns the newest mark per (task, assessor) for every task
// track of the project in one read, and nothing else.
// ---------------------------------------------------------------------------

func TestProgressMarks_LatestByTask_BatchesAllTaskTracks(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	taskA := seedTask(t, ts, p, cols["Backlog"], "A", "tester")
	taskB := seedTask(t, ts, p, cols["Backlog"], "B", "tester")
	taskC := seedTask(t, ts, p, cols["Backlog"], "C", "tester") // no marks on purpose

	addMark(t, ts, &domain.ProgressMark{ID: "pm-a1", ProjectID: p.ID, TaskID: &taskA.ID, Assessor: "alpha", Percent: 10, CreatedAt: progressBase})
	addMark(t, ts, &domain.ProgressMark{ID: "pm-a2", ProjectID: p.ID, TaskID: &taskA.ID, Assessor: "alpha", Percent: 50, CreatedAt: progressBase.Add(time.Second)})
	addMark(t, ts, &domain.ProgressMark{ID: "pm-a3", ProjectID: p.ID, TaskID: &taskA.ID, Assessor: "beta", Percent: 60, CreatedAt: progressBase.Add(2 * time.Second)})
	addMark(t, ts, &domain.ProgressMark{ID: "pm-b1", ProjectID: p.ID, TaskID: &taskB.ID, Assessor: "alpha", Percent: 80, CreatedAt: progressBase.Add(3 * time.Second)})
	// Project-level marks belong to LatestByAssessor's scope, not here.
	addMark(t, ts, &domain.ProgressMark{ID: "pm-p1", ProjectID: p.ID, Assessor: "gamma", Percent: 15})
	// Another project's marks must not leak across the project boundary.
	other := &domain.Project{
		ID: "other-project", Key: "OTHER", Name: "Other",
		Version: 1, NextTaskSeq: 1, EstimateUnit: "h",
		EnforceDependencies: true, StrictDone: false, ClaimTTLSeconds: 3600,
	}
	if err := ts.Write(context.Background(), func(tx Tx) error {
		return ts.Projects().Create(tx, other)
	}); err != nil {
		t.Fatalf("seed other project: %v", err)
	}
	addMark(t, ts, &domain.ProgressMark{ID: "pm-o1", ProjectID: other.ID, TaskID: &taskA.ID, Assessor: "ghost", Percent: 1})

	var got map[string][]domain.ProgressMark
	if err := ts.Write(context.Background(), func(tx Tx) error {
		var err error
		got, err = ts.Progress().LatestByTask(tx, p.ID)
		return err
	}); err != nil {
		t.Fatalf("LatestByTask: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("tasks with tracks = %d, want 2 (task C has no marks, project-level and other-project marks excluded)", len(got))
	}
	if marks := got[taskA.ID]; len(marks) != 2 {
		t.Fatalf("task A marks = %+v, want exactly one latest per assessor (alpha, beta)", marks)
	} else {
		byAssessor := map[string]int{}
		for _, m := range marks {
			byAssessor[m.Assessor] = m.Percent
		}
		if byAssessor["alpha"] != 50 || byAssessor["beta"] != 60 {
			t.Fatalf("task A latest = %v, want alpha=50 beta=60", byAssessor)
		}
	}
	if marks := got[taskB.ID]; len(marks) != 1 || marks[0].Assessor != "alpha" || marks[0].Percent != 80 {
		t.Fatalf("task B marks = %+v, want [alpha/80]", marks)
	}
	if _, ok := got[taskC.ID]; ok {
		t.Fatalf("task C has no marks but appears in the map: %+v", got[taskC.ID])
	}
	if _, ok := got[""]; ok {
		t.Fatalf("project-level marks leaked into the task map under a \"\" key")
	}
}
