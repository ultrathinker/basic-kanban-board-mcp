package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// KANB-30 — the task lifecycle journal.
// ---------------------------------------------------------------------------

func historyOf(t *testing.T, s Store, projectID string) []TaskHistoryEntry {
	t.Helper()
	var out []TaskHistoryEntry
	if err := s.Read(context.Background(), func(tx Tx) error {
		var err error
		out, err = s.TaskHistory().ListByProject(tx, projectID)
		return err
	}); err != nil {
		t.Fatalf("read task history: %v", err)
	}
	return out
}

func historyOrigin(t *testing.T, s Store) time.Time {
	t.Helper()
	var at time.Time
	if err := s.Read(context.Background(), func(tx Tx) error {
		var err error
		at, err = s.TaskHistory().Origin(tx)
		return err
	}); err != nil {
		t.Fatalf("read history origin: %v", err)
	}
	return at
}

func kindsOf(entries []TaskHistoryEntry) []TaskHistoryKind {
	out := make([]TaskHistoryKind, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Kind)
	}
	return out
}

// Acceptance 3: all four kinds of change are journalled — move, create /
// archive / restore, estimate, parent. Each is driven through the repository
// method the rest of the product actually calls, not through a helper written
// for the test.
func TestTaskHistory_CoversEveryKindOfChange(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	ctx := context.Background()

	parent := seedTask(t, ts, p, cols["Backlog"], "umbrella", "alice")
	child := seedTask(t, ts, p, cols["Backlog"], "leaf", "alice")

	// Move: Backlog -> Done.
	if err := ts.Write(ctx, func(tx Tx) error {
		return ts.Tasks().Move(tx, child.ID, cols["Done"].ID, 2*domain.RankStep, "bob")
	}); err != nil {
		t.Fatalf("move: %v", err)
	}
	// Estimate and parent, in one Update — the real shape of a task_update.
	if err := ts.Write(ctx, func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, child.ID)
		if err != nil {
			return err
		}
		est := 3.5
		cur.Estimate = &est
		cur.ParentID = &parent.ID
		cur.UpdatedBy = "carol"
		return ts.Tasks().Update(tx, cur, nil)
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	// Archive, then restore.
	if err := ts.Write(ctx, func(tx Tx) error {
		return ts.Tasks().Archive(tx, child.ID, true, "dave")
	}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := ts.Write(ctx, func(tx Tx) error {
		return ts.Tasks().Archive(tx, child.ID, false, "dave")
	}); err != nil {
		t.Fatalf("restore: %v", err)
	}

	var own []TaskHistoryEntry
	for _, e := range historyOf(t, ts, p.ID) {
		if e.TaskID != nil && *e.TaskID == child.ID {
			own = append(own, e)
		}
	}
	want := []TaskHistoryKind{
		HistoryCreated, HistoryMoved, HistoryEstimate, HistoryParent,
		HistoryArchived, HistoryRestored,
	}
	got := kindsOf(own)
	if len(got) != len(want) {
		t.Fatalf("journal kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("journal kinds = %v, want %v", got, want)
		}
	}

	created, moved, estimate, parentEntry := own[0], own[1], own[2], own[3]
	if created.ToColumn == nil || *created.ToColumn != cols["Backlog"].ID {
		t.Fatalf("created entry column = %v, want %s", created.ToColumn, cols["Backlog"].ID)
	}
	if created.ToKind == nil || *created.ToKind != domain.KindBacklog {
		t.Fatalf("created entry column kind = %v, want backlog", created.ToKind)
	}
	if created.Archived == nil || *created.Archived {
		t.Fatalf("created entry archived = %v, want false", created.Archived)
	}
	if created.Reconstructed {
		t.Fatal("an observed creation must not be marked reconstructed")
	}
	if created.Actor != "alice" {
		t.Fatalf("created entry actor = %q, want alice", created.Actor)
	}

	if moved.FromColumn == nil || *moved.FromColumn != cols["Backlog"].ID ||
		moved.ToColumn == nil || *moved.ToColumn != cols["Done"].ID {
		t.Fatalf("moved entry columns = %v -> %v", moved.FromColumn, moved.ToColumn)
	}
	if moved.FromKind == nil || *moved.FromKind != domain.KindBacklog ||
		moved.ToKind == nil || *moved.ToKind != domain.KindDone {
		t.Fatalf("moved entry kinds = %v -> %v, want backlog -> done", moved.FromKind, moved.ToKind)
	}
	if moved.Actor != "bob" {
		t.Fatalf("moved entry actor = %q, want bob", moved.Actor)
	}

	if estimate.OldEstimate != nil {
		t.Fatalf("estimate entry old = %v, want nil (was unestimated)", *estimate.OldEstimate)
	}
	if estimate.NewEstimate == nil || *estimate.NewEstimate != 3.5 {
		t.Fatalf("estimate entry new = %v, want 3.5", estimate.NewEstimate)
	}
	if parentEntry.OldParent != nil {
		t.Fatalf("parent entry old = %v, want nil", *parentEntry.OldParent)
	}
	if parentEntry.NewParent == nil || *parentEntry.NewParent != parent.ID {
		t.Fatalf("parent entry new = %v, want %s", parentEntry.NewParent, parent.ID)
	}
}

// A column's kind change touches no task row, yet moves every card in the
// column between buckets. It has to be in the journal or a replay silently
// disagrees with the live board — see the service-level reconciliation test.
func TestTaskHistory_ColumnKindChangeIsRecorded(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	ctx := context.Background()

	col := cols["Doing"]
	if col == nil || col.Kind != domain.KindActive {
		t.Fatalf("expected an active column named Doing, got %+v", col)
	}
	if err := ts.Write(WithActor(ctx, "erin"), func(tx Tx) error {
		c, err := ts.Columns().GetByID(tx, col.ID)
		if err != nil {
			return err
		}
		c.Kind = domain.KindDone
		return ts.Columns().Update(tx, c)
	}); err != nil {
		t.Fatalf("update column: %v", err)
	}

	var found *TaskHistoryEntry
	entries := historyOf(t, ts, p.ID)
	for i := range entries {
		if entries[i].Kind == HistoryColumnKind {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		t.Fatal("no column_kind entry after a column changed kind")
	}
	if found.TaskID != nil {
		t.Fatalf("column_kind entry task_id = %v, want NULL", *found.TaskID)
	}
	if found.ToColumn == nil || *found.ToColumn != col.ID {
		t.Fatalf("column_kind entry column = %v, want %s", found.ToColumn, col.ID)
	}
	if found.FromKind == nil || *found.FromKind != domain.KindActive ||
		found.ToKind == nil || *found.ToKind != domain.KindDone {
		t.Fatalf("column_kind entry kinds = %v -> %v, want active -> done", found.FromKind, found.ToKind)
	}
	if found.Actor != "erin" {
		t.Fatalf("column_kind entry actor = %q, want erin", found.Actor)
	}
}

// Acceptance 2: the entry lives in the mutation's own transaction. If the
// transaction rolls back, so does the journal — a journal that survived a
// rolled-back move would describe something that never happened.
func TestTaskHistory_RollsBackWithTheMutation(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	ctx := context.Background()
	task := seedTask(t, ts, p, cols["Backlog"], "doomed", "alice")

	before := len(historyOf(t, ts, p.ID))
	boom := errors.New("deliberate rollback")
	err := ts.Write(ctx, func(tx Tx) error {
		if err := ts.Tasks().Move(tx, task.ID, cols["Done"].ID, 2*domain.RankStep, "bob"); err != nil {
			return err
		}
		if err := ts.Tasks().Archive(tx, task.ID, true, "bob"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Write error = %v, want the deliberate rollback", err)
	}
	if got := len(historyOf(t, ts, p.ID)); got != before {
		t.Fatalf("journal grew by %d entries across a rolled-back transaction", got-before)
	}
}

// Acceptance 5: the events pruner physically cannot reach the journal. It
// deletes by age from `events` and names no other table; task_history rows
// older than any cutoff survive it untouched.
func TestTaskHistory_EventsPrunerCannotReachIt(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	ctx := context.Background()
	task := seedTask(t, ts, p, cols["Backlog"], "ancient", "alice")

	ancient := time.Now().UTC().Add(-365 * 24 * time.Hour)
	// One event and one journal entry, both far older than any retention
	// window the maintenance job uses (30 days).
	if err := ts.Write(ctx, func(tx Tx) error {
		if err := ts.Events().Append(tx, &domain.Event{
			TS: ancient, Actor: "alice", Type: domain.EventTaskMoved,
			ProjectID: p.ID, TaskID: &task.ID,
		}); err != nil {
			return err
		}
		return ts.TaskHistory().Append(tx, &TaskHistoryEntry{
			TS: ancient, Actor: "alice", ProjectID: p.ID, TaskID: &task.ID,
			Kind: HistoryArchived, Archived: new(bool),
		})
	}); err != nil {
		t.Fatalf("seed old rows: %v", err)
	}

	beforeJournal := countRows(t, ts, "SELECT COUNT(*) FROM task_history")
	var pruned int64
	if err := ts.Write(ctx, func(tx Tx) error {
		var err error
		pruned, err = ts.Events().Prune(tx, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned == 0 {
		t.Fatal("the pruner deleted nothing; the test is not exercising it")
	}
	if got := countRows(t, ts, "SELECT COUNT(*) FROM events"); got != 0 {
		t.Fatalf("events after prune = %d, want 0", got)
	}
	if got := countRows(t, ts, "SELECT COUNT(*) FROM task_history"); got != beforeJournal {
		t.Fatalf("task_history after prune = %d, want %d — the pruner reached the journal",
			got, beforeJournal)
	}
	if got := countRows(t, ts, "SELECT COUNT(*) FROM task_history_origin"); got != 1 {
		t.Fatalf("task_history_origin after prune = %d rows, want 1", got)
	}
}

// Acceptance 8, 9 and 10: the migration seeds a baseline for a board that is
// already full. The test runs the REAL migration body from the embedded FS
// against a populated database — dropping the tables the fresh Open created is
// how a test reaches the "0006 runs on an existing board" state.
func TestTaskHistory_MigrationSeedsBaselineOnANonEmptyBoard(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	ctx := context.Background()

	plain := seedTask(t, ts, p, cols["Backlog"], "plain", "alice")
	finished := seedTask(t, ts, p, cols["Backlog"], "finished", "alice")
	gone := seedTask(t, ts, p, cols["Backlog"], "gone", "alice")
	if err := ts.Write(ctx, func(tx Tx) error {
		if err := ts.Tasks().Move(tx, finished.ID, cols["Done"].ID, 2*domain.RankStep, "bob"); err != nil {
			return err
		}
		if err := ts.Tasks().Archive(tx, gone.ID, true, "bob"); err != nil {
			return err
		}
		cur, err := ts.Tasks().GetByID(tx, plain.ID)
		if err != nil {
			return err
		}
		est := 8.0
		cur.Estimate = &est
		cur.ParentID = &finished.ID
		return ts.Tasks().Update(tx, cur, nil)
	}); err != nil {
		t.Fatalf("shape the board: %v", err)
	}

	// Re-run 0006 verbatim over the now-populated board.
	files, err := collectMigrations()
	if err != nil {
		t.Fatalf("collectMigrations: %v", err)
	}
	var body string
	for _, m := range files {
		if m.version == 6 {
			body = m.body
		}
	}
	if body == "" {
		t.Fatal("migration 0006 is missing from the embedded FS")
	}
	if err := ts.Write(ctx, func(tx Tx) error {
		tw := tx.(*txWrap)
		if _, err := tw.tx.ExecContext(ctx, "DROP TABLE task_history; DROP TABLE task_history_origin;"); err != nil {
			return err
		}
		_, err := tw.tx.ExecContext(ctx, body)
		return err
	}); err != nil {
		t.Fatalf("re-run 0006 on a populated board: %v", err)
	}

	origin := historyOrigin(t, ts)
	entries := historyOf(t, ts, p.ID)
	if len(entries) != 3 {
		t.Fatalf("baseline seeded %d entries, want one per task (3): %v", len(entries), kindsOf(entries))
	}
	byTask := map[string]TaskHistoryEntry{}
	for _, e := range entries {
		if e.Kind != HistoryExists {
			t.Fatalf("baseline entry kind = %q, want exists", e.Kind)
		}
		if !e.Reconstructed {
			t.Fatal("a seeded baseline entry must be marked reconstructed")
		}
		if !e.TS.Equal(origin) {
			t.Fatalf("baseline entry ts = %s, want the origin instant %s", e.TS, origin)
		}
		byTask[*e.TaskID] = e
	}

	// Archivedness, column + kind, parent and estimate all made it in.
	if e := byTask[gone.ID]; e.Archived == nil || !*e.Archived {
		t.Fatalf("archived card seeded with archived = %v, want true", e.Archived)
	}
	if e := byTask[plain.ID]; e.Archived == nil || *e.Archived {
		t.Fatalf("live card seeded with archived = %v, want false", e.Archived)
	}
	if e := byTask[finished.ID]; e.ToColumn == nil || *e.ToColumn != cols["Done"].ID ||
		e.ToKind == nil || *e.ToKind != domain.KindDone {
		t.Fatalf("finished card seeded with column %v / kind %v, want the Done column", e.ToColumn, e.ToKind)
	}
	if e := byTask[plain.ID]; e.NewEstimate == nil || *e.NewEstimate != 8 {
		t.Fatalf("estimate seeded as %v, want 8", e.NewEstimate)
	}
	if e := byTask[plain.ID]; e.NewParent == nil || *e.NewParent != finished.ID {
		t.Fatalf("parent seeded as %v, want %s", e.NewParent, finished.ID)
	}

	// Acceptance 10: the past is NOT reconstructed from created_at. Every card
	// here was created before the origin instant, and not one baseline entry
	// borrowed that earlier timestamp.
	for _, e := range entries {
		if e.TS.Before(origin) {
			t.Fatalf("baseline entry dated %s, before the origin %s — the past is being invented", e.TS, origin)
		}
	}
	if !plain.CreatedAt.Before(origin) {
		t.Fatalf("test setup is wrong: task created at %s, origin %s", plain.CreatedAt, origin)
	}
}

// Three "don't write noise" guards exist in the journal writers because the
// table is kept forever and a row saying "estimate 3 became estimate 3" or
// "archived already archived" is junk that nobody can prune. Each guard is
// load-bearing on its own and a regression that lifts one of them is
// invisible at every other layer (live counters agree, curve is unchanged,
// reconciliation passes) — the only thing that notices is the row count
// after a deliberate no-op.

// Archive guard: a second Archive(card, true) on a card that is already
// archived writes nothing. Otherwise the table grows on every retry of a
// user who clicks "Archive" twice.
func TestTaskHistory_ArchiveIsIdempotent(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	ctx := context.Background()
	task := seedTask(t, ts, p, cols["Backlog"], "stays archived", "alice")

	if err := ts.Write(ctx, func(tx Tx) error {
		return ts.Tasks().Archive(tx, task.ID, true, "bob")
	}); err != nil {
		t.Fatalf("first archive: %v", err)
	}
	afterFirst := len(historyOf(t, ts, p.ID))

	if err := ts.Write(ctx, func(tx Tx) error {
		return ts.Tasks().Archive(tx, task.ID, true, "bob")
	}); err != nil {
		t.Fatalf("second archive: %v", err)
	}
	if got := len(historyOf(t, ts, p.ID)); got != afterFirst {
		t.Fatalf("re-archiving a card that is already archived added %d journal rows; the guard must keep it at %d",
			got-afterFirst, afterFirst)
	}

	// The matching guard for restore: un-archiving an un-archived card
	// also writes nothing.
	if err := ts.Write(ctx, func(tx Tx) error {
		return ts.Tasks().Archive(tx, task.ID, false, "bob")
	}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	afterRestore := len(historyOf(t, ts, p.ID))

	if err := ts.Write(ctx, func(tx Tx) error {
		return ts.Tasks().Archive(tx, task.ID, false, "bob")
	}); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	if got := len(historyOf(t, ts, p.ID)); got != afterRestore {
		t.Fatalf("restoring an already-live card added %d journal rows; the guard must keep it at %d",
			got-afterRestore, afterRestore)
	}
}

// Move guard: a Move to the column the card is already in writes nothing.
// Without it a UI that bounces the card back into the same slot during a
// drag-drop leaves a journal trail that says the card moved.
func TestTaskHistory_MoveToSameColumnWritesNothing(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	ctx := context.Background()
	task := seedTask(t, ts, p, cols["Backlog"], "stays put", "alice")

	afterCreate := len(historyOf(t, ts, p.ID))
	if err := ts.Write(ctx, func(tx Tx) error {
		return ts.Tasks().Move(tx, task.ID, cols["Backlog"].ID, 2*domain.RankStep, "bob")
	}); err != nil {
		t.Fatalf("move to same column: %v", err)
	}
	if got := len(historyOf(t, ts, p.ID)); got != afterCreate {
		t.Fatalf("moving to the column the card was already in added %d journal rows; the guard must keep it at %d",
			got-afterCreate, afterCreate)
	}
}

// Content guard: an Update that touches neither estimate nor parent writes
// no journal entry at all. The reverse is also true: changing estimate
// writes only an estimate entry, changing parent writes only a parent
// entry, changing both writes two — but never a duplicate "estimate 3
// became estimate 3".
func TestTaskHistory_UpdateWithNoRealChangeWritesNothing(t *testing.T) {
	ts := openTestStore(t)
	p, cols := seedProject(t, ts)
	ctx := context.Background()
	task := seedTask(t, ts, p, cols["Backlog"], "body change only", "alice")
	est := 5.0
	if err := ts.Write(ctx, func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, task.ID)
		if err != nil {
			return err
		}
		cur.Estimate = &est
		cur.UpdatedBy = "alice"
		return ts.Tasks().Update(tx, cur, nil)
	}); err != nil {
		t.Fatalf("seed estimate: %v", err)
	}
	afterSeed := len(historyOf(t, ts, p.ID))

	// Update with the SAME estimate and the SAME parent (still nil): no
	// change to either field, so neither guard trips.
	if err := ts.Write(ctx, func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, task.ID)
		if err != nil {
			return err
		}
		cur.Estimate = &est
		cur.UpdatedBy = "bob"
		return ts.Tasks().Update(tx, cur, nil)
	}); err != nil {
		t.Fatalf("update with no real change: %v", err)
	}
	if got := len(historyOf(t, ts, p.ID)); got != afterSeed {
		t.Fatalf("an Update that changed neither estimate nor parent added %d journal rows; the guard must keep it at %d",
			got-afterSeed, afterSeed)
	}

	// Now change ONLY the parent. The delta must be exactly one parent
	// entry, with no estimate row alongside it.
	other := seedTask(t, ts, p, cols["Backlog"], "new parent", "alice")
	beforeReparent := len(historyOf(t, ts, p.ID))
	if err := ts.Write(ctx, func(tx Tx) error {
		cur, err := ts.Tasks().GetByID(tx, task.ID)
		if err != nil {
			return err
		}
		cur.ParentID = &other.ID
		cur.UpdatedBy = "bob"
		return ts.Tasks().Update(tx, cur, nil)
	}); err != nil {
		t.Fatalf("reparent: %v", err)
	}
	added := historyOf(t, ts, p.ID)[beforeReparent:]
	if len(added) != 1 {
		t.Fatalf("the parent-only update added %d journal rows, want 1", len(added))
	}
	if added[0].Kind != HistoryParent {
		t.Fatalf("the parent-only update added a %q entry, want parent", added[0].Kind)
	}
}
