package service

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ---------------------------------------------------------------------------
// KANB-60 — executor keys: a named, expiring key that writes only to the
// cards assigned to its name and can never accept its own work.
// ---------------------------------------------------------------------------

// execEnv is a board with a Review column (active kind — handing work over
// is an ordinary move) and one issued executor key.
type execEnv struct {
	*testEnv
	exec   Actor
	secret string
	review *domain.Column
}

func openExecEnv(t *testing.T) *execEnv {
	t.Helper()
	env := openTestEnv(t)
	review := &domain.Column{ID: uuid.NewString(), ProjectID: env.proj.ID, Name: "Review", Position: 10, Kind: domain.KindActive}
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		return env.Columns().Create(tx, review)
	}); err != nil {
		t.Fatalf("seed Review column: %v", err)
	}
	res, err := env.svc.ExecutorKeyIssue(context.Background(), env.actor, ExecutorKeyIssueInput{
		Name: "exec-1", ProjectKeys: []string{"bmb"},
	})
	if err != nil {
		t.Fatalf("issue executor key: %v", err)
	}
	return &execEnv{testEnv: env, exec: actorOfStoredToken(t, env, res.TokenID), secret: res.Secret, review: review}
}

// actorOfStoredToken builds the Actor exactly as authentication would, from
// the stored row: the scopes under test are the ones the key really carries.
func actorOfStoredToken(t *testing.T, env *testEnv, id string) Actor {
	t.Helper()
	var tok *domain.Token
	if err := env.Read(context.Background(), func(tx store.Tx) error {
		var err error
		tok, err = env.Tokens().GetByID(tx, id)
		return err
	}); err != nil {
		t.Fatalf("read token %s: %v", id, err)
	}
	return Actor{TokenID: tok.ID, Name: tok.Name, Scopes: tok.Scopes, ProjectKeys: tok.ProjectKeys}
}

// card creates a task through the service (so the assignee check runs) and
// returns its key and version.
func (e *execEnv) card(t *testing.T, title, assignee string) (string, int) {
	t.Helper()
	nt := NewTask{ProjectKey: "BMB", Type: domain.TypeTask, Title: title}
	if assignee != "" {
		nt.Assignee = &assignee
	}
	res, err := e.svc.TaskCreate(context.Background(), e.actor, TaskCreateInput{Tasks: []NewTask{nt}})
	if err != nil {
		t.Fatalf("create %q: %v", title, err)
	}
	return res.Tasks[0].Key, res.Tasks[0].Version
}

func (e *execEnv) get(t *testing.T, key string) domain.TaskView {
	t.Helper()
	res, err := e.svc.TaskGet(context.Background(), e.actor, TaskGetInput{Keys: []string{key}})
	if err != nil || len(res.Tasks) != 1 {
		t.Fatalf("get %s: %v", key, err)
	}
	return res.Tasks[0]
}

func (e *execEnv) update(t *testing.T, a Actor, p TaskPatch) ItemResult {
	t.Helper()
	res, err := e.svc.TaskUpdate(context.Background(), a, TaskUpdateInput{Patches: []TaskPatch{p}})
	if err != nil {
		t.Fatalf("TaskUpdate transport error: %v", err)
	}
	return res.Items[0]
}

// wantForbidden fails unless err is a forbidden error whose message
// contains every fragment and which carries a remediation.
func wantForbidden(t *testing.T, label string, err error, fragments ...string) {
	t.Helper()
	e := domain.AsError(err)
	if e == nil || e.Code != domain.CodeForbidden {
		t.Fatalf("%s: got %v, want forbidden", label, err)
	}
	for _, f := range fragments {
		if !strings.Contains(e.Message, f) {
			t.Errorf("%s: message %q lacks %q", label, e.Message, f)
		}
	}
	if e.Remediation == "" {
		t.Errorf("%s: forbidden without a remediation", label)
	}
}

// ---------------------------------------------------------------------------
// Moves
// ---------------------------------------------------------------------------

func TestExecutor_CannotMoveIntoDone(t *testing.T) {
	e := openExecEnv(t)
	key, v := e.card(t, "own work", "exec-1")

	it := e.update(t, e.exec, TaskPatch{Key: key, IfVersion: &v, Column: "Done"})
	if it.OK {
		t.Fatal("an executor key moved its own card into Done")
	}
	wantForbidden(t, "move into Done", it.Err, key, "exec-1", "done column")
	if got := e.get(t, key); got.ColumnID != e.cols["Backlog"].ID || got.DoneAt != nil {
		t.Fatalf("refused move left the card in column %s (done_at %v)", got.ColumnID, got.DoneAt)
	}

	// Handing over is an ordinary move into the (active) review column.
	it = e.update(t, e.exec, TaskPatch{Key: key, IfVersion: &v, Column: "Review"})
	if !it.OK {
		t.Fatalf("executor could not hand its card over to Review: %v", it.Err)
	}
}

func TestExecutor_CannotMoveOutOfDone(t *testing.T) {
	e := openExecEnv(t)
	key, v := e.card(t, "accepted work", "exec-1")
	if it := e.update(t, e.actor, TaskPatch{Key: key, IfVersion: &v, Column: "Done"}); !it.OK {
		t.Fatalf("reviewer close: %v", it.Err)
	}
	v = e.get(t, key).Version

	it := e.update(t, e.exec, TaskPatch{Key: key, IfVersion: &v, Column: "Doing"})
	wantForbidden(t, "reopen from Done", it.Err, key, "out of")
	if got := e.get(t, key); got.ColumnID != e.cols["Done"].ID {
		t.Fatal("refused reopen moved the card anyway")
	}
}

// ---------------------------------------------------------------------------
// Someone else's card
// ---------------------------------------------------------------------------

func TestExecutor_CannotTouchForeignCard(t *testing.T) {
	e := openExecEnv(t)
	ctx := context.Background()
	foreign, fv := e.card(t, "admin's work", "test-admin")
	unassigned, uv := e.card(t, "nobody's work", "")

	for _, tc := range []struct {
		key, owner string
		v          int
	}{{foreign, "test-admin", fv}, {unassigned, "nobody", uv}} {
		v := tc.v
		it := e.update(t, e.exec, TaskPatch{Key: tc.key, Note: "sneaking in"})
		wantForbidden(t, tc.key+" note", it.Err, tc.key, "assigned to "+tc.owner, "exec-1")

		it = e.update(t, e.exec, TaskPatch{Key: tc.key, IfVersion: &v, Column: "Doing"})
		wantForbidden(t, tc.key+" move", it.Err, tc.key, "own cards")

		for _, action := range []ClaimAction{ClaimTake, ClaimRenew, ClaimRelease} {
			_, err := e.svc.TaskClaim(ctx, e.exec, TaskClaimInput{Key: tc.key, Action: action})
			wantForbidden(t, tc.key+" "+string(action), err, tc.key)
		}

		_, err := e.svc.ProgressSet(ctx, e.exec, ProgressSetInput{TaskKey: tc.key, Assessor: "exec-1", Percent: 40})
		wantForbidden(t, tc.key+" progress", err, tc.key)

		got := e.get(t, tc.key)
		if got.ClaimedBy != nil || got.ColumnID != e.cols["Backlog"].ID || got.Version != tc.v {
			t.Errorf("%s changed under refused writes: claimed_by=%v column=%s v%d", tc.key, got.ClaimedBy, got.ColumnID, got.Version)
		}
	}
}

// ---------------------------------------------------------------------------
// Fields: assignee, reviewer and everything else outside the allowance
// ---------------------------------------------------------------------------

func TestExecutor_PatchFieldAllowance(t *testing.T) {
	e := openExecEnv(t)
	key, _ := e.card(t, "own work", "exec-1")
	title, outcome, prio := "renamed", domain.OutcomeHolds, domain.PriorityHigh
	focus := true

	refused := []struct {
		field string
		patch TaskPatch
	}{
		{"assignee", TaskPatch{Assignee: FieldString{Set: true, Value: "test-admin"}}},
		{"assignee", TaskPatch{Assignee: FieldString{Clear: true}}},
		{"reviewer", TaskPatch{Reviewer: FieldString{Set: true, Value: "exec-1"}}},
		{"reviewer", TaskPatch{Reviewer: FieldString{Clear: true}}},
		{"title", TaskPatch{Title: &title}},
		{"body_append", TaskPatch{BodyAppend: &title}},
		{"priority", TaskPatch{Priority: &prio}},
		{"outcome", TaskPatch{Outcome: &outcome}},
		{"acceptance_check", TaskPatch{AcceptanceCheck: []int{0}}},
		{"acceptance_add", TaskPatch{AcceptanceAdd: []string{"self-certified"}}},
		{"tags_add", TaskPatch{TagsAdd: []string{"x"}}},
		{"focus", TaskPatch{Focus: &focus}},
		{"metadata_merge", TaskPatch{MetadataMerge: map[string]any{"k": "v"}}},
		{"force", TaskPatch{Column: "Done", Force: true, Reason: "trust me"}},
	}
	for _, tc := range refused {
		v := e.get(t, key).Version
		p := tc.patch
		p.Key, p.IfVersion = key, &v
		it := e.update(t, e.exec, p)
		if it.OK {
			t.Errorf("%s: executor patch accepted", tc.field)
			continue
		}
		wantForbidden(t, tc.field, it.Err, tc.field, key)
	}
	got := e.get(t, key)
	if got.Assignee == nil || *got.Assignee != "exec-1" || got.Reviewer != nil || got.Title != "own work" || got.ColumnID != e.cols["Backlog"].ID {
		t.Fatalf("refused patches changed the card: %+v", got.Task)
	}

	// The allowance itself: note, column, rank.
	v := got.Version
	it := e.update(t, e.exec, TaskPatch{Key: key, IfVersion: &v, Column: "Doing", Rank: "top", Note: "started on it"})
	if !it.OK {
		t.Fatalf("allowed patch refused: %v", it.Err)
	}
	if it.Task.ColumnID != e.cols["Doing"].ID {
		t.Fatalf("allowed move did not land: column %s", it.Task.ColumnID)
	}
}

// ---------------------------------------------------------------------------
// What an executor may do on its own card
// ---------------------------------------------------------------------------

func TestExecutor_ClaimsRenewsReleasesOwnCard(t *testing.T) {
	e := openExecEnv(t)
	ctx := context.Background()
	key, _ := e.card(t, "own work", "exec-1")
	for _, action := range []ClaimAction{ClaimTake, ClaimRenew, ClaimRelease} {
		if _, err := e.svc.TaskClaim(ctx, e.exec, TaskClaimInput{Key: key, Action: action}); err != nil {
			t.Fatalf("%s own card: %v", action, err)
		}
	}
	_, err := e.svc.TaskClaim(ctx, e.exec, TaskClaimInput{Key: key, Action: ClaimTake, Force: true})
	wantForbidden(t, "claim force", err, "force")
}

func TestExecutor_TaskNextOffersAndTakesOnlyOwnCards(t *testing.T) {
	e := openExecEnv(t)
	ctx := context.Background()
	foreign, _ := e.card(t, "urgent but not mine", "test-admin")
	if it := e.update(t, e.actor, TaskPatch{Key: foreign, IfVersion: intPtrLocal(1), Priority: ptrPriority(domain.PriorityCritical)}); !it.OK {
		t.Fatalf("raise priority: %v", it.Err)
	}
	e.card(t, "unassigned", "")

	// With no card of its own there is nothing to take — and nothing foreign
	// is offered as a consolation.
	peek, err := e.svc.TaskNext(ctx, e.exec, TaskNextInput{ProjectKey: "BMB"})
	if err != nil {
		t.Fatal(err)
	}
	if len(peek.Tasks) != 0 {
		t.Fatalf("peek offered %d foreign cards to an executor", len(peek.Tasks))
	}
	start, err := e.svc.TaskNext(ctx, e.exec, TaskNextInput{ProjectKey: "BMB", Action: NextStart})
	if err != nil {
		t.Fatal(err)
	}
	if start.StartedKey != "" {
		t.Fatalf("executor started %s, which is not its card", start.StartedKey)
	}

	own, _ := e.card(t, "mine", "exec-1")
	start, err = e.svc.TaskNext(ctx, e.exec, TaskNextInput{ProjectKey: "BMB", Action: NextStart})
	if err != nil {
		t.Fatal(err)
	}
	if start.StartedKey != own {
		t.Fatalf("started %q, want own card %s over the higher-priority foreign %s", start.StartedKey, own, foreign)
	}
	got := e.get(t, own)
	if got.ClaimedBy == nil || *got.ClaimedBy != "exec-1" || got.ColumnID != e.cols["Doing"].ID {
		t.Fatalf("start did not claim+move: claimed_by=%v column=%s", got.ClaimedBy, got.ColumnID)
	}
	if f := e.get(t, foreign); f.ClaimedBy != nil {
		t.Fatal("the foreign card was claimed")
	}
}

func TestExecutor_ProgressOnOwnCardAsItself(t *testing.T) {
	e := openExecEnv(t)
	ctx := context.Background()
	key, _ := e.card(t, "own work", "exec-1")

	if _, err := e.svc.ProgressSet(ctx, e.exec, ProgressSetInput{TaskKey: key, Assessor: "exec-1", Percent: 30}); err != nil {
		t.Fatalf("progress on own card: %v", err)
	}
	_, err := e.svc.ProgressSet(ctx, e.exec, ProgressSetInput{TaskKey: key, Assessor: "reviewer", Percent: 100})
	wantForbidden(t, "progress under another name", err, "reviewer")
	_, err = e.svc.ProgressSet(ctx, e.exec, ProgressSetInput{ProjectKey: "BMB", Assessor: "exec-1", Percent: 50})
	wantForbidden(t, "project progress", err, "project-level progress")
}

func TestExecutor_PostsToProjectFeed(t *testing.T) {
	e := openExecEnv(t)
	msg, err := e.svc.ChatAdd(context.Background(), e.exec, ChatAddInput{ProjectKey: "BMB", Author: "Claude", Body: "handing BMB-1 over"})
	if err != nil {
		t.Fatalf("executor project_post: %v", err)
	}
	if msg.AuthorTokenID != e.exec.TokenID {
		t.Fatalf("post attributed to token %q, want the executor key %q", msg.AuthorTokenID, e.exec.TokenID)
	}
}

// ---------------------------------------------------------------------------
// Operations an executor key cannot perform at all
// ---------------------------------------------------------------------------

func TestExecutor_RefusedOperations(t *testing.T) {
	e := openExecEnv(t)
	ctx := context.Background()
	own, v := e.card(t, "own work", "exec-1")
	other, _ := e.card(t, "other", "exec-1")
	title := "x"

	for _, tc := range []struct {
		op   string
		call func() error
	}{
		{"create tasks", func() error {
			_, err := e.svc.TaskCreate(ctx, e.exec, TaskCreateInput{Tasks: []NewTask{{ProjectKey: "BMB", Type: domain.TypeTask, Title: title}}})
			return err
		}},
		{"archive or restore tasks", func() error {
			_, err := e.svc.TaskRemove(ctx, e.exec, TaskRemoveInput{Items: []RemoveItem{{Key: own, IfVersion: &v}}})
			return err
		}},
		{"add or remove dependency links", func() error {
			_, err := e.svc.TaskLink(ctx, e.exec, TaskLinkInput{Add: []LinkPair{{Blocker: own, Blocked: other}}})
			return err
		}},
		{"create or change projects", func() error {
			_, err := e.svc.ProjectUpsert(ctx, e.exec, ProjectUpsertInput{Mode: UpsertUpdate, Key: "BMB", IfVersion: intPtrLocal(1), Name: "hijacked"})
			return err
		}},
		{"issue executor keys", func() error {
			_, err := e.svc.ExecutorKeyIssue(ctx, e.exec, ExecutorKeyIssueInput{Name: "exec-2", ProjectKeys: []string{"BMB"}})
			return err
		}},
		{"delete progress tracks", func() error {
			_, err := e.svc.ProgressTrackDelete(ctx, e.exec, ProgressTrackDeleteInput{ProjectKey: "BMB", TaskKey: own, Assessor: "exec-1"})
			return err
		}},
	} {
		wantForbidden(t, tc.op, tc.call(), "executor key exec-1 cannot "+tc.op)
	}
}

// ---------------------------------------------------------------------------
// executor_key_issue
// ---------------------------------------------------------------------------

func TestExecutorKeyIssue_ParticipantWithSecretShownOnce(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	res, err := env.svc.ExecutorKeyIssue(ctx, env.actor, ExecutorKeyIssueInput{Name: "exec-1", ProjectKeys: []string{"BMB"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Secret, auth.TokenPrefix) || res.Name != "exec-1" || res.Reissued {
		t.Fatalf("unexpected result: %+v", res)
	}

	// The secret authenticates as the key: its hash is what is stored.
	var stored *domain.Token
	if err := env.Read(ctx, func(tx store.Tx) error {
		var err error
		stored, err = env.Tokens().GetByHash(tx, auth.HashToken(res.Secret))
		return err
	}); err != nil {
		t.Fatalf("secret does not resolve to the stored key: %v", err)
	}
	if stored.ID != res.TokenID || !stored.Scopes.OwnCardsOnly() || len(stored.ProjectKeys) != 1 || stored.ProjectKeys[0] != "BMB" {
		t.Fatalf("stored key = %+v", stored)
	}
	if strings.Contains(string(stored.Hash), res.Secret) {
		t.Fatal("the stored row carries the plaintext secret")
	}

	// Participant of the project, visible on the board.
	board, err := env.svc.BoardGet(ctx, env.actor, BoardGetInput{ProjectKey: "BMB", View: ViewSummary})
	if err != nil {
		t.Fatal(err)
	}
	if !hasParticipant(board.Projects[0].Participants, "exec-1") {
		t.Fatalf("participants %v lack exec-1", board.Projects[0].Participants)
	}

	// Nowhere else: not in the board, not in any event, and the name cannot
	// be issued again while live, so there is no second way to see a secret.
	boardJSON, _ := json.Marshal(board)
	if strings.Contains(string(boardJSON), res.Secret) {
		t.Fatal("board_get carries the secret")
	}
	var events []domain.Event
	if err := env.Read(ctx, func(tx store.Tx) error {
		var err error
		events, err = env.Events().Since(tx, env.proj.ID, 0, 1000)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	issued := 0
	for _, ev := range events {
		raw, _ := json.Marshal(ev)
		if strings.Contains(string(raw), res.Secret) || strings.Contains(string(raw), strings.TrimPrefix(res.Secret, auth.TokenPrefix)) {
			t.Fatalf("event %s carries the secret: %s", ev.Type, raw)
		}
		if ev.Type == domain.EventExecutorKeyIssued {
			issued++
			if ev.Payload["name"] != "exec-1" {
				t.Errorf("issued event payload = %v", ev.Payload)
			}
		}
	}
	if issued != 1 {
		t.Fatalf("%d executor_key.issued events, want 1 per project", issued)
	}
	again, err := env.svc.ExecutorKeyIssue(ctx, env.actor, ExecutorKeyIssueInput{Name: "exec-1", ProjectKeys: []string{"BMB"}})
	if err == nil || again != nil {
		t.Fatalf("a live key was issued twice (second secret %v)", again)
	}
}

func hasParticipant(ps []Participant, name string) bool {
	for _, p := range ps {
		if p.Name == name {
			return true
		}
	}
	return false
}

func TestExecutorKeyIssue_Lifetime(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	before := time.Now().UTC()
	def, err := env.svc.ExecutorKeyIssue(ctx, env.actor, ExecutorKeyIssueInput{Name: "exec-default", ProjectKeys: []string{"BMB"}})
	if err != nil {
		t.Fatal(err)
	}
	if d := def.ExpiresAt.Sub(before); d < 24*time.Hour-time.Minute || d > 24*time.Hour+time.Minute {
		t.Fatalf("default lifetime %v, want 24h", d)
	}
	short, err := env.svc.ExecutorKeyIssue(ctx, env.actor, ExecutorKeyIssueInput{Name: "exec-short", ProjectKeys: []string{"BMB"}, TTLSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	if d := short.ExpiresAt.Sub(before); d < time.Hour-time.Minute || d > time.Hour+time.Minute {
		t.Fatalf("ttl_seconds=3600 gave %v", d)
	}
}

func TestExecutorKeyIssue_Refusals(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	other := &domain.Project{ID: uuid.NewString(), Key: "OPS", Name: "Ops", Version: 1, NextTaskSeq: 1, EstimateUnit: "h", ClaimTTLSeconds: 3600}
	if err := env.Write(ctx, func(tx store.Tx) error { return env.Projects().Create(tx, other) }); err != nil {
		t.Fatal(err)
	}
	seedToken(t, env, "tok-writer", "writer", domain.Scopes{domain.ScopeWrite})
	seedToken(t, env, "tok-coord", "coord", domain.Scopes{domain.ScopeWrite})
	seedToken(t, env, "tok-reader", "reader", domain.Scopes{domain.ScopeRead})
	setCoordinator(t, env, env.actor, "tok-coord")
	if _, err := env.svc.ExecutorKeyIssue(ctx, env.actor, ExecutorKeyIssueInput{Name: "exec-live", ProjectKeys: []string{"BMB"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.ExecutorKeyIssue(ctx, env.actor, ExecutorKeyIssueInput{Name: "exec-dead", ProjectKeys: []string{"BMB"}}); err != nil {
		t.Fatal(err)
	}
	revokeToken(t, env, "exec-dead")

	admin := env.actor
	writer := Actor{TokenID: "tok-writer", Name: "writer", Scopes: domain.Scopes{domain.ScopeWrite}}
	coord := Actor{TokenID: "tok-coord", Name: "coord", Scopes: domain.Scopes{domain.ScopeWrite}}
	reader := Actor{TokenID: "tok-reader", Name: "reader", Scopes: domain.Scopes{domain.ScopeRead}}
	in := func(name string, ttl int, projects ...string) ExecutorKeyIssueInput {
		return ExecutorKeyIssueInput{Name: name, ProjectKeys: projects, TTLSeconds: ttl}
	}

	for _, tc := range []struct {
		label string
		a     Actor
		in    ExecutorKeyIssueInput
		code  domain.Code
		frag  string
	}{
		{"write token that coordinates nothing", writer, in("e1", 0, "BMB"), domain.CodeForbidden, "not the coordinator of project BMB"},
		{"coordinator of BMB asking for OPS too", coord, in("e2", 0, "BMB", "OPS"), domain.CodeForbidden, "not the coordinator of project OPS"},
		{"read token", reader, in("e3", 0, "BMB"), domain.CodeForbidden, "write scope"},
		{"empty name", admin, in("", 0, "BMB"), domain.CodeValidation, "name is required"},
		{"name with a space", admin, in("exec one", 0, "BMB"), domain.CodeValidation, "whitespace"},
		{"no projects", admin, in("e4", 0), domain.CodeValidation, "at least one project"},
		{"project listed twice", admin, in("e5", 0, "BMB", "bmb"), domain.CodeValidation, "listed twice"},
		{"unknown project", admin, in("e6", 0, "NOPE"), domain.CodeNotFound, "NOPE"},
		{"ttl below minimum", admin, in("e7", 60, "BMB"), domain.CodeValidation, "ttl_seconds"},
		{"ttl above maximum", admin, in("e8", 8*24*3600, "BMB"), domain.CodeValidation, "ttl_seconds"},
		{"negative ttl", admin, in("e9", -1, "BMB"), domain.CodeValidation, "ttl_seconds"},
		{"name of a standing token", admin, in("writer", 0, "BMB"), domain.CodeValidation, "standing write token"},
		{"name of the admin token, other case", admin, in("TEST-ADMIN", 0, "BMB"), domain.CodeValidation, "regardless of case"},
		{"name of a live key", admin, in("exec-live", 0, "BMB"), domain.CodeValidation, "live until"},
		{"name of a revoked key", admin, in("exec-dead", 0, "BMB"), domain.CodeValidation, "revoked"},
	} {
		_, err := env.svc.ExecutorKeyIssue(ctx, tc.a, tc.in)
		e := domain.AsError(err)
		if e == nil || e.Code != tc.code {
			t.Errorf("%s: got %v, want %s", tc.label, err, tc.code)
			continue
		}
		if !strings.Contains(e.Message, tc.frag) {
			t.Errorf("%s: message %q lacks %q", tc.label, e.Message, tc.frag)
		}
		if e.Remediation == "" {
			t.Errorf("%s: no remediation", tc.label)
		}
	}

	// A refused multi-project issue leaves nothing behind.
	if err := env.Read(ctx, func(tx store.Tx) error {
		_, err := env.Tokens().GetByName(tx, "e2")
		if !isNotFound(err) {
			t.Errorf("refused issue created token e2: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The coordinator may issue for the project it coordinates.
	if _, err := env.svc.ExecutorKeyIssue(ctx, coord, in("e-coord", 0, "BMB")); err != nil {
		t.Fatalf("coordinator issuing for its own project: %v", err)
	}
}

// expireKey pushes an issued key's expiry into the past through the store,
// which is what the clock would do by itself a day later.
func expireKey(t *testing.T, env *testEnv, name string) *domain.Token {
	t.Helper()
	var tok *domain.Token
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		var err error
		if tok, err = env.Tokens().GetByName(tx, name); err != nil {
			return err
		}
		past := time.Now().UTC().Add(-time.Minute)
		tok.ExpiresAt = &past
		return env.Tokens().Reissue(tx, tok)
	}); err != nil {
		t.Fatalf("expire %s: %v", name, err)
	}
	return tok
}

func TestExecutorKey_ExpiredLeavesParticipantsAndCanBeReissued(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	first, err := env.svc.ExecutorKeyIssue(ctx, env.actor, ExecutorKeyIssueInput{Name: "exec-1", ProjectKeys: []string{"BMB"}})
	if err != nil {
		t.Fatal(err)
	}
	expireKey(t, env, "exec-1")

	board, err := env.svc.BoardGet(ctx, env.actor, BoardGetInput{ProjectKey: "BMB", View: ViewSummary})
	if err != nil {
		t.Fatal(err)
	}
	if hasParticipant(board.Projects[0].Participants, "exec-1") {
		t.Fatal("an expired key is still listed as a participant")
	}
	assignee := "exec-1"
	_, err = env.svc.TaskCreate(ctx, env.actor, TaskCreateInput{Tasks: []NewTask{{ProjectKey: "BMB", Type: domain.TypeTask, Title: "t", Assignee: &assignee}}})
	if e := domain.AsError(err); e == nil || e.Code != domain.CodeValidation {
		t.Fatalf("assigning to an expired key: %v, want validation", err)
	}

	second, err := env.svc.ExecutorKeyIssue(ctx, env.actor, ExecutorKeyIssueInput{Name: "exec-1", ProjectKeys: []string{"BMB"}})
	if err != nil {
		t.Fatalf("re-issue after expiry: %v", err)
	}
	if !second.Reissued || second.TokenID != first.TokenID || second.Secret == first.Secret {
		t.Fatalf("re-issue = %+v, want same token id, new secret, reissued", second)
	}
	if err := env.Read(ctx, func(tx store.Tx) error {
		if _, err := env.Tokens().GetByHash(tx, auth.HashToken(first.Secret)); !isNotFound(err) {
			t.Errorf("the old secret still resolves after re-issue: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	board, err = env.svc.BoardGet(ctx, env.actor, BoardGetInput{ProjectKey: "BMB", View: ViewSummary})
	if err != nil {
		t.Fatal(err)
	}
	if !hasParticipant(board.Projects[0].Participants, "exec-1") {
		t.Fatal("the re-issued key is not a participant")
	}
}

// Two orchestrators issuing the same name at once: the write transaction
// serialises them, exactly one key exists, and the loser is told why.
func TestExecutorKeyIssue_ConcurrentSameNameIssuesOnce(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	const n = 8
	var wg sync.WaitGroup
	secrets := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := env.svc.ExecutorKeyIssue(ctx, env.actor, ExecutorKeyIssueInput{Name: "exec-race", ProjectKeys: []string{"BMB"}})
			if err != nil {
				errs <- err
				return
			}
			secrets <- res.Secret
		}()
	}
	wg.Wait()
	close(secrets)
	close(errs)
	if len(secrets) != 1 {
		t.Fatalf("%d concurrent issues of one name succeeded, want exactly 1", len(secrets))
	}
	for err := range errs {
		if e := domain.AsError(err); e == nil || e.Code != domain.CodeValidation {
			t.Errorf("losing issue: %v, want validation (live key)", err)
		}
	}
}

// ---------------------------------------------------------------------------
// assignee must be a participant
// ---------------------------------------------------------------------------

func TestAssignee_MustBeParticipantOnWrite(t *testing.T) {
	e := openExecEnv(t)
	ctx := context.Background()
	typo := "exec_1"

	_, err := e.svc.TaskCreate(ctx, e.actor, TaskCreateInput{Tasks: []NewTask{{ProjectKey: "BMB", Type: domain.TypeTask, Title: "t", Assignee: &typo}}})
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeValidation || de.Field != "assignee" {
		t.Fatalf("unknown assignee on create: %v, want validation on assignee", err)
	}
	for _, known := range []string{"exec-1", "test-admin"} {
		if !strings.Contains(de.Message, known) {
			t.Errorf("refusal %q does not list participant %s", de.Message, known)
		}
	}

	key, v := e.card(t, "t", "exec-1")
	it := e.update(t, e.actor, TaskPatch{Key: key, IfVersion: &v, Assignee: FieldString{Set: true, Value: typo}})
	if it.OK || it.Err.Code != domain.CodeValidation || !strings.Contains(it.Err.Message, "exec-1") {
		t.Fatalf("unknown assignee on update: %+v", it.Err)
	}
	if it := e.update(t, e.actor, TaskPatch{Key: key, IfVersion: &v, Assignee: FieldString{Set: true, Value: ""}}); !it.OK {
		t.Fatalf("clearing via empty assignee refused: %v", it.Err)
	}
	v = e.get(t, key).Version
	if it := e.update(t, e.actor, TaskPatch{Key: key, IfVersion: &v, Assignee: FieldString{Clear: true}}); !it.OK {
		t.Fatalf("clearing via null assignee refused: %v", it.Err)
	}
	v = e.get(t, key).Version
	if it := e.update(t, e.actor, TaskPatch{Key: key, IfVersion: &v, Assignee: FieldString{Set: true, Value: "test-admin"}}); !it.OK {
		t.Fatalf("reassigning to a participant refused: %v", it.Err)
	}
}

// A card whose stored assignee predates the rule keeps working: only a NEW
// value is checked.
func TestAssignee_LegacyValueIsNotBroken(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "legacy")
	legacy := "Claude Opus (subagent 3)"
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		cur, err := env.Tasks().GetByID(tx, task.ID)
		if err != nil {
			return err
		}
		cur.Assignee = &legacy
		return env.Tasks().Update(tx, cur, nil)
	}); err != nil {
		t.Fatal(err)
	}
	e := &execEnv{testEnv: env}
	v := e.get(t, task.Key).Version
	title := "legacy, retitled"
	if it := e.update(t, env.actor, TaskPatch{Key: task.Key, IfVersion: &v, Title: &title, Column: "Doing"}); !it.OK {
		t.Fatalf("editing a card with a legacy assignee: %v", it.Err)
	}
	v = e.get(t, task.Key).Version
	if it := e.update(t, env.actor, TaskPatch{Key: task.Key, IfVersion: &v, Assignee: FieldString{Set: true, Value: legacy}}); !it.OK {
		t.Fatalf("re-sending the unchanged legacy assignee: %v", it.Err)
	}
}

// kanban import recreates cards whose assignees are history; Restore carries
// them over, and only an admin may say so.
func TestTaskCreate_RestoreKeepsHistoricalAssignee(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	gone := "exec-from-last-week"
	res, err := env.svc.TaskCreate(ctx, env.actor, TaskCreateInput{Restore: true,
		Tasks: []NewTask{{ProjectKey: "BMB", Type: domain.TypeTask, Title: "imported", Assignee: &gone}}})
	if err != nil {
		t.Fatalf("admin restore with a historical assignee: %v", err)
	}
	if a := res.Tasks[0].Assignee; a == nil || *a != gone {
		t.Fatalf("restored assignee = %v", a)
	}
	writer := env.actorWith(t, nonAdmin)
	_, err = env.svc.TaskCreate(ctx, writer, TaskCreateInput{Restore: true,
		Tasks: []NewTask{{ProjectKey: "BMB", Type: domain.TypeTask, Title: "imported", Assignee: &gone}}})
	wantForbidden(t, "non-admin restore", err, "admin")
}

func ptrPriority(p domain.Priority) *domain.Priority { return &p }
