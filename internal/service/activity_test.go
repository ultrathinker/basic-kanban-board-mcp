package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ---------------------------------------------------------------------------
// Idle time and the attention line (KANB-61)
// ---------------------------------------------------------------------------

// pinClock fixes the service clock the board read measures idle time
// against. Writes keep stamping rows with the database clock, so a test
// reads the stored timestamps back and places "now" relative to them.
func (env *testEnv) pinClock(at time.Time) {
	env.svc.(*svc).clock = func(store.Tx) (time.Time, error) { return at, nil }
}

// makeTaskIn inserts a task straight into a column, bypassing move rules
// the idle check does not depend on, and returns it as stored.
func makeTaskIn(t *testing.T, env *testEnv, col *domain.Column, title string, mutate func(*domain.Task)) *domain.Task {
	t.Helper()
	var out *domain.Task
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		seq, err := env.Projects().NextTaskSeq(tx, env.proj.ID)
		if err != nil {
			return err
		}
		task := &domain.Task{
			ID:        uuid.NewString(),
			Key:       domain.TaskKey(env.proj.Key, seq),
			ProjectID: env.proj.ID,
			ColumnID:  col.ID,
			Rank:      domain.RankStep,
			Title:     title,
			Type:      domain.TypeTask,
			CreatedBy: env.actor.Name,
			UpdatedBy: env.actor.Name,
		}
		if mutate != nil {
			mutate(task)
		}
		if err := env.Tasks().Create(tx, task); err != nil {
			return err
		}
		out, err = env.Tasks().GetByID(tx, task.ID)
		return err
	}); err != nil {
		t.Fatalf("makeTaskIn %s: %v", col.Name, err)
	}
	return out
}

// rowMovement is the newest movement the task row itself records.
func rowMovement(task *domain.Task) time.Time {
	return lastMovement(task, time.Time{}, time.Time{})
}

func (env *testEnv) boardProject(t *testing.T, in BoardGetInput) BoardProject {
	t.Helper()
	in.ProjectKey = env.proj.Key
	b, err := env.svc.BoardGet(context.Background(), env.actor, in)
	if err != nil {
		t.Fatalf("BoardGet: %v", err)
	}
	if len(b.Projects) != 1 {
		t.Fatalf("BoardGet returned %d projects, want 1", len(b.Projects))
	}
	return b.Projects[0]
}

func (env *testEnv) addNoteAt(t *testing.T, task *domain.Task, at time.Time) {
	t.Helper()
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		return env.Notes().Add(tx, &domain.Note{ID: uuid.NewString(), TaskID: task.ID, Author: "worker", Body: "still on it", CreatedAt: at})
	}); err != nil {
		t.Fatalf("add note: %v", err)
	}
}

func (env *testEnv) addMarkAt(t *testing.T, task *domain.Task, percent int, at time.Time) {
	t.Helper()
	id := task.ID
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		return env.Progress().Add(tx, &domain.ProgressMark{ID: uuid.NewString(), ProjectID: env.proj.ID, TaskID: &id, Assessor: "worker", Percent: percent, CreatedAt: at})
	}); err != nil {
		t.Fatalf("add mark: %v", err)
	}
}

func attentionKeys(a *Attention) []string {
	if a == nil {
		return nil
	}
	out := make([]string, len(a.Sample))
	for i, it := range a.Sample {
		out[i] = it.Key
	}
	return out
}

// The threshold is the project's claim TTL, and "older than" is strict: a
// card one second short of it, or exactly at it, is not stale; one second
// past it is. Both summary and tasks views carry the line.
func TestAttention_ThresholdIsTheClaimTTL(t *testing.T) {
	env := openTestEnv(t)
	task := makeTaskIn(t, env, env.cols["Doing"], "working", nil)
	ttl := time.Duration(env.proj.ClaimTTLSeconds) * time.Second
	t0 := rowMovement(task)

	cases := []struct {
		name  string
		now   time.Time
		stale bool
	}{
		{"one second younger than the ttl", t0.Add(ttl - time.Second), false},
		{"exactly the ttl", t0.Add(ttl), false},
		{"one second older than the ttl", t0.Add(ttl + time.Second), true},
	}
	for _, tc := range cases {
		for _, view := range []BoardView{ViewTasks, ViewSummary} {
			env.pinClock(tc.now)
			bp := env.boardProject(t, BoardGetInput{View: view})
			if !tc.stale {
				if bp.Attention != nil {
					t.Errorf("%s, view %s: attention %+v, want none", tc.name, view, *bp.Attention)
				}
				continue
			}
			if bp.Attention == nil || bp.Attention.Count != 1 || len(bp.Attention.Sample) != 1 {
				t.Fatalf("%s, view %s: attention %+v, want exactly %s", tc.name, view, bp.Attention, task.Key)
			}
			if got := bp.Attention.Sample[0]; got.Key != task.Key || got.Idle != ttl+time.Second {
				t.Errorf("%s, view %s: sample %+v, want %s idle %v", tc.name, view, got, task.Key, ttl+time.Second)
			}
		}
	}
}

// A card parked in waiting is parked on purpose, and a backlog card has not
// started: neither is ever stale, however old. The stale active card beside
// them proves the check actually ran on this board.
func TestAttention_WaitingAndBacklogNeverCount(t *testing.T) {
	env := openTestEnv(t)
	addWaitingColumn(t, env)
	waiting := makeTaskIn(t, env, env.cols["Waiting"], "parked", nil)
	backlog := makeTaskIn(t, env, env.cols["Backlog"], "queued", nil)
	doing := makeTaskIn(t, env, env.cols["Doing"], "forgotten", nil)

	env.pinClock(rowMovement(doing).Add(30 * 24 * time.Hour))
	bp := env.boardProject(t, BoardGetInput{})
	keys := attentionKeys(bp.Attention)
	if len(keys) != 1 || keys[0] != doing.Key || bp.Attention.Count != 1 {
		t.Fatalf("attention = %v, want only the active card %s (waiting %s and backlog %s never count)", keys, doing.Key, waiting.Key, backlog.Key)
	}
}

// A note or a progress mark is movement even though neither touches the
// task row: a fresh one takes an old card off the line.
func TestAttention_FreshNoteOrMarkClearsAnOldCard(t *testing.T) {
	for _, source := range []string{"note", "progress mark"} {
		t.Run(source, func(t *testing.T) {
			env := openTestEnv(t)
			task := makeTaskIn(t, env, env.cols["Doing"], "long job", nil)
			ttl := time.Duration(env.proj.ClaimTTLSeconds) * time.Second
			fresh := rowMovement(task).Add(3 * ttl)
			env.pinClock(fresh.Add(ttl - time.Second))

			if bp := env.boardProject(t, BoardGetInput{}); bp.Attention == nil {
				t.Fatalf("before the %s: the card is 4h-1s old on its row and must be on the line", source)
			}

			if source == "note" {
				env.addNoteAt(t, task, fresh)
			} else {
				env.addMarkAt(t, task, 40, fresh)
			}
			if bp := env.boardProject(t, BoardGetInput{}); bp.Attention != nil {
				t.Fatalf("after a %s %v before now: attention %+v, want none", source, ttl-time.Second, *bp.Attention)
			}

			// ...and the card comes back once the note itself is older than
			// the ttl, so the fresh source did not simply switch the check off.
			env.pinClock(fresh.Add(ttl + time.Second))
			bp := env.boardProject(t, BoardGetInput{})
			if bp.Attention == nil || bp.Attention.Sample[0].Idle != ttl+time.Second {
				t.Fatalf("the %s aged past the ttl: attention %+v, want idle %v measured from it", source, bp.Attention, ttl+time.Second)
			}
		})
	}
}

// The line follows the board filter, like the column counts beside it: a
// read narrowed to someone else's cards does not name mine.
func TestAttention_FollowsTheFilter(t *testing.T) {
	env := openTestEnv(t)
	mine := "me"
	task := makeTaskIn(t, env, env.cols["Doing"], "mine", func(tk *domain.Task) { tk.Assignee = &mine })
	env.pinClock(rowMovement(task).Add(48 * time.Hour))

	other := "someone-else"
	if bp := env.boardProject(t, BoardGetInput{Filter: BoardFilter{Assignee: &other}}); bp.Attention != nil {
		t.Errorf("filtered to %s: attention %+v, want none", other, *bp.Attention)
	}
	if bp := env.boardProject(t, BoardGetInput{Filter: BoardFilter{Assignee: &mine}}); bp.Attention == nil {
		t.Errorf("filtered to %s: the stale card is missing from attention", mine)
	}
}

// lastMovement takes the newest of every source; dropping any one of them
// makes this table fail on the row where that source is the newest.
func TestLastMovement_EachSourceCanWin(t *testing.T) {
	base := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	later := base.Add(time.Hour)
	cases := []struct {
		name           string
		entered, upd   time.Time
		claimed        *time.Time
		noteAt, markAt time.Time
	}{
		{"column entry", later, base, nil, base, base},
		{"edit", base, later, nil, base, base},
		{"claim or renewal", base, base, &later, base, base},
		{"note", base, base, &base, later, base},
		{"progress mark", base, base, nil, time.Time{}, later},
	}
	for _, tc := range cases {
		task := &domain.Task{ColumnEnteredAt: tc.entered, UpdatedAt: tc.upd, ClaimedAt: tc.claimed}
		if got := lastMovement(task, tc.noteAt, tc.markAt); !got.Equal(later) {
			t.Errorf("%s newest: lastMovement = %v, want %v", tc.name, got, later)
		}
	}
}

func TestBuildAttention_OrdersSamplesAndCounts(t *testing.T) {
	h := time.Hour
	idle := []IdleTask{
		{"BMB-1", 2 * h}, {"BMB-2", 30 * time.Minute}, {"BMB-3", 5 * h}, {"BMB-4", 26 * h},
		{"BMB-5", 3 * h}, {"BMB-6", 3 * h}, {"BMB-7", 4 * h}, {"BMB-8", h + time.Second}, {"BMB-9", h},
	}
	a := buildAttention(idle, h)
	if a == nil {
		t.Fatal("attention is nil, want 7 stale cards")
	}
	if a.Count != 7 {
		t.Errorf("Count = %d, want 7 (BMB-2 is fresh, BMB-9 sits exactly at the threshold)", a.Count)
	}
	want := []string{"BMB-4", "BMB-3", "BMB-7", "BMB-5", "BMB-6"}
	got := attentionKeys(a)
	if len(got) != len(want) {
		t.Fatalf("sample = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample = %v, want %v (most idle first, ties by key)", got, want)
		}
	}
	if buildAttention([]IdleTask{{"BMB-1", h}}, h) != nil {
		t.Error("no card past the threshold must give nil, not an empty attention")
	}
}

// ---------------------------------------------------------------------------
// include:["progress"]
// ---------------------------------------------------------------------------

func TestBoardProgress_ActiveCardsOnly(t *testing.T) {
	env := openTestEnv(t)
	accept := func(tk *domain.Task) {
		tk.Acceptance = []domain.AcceptanceItem{{Text: "a", Done: true}, {Text: "b"}, {Text: "c", Done: true}}
	}
	doing := makeTaskIn(t, env, env.cols["Doing"], "doing", accept)
	bare := makeTaskIn(t, env, env.cols["Doing"], "no criteria, no marks", nil)
	backlog := makeTaskIn(t, env, env.cols["Backlog"], "queued", accept)
	markAt := rowMovement(doing).Add(10 * time.Minute)
	env.addMarkAt(t, doing, 40, markAt.Add(-time.Minute))
	env.addMarkAt(t, doing, 60, markAt)
	env.pinClock(markAt.Add(25 * time.Minute))

	bp := env.boardProject(t, BoardGetInput{Include: Includes{IncludeProgress}})
	got, ok := bp.Activity[doing.Key]
	if !ok {
		t.Fatalf("activity has no entry for the active card %s: %+v", doing.Key, bp.Activity)
	}
	if got.AcceptanceDone != 2 || got.AcceptanceTotal != 3 {
		t.Errorf("acceptance = %d/%d, want 2/3", got.AcceptanceDone, got.AcceptanceTotal)
	}
	if got.Percent == nil || *got.Percent != 60 {
		t.Errorf("percent = %v, want 60 (the assessor's latest mark, not the first)", got.Percent)
	}
	if !got.LastActivityAt.Equal(markAt) || got.Idle != 25*time.Minute {
		t.Errorf("last activity %v idle %v, want %v and 25m", got.LastActivityAt, got.Idle, markAt)
	}
	if b, ok := bp.Activity[bare.Key]; !ok || b.Percent != nil || b.AcceptanceTotal != 0 {
		t.Errorf("unassessed card: %+v (present %v), want an entry with nil percent and 0/0", b, ok)
	}
	if _, ok := bp.Activity[backlog.Key]; ok {
		t.Errorf("backlog card %s has an activity entry; progress covers active cards only", backlog.Key)
	}

	if bp := env.boardProject(t, BoardGetInput{}); bp.Activity != nil {
		t.Errorf("without include progress: activity %+v, want nil", bp.Activity)
	}
}

func TestBoardProgress_SummaryViewRefused(t *testing.T) {
	env := openTestEnv(t)
	_, err := env.svc.BoardGet(context.Background(), env.actor, BoardGetInput{
		ProjectKey: env.proj.Key, View: ViewSummary, Include: Includes{IncludeProgress},
	})
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeValidation || de.Remediation == "" {
		t.Fatalf("err = %v, want a validation error with a remediation", err)
	}
}

// ---------------------------------------------------------------------------
// task_get: the epic summary
// ---------------------------------------------------------------------------

func TestTaskGet_SubtaskKinds(t *testing.T) {
	env := openTestEnv(t)
	addWaitingColumn(t, env)
	parent := makeTaskIn(t, env, env.cols["Doing"], "epic", nil)
	leaf := makeTaskIn(t, env, env.cols["Backlog"], "leaf", nil)
	child := func(col string) *domain.Task {
		return makeTaskIn(t, env, env.cols[col], "child in "+col, func(tk *domain.Task) { tk.ParentID = &parent.ID })
	}
	child("Backlog")
	child("Backlog")
	child("Doing")
	child("Waiting")
	child("Done")
	child("Done")
	child("Done")
	archived := child("Waiting")
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		return env.Tasks().Archive(tx, archived.ID, true, env.actor.Name)
	}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	res, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{Keys: []string{parent.Key, leaf.Key}})
	if err != nil {
		t.Fatalf("TaskGet: %v", err)
	}
	want := SubtaskKinds{Backlog: 2, Active: 1, Waiting: 1, Done: 3}
	if got, ok := res.Subtasks[parent.Key]; !ok || got != want {
		t.Errorf("parent subtasks = %+v (present %v), want %+v — the archived second waiting child must not count", got, ok, want)
	}
	if _, ok := res.Subtasks[leaf.Key]; ok {
		t.Errorf("a task without subtasks has a breakdown: %+v", res.Subtasks[leaf.Key])
	}
}
