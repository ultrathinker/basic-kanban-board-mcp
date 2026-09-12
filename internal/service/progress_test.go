package service

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// progressTestBase anchors every test timestamp. Offsets are whole seconds so
// the stored millisecond format keeps the intended order.
var progressTestBase = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// addProgressMark appends one mark through the store. The service reads
// aggregates; the marks themselves are written at the storage boundary, the
// same way any assessing agent would reach them.
func addProgressMark(t *testing.T, env *testEnv, id string, taskID *string, assessor string, percent int, at time.Time) {
	t.Helper()
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		return env.Progress().Add(tx, &domain.ProgressMark{
			ID:        id,
			ProjectID: env.proj.ID,
			TaskID:    taskID,
			Assessor:  assessor,
			Percent:   percent,
			CreatedAt: at,
		})
	}); err != nil {
		t.Fatalf("add progress mark %s: %v", id, err)
	}
}

// moveTaskToColumn moves a task through the service itself, so the version
// bump and column bookkeeping behave exactly as in production.
func moveTaskToColumn(t *testing.T, env *testEnv, key, column string) {
	t.Helper()
	got, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{Keys: []string{key}})
	if err != nil {
		t.Fatalf("task_get %s: %v", key, err)
	}
	if len(got.Tasks) != 1 {
		t.Fatalf("task_get %s: %d tasks, want 1", key, len(got.Tasks))
	}
	v := got.Tasks[0].Version
	if _, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{Key: key, IfVersion: &v, Column: column}},
	}); err != nil {
		t.Fatalf("move %s to %s: %v", key, column, err)
	}
}

// percentString renders an optional percent for failure messages: "nil" for
// no value, the number otherwise. Printing the pointer itself would show an
// address, which is no help to anyone reading a failure.
func percentString(p *int) string {
	if p == nil {
		return "nil"
	}
	return strconv.Itoa(*p)
}

// ---------------------------------------------------------------------------
// 1. Summary progress of a task = mean of the LATEST mark of each assessor.
// A fifty-revision track still counts exactly once.
// ---------------------------------------------------------------------------

func TestTaskProgress_MeanOfLatestPerAssessor(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	t1 := makeBacklogTask(t, env, "assessed by three")
	t2 := makeBacklogTask(t, env, "fifty-revision track")

	// t1: alpha revises 10 -> 50 -> 80 (only 80 counts), beta 30 -> 60,
	// gamma says 100 once. Latest marks: 80, 60, 100 -> mean 80.
	addProgressMark(t, env, "m1", &t1.ID, "alpha", 10, progressTestBase)
	addProgressMark(t, env, "m2", &t1.ID, "alpha", 50, progressTestBase.Add(time.Second))
	addProgressMark(t, env, "m3", &t1.ID, "alpha", 80, progressTestBase.Add(2*time.Second))
	addProgressMark(t, env, "m4", &t1.ID, "beta", 30, progressTestBase)
	addProgressMark(t, env, "m5", &t1.ID, "beta", 60, progressTestBase.Add(time.Second))
	addProgressMark(t, env, "m6", &t1.ID, "gamma", 100, progressTestBase)

	// t2: fifty revisions 0..49 from one assessor. The track is long; the
	// mean must see exactly one mark, the newest (49) — not the average 24.5.
	for i := 0; i < 50; i++ {
		addProgressMark(t, env, "d"+string(rune('a'+i/26))+string(rune('a'+i%26)),
			&t2.ID, "delta", i, progressTestBase.Add(time.Duration(i)*time.Millisecond))
	}

	got, err := env.svc.TaskProgress(ctx, env.actor, TaskProgressInput{
		ProjectKey: env.proj.Key,
		Keys:       []string{t1.Key, t2.Key, "BMB-999"},
	})
	if err != nil {
		t.Fatalf("TaskProgress: %v", err)
	}
	if len(got.Items) != 2 || len(got.NotFound) != 1 || got.NotFound[0] != "BMB-999" {
		t.Fatalf("items=%+v notfound=%v, want 2 items in order and [BMB-999]", got.Items, got.NotFound)
	}
	if got.Items[0].Key != t1.Key || got.Items[1].Key != t2.Key {
		t.Fatalf("items out of requested order: %s, %s", got.Items[0].Key, got.Items[1].Key)
	}
	if got.Items[0].Percent == nil || *got.Items[0].Percent != 80 {
		t.Fatalf("t1 percent = %s, want 80 (mean of the three latest: 80, 60, 100)", percentString(got.Items[0].Percent))
	}
	if got.Items[0].Assessors != 3 {
		t.Fatalf("t1 assessors = %d, want 3", got.Items[0].Assessors)
	}
	if got.Items[1].Percent == nil || *got.Items[1].Percent != 49 {
		t.Fatalf("t2 percent = %s, want 49 (the newest of the 50-revision track, counted once)", percentString(got.Items[1].Percent))
	}
	if got.Items[1].Assessors != 1 {
		t.Fatalf("t2 assessors = %d, want 1", got.Items[1].Assessors)
	}
}

// ---------------------------------------------------------------------------
// 2. Nobody assessed = no value, distinct in the type from an assessed 0%.
// ---------------------------------------------------------------------------

func TestTaskProgress_NoMarksIsDistinctFromZero(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	untouched := makeBacklogTask(t, env, "nobody assessed this")
	atZero := makeBacklogTask(t, env, "assessed at exactly 0")
	addProgressMark(t, env, "z1", &atZero.ID, "alpha", 0, progressTestBase)

	got, err := env.svc.TaskProgress(ctx, env.actor, TaskProgressInput{
		ProjectKey: env.proj.Key,
		Keys:       []string{untouched.Key, atZero.Key},
	})
	if err != nil {
		t.Fatalf("TaskProgress: %v", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(got.Items))
	}
	if got.Items[0].Percent != nil {
		t.Fatalf("untouched task percent = %v, want nil: nobody assessed it", got.Items[0].Percent)
	}
	if got.Items[0].Assessors != 0 {
		t.Fatalf("untouched task assessors = %d, want 0", got.Items[0].Assessors)
	}
	if got.Items[1].Percent == nil {
		t.Fatal("assessed task percent = nil, want a value: an assessed 0% must not look like no data")
	}
	if *got.Items[1].Percent != 0 {
		t.Fatalf("assessed task percent = %d, want 0", *got.Items[1].Percent)
	}
}

// ---------------------------------------------------------------------------
// 3. Manual project progress uses ONLY the project-level marks. Task marks
// never bleed in — that contamination would destroy the manual-vs-auto
// comparison the pair exists for.
// ---------------------------------------------------------------------------

func TestProjectProgress_ManualIgnoresTaskMarks(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	task := makeBacklogTask(t, env, "task-level opinions do not count")
	addProgressMark(t, env, "t1", &task.ID, "alpha", 100, progressTestBase)
	// Project-level: alpha 20 (revised from 90), beta 40. Expected manual =
	// mean(20, 40) = 30. If the task mark bled in, alpha's latest would look
	// like 100 and the mean would move.
	addProgressMark(t, env, "p1", nil, "alpha", 90, progressTestBase)
	addProgressMark(t, env, "p2", nil, "alpha", 20, progressTestBase.Add(time.Second))
	addProgressMark(t, env, "p3", nil, "beta", 40, progressTestBase)

	got, err := env.svc.ProjectProgress(ctx, env.actor, ProjectProgressInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("ProjectProgress: %v", err)
	}
	if got.Manual == nil {
		t.Fatal("manual progress = nil, want 30")
	}
	if *got.Manual != 30 {
		t.Fatalf("manual progress = %d, want 30 (mean of project-level latest only: 20, 40)", *got.Manual)
	}
	if got.ManualAssessors != 2 {
		t.Fatalf("manual assessors = %d, want 2", got.ManualAssessors)
	}
}

// ---------------------------------------------------------------------------
// 4. Auto progress = the share of tasks in done-kind columns, from the board.
// A project without tasks reports no value, not zero.
// ---------------------------------------------------------------------------

func TestProjectProgress_AutoShareAndEmptyProject(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	keys := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		task := makeBacklogTask(t, env, "task "+string(rune('A'+i)))
		keys = append(keys, task.Key)
	}
	moveTaskToColumn(t, env, keys[0], "Done")
	moveTaskToColumn(t, env, keys[1], "Done")

	got, err := env.svc.ProjectProgress(ctx, env.actor, ProjectProgressInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("ProjectProgress: %v", err)
	}
	if got.Auto == nil {
		t.Fatal("auto progress = nil, want 50")
	}
	if *got.Auto != 50 {
		t.Fatalf("auto progress = %s, want 50 (2 of 4 tasks done)", percentString(got.Auto))
	}
	if got.DoneTasks != 2 || got.TotalTasks != 4 {
		t.Fatalf("done/total = %d/%d, want 2/4", got.DoneTasks, got.TotalTasks)
	}

	// Everything done reads 100, not "no data".
	moveTaskToColumn(t, env, keys[2], "Done")
	moveTaskToColumn(t, env, keys[3], "Done")
	got, err = env.svc.ProjectProgress(ctx, env.actor, ProjectProgressInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("ProjectProgress (all done): %v", err)
	}
	if got.Auto == nil || *got.Auto != 100 {
		t.Fatalf("auto progress = %s, want 100", percentString(got.Auto))
	}

	// A project with no tasks has no measured progress: Auto and Manual are
	// both nil — not 0, and certainly no division by zero.
	if err := env.Write(ctx, func(tx store.Tx) error {
		return env.Projects().Create(tx, &domain.Project{
			ID: "empty-project", Key: "EMPTY", Name: "Empty",
			Version: 1, NextTaskSeq: 1, EstimateUnit: "h",
			EnforceDependencies: true, StrictDone: false, ClaimTTLSeconds: 3600,
		})
	}); err != nil {
		t.Fatalf("seed empty project: %v", err)
	}
	empty, err := env.svc.ProjectProgress(ctx, env.actor, ProjectProgressInput{ProjectKey: "EMPTY"})
	if err != nil {
		t.Fatalf("ProjectProgress (empty): %v", err)
	}
	if empty.Auto != nil {
		t.Fatalf("empty project auto = %s, want nil", percentString(empty.Auto))
	}
	if empty.Manual != nil {
		t.Fatalf("empty project manual = %s, want nil", percentString(empty.Manual))
	}
	if empty.TotalTasks != 0 || empty.DoneTasks != 0 {
		t.Fatalf("empty project totals = %d/%d, want 0/0", empty.DoneTasks, empty.TotalTasks)
	}
}

// ---------------------------------------------------------------------------
// 5. Rounding: halves go UP, everywhere, pinned as a table and once through
// the real service path.
// ---------------------------------------------------------------------------

func TestRoundMeanHalfUp(t *testing.T) {
	cases := []struct {
		sum, n, want int
	}{
		{1, 2, 1},    // 0.5 -> 1 (the rule itself)
		{3, 2, 2},    // 1.5 -> 2
		{5, 2, 3},    // 2.5 -> 3
		{12, 8, 2},   // 1.5 -> 2
		{2, 4, 1},    // 0.5 -> 1
		{1, 3, 0},    // 0.33.. rounds down
		{2, 3, 1},    // 0.67.. rounds up
		{240, 3, 80}, // exact mean stays exact
		{0, 5, 0},    // measured zero stays zero
		{1, 8, 0},    // 0.125 rounds down
	}
	for _, tc := range cases {
		if got := RoundMeanHalfUp(tc.sum, tc.n); got != tc.want {
			t.Errorf("RoundMeanHalfUp(%d, %d) = %d, want %d", tc.sum, tc.n, got, tc.want)
		}
	}

	// The same rule on the live path: two assessors at 50 and 51 average
	// 50.5, and the service must say 51, not 50.
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "half-up on the wire")
	addProgressMark(t, env, "h1", &task.ID, "alpha", 50, progressTestBase)
	addProgressMark(t, env, "h2", &task.ID, "beta", 51, progressTestBase)
	got, err := env.svc.TaskProgress(context.Background(), env.actor, TaskProgressInput{
		ProjectKey: env.proj.Key,
		Keys:       []string{task.Key},
	})
	if err != nil {
		t.Fatalf("TaskProgress: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].Percent == nil || *got.Items[0].Percent != 51 {
		t.Fatalf("service rounding = %+v, want 51 (50.5 halves up)", got.Items)
	}
}

// ---------------------------------------------------------------------------
// 6. Batch read: the store-call count is fixed, never a query per task.
// ---------------------------------------------------------------------------

// countingStore wraps the real store and counts every repository call the
// service could plausibly make on the progress read paths. Every one of them
// is counted, so a per-task query hiding inside the service shows up
// immediately instead of delegating invisibly to the embedded repo.
type countingStore struct {
	store.Store
	calls int
}

func (c *countingStore) Projects() store.ProjectRepo {
	return &countingProjects{c.Store.Projects(), c}
}

type countingProjects struct {
	store.ProjectRepo
	c *countingStore
}

func (p *countingProjects) GetByKey(tx store.Tx, key string) (*domain.Project, error) {
	p.c.calls++
	return p.ProjectRepo.GetByKey(tx, key)
}

func (p *countingProjects) GetByID(tx store.Tx, id string) (*domain.Project, error) {
	p.c.calls++
	return p.ProjectRepo.GetByID(tx, id)
}

func (c *countingStore) Tasks() store.TaskRepo {
	return &countingTasks{c.Store.Tasks(), c}
}

type countingTasks struct {
	store.TaskRepo
	c *countingStore
}

func (r *countingTasks) GetByKey(tx store.Tx, key string) (*domain.Task, error) {
	r.c.calls++
	return r.TaskRepo.GetByKey(tx, key)
}

func (r *countingTasks) GetManyByKeys(tx store.Tx, keys []string) (map[string]*domain.Task, error) {
	r.c.calls++
	return r.TaskRepo.GetManyByKeys(tx, keys)
}

func (r *countingTasks) List(tx store.Tx, f store.TaskFilter) ([]*domain.Task, error) {
	r.c.calls++
	return r.TaskRepo.List(tx, f)
}

func (c *countingStore) Progress() store.ProgressRepo {
	return &countingProgress{c.Store.Progress(), c}
}

type countingProgress struct {
	store.ProgressRepo
	c *countingStore
}

func (r *countingProgress) LatestByTask(tx store.Tx, projectID string) (map[string][]domain.ProgressMark, error) {
	r.c.calls++
	return r.ProgressRepo.LatestByTask(tx, projectID)
}

func (r *countingProgress) LatestByAssessor(tx store.Tx, projectID string, taskID *string) ([]domain.ProgressMark, error) {
	r.c.calls++
	return r.ProgressRepo.LatestByAssessor(tx, projectID, taskID)
}

func (r *countingProgress) History(tx store.Tx, projectID string, taskID *string) ([]domain.ProgressMark, error) {
	r.c.calls++
	return r.ProgressRepo.History(tx, projectID, taskID)
}

func TestTaskProgress_BatchStoreCallsDoNotScale(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	first := makeBacklogTask(t, env, "the only card on a small board")
	addProgressMark(t, env, "b1", &first.ID, "alpha", 25, progressTestBase)

	cs := &countingStore{Store: env.Store}
	svc := New(cs, nil)

	one, err := svc.TaskProgress(ctx, env.actor, TaskProgressInput{
		ProjectKey: env.proj.Key,
		Keys:       []string{first.Key},
	})
	if err != nil {
		t.Fatalf("TaskProgress (1 task): %v", err)
	}
	if len(one.Items) != 1 || one.Items[0].Percent == nil || *one.Items[0].Percent != 25 {
		t.Fatalf("batch result wrong through the counting store: %+v", one.Items)
	}
	smallBoard := cs.calls
	if smallBoard == 0 {
		t.Fatal("no store calls recorded; the counter wraps nothing")
	}

	for i := 0; i < 9; i++ {
		extra := makeBacklogTask(t, env, "filler card")
		addProgressMark(t, env, "b"+strings.Repeat("x", i+1), &extra.ID, "assessor", i*10, progressTestBase)
	}
	keys := []string{first.Key}
	for i := 0; i < 9; i++ {
		keys = append(keys, domain.TaskKey(env.proj.Key, i+2))
	}
	cs.calls = 0
	big, err := svc.TaskProgress(ctx, env.actor, TaskProgressInput{
		ProjectKey: env.proj.Key,
		Keys:       keys,
	})
	if err != nil {
		t.Fatalf("TaskProgress (10 tasks): %v", err)
	}
	if len(big.Items) != 10 {
		t.Fatalf("items = %d, want 10", len(big.Items))
	}
	if cs.calls != smallBoard {
		t.Fatalf("store calls grew with the task count: %d for 1 task, %d for 10 tasks", smallBoard, cs.calls)
	}
	t.Logf("store calls for the batch read: %d (1 task) == %d (10 tasks)", smallBoard, cs.calls)
}

// ---------------------------------------------------------------------------
// 7. Track deletion proxies to the store as-is: keys become ids, the write
// scope applies, and no extra rules appear.
// ---------------------------------------------------------------------------

func TestProgressTrackDelete_ProxiesToStore(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	task := makeBacklogTask(t, env, "has two tracks")
	addProgressMark(t, env, "x1", &task.ID, "alpha", 10, progressTestBase)
	addProgressMark(t, env, "x2", &task.ID, "alpha", 20, progressTestBase.Add(time.Second))
	addProgressMark(t, env, "x3", &task.ID, "beta", 70, progressTestBase)
	addProgressMark(t, env, "x4", nil, "alpha", 30, progressTestBase)

	// A read-only token cannot delete a track: it is a mutation.
	reader := env.actor
	reader.Scopes = domain.Scopes{domain.ScopeRead}
	if _, err := env.svc.ProgressTrackDelete(ctx, reader, ProgressTrackDeleteInput{
		ProjectKey: env.proj.Key, TaskKey: task.Key, Assessor: "alpha",
	}); err == nil {
		t.Fatal("read-only actor deleted a track; want forbidden")
	} else if !strings.Contains(err.Error(), "write") {
		t.Fatalf("error for read-only actor should name the missing scope: %v", err)
	}

	got, err := env.svc.ProgressTrackDelete(ctx, env.actor, ProgressTrackDeleteInput{
		ProjectKey: env.proj.Key, TaskKey: task.Key, Assessor: "alpha",
	})
	if err != nil {
		t.Fatalf("ProgressTrackDelete: %v", err)
	}
	if got.Removed != 2 {
		t.Fatalf("removed %d marks, want 2 (alpha's whole task track)", got.Removed)
	}
	// Beta's track and the project-level alpha track survive.
	if err := env.Read(ctx, func(tx store.Tx) error {
		hist, err := env.Progress().History(tx, env.proj.ID, &task.ID)
		if err != nil {
			return err
		}
		if len(hist) != 1 || hist[0].Assessor != "beta" {
			t.Fatalf("task history after track delete = %+v, want [beta only]", hist)
		}
		proj, err := env.Progress().History(tx, env.proj.ID, nil)
		if err != nil {
			return err
		}
		if len(proj) != 1 || proj[0].Assessor != "alpha" {
			t.Fatalf("project-level history after task-track delete = %+v, want [alpha only]", proj)
		}
		return nil
	}); err != nil {
		t.Fatalf("read after delete: %v", err)
	}

	// The project-level track deletes through the same door with no task key.
	got, err = env.svc.ProgressTrackDelete(ctx, env.actor, ProgressTrackDeleteInput{
		ProjectKey: env.proj.Key, Assessor: "alpha",
	})
	if err != nil {
		t.Fatalf("ProgressTrackDelete (project track): %v", err)
	}
	if got.Removed != 1 {
		t.Fatalf("removed %d project-level marks, want 1", got.Removed)
	}

	// An unknown assessor is a clean no-op, exactly like the store's own
	// behaviour — the proxy adds no rules, including no new errors.
	got, err = env.svc.ProgressTrackDelete(ctx, env.actor, ProgressTrackDeleteInput{
		ProjectKey: env.proj.Key, TaskKey: task.Key, Assessor: "nobody",
	})
	if err != nil {
		t.Fatalf("ProgressTrackDelete (unknown assessor): %v", err)
	}
	if got.Removed != 0 {
		t.Fatalf("unknown assessor removed %d marks, want 0", got.Removed)
	}
}
