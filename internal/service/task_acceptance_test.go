package service

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ---------------------------------------------------------------------------
// KANB-47 — task_create(source_message): atomic acceptance of a command.
// ---------------------------------------------------------------------------

// acceptanceEnv is the scene every acceptance test needs: a project with a
// coordinator, an executor token and a command waiting to be accepted.
type acceptanceEnv struct {
	*testEnv
	coordinator Actor
	executor    Actor
	command     *domain.ChatMessage
}

func openAcceptanceEnv(t *testing.T) *acceptanceEnv {
	t.Helper()
	env := openTestEnv(t)
	seedToken(t, env, "tok-lead", "lead", domain.Scopes{domain.ScopeWrite})
	seedToken(t, env, "tok-exec", "executor", domain.Scopes{domain.ScopeWrite}, env.proj.Key)
	setCoordinator(t, env, env.actor, "tok-lead")

	executor := Actor{TokenID: "tok-exec", Name: "executor", Scopes: domain.Scopes{domain.ScopeWrite}}
	lead := Actor{TokenID: "tok-lead", Name: "lead", Scopes: domain.Scopes{domain.ScopeWrite}}
	cmd, err := env.svc.ChatAdd(context.Background(), lead, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "lead", Body: "Executor: ship the report", Kind: "command",
		Recipient: "tok-exec",
	})
	if err != nil {
		t.Fatalf("post command: %v", err)
	}
	return &acceptanceEnv{testEnv: env, coordinator: lead, executor: executor, command: cmd}
}

// acceptBatch is the batch every accepting call uses.
func acceptBatch() []NewTask {
	return []NewTask{{
		ProjectKey: "BMB", Title: "Write the report", Body: "The commanded work",
		Type: domain.TypeTask, Acceptance: []string{"report exists"},
	}}
}

func accept(t *testing.T, env *acceptanceEnv, a Actor, tasks []NewTask) (*TaskCreateResult, error) {
	t.Helper()
	return env.svc.TaskCreate(context.Background(), a, TaskCreateInput{Tasks: tasks, SourceMessage: env.command.ID})
}

// TestTaskCreate_SourceMessageOptional: without source_message nothing about
// task_create changes (KANB-47 acceptance 1).
func TestTaskCreate_SourceMessageOptional(t *testing.T) {
	env := openAcceptanceEnv(t)

	res, err := env.svc.TaskCreate(context.Background(), env.executor, TaskCreateInput{Tasks: acceptBatch()})
	if err != nil {
		t.Fatalf("plain create: %v", err)
	}
	if res.AlreadyAccepted {
		t.Fatal("ordinary create reported already_accepted")
	}
	if len(res.Tasks) != 1 || res.Tasks[0].Key == "" {
		t.Fatalf("ordinary create = %+v", res.Tasks)
	}

	// The unaccepted command is still waiting: its feed entry carries no
	// task_keys.
	feed, err := env.svc.ChatFeed(context.Background(), env.actor, ChatFeedInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("ChatFeed: %v", err)
	}
	if len(feed.Messages) != 1 || len(feed.Messages[0].TaskKeys) != 0 {
		t.Fatalf("unaccepted command shows task_keys = %+v", feed.Messages)
	}
}

// TestTaskCreate_AcceptanceRules: the guard rails of an acceptance — command
// existence and kind, executor identity, replay, foreign executor, changed
// content (KANB-47 acceptance 2, 3, 6).
func TestTaskCreate_AcceptanceRules(t *testing.T) {
	env := openAcceptanceEnv(t)
	ctx := context.Background()

	// The accepting call itself.
	res, err := accept(t, env, env.executor, acceptBatch())
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if res.AlreadyAccepted || len(res.Tasks) != 1 {
		t.Fatalf("first acceptance = %+v (already=%v)", res.Tasks, res.AlreadyAccepted)
	}
	originalKey := res.Tasks[0].Key

	// Repeat by the same executor with the same content: the same keys, no
	// second batch.
	again, err := accept(t, env, env.executor, acceptBatch())
	if err != nil {
		t.Fatalf("repeat acceptance: %v", err)
	}
	if !again.AlreadyAccepted {
		t.Fatal("repeat acceptance did not report already_accepted")
	}
	if len(again.Tasks) != 1 || again.Tasks[0].Key != originalKey {
		t.Fatalf("repeat acceptance returned %+v, want the original key %s", again.Tasks, originalKey)
	}

	// Repeat with DIFFERENT content: a loud conflict, the original tasks
	// untouched.
	changed := acceptBatch()
	changed[0].Title = "A different report entirely"
	_, err = accept(t, env, env.executor, changed)
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeIdempotencyMismatch {
		t.Fatalf("changed-content acceptance: got %v, want idempotency_mismatch", err)
	}
	if !strings.Contains(de.Message, originalKey) {
		t.Fatalf("conflict message %q does not name the original tasks", de.Message)
	}
	board, err := env.svc.BoardGet(ctx, env.actor, BoardGetInput{ProjectKey: env.proj.Key, View: ViewTasks})
	if err != nil {
		t.Fatalf("board_get: %v", err)
	}
	total := 0
	for _, c := range board.Projects[0].Columns {
		total += len(c.Tasks)
	}
	if total != 1 {
		t.Fatalf("board holds %d tasks after a refused retry, want exactly the 1 original", total)
	}

	// A different token cannot accept, before OR after the real acceptance.
	// (Here: after — the earlier refusal is covered by the dedicated case.)
	seedToken(t, env.testEnv, "tok-stranger", "stranger", domain.Scopes{domain.ScopeWrite}, env.proj.Key)
	stranger := Actor{TokenID: "tok-stranger", Name: "stranger", Scopes: domain.Scopes{domain.ScopeWrite}}
	_, err = accept(t, env, stranger, acceptBatch())
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeForbidden {
		t.Fatalf("stranger acceptance: got %v, want forbidden", err)
	}

	// The feed shows the accepted command with its task keys.
	feed, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("ChatFeed: %v", err)
	}
	if len(feed.Messages) != 1 || len(feed.Messages[0].TaskKeys) != 1 || feed.Messages[0].TaskKeys[0] != originalKey {
		t.Fatalf("accepted command's feed entry = %+v, want task_keys [%s]", feed.Messages, originalKey)
	}
}

// TestTaskCreate_AcceptanceRefusals: the refusal cases, each with the field
// and code a caller can act on.
func TestTaskCreate_AcceptanceRefusals(t *testing.T) {
	env := openAcceptanceEnv(t)
	ctx := context.Background()

	// A question is not a command: answering it with tasks would invent work.
	q, err := env.svc.ChatAdd(ctx, env.actor, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "lead", Body: "why?", Kind: "question", Recipient: "tok-exec",
	})
	if err != nil {
		t.Fatalf("post question: %v", err)
	}
	_, err = env.svc.TaskCreate(ctx, env.executor, TaskCreateInput{Tasks: acceptBatch(), SourceMessage: q.ID})
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeValidation || de.Field != "source_message" {
		t.Fatalf("question acceptance: got %v, want validation on source_message", err)
	}

	// A missing message id.
	_, err = env.svc.TaskCreate(ctx, env.executor, TaskCreateInput{Tasks: acceptBatch(), SourceMessage: "no-such-message"})
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeNotFound {
		t.Fatalf("missing message acceptance: got %v, want not_found", err)
	}

	// A token that is not the resolved executor — even one allowed to write
	// in the project.
	seedToken(t, env.testEnv, "tok-bypass", "bypass", domain.Scopes{domain.ScopeAdmin})
	bypass := Actor{TokenID: "tok-bypass", Name: "bypass", Scopes: domain.Scopes{domain.ScopeAdmin}}
	_, err = accept(t, env, bypass, acceptBatch())
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeForbidden ||
		!strings.Contains(de.Message, "executor") {
		t.Fatalf("admin bystander acceptance: got %v, want forbidden naming the executor", err)
	}

	// A batch that targets a different project than the command.
	foreign := acceptBatch()
	foreign[0].ProjectKey = "OTHER"
	_, err = accept(t, env, env.executor, foreign)
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeValidation || de.Field != "source_message" {
		t.Fatalf("cross-project batch: got %v, want validation on source_message", err)
	}
}

// TestTaskCreate_AcceptanceAtomic: a failure in the middle of the batch
// leaves NOTHING behind — no tasks, no acceptance — and the command can then
// be accepted cleanly (KANB-47 acceptance 5).
func TestTaskCreate_AcceptanceAtomic(t *testing.T) {
	env := openAcceptanceEnv(t)
	ctx := context.Background()

	// First item valid, second invalid: the failure lands after the plan
	// phase has begun and before any commit — the entire transaction,
	// acceptance included, must roll back.
	batch := append(acceptBatch(), NewTask{
		ProjectKey: "BMB", Title: "", Type: domain.TypeTask, // empty title never validates
	})
	if _, err := accept(t, env, env.executor, batch); err == nil {
		t.Fatal("batch with an invalid item was accepted")
	}

	// Nothing landed: no tasks, no acceptance.
	board, err := env.svc.BoardGet(ctx, env.actor, BoardGetInput{ProjectKey: env.proj.Key, View: ViewTasks})
	if err != nil {
		t.Fatalf("board_get: %v", err)
	}
	total := 0
	for _, c := range board.Projects[0].Columns {
		total += len(c.Tasks)
	}
	if total != 0 {
		t.Fatalf("rolled-back acceptance left %d tasks behind", total)
	}
	if err := env.Read(ctx, func(tx store.Tx) error {
		_, aerr := env.Acceptances().GetByMessage(tx, env.command.ID)
		return aerr
	}); err != nil && !isNotFound(err) {
		t.Fatalf("rolled-back acceptance left a link behind: %v", err)
	} else if err == nil {
		t.Fatal("rolled-back acceptance left a link behind")
	}

	// The command is still acceptable, and now succeeds.
	res, err := accept(t, env, env.executor, acceptBatch())
	if err != nil {
		t.Fatalf("clean acceptance after rollback: %v", err)
	}
	if res.AlreadyAccepted || len(res.Tasks) != 1 {
		t.Fatalf("clean acceptance = %+v", res.Tasks)
	}
}

// TestTaskCreate_AcceptanceSurvivesRestart: the link lives in the database,
// not in process state — reopening the store keeps the feed's task_keys and
// the replay answer intact (KANB-47 acceptance 4).
func TestTaskCreate_AcceptanceSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kanban.db")
	ctx := context.Background()
	admin := Actor{
		TokenID: "tok-admin", Name: "test-admin",
		Scopes: domain.Scopes{domain.ScopeAdmin, domain.ScopeWrite, domain.ScopeRead},
	}

	open := func() *testEnv {
		st, err := store.Open(ctx, store.Config{Path: dbPath, ReadPoolSize: 4})
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		return &testEnv{Store: st, svc: New(st, nil), actor: admin}
	}

	// First life: seed the project, tokens, command, and accept it.
	env := open()
	p, _ := seedTestProject(t, env)
	env.proj = p
	seedToken(t, env, "tok-lead", "lead", domain.Scopes{domain.ScopeWrite})
	seedToken(t, env, "tok-exec", "executor", domain.Scopes{domain.ScopeWrite}, env.proj.Key)
	setCoordinator(t, env, admin, "tok-lead")
	executor := Actor{TokenID: "tok-exec", Name: "executor", Scopes: domain.Scopes{domain.ScopeWrite}}
	lead := Actor{TokenID: "tok-lead", Name: "lead", Scopes: domain.Scopes{domain.ScopeWrite}}
	cmd, err := env.svc.ChatAdd(ctx, lead, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "lead", Body: "survive me", Kind: "command", Recipient: "tok-exec",
	})
	if err != nil {
		t.Fatalf("post command: %v", err)
	}
	created, err := env.svc.TaskCreate(ctx, executor, TaskCreateInput{
		Tasks:         []NewTask{{ProjectKey: env.proj.Key, Title: "The work", Type: domain.TypeTask}},
		SourceMessage: cmd.ID,
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	wantKey := created.Tasks[0].Key
	if err := env.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	// Second life: the acceptance, its tasks and the feed's task_keys are
	// all still there, and a repeat replays instead of duplicating.
	env2 := open()
	defer func() { _ = env2.Close() }()
	if err := env2.Read(ctx, func(tx store.Tx) error {
		p, perr := env2.Projects().GetByKey(tx, env.proj.Key)
		if perr != nil {
			return perr
		}
		env2.proj = p
		return nil
	}); err != nil {
		t.Fatalf("reload project: %v", err)
	}

	feed, err := env2.svc.ChatFeed(ctx, admin, ChatFeedInput{ProjectKey: env2.proj.Key})
	if err != nil {
		t.Fatalf("ChatFeed after restart: %v", err)
	}
	if len(feed.Messages) != 1 || len(feed.Messages[0].TaskKeys) != 1 || feed.Messages[0].TaskKeys[0] != wantKey {
		t.Fatalf("feed after restart = %+v, want task_keys [%s] on the command", feed.Messages, wantKey)
	}

	replay, err := env2.svc.TaskCreate(ctx, executor, TaskCreateInput{
		Tasks:         []NewTask{{ProjectKey: env2.proj.Key, Title: "The work", Type: domain.TypeTask}},
		SourceMessage: cmd.ID,
	})
	if err != nil {
		t.Fatalf("replay after restart: %v", err)
	}
	if !replay.AlreadyAccepted || len(replay.Tasks) != 1 || replay.Tasks[0].Key != wantKey {
		t.Fatalf("replay after restart = %+v (already=%v), want key %s and already_accepted", replay.Tasks, replay.AlreadyAccepted, wantKey)
	}

	board, err := env2.svc.BoardGet(ctx, admin, BoardGetInput{ProjectKey: env2.proj.Key, View: ViewTasks})
	if err != nil {
		t.Fatalf("board_get after restart: %v", err)
	}
	total := 0
	for _, c := range board.Projects[0].Columns {
		total += len(c.Tasks)
	}
	if total != 1 {
		t.Fatalf("board holds %d tasks after restart+replay, want 1 — the replay created a second batch", total)
	}
}
