package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ---------------------------------------------------------------------------
// KANB-68 — reviewer and assessor come from the participants, a participant
// can exist without a key, and an executor key can be renewed without a new
// secret.
// ---------------------------------------------------------------------------

func wantValidation(t *testing.T, label string, err error, field string, fragments ...string) {
	t.Helper()
	e := domain.AsError(err)
	if e == nil || e.Code != domain.CodeValidation {
		t.Fatalf("%s: got %v, want validation", label, err)
	}
	if field != "" && e.Field != field {
		t.Errorf("%s: field %q, want %q", label, e.Field, field)
	}
	for _, f := range fragments {
		if !strings.Contains(e.Message, f) {
			t.Errorf("%s: message %q lacks %q", label, e.Message, f)
		}
	}
	if e.Remediation == "" {
		t.Errorf("%s: refusal without a remediation", label)
	}
}

// authManager authenticates against the real store exactly as the server
// does, so "can sign in" means what it means in production.
func authManager(env *testEnv) *auth.Manager {
	return auth.NewManagerFromStore(env.Store, nil, "", true, nil)
}

func storedToken(t *testing.T, env *testEnv, name string) *domain.Token {
	t.Helper()
	var tok *domain.Token
	if err := env.Read(context.Background(), func(tx store.Tx) error {
		var err error
		tok, err = env.Tokens().GetByName(tx, name)
		return err
	}); err != nil {
		t.Fatalf("read token %s: %v", name, err)
	}
	return tok
}

func seedProject(t *testing.T, env *testEnv, key string) {
	t.Helper()
	p := &domain.Project{ID: uuid.NewString(), Key: key, Name: key, Version: 1, NextTaskSeq: 1,
		EstimateUnit: "h", ClaimTTLSeconds: 3600}
	col := &domain.Column{ID: uuid.NewString(), ProjectID: p.ID, Name: "Backlog", Position: 0, Kind: domain.KindBacklog}
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		if err := env.Projects().Create(tx, p); err != nil {
			return err
		}
		return env.Columns().Create(tx, col)
	}); err != nil {
		t.Fatalf("seed project %s: %v", key, err)
	}
}

// ---------------------------------------------------------------------------
// reviewer
// ---------------------------------------------------------------------------

func TestReviewer_MustBeParticipantOnWrite(t *testing.T) {
	e := openExecEnv(t)
	ctx := context.Background()
	typo := "exec_1"

	_, err := e.svc.TaskCreate(ctx, e.actor, TaskCreateInput{Tasks: []NewTask{{ProjectKey: "BMB", Type: domain.TypeTask, Title: "t", Reviewer: &typo}}})
	wantValidation(t, "unknown reviewer on create", err, "reviewer", "exec_1", "exec-1", "test-admin")

	reviewer := "exec-1"
	res, err := e.svc.TaskCreate(ctx, e.actor, TaskCreateInput{Tasks: []NewTask{{ProjectKey: "BMB", Type: domain.TypeTask, Title: "t", Reviewer: &reviewer}}})
	if err != nil {
		t.Fatalf("participant reviewer on create: %v", err)
	}
	key, v := res.Tasks[0].Key, res.Tasks[0].Version

	it := e.update(t, e.actor, TaskPatch{Key: key, IfVersion: &v, Reviewer: FieldString{Set: true, Value: typo}})
	if it.OK || it.Err.Code != domain.CodeValidation || it.Err.Field != "reviewer" || !strings.Contains(it.Err.Message, "exec-1") {
		t.Fatalf("unknown reviewer on update: %+v", it.Err)
	}
	if it := e.update(t, e.actor, TaskPatch{Key: key, IfVersion: &v, Reviewer: FieldString{Set: true, Value: "test-admin"}}); !it.OK {
		t.Fatalf("participant reviewer on update: %v", it.Err)
	}
	v = e.get(t, key).Version
	if it := e.update(t, e.actor, TaskPatch{Key: key, IfVersion: &v, Reviewer: FieldString{Clear: true}}); !it.OK {
		t.Fatalf("clearing the reviewer: %v", it.Err)
	}
	if got := e.get(t, key).Reviewer; got != nil {
		t.Fatalf("reviewer after clear = %q", *got)
	}
}

// A card whose stored reviewer predates the rule keeps working, and the
// value can still be removed.
func TestReviewer_LegacyValueIsNotBroken(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "legacy")
	legacy := "Owner (by hand)"
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		cur, err := env.Tasks().GetByID(tx, task.ID)
		if err != nil {
			return err
		}
		cur.Reviewer = &legacy
		return env.Tasks().Update(tx, cur, nil)
	}); err != nil {
		t.Fatal(err)
	}
	e := &execEnv{testEnv: env}
	v := e.get(t, task.Key).Version
	title := "legacy, retitled"
	if it := e.update(t, env.actor, TaskPatch{Key: task.Key, IfVersion: &v, Title: &title, Column: "Doing"}); !it.OK {
		t.Fatalf("editing a card with a legacy reviewer: %v", it.Err)
	}
	v = e.get(t, task.Key).Version
	if it := e.update(t, env.actor, TaskPatch{Key: task.Key, IfVersion: &v, Reviewer: FieldString{Set: true, Value: legacy}}); !it.OK {
		t.Fatalf("re-sending the unchanged legacy reviewer: %v", it.Err)
	}
	v = e.get(t, task.Key).Version
	if it := e.update(t, env.actor, TaskPatch{Key: task.Key, IfVersion: &v, Reviewer: FieldString{Set: true, Value: ""}}); !it.OK {
		t.Fatalf("clearing a legacy reviewer with an empty value: %v", it.Err)
	}
}

func TestTaskCreate_RestoreKeepsHistoricalReviewer(t *testing.T) {
	env := openTestEnv(t)
	gone := "reviewer-from-last-year"
	res, err := env.svc.TaskCreate(context.Background(), env.actor, TaskCreateInput{Restore: true,
		Tasks: []NewTask{{ProjectKey: "BMB", Type: domain.TypeTask, Title: "imported", Reviewer: &gone}}})
	if err != nil {
		t.Fatalf("restore with a historical reviewer: %v", err)
	}
	if r := res.Tasks[0].Reviewer; r == nil || *r != gone {
		t.Fatalf("restored reviewer = %v", r)
	}
}

// ---------------------------------------------------------------------------
// progress_set assessor
// ---------------------------------------------------------------------------

func TestProgressSet_AssessorDefaultsToTokenName(t *testing.T) {
	e := openExecEnv(t)
	ctx := context.Background()
	own, _ := e.card(t, "own", "exec-1")

	cases := []struct {
		label    string
		actor    Actor
		assessor string
		want     string // recorded assessor; "" = refused
	}{
		{"writer, no assessor", e.actor, "", "test-admin"},
		{"writer, blank assessor", e.actor, "  ", "test-admin"},
		{"writer for someone else", e.actorWith(t, nonAdmin), "owner", "owner"},
		{"executor, no assessor", e.exec, "", "exec-1"},
		{"executor as itself", e.exec, "exec-1", "exec-1"},
		{"executor as someone else", e.exec, "owner", ""},
	}
	for _, tc := range cases {
		res, err := e.svc.ProgressSet(ctx, tc.actor, ProgressSetInput{TaskKey: own, Assessor: tc.assessor, Percent: 40})
		if tc.want == "" {
			wantForbidden(t, tc.label, err, "exec-1", "owner")
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.label, err)
		}
		if res.Mark.Assessor != tc.want {
			t.Errorf("%s: recorded assessor %q, want %q", tc.label, res.Mark.Assessor, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// participant without a key
// ---------------------------------------------------------------------------

func TestParticipantWithoutKey_OneCallNoSecretAssignableNoSignIn(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	seedProject(t, env, "OTHER")

	res, err := env.svc.ExecutorKeyIssue(ctx, env.actor, ExecutorKeyIssueInput{Name: "owner", ProjectKeys: []string{"bmb"}, ParticipantOnly: true})
	if err != nil {
		t.Fatalf("participant_only: %v", err)
	}
	if res.Secret != "" || !res.ParticipantOnly || !res.ExpiresAt.IsZero() || res.Renewed || res.Reissued {
		t.Fatalf("result = %+v, want no secret, no expiry, participant_only", res)
	}
	if strings.Join(res.ProjectKeys, ",") != "BMB" || strings.Join(res.AddedProjects, ",") != "BMB" {
		t.Fatalf("projects = %v added = %v", res.ProjectKeys, res.AddedProjects)
	}

	// Nothing to sign in with: the stored hash is no SHA-256 digest at all,
	// the row never expires, and no presented secret resolves to it.
	tok := storedToken(t, env, "owner")
	if !tok.HasNoSecret() || len(tok.Hash) == auth.HashBytes || tok.ExpiresAt != nil || tok.ID != res.TokenID {
		t.Fatalf("stored row = %+v", tok)
	}
	m := authManager(env)
	for _, candidate := range []string{auth.TokenPrefix + string(tok.Hash), auth.TokenPrefix + "owner", auth.TokenPrefix + tok.ID} {
		if got, err := m.VerifyToken(ctx, candidate); err != nil || got != nil {
			t.Fatalf("VerifyToken(%q) = %v, %v; a participant without a key must never authenticate", candidate, got, err)
		}
	}

	// A participant: listed, and a card can be assigned to and reviewed by it.
	board, err := env.svc.BoardGet(ctx, env.actor, BoardGetInput{ProjectKey: "BMB", View: ViewSummary})
	if err != nil {
		t.Fatal(err)
	}
	if !hasParticipant(board.Projects[0].Participants, "owner") {
		t.Fatalf("participants %v lack owner", board.Projects[0].Participants)
	}
	owner := "owner"
	if _, err := env.svc.TaskCreate(ctx, env.actor, TaskCreateInput{Tasks: []NewTask{{ProjectKey: "BMB", Type: domain.TypeTask, Title: "for the owner", Assignee: &owner, Reviewer: &owner}}}); err != nil {
		t.Fatalf("assigning to a participant without a key: %v", err)
	}
	_, err = env.svc.TaskCreate(ctx, env.actor, TaskCreateInput{Tasks: []NewTask{{ProjectKey: "OTHER", Type: domain.TypeTask, Title: "elsewhere", Assignee: &owner}}})
	wantValidation(t, "owner in a project it does not participate in", err, "assignee", "OTHER")

	// Again with another project: same row, the project is added.
	again, err := env.svc.ExecutorKeyIssue(ctx, env.actor, ExecutorKeyIssueInput{Name: "owner", ProjectKeys: []string{"OTHER", "BMB"}, ParticipantOnly: true})
	if err != nil {
		t.Fatalf("adding a project: %v", err)
	}
	if again.TokenID != res.TokenID || again.Secret != "" ||
		strings.Join(again.ProjectKeys, ",") != "BMB,OTHER" || strings.Join(again.AddedProjects, ",") != "OTHER" {
		t.Fatalf("second call = %+v", again)
	}
	if got := storedToken(t, env, "owner"); strings.Join(got.ProjectKeys, ",") != "BMB,OTHER" || !got.HasNoSecret() {
		t.Fatalf("stored after adding = %+v", got)
	}
	if _, err := env.svc.TaskCreate(ctx, env.actor, TaskCreateInput{Tasks: []NewTask{{ProjectKey: "OTHER", Type: domain.TypeTask, Title: "elsewhere", Assignee: &owner}}}); err != nil {
		t.Fatalf("assigning in the added project: %v", err)
	}
	repeat, err := env.svc.ExecutorKeyIssue(ctx, env.actor, ExecutorKeyIssueInput{Name: "owner", ProjectKeys: []string{"OTHER"}, ParticipantOnly: true})
	if err != nil || len(repeat.AddedProjects) != 0 || strings.Join(repeat.ProjectKeys, ",") != "BMB,OTHER" {
		t.Fatalf("repeat = %+v, %v; want a harmless no-op", repeat, err)
	}
}

func TestParticipantWithoutKey_Refusals(t *testing.T) {
	e := openExecEnv(t)
	ctx := context.Background()
	seedToken(t, e.testEnv, "tok-standing", "standing", domain.Scopes{domain.ScopeWrite})
	if _, err := e.svc.ExecutorKeyIssue(ctx, e.actor, ExecutorKeyIssueInput{Name: "owner", ProjectKeys: []string{"BMB"}, ParticipantOnly: true}); err != nil {
		t.Fatal(err)
	}

	issue := func(a Actor, in ExecutorKeyIssueInput) error {
		_, err := e.svc.ExecutorKeyIssue(ctx, a, in)
		return err
	}
	wantValidation(t, "participant_only over an executor key",
		issue(e.actor, ExecutorKeyIssueInput{Name: "exec-1", ProjectKeys: []string{"BMB"}, ParticipantOnly: true}), "name", "executor key")
	wantValidation(t, "participant_only over a standing token",
		issue(e.actor, ExecutorKeyIssueInput{Name: "standing", ProjectKeys: []string{"BMB"}, ParticipantOnly: true}), "name", "standing write token")
	wantValidation(t, "participant_only with a ttl",
		issue(e.actor, ExecutorKeyIssueInput{Name: "p2", ProjectKeys: []string{"BMB"}, ParticipantOnly: true, TTLSeconds: 3600}), "ttl_seconds")
	wantValidation(t, "participant_only without projects",
		issue(e.actor, ExecutorKeyIssueInput{Name: "p2", ParticipantOnly: true}), "projects")
	wantValidation(t, "participant_only and renew",
		issue(e.actor, ExecutorKeyIssueInput{Name: "owner", ProjectKeys: []string{"BMB"}, ParticipantOnly: true, Renew: true}), "renew")
	wantValidation(t, "a key over a participant without one",
		issue(e.actor, ExecutorKeyIssueInput{Name: "owner", ProjectKeys: []string{"BMB"}}), "name", "participant without a key")
	wantForbidden(t, "a writer who coordinates nothing",
		issue(e.actorWith(t, nonAdmin), ExecutorKeyIssueInput{Name: "p3", ProjectKeys: []string{"BMB"}, ParticipantOnly: true}), "coordinator")
	wantForbidden(t, "an executor key",
		issue(e.exec, ExecutorKeyIssueInput{Name: "p4", ProjectKeys: []string{"BMB"}, ParticipantOnly: true}))

	revokeToken(t, e.testEnv, "owner")
	wantValidation(t, "a revoked participant",
		issue(e.actor, ExecutorKeyIssueInput{Name: "owner", ProjectKeys: []string{"BMB"}, ParticipantOnly: true}), "name", "revoked")

	// The coordinator of the project may add participants to it.
	coord := seedToken(t, e.testEnv, "tok-coord", "coord", domain.Scopes{domain.ScopeWrite})
	setCoordinator(t, e.testEnv, e.actor, coord.ID)
	coordActor := Actor{TokenID: coord.ID, Name: coord.Name, Scopes: coord.Scopes}
	if err := issue(coordActor, ExecutorKeyIssueInput{Name: "person", ProjectKeys: []string{"BMB"}, ParticipantOnly: true}); err != nil {
		t.Fatalf("coordinator adding a participant: %v", err)
	}
}

// ---------------------------------------------------------------------------
// renewal
// ---------------------------------------------------------------------------

func TestExecutorKeyRenew_KeepsSecretMovesExpiry(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	m := authManager(env)
	first, err := env.svc.ExecutorKeyIssue(ctx, env.actor, ExecutorKeyIssueInput{Name: "exec-1", ProjectKeys: []string{"BMB"}, TTLSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}

	// A live key: renewed to now + ttl, same row, no secret in the answer,
	// and the secret the agent already holds still authenticates.
	before := time.Now().UTC()
	live, err := env.svc.ExecutorKeyIssue(ctx, env.actor, ExecutorKeyIssueInput{Name: "exec-1", Renew: true, TTLSeconds: 7200})
	if err != nil {
		t.Fatalf("renew a live key: %v", err)
	}
	if !live.Renewed || live.Secret != "" || live.TokenID != first.TokenID || strings.Join(live.ProjectKeys, ",") != "BMB" {
		t.Fatalf("renew result = %+v", live)
	}
	if d := live.ExpiresAt.Sub(before); d < 2*time.Hour-time.Minute || d > 2*time.Hour+time.Minute {
		t.Fatalf("renewed for %v, want 2h", d)
	}
	if got := storedToken(t, env, "exec-1"); got.ExpiresAt == nil || !got.ExpiresAt.Equal(live.ExpiresAt) {
		t.Fatalf("stored expiry %v, want %v", got.ExpiresAt, live.ExpiresAt)
	}
	if tok, err := m.VerifyToken(ctx, first.Secret); err != nil || tok == nil || tok.ID != first.TokenID {
		t.Fatalf("the original secret after renewal: %v, %v", tok, err)
	}

	// An expired key: the secret stops working, and a renewal brings the
	// same secret back, with the key back on the participant list.
	expireKey(t, env, "exec-1")
	if tok, _ := m.VerifyToken(ctx, first.Secret); tok != nil {
		t.Fatal("an expired key still authenticates")
	}
	expired, err := env.svc.ExecutorKeyIssue(ctx, env.actor, ExecutorKeyIssueInput{Name: "exec-1", Renew: true})
	if err != nil {
		t.Fatalf("renew an expired key: %v", err)
	}
	if d := expired.ExpiresAt.Sub(before); d < 24*time.Hour-time.Minute || d > 24*time.Hour+time.Minute {
		t.Fatalf("default renewal %v, want 24h", d)
	}
	if tok, err := m.VerifyToken(ctx, first.Secret); err != nil || tok == nil || tok.ID != first.TokenID {
		t.Fatalf("the original secret after renewing an expired key: %v, %v", tok, err)
	}
	board, err := env.svc.BoardGet(ctx, env.actor, BoardGetInput{ProjectKey: "BMB", View: ViewSummary})
	if err != nil {
		t.Fatal(err)
	}
	if !hasParticipant(board.Projects[0].Participants, "exec-1") {
		t.Fatal("a renewed key is not a participant again")
	}
}

func TestExecutorKeyRenew_Refusals(t *testing.T) {
	e := openExecEnv(t)
	ctx := context.Background()
	seedToken(t, e.testEnv, "tok-standing", "standing", domain.Scopes{domain.ScopeWrite})
	if _, err := e.svc.ExecutorKeyIssue(ctx, e.actor, ExecutorKeyIssueInput{Name: "owner", ProjectKeys: []string{"BMB"}, ParticipantOnly: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ExecutorKeyIssue(ctx, e.actor, ExecutorKeyIssueInput{Name: "exec-dead", ProjectKeys: []string{"BMB"}}); err != nil {
		t.Fatal(err)
	}
	revokeToken(t, e.testEnv, "exec-dead")

	renew := func(a Actor, in ExecutorKeyIssueInput) error {
		in.Renew = true
		_, err := e.svc.ExecutorKeyIssue(ctx, a, in)
		return err
	}
	wantValidation(t, "renew a revoked key", renew(e.actor, ExecutorKeyIssueInput{Name: "exec-dead"}), "name", "revoked")
	wantValidation(t, "renew a standing token", renew(e.actor, ExecutorKeyIssueInput{Name: "standing"}), "name", "standing write token")
	wantValidation(t, "renew a participant without a key", renew(e.actor, ExecutorKeyIssueInput{Name: "owner"}), "name", "participant without a key")
	wantValidation(t, "renew with projects", renew(e.actor, ExecutorKeyIssueInput{Name: "exec-1", ProjectKeys: []string{"BMB"}}), "projects")
	wantValidation(t, "renew past the maximum", renew(e.actor, ExecutorKeyIssueInput{Name: "exec-1", TTLSeconds: int(domain.ExecutorKeyMaxTTL.Seconds()) + 1}), "ttl_seconds")
	if e := domain.AsError(renew(e.actor, ExecutorKeyIssueInput{Name: "nobody"})); e == nil || e.Code != domain.CodeNotFound || e.Remediation == "" {
		t.Fatalf("renew an unknown name: %v, want not_found", e)
	}
	wantForbidden(t, "renew by a writer who coordinates nothing", renew(e.actorWith(t, nonAdmin), ExecutorKeyIssueInput{Name: "exec-1"}), "coordinate")
	wantForbidden(t, "renew by the executor itself", renew(e.exec, ExecutorKeyIssueInput{Name: "exec-1"}))

	coord := seedToken(t, e.testEnv, "tok-coord", "coord", domain.Scopes{domain.ScopeWrite})
	setCoordinator(t, e.testEnv, e.actor, coord.ID)
	if err := renew(Actor{TokenID: coord.ID, Name: coord.Name, Scopes: coord.Scopes}, ExecutorKeyIssueInput{Name: "exec-1"}); err != nil {
		t.Fatalf("renew by the project's coordinator: %v", err)
	}
}
