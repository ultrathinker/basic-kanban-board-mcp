package service

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------
//
// Every service test gets a fresh temp-file SQLite store so the two
// pools, WAL and BEGIN IMMEDIATE serialization all behave the way they
// do in production. :memory: would skip WAL entirely and hide the
// concurrency contract — exactly what we are trying to test.

type testEnv struct {
	store.Store
	svc   Service
	proj  *domain.Project
	cols  map[string]*domain.Column
	actor Actor
}

func openTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kanban.db")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := store.Open(ctx, store.Config{Path: dbPath, ReadPoolSize: 4})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	env := &testEnv{
		Store: st,
		svc:   New(st, nil),
		actor: Actor{
			TokenID: "tok-admin",
			Name:    "test-admin",
			Scopes:  domain.Scopes{domain.ScopeAdmin, domain.ScopeWrite, domain.ScopeRead},
		},
	}
	env.proj, env.cols = seedTestProject(t, env)
	return env
}

// seedTestProject creates a project with the four default columns plus a
// token used to satisfy the idempotency FK.
func seedTestProject(t *testing.T, env *testEnv) (*domain.Project, map[string]*domain.Column) {
	t.Helper()
	p := &domain.Project{
		ID:                  uuid.NewString(),
		Key:                 "BMB",
		Name:                "Test",
		Version:             1,
		NextTaskSeq:         1,
		EstimateUnit:        "h",
		EnforceDependencies: true,
		StrictDone:          false,
		ClaimTTLSeconds:     3600,
	}
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		return env.Projects().Create(tx, p)
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	cols := map[string]*domain.Column{}
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		for i, def := range domain.DefaultColumns {
			c := &domain.Column{
				ID:        uuid.NewString(),
				ProjectID: p.ID,
				Name:      def.Name,
				Position:  i,
				Kind:      def.Kind,
				WIPLimit:  def.WIPLimit,
			}
			if err := env.Columns().Create(tx, c); err != nil {
				return err
			}
			cols[def.Name] = c
		}
		// Seed the token so idempotency records have a valid FK target.
		tok := &domain.Token{
			ID:     env.actor.TokenID,
			Name:   env.actor.Name,
			Hash:   []byte("test-hash"),
			Scopes: env.actor.Scopes,
		}
		return env.Tokens().Create(tx, tok)
	}); err != nil {
		t.Fatalf("seed columns: %v", err)
	}
	return p, cols
}

// makeBacklogTask inserts a single task into the project's Backlog column.
func makeBacklogTask(t *testing.T, env *testEnv, title string) *domain.Task {
	t.Helper()
	var out *domain.Task
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		seq, err := env.Projects().NextTaskSeq(tx, env.proj.ID)
		if err != nil {
			return err
		}
		out = &domain.Task{
			ID:        uuid.NewString(),
			Key:       domain.TaskKey(env.proj.Key, seq),
			ProjectID: env.proj.ID,
			ColumnID:  env.cols["Backlog"].ID,
			Rank:      domain.RankStep,
			Title:     title,
			Type:      domain.TypeTask,
			Priority:  domain.PriorityMedium,
			CreatedBy: env.actor.Name,
			UpdatedBy: env.actor.Name,
		}
		return env.Tasks().Create(tx, out)
	}); err != nil {
		t.Fatalf("makeBacklogTask: %v", err)
	}
	return out
}

// actorWith returns env.actor with extra scoping/permissions applied.
func (env *testEnv) actorWith(_ *testing.T, opts ...func(*Actor)) Actor {
	a := env.actor
	for _, o := range opts {
		o(&a)
	}
	return a
}

func nonAdmin(a *Actor) { a.Scopes = domain.Scopes{domain.ScopeWrite, domain.ScopeRead} }
func restrictedTo(key string) func(*Actor) {
	return func(a *Actor) { a.ProjectKeys = []string{key} }
}

func strPtrLocal(s string) *string { return &s }
func intPtrLocal(i int) *int       { return &i }

// ---------------------------------------------------------------------------
// task_next: peek / claim / start
// ---------------------------------------------------------------------------

func TestTaskNext_PeekReturnsReady(t *testing.T) {
	env := openTestEnv(t)
	makeBacklogTask(t, env, "first")
	makeBacklogTask(t, env, "second")

	res, err := env.svc.TaskNext(context.Background(), env.actor, TaskNextInput{
		ProjectKey: env.proj.Key,
		Action:     NextPeek,
		Limit:      5,
	})
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if len(res.Tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(res.Tasks))
	}
	if res.WIPFull {
		t.Fatalf("WIPFull = true, want false (Doing is empty)")
	}
}

func TestTaskNext_StartMovesAndClaimsAtomically(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "do work")

	res, err := env.svc.TaskNext(context.Background(), env.actor, TaskNextInput{
		ProjectKey: env.proj.Key,
		Action:     NextStart,
		Limit:      1,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if res.StartedKey != task.Key {
		t.Fatalf("StartedKey = %q, want %q", res.StartedKey, task.Key)
	}
	got, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{Keys: []string{task.Key}})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Tasks) != 1 {
		t.Fatalf("get returned %d tasks, want 1", len(got.Tasks))
	}
	view := got.Tasks[0]
	if view.ColumnName != "Doing" {
		t.Fatalf("ColumnName = %q, want Doing", view.ColumnName)
	}
	if view.ClaimedBy == nil || *view.ClaimedBy != env.actor.Name {
		t.Fatalf("ClaimedBy = %v, want %q", view.ClaimedBy, env.actor.Name)
	}
	if view.StartedAt == nil {
		t.Fatalf("StartedAt is nil after start")
	}
}

func TestTaskNext_StartWIPFullRefuses(t *testing.T) {
	env := openTestEnv(t)
	// Seed 3 backlog tasks so we can fill Doing to WIP=3.
	for i := 0; i < 3; i++ {
		makeBacklogTask(t, env, "filler")
	}
	// Fill Doing to WIP=3.
	for i := 0; i < 3; i++ {
		if _, err := env.svc.TaskNext(context.Background(), env.actor, TaskNextInput{
			ProjectKey: env.proj.Key, Action: NextStart, Limit: 1,
		}); err != nil {
			t.Fatalf("filler start %d: %v", i, err)
		}
	}
	// Now add one more in Backlog.
	makeBacklogTask(t, env, "blocked")
	peek, err := env.svc.TaskNext(context.Background(), env.actor, TaskNextInput{
		ProjectKey: env.proj.Key, Action: NextPeek, Limit: 1,
	})
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if !peek.WIPFull {
		t.Fatalf("peek.WIPFull = false, want true (Doing is full)")
	}
	if len(peek.Tasks) == 0 {
		t.Fatalf("peek returned no tasks, but PLAN says peek still returns ready work")
	}
	_, err = env.svc.TaskNext(context.Background(), env.actor, TaskNextInput{
		ProjectKey: env.proj.Key, Action: NextStart, Limit: 1,
	})
	if err == nil {
		t.Fatalf("start succeeded with full WIP, want wip_exceeded")
	}
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeWIPExceeded {
		t.Fatalf("start error code = %v, want wip_exceeded", de)
	}
}

// TestTaskNext_ConcurrentStartExactlyOneWins is the headline concurrency
// test: many goroutines call TaskNext(start) at once against a WIP=1
// column, and exactly one wins.
func TestTaskNext_ConcurrentStartExactlyOneWins(t *testing.T) {
	env := openTestEnv(t)
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		env.cols["Doing"].WIPLimit = intPtrLocal(1)
		return env.Columns().Update(tx, env.cols["Doing"])
	}); err != nil {
		t.Fatalf("lower WIP: %v", err)
	}
	const N = 10
	for i := 0; i < N; i++ {
		makeBacklogTask(t, env, "candidate")
	}

	const goroutines = 20
	var wins int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := env.svc.TaskNext(context.Background(), env.actor, TaskNextInput{
				ProjectKey: env.proj.Key, Action: NextStart, Limit: 1,
			})
			if err == nil && res.StartedKey != "" {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1", wins)
	}
}

// ---------------------------------------------------------------------------
// Version conflict + lease renewal doesn't invalidate if_version
// ---------------------------------------------------------------------------

func TestTaskUpdate_VersionConflictCarriesCurrent(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "x")
	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key:       task.Key,
			IfVersion: intPtrLocal(99),
			Title:     strPtrLocal("stale write"),
		}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].OK {
		t.Fatalf("expected per-item conflict, got %+v", res.Items)
	}
	de := res.Items[0].Err
	if de == nil || de.Code != domain.CodeConflict {
		t.Fatalf("conflict code = %v, want conflict", de)
	}
	if de.Current == nil {
		t.Fatalf("conflict has no Current attached")
	}
}

func TestTaskClaim_RenewDoesNotInvalidateIfVersion(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "x")
	if _, err := env.svc.TaskClaim(context.Background(), env.actor, TaskClaimInput{
		Key: task.Key, Action: ClaimTake,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	after, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{Keys: []string{task.Key}})
	if err != nil || len(after.Tasks) != 1 {
		t.Fatalf("get: %v / %d", err, len(after.Tasks))
	}
	v := after.Tasks[0].Version

	if _, err := env.svc.TaskClaim(context.Background(), env.actor, TaskClaimInput{
		Key: task.Key, Action: ClaimRenew,
	}); err != nil {
		t.Fatalf("renew: %v", err)
	}
	after2, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{Keys: []string{task.Key}})
	if err != nil || len(after2.Tasks) != 1 {
		t.Fatalf("get2: %v / %d", err, len(after2.Tasks))
	}
	if after2.Tasks[0].Version != v {
		t.Fatalf("version changed by renew: %d -> %d", v, after2.Tasks[0].Version)
	}

	other := env.actorWith(t, nonAdmin)
	other.Name = "codex"
	res, err := env.svc.TaskUpdate(context.Background(), other, TaskUpdateInput{
		Patches: []TaskPatch{{Key: task.Key, IfVersion: &v, Title: strPtrLocal("renamed")}},
	})
	if err != nil {
		t.Fatalf("update after renew: %v", err)
	}
	if !res.Items[0].OK {
		t.Fatalf("update after renew failed: %+v", res.Items[0].Err)
	}
}

// ---------------------------------------------------------------------------
// task_create: ref parent + blockers, rollback, idempotency
// ---------------------------------------------------------------------------

func TestTaskCreate_BatchWithRefParentAndBlockers(t *testing.T) {
	env := openTestEnv(t)
	res, err := env.svc.TaskCreate(context.Background(), env.actor, TaskCreateInput{
		Tasks: []NewTask{
			{ProjectKey: env.proj.Key, Title: "schema", Ref: "schema", Type: domain.TypeTask, Priority: domain.PriorityHigh},
			{ProjectKey: env.proj.Key, Title: "validator", Ref: "validator", Type: domain.TypeTask, Priority: domain.PriorityMedium,
				Parent: "@schema"},
			{ProjectKey: env.proj.Key, Title: "ingest", Ref: "ingest", Type: domain.TypeTask, Priority: domain.PriorityMedium,
				BlockedBy: []string{"@schema", "@validator"}},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(res.Tasks) != 3 {
		t.Fatalf("got %d tasks, want 3", len(res.Tasks))
	}
	if res.Replayed {
		t.Fatalf("Replayed = true on fresh create")
	}
	// Map keys to titles so we can verify by title.
	keyToTitle := map[string]string{}
	keyToView := map[string]domain.TaskView{}
	for _, tv := range res.Tasks {
		keyToTitle[tv.Key] = tv.Title
		keyToView[tv.Key] = tv
	}
	// Find ingest by title; it should have 2 open blockers.
	var ingest domain.TaskView
	for _, tv := range res.Tasks {
		if tv.Title == "ingest" {
			ingest = tv
		}
	}
	if len(ingest.BlockedBy) != 2 {
		t.Fatalf("ingest BlockedBy = %v, want 2 entries", ingest.BlockedBy)
	}
	// The validator should have a non-nil parent.
	var validator domain.TaskView
	for _, tv := range res.Tasks {
		if tv.Title == "validator" {
			validator = tv
		}
	}
	if validator.ParentID == nil {
		t.Fatalf("validator.ParentID is nil; want non-nil (schema)")
	}
	_ = keyToTitle
	_ = keyToView
}

func TestTaskCreate_RollbackOnOneBadItem(t *testing.T) {
	env := openTestEnv(t)
	_, err := env.svc.TaskCreate(context.Background(), env.actor, TaskCreateInput{
		Tasks: []NewTask{
			{ProjectKey: env.proj.Key, Title: "good", Type: domain.TypeTask},
			{ProjectKey: env.proj.Key, Title: "bad", Parent: "@does-not-exist", Type: domain.TypeTask},
		},
	})
	if err == nil {
		t.Fatalf("create succeeded; want validation error")
	}
	board, err := env.svc.BoardGet(context.Background(), env.actor, BoardGetInput{ProjectKey: env.proj.Key, View: ViewTasks})
	if err != nil {
		t.Fatalf("board: %v", err)
	}
	for _, bp := range board.Projects {
		for _, col := range bp.Columns {
			if len(col.Tasks) != 0 {
				t.Fatalf("column %q has %d tasks after rollback, want 0", col.Name, len(col.Tasks))
			}
		}
	}
}

func TestTaskCreate_IdempotentReplayReturnsOriginal(t *testing.T) {
	env := openTestEnv(t)
	in := TaskCreateInput{
		Tasks: []NewTask{
			{ProjectKey: env.proj.Key, Title: "first", Type: domain.TypeTask, IdempotencyKey: "k1"},
		},
	}
	first, err := env.svc.TaskCreate(context.Background(), env.actor, in)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if first.Replayed {
		t.Fatalf("first.Replayed = true")
	}
	if len(first.Tasks) != 1 {
		t.Fatalf("first returned %d tasks", len(first.Tasks))
	}
	firstKey := first.Tasks[0].Key

	// Capture the project's seq counter after the first create.
	var seqAfter int
	if err := env.Read(context.Background(), func(tx store.Tx) error {
		p, err := env.Projects().GetByKey(tx, env.proj.Key)
		if err != nil {
			return err
		}
		seqAfter = p.NextTaskSeq
		return nil
	}); err != nil {
		t.Fatalf("read seq: %v", err)
	}

	second, err := env.svc.TaskCreate(context.Background(), env.actor, in)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if !second.Replayed {
		t.Fatalf("second.Replayed = false; want true")
	}
	if len(second.Tasks) != 1 || second.Tasks[0].Key != firstKey {
		t.Fatalf("replay returned different task: %q vs %q", second.Tasks[0].Key, firstKey)
	}

	if err := env.Read(context.Background(), func(tx store.Tx) error {
		p, err := env.Projects().GetByKey(tx, env.proj.Key)
		if err != nil {
			return err
		}
		if p.NextTaskSeq != seqAfter {
			t.Fatalf("NextTaskSeq advanced on replay: %d -> %d", seqAfter, p.NextTaskSeq)
		}
		return nil
	}); err != nil {
		t.Fatalf("read seq 2: %v", err)
	}
}

func TestTaskCreate_IdempotencyMismatchFails(t *testing.T) {
	env := openTestEnv(t)
	in1 := TaskCreateInput{
		Tasks: []NewTask{
			{ProjectKey: env.proj.Key, Title: "first", Type: domain.TypeTask, IdempotencyKey: "k2"},
		},
	}
	if _, err := env.svc.TaskCreate(context.Background(), env.actor, in1); err != nil {
		t.Fatalf("first: %v", err)
	}
	in2 := TaskCreateInput{
		Tasks: []NewTask{
			{ProjectKey: env.proj.Key, Title: "DIFFERENT", Type: domain.TypeTask, IdempotencyKey: "k2"},
		},
	}
	_, err := env.svc.TaskCreate(context.Background(), env.actor, in2)
	if err == nil {
		t.Fatalf("mismatch replay succeeded; want idempotency_mismatch")
	}
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeIdempotencyMismatch {
		t.Fatalf("code = %v, want idempotency_mismatch", de)
	}
}

// ---------------------------------------------------------------------------
// task_update: per-item conflict + atomic rollback
// ---------------------------------------------------------------------------

func TestTaskUpdate_PerItemConflictDoesNotSinkBatch(t *testing.T) {
	env := openTestEnv(t)
	tasks := make([]*domain.Task, 10)
	for i := range tasks {
		tasks[i] = makeBacklogTask(t, env, "task")
	}
	patches := make([]TaskPatch, 10)
	for i, tk := range tasks {
		patches[i] = TaskPatch{Key: tk.Key, IfVersion: intPtrLocal(tk.Version), Title: strPtrLocal("new " + tk.Key)}
	}
	patches[3].IfVersion = intPtrLocal(999)

	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{Patches: patches})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	successes, conflicts := 0, 0
	for _, it := range res.Items {
		if it.OK {
			successes++
		} else if it.Err != nil && it.Err.Code == domain.CodeConflict {
			conflicts++
		}
	}
	if successes != 9 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d, want 9/1", successes, conflicts)
	}
}

func TestTaskUpdate_AtomicBatchRollsBackOnConflict(t *testing.T) {
	env := openTestEnv(t)
	tasks := make([]*domain.Task, 3)
	for i := range tasks {
		tasks[i] = makeBacklogTask(t, env, "task")
	}
	patches := make([]TaskPatch, 3)
	for i, tk := range tasks {
		patches[i] = TaskPatch{Key: tk.Key, IfVersion: intPtrLocal(tk.Version), Title: strPtrLocal("new")}
	}
	patches[1].IfVersion = intPtrLocal(999)

	_, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{Patches: patches, Atomic: true})
	if err == nil {
		t.Fatalf("atomic update with conflict succeeded")
	}
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeConflict {
		t.Fatalf("atomic error code = %v, want conflict", de)
	}
	for _, tk := range tasks {
		got, gerr := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{Keys: []string{tk.Key}})
		if gerr != nil || len(got.Tasks) != 1 {
			t.Fatalf("get %s: %v / %d", tk.Key, gerr, len(got.Tasks))
		}
		if got.Tasks[0].Title != "task" {
			t.Fatalf("%s title = %q, want unchanged 'task'", tk.Key, got.Tasks[0].Title)
		}
	}
}

// ---------------------------------------------------------------------------
// Move validation
// ---------------------------------------------------------------------------

func TestTaskUpdate_MoveBlockedRefuses(t *testing.T) {
	env := openTestEnv(t)
	blocker := makeBacklogTask(t, env, "blocker")
	blocked := makeBacklogTask(t, env, "blocked")
	if _, err := env.svc.TaskLink(context.Background(), env.actor, TaskLinkInput{
		Add: []LinkPair{{Blocker: blocker.Key, Blocked: blocked.Key}},
	}); err != nil {
		t.Fatalf("link: %v", err)
	}
	// Linking bumps the version of both endpoints. Re-read.
	refreshed, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{Keys: []string{blocked.Key}})
	if err != nil || len(refreshed.Tasks) != 1 {
		t.Fatalf("refresh: %v / %d", err, len(refreshed.Tasks))
	}
	blockedV := refreshed.Tasks[0].Version
	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{Key: blocked.Key, IfVersion: &blockedV, Column: "Doing"}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if res.Items[0].OK {
		t.Fatalf("blocked task moved successfully; want blocked error")
	}
	if res.Items[0].Err == nil || res.Items[0].Err.Code != domain.CodeBlocked {
		t.Fatalf("code = %v, want blocked", res.Items[0].Err)
	}
}

func TestTaskUpdate_ForceAllowedForAdmin(t *testing.T) {
	env := openTestEnv(t)
	blocker := makeBacklogTask(t, env, "blocker")
	blocked := makeBacklogTask(t, env, "blocked")
	if _, err := env.svc.TaskLink(context.Background(), env.actor, TaskLinkInput{
		Add: []LinkPair{{Blocker: blocker.Key, Blocked: blocked.Key}},
	}); err != nil {
		t.Fatalf("link: %v", err)
	}
	refreshed, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{Keys: []string{blocked.Key}})
	if err != nil || len(refreshed.Tasks) != 1 {
		t.Fatalf("refresh: %v / %d", err, len(refreshed.Tasks))
	}
	blockedV := refreshed.Tasks[0].Version
	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key: blocked.Key, IfVersion: &blockedV,
			Column: "Doing", Force: true, Reason: "manual override for test",
		}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !res.Items[0].OK {
		t.Fatalf("admin force failed: %+v", res.Items[0].Err)
	}
	if res.Items[0].Task.ColumnName != "Doing" {
		t.Fatalf("ColumnName = %q, want Doing", res.Items[0].Task.ColumnName)
	}
}

func TestTaskUpdate_ForceRefusedForNonAdmin(t *testing.T) {
	env := openTestEnv(t)
	writer := env.actorWith(t, nonAdmin)
	task := makeBacklogTask(t, env, "x")
	res, err := env.svc.TaskUpdate(context.Background(), writer, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key: task.Key, IfVersion: intPtrLocal(task.Version),
			Column: "Doing", Force: true, Reason: "trying anyway",
		}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if res.Items[0].OK {
		t.Fatalf("non-admin force succeeded; want forbidden")
	}
	if res.Items[0].Err == nil || res.Items[0].Err.Code != domain.CodeForbidden {
		t.Fatalf("code = %v, want forbidden", res.Items[0].Err)
	}
}

func TestTaskUpdate_ForceWithoutReasonRefused(t *testing.T) {
	env := openTestEnv(t)
	blocker := makeBacklogTask(t, env, "blocker")
	blocked := makeBacklogTask(t, env, "blocked")
	if _, err := env.svc.TaskLink(context.Background(), env.actor, TaskLinkInput{
		Add: []LinkPair{{Blocker: blocker.Key, Blocked: blocked.Key}},
	}); err != nil {
		t.Fatalf("link: %v", err)
	}
	refreshed, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{Keys: []string{blocked.Key}})
	if err != nil || len(refreshed.Tasks) != 1 {
		t.Fatalf("refresh: %v / %d", err, len(refreshed.Tasks))
	}
	blockedV := refreshed.Tasks[0].Version
	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{
			Key: blocked.Key, IfVersion: &blockedV,
			Column: "Doing", Force: true,
		}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if res.Items[0].OK {
		t.Fatalf("force without reason succeeded")
	}
	if res.Items[0].Err == nil || res.Items[0].Err.Code != domain.CodeValidation {
		t.Fatalf("code = %v, want validation", res.Items[0].Err)
	}
}

// ---------------------------------------------------------------------------
// Scope / project restriction
// ---------------------------------------------------------------------------

func TestTaskCreate_ScopeRestrictedActor(t *testing.T) {
	env := openTestEnv(t)
	otherProj := &domain.Project{
		ID: uuid.NewString(), Key: "OTH", Name: "Other", Version: 1, NextTaskSeq: 1,
		EstimateUnit: "h", EnforceDependencies: true, ClaimTTLSeconds: 3600,
	}
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		return env.Projects().Create(tx, otherProj)
	}); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	scoped := env.actorWith(t, restrictedTo("BMB"))
	if _, err := env.svc.TaskCreate(context.Background(), scoped, TaskCreateInput{
		Tasks: []NewTask{{ProjectKey: "BMB", Title: "ok", Type: domain.TypeTask}},
	}); err != nil {
		t.Fatalf("create in BMB: %v", err)
	}
	_, err := env.svc.TaskCreate(context.Background(), scoped, TaskCreateInput{
		Tasks: []NewTask{{ProjectKey: "OTH", Title: "no", Type: domain.TypeTask}},
	})
	if err == nil {
		t.Fatalf("create in OTH succeeded for restricted actor")
	}
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeForbidden {
		t.Fatalf("code = %v, want forbidden", de)
	}
}

// ---------------------------------------------------------------------------
// Batch invariants — no TOCTOU inside a single call (PLAN §11 invariant 6)
// ---------------------------------------------------------------------------

// moveTo builds a move patch for a task, reading its current version first so
// the caller does not have to track version bumps by hand.
func (env *testEnv) moveTo(t *testing.T, key, column string) TaskPatch {
	t.Helper()
	got, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{Keys: []string{key}})
	if err != nil || len(got.Tasks) != 1 {
		t.Fatalf("read %s: %v / %d", key, err, len(got.Tasks))
	}
	v := got.Tasks[0].Version
	return TaskPatch{Key: key, IfVersion: &v, Column: column}
}

// countIn returns the live occupancy of a column straight from the store, so
// the assertion does not lean on the code path it is checking.
func (env *testEnv) countIn(t *testing.T, col *domain.Column) int {
	t.Helper()
	var n int
	if err := env.Read(context.Background(), func(tx store.Tx) error {
		var err error
		n, err = env.Columns().CountTasks(tx, col.ID, "")
		return err
	}); err != nil {
		t.Fatalf("count %s: %v", col.Name, err)
	}
	return n
}

// TestTaskUpdate_BatchCannotOverfillWIP is the regression test for the
// intra-batch WIP bypass: validating every patch against the pre-batch
// occupancy let two moves share the one free slot, so a limit of 3 ended
// up holding 4.
func TestTaskUpdate_BatchCannotOverfillWIP(t *testing.T) {
	env := openTestEnv(t)
	doing := env.cols["Doing"]
	if doing.WIPLimit == nil || *doing.WIPLimit != 3 {
		t.Fatalf("Doing WIP limit = %v, want 3", doing.WIPLimit)
	}
	tasks := make([]*domain.Task, 4)
	for i := range tasks {
		tasks[i] = makeBacklogTask(t, env, "task")
	}

	// Fill Doing to 2 of 3, one move per call, leaving a single slot.
	for _, tk := range tasks[:2] {
		res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
			Patches: []TaskPatch{env.moveTo(t, tk.Key, "Doing")},
		})
		if err != nil {
			t.Fatalf("seed move %s: %v", tk.Key, err)
		}
		if !res.Items[0].OK {
			t.Fatalf("seed move %s refused: %v", tk.Key, res.Items[0].Err)
		}
	}
	if got := env.countIn(t, doing); got != 2 {
		t.Fatalf("Doing holds %d before the batch, want 2", got)
	}

	// One call, two moves, one free slot.
	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{
			env.moveTo(t, tasks[2].Key, "Doing"),
			env.moveTo(t, tasks[3].Key, "Doing"),
		},
	})
	if err != nil {
		t.Fatalf("batch update: %v", err)
	}
	ok, refused := 0, 0
	for _, it := range res.Items {
		switch {
		case it.OK:
			ok++
		case it.Err != nil && it.Err.Code == domain.CodeWIPExceeded:
			refused++
		default:
			t.Fatalf("unexpected item %+v", it)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("ok=%d wip_exceeded=%d, want 1/1", ok, refused)
	}
	if got := env.countIn(t, doing); got != 3 {
		t.Fatalf("Doing holds %d after the batch, want 3 (the limit)", got)
	}
}

// TestTaskUpdate_AtomicBatchOverfillingWIPLandsNothing is the atomic
// counterpart: the whole call is refused instead of half-applied.
func TestTaskUpdate_AtomicBatchOverfillingWIPLandsNothing(t *testing.T) {
	env := openTestEnv(t)
	doing := env.cols["Doing"]
	tasks := make([]*domain.Task, 4)
	for i := range tasks {
		tasks[i] = makeBacklogTask(t, env, "task")
	}
	for _, tk := range tasks[:2] {
		if _, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
			Patches: []TaskPatch{env.moveTo(t, tk.Key, "Doing")},
		}); err != nil {
			t.Fatalf("seed move %s: %v", tk.Key, err)
		}
	}

	_, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Atomic: true,
		Patches: []TaskPatch{
			env.moveTo(t, tasks[2].Key, "Doing"),
			env.moveTo(t, tasks[3].Key, "Doing"),
		},
	})
	if err == nil {
		t.Fatalf("atomic batch overfilling WIP succeeded")
	}
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeWIPExceeded {
		t.Fatalf("code = %v, want wip_exceeded", de)
	}
	if got := env.countIn(t, doing); got != 2 {
		t.Fatalf("Doing holds %d, want 2 — a refused atomic batch must land nothing", got)
	}
}

// TestTaskUpdate_ConcurrentMovesRespectWIP is the same invariant across
// requests rather than within one call: six real goroutines against the
// real on-disk store, each moving its own task into a column with room
// for three.
func TestTaskUpdate_ConcurrentMovesRespectWIP(t *testing.T) {
	env := openTestEnv(t)
	doing := env.cols["Doing"]
	const movers = 6
	tasks := make([]*domain.Task, movers)
	patches := make([]TaskPatch, movers)
	for i := range tasks {
		tasks[i] = makeBacklogTask(t, env, "task")
	}
	for i, tk := range tasks {
		patches[i] = env.moveTo(t, tk.Key, "Doing")
	}

	var wins, refusals atomic.Int64
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := 0; i < movers; i++ {
		done.Add(1)
		go func(p TaskPatch) {
			defer done.Done()
			start.Wait()
			res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
				Patches: []TaskPatch{p},
			})
			if err != nil {
				return
			}
			switch {
			case res.Items[0].OK:
				wins.Add(1)
			case res.Items[0].Err != nil && res.Items[0].Err.Code == domain.CodeWIPExceeded:
				refusals.Add(1)
			}
		}(patches[i])
	}
	start.Done()
	done.Wait()

	if wins.Load() != 3 || refusals.Load() != movers-3 {
		t.Fatalf("wins=%d refusals=%d, want 3/%d", wins.Load(), refusals.Load(), movers-3)
	}
	if got := env.countIn(t, doing); got != 3 {
		t.Fatalf("Doing holds %d, want 3 (the limit)", got)
	}
}

// ---------------------------------------------------------------------------
// focus:false clears only the focus the patched task holds (PLAN §6.5)
// ---------------------------------------------------------------------------

// projectFocus returns the project's current focus task id, or "" when unset.
func (env *testEnv) projectFocus(t *testing.T) string {
	t.Helper()
	var out string
	if err := env.Read(context.Background(), func(tx store.Tx) error {
		p, err := env.Projects().GetByID(tx, env.proj.ID)
		if err != nil {
			return err
		}
		if p.FocusTaskID != nil {
			out = *p.FocusTaskID
		}
		return nil
	}); err != nil {
		t.Fatalf("read focus: %v", err)
	}
	return out
}

// projectVersion reads the project's version straight from the store.
func (env *testEnv) projectVersion(t *testing.T) int {
	t.Helper()
	var v int
	if err := env.Read(context.Background(), func(tx store.Tx) error {
		p, err := env.Projects().GetByID(tx, env.proj.ID)
		if err != nil {
			return err
		}
		v = p.Version
		return nil
	}); err != nil {
		t.Fatalf("read project version: %v", err)
	}
	return v
}

func (env *testEnv) setFocus(t *testing.T, key string, on bool) {
	t.Helper()
	res, err := env.svc.TaskUpdate(context.Background(), env.actor, TaskUpdateInput{
		Patches: []TaskPatch{{Key: key, Focus: &on}},
	})
	if err != nil {
		t.Fatalf("focus=%v on %s: %v", on, key, err)
	}
	if !res.Items[0].OK {
		t.Fatalf("focus=%v on %s refused: %v", on, key, res.Items[0].Err)
	}
}

func TestTaskUpdate_FocusFalseDoesNotClearAnotherTasksFocus(t *testing.T) {
	env := openTestEnv(t)
	a := makeBacklogTask(t, env, "A holds focus")
	b := makeBacklogTask(t, env, "B does not")

	env.setFocus(t, a.Key, true)
	if got := env.projectFocus(t); got != a.ID {
		t.Fatalf("focus = %q after A claimed it, want %q", got, a.ID)
	}
	before := env.projectVersion(t)

	// B sends focus:false. B never held focus, so this is a no-op — not a
	// way to knock A's focus banner off the board.
	env.setFocus(t, b.Key, false)
	if got := env.projectFocus(t); got != a.ID {
		t.Fatalf("focus = %q after B sent focus:false, want A (%q) to keep it", got, a.ID)
	}
	if after := env.projectVersion(t); after != before {
		t.Fatalf("project version %d -> %d for a no-op focus:false", before, after)
	}
}

func TestTaskUpdate_FocusFalseClearsOwnFocus(t *testing.T) {
	env := openTestEnv(t)
	a := makeBacklogTask(t, env, "A holds focus")

	env.setFocus(t, a.Key, true)
	if got := env.projectFocus(t); got != a.ID {
		t.Fatalf("focus = %q, want %q", got, a.ID)
	}
	env.setFocus(t, a.Key, false)
	if got := env.projectFocus(t); got != "" {
		t.Fatalf("focus = %q after the holder cleared it, want empty", got)
	}
}
