package service

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ---------------------------------------------------------------------------
// KANB-44 — coordinator in project settings, participants derived from tokens,
// identity = tokens.id.
// ---------------------------------------------------------------------------

// seedToken inserts a token row so coordinator appointments and participant
// derivation have real rows to resolve.
func seedToken(t *testing.T, env *testEnv, id, name string, scopes domain.Scopes, projectKeys ...string) *domain.Token {
	t.Helper()
	tok := &domain.Token{ID: id, Name: name, Hash: []byte("hash-" + id), Scopes: scopes, ProjectKeys: projectKeys}
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		return env.Tokens().Create(tx, tok)
	}); err != nil {
		t.Fatalf("seed token %s: %v", name, err)
	}
	return tok
}

func revokeToken(t *testing.T, env *testEnv, name string) {
	t.Helper()
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		return env.Tokens().Revoke(tx, name)
	}); err != nil {
		t.Fatalf("revoke token %s: %v", name, err)
	}
}

// setCoordinator applies settings.coordinator through the public upsert and
// returns the project's new version.
func setCoordinator(t *testing.T, env *testEnv, a Actor, tokenID string) int {
	t.Helper()
	var version int
	if err := env.Read(context.Background(), func(tx store.Tx) error {
		p, err := env.Projects().GetByKey(tx, env.proj.Key)
		if err != nil {
			return err
		}
		version = p.Version
		return nil
	}); err != nil {
		t.Fatalf("read project version: %v", err)
	}
	res, err := env.svc.ProjectUpsert(context.Background(), a, ProjectUpsertInput{
		Mode:      UpsertUpdate,
		Key:       env.proj.Key,
		IfVersion: &version,
		Settings:  &ProjectSettings{Coordinator: &tokenID},
	})
	if err != nil {
		t.Fatalf("set coordinator %q: %v", tokenID, err)
	}
	return res.Project.Version
}

// TestProjectUpsert_CoordinatorAdminOnly: the coordinator is part of project
// configuration, and project_upsert is admin-only — so a write token cannot
// appoint a coordinator, not even one pointing at itself (KANB-44 §13.1).
func TestProjectUpsert_CoordinatorAdminOnly(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	writer := Actor{TokenID: "tok-w", Name: "writer", Scopes: domain.Scopes{domain.ScopeWrite}}

	self := "tok-w"
	_, err := env.svc.ProjectUpsert(ctx, writer, ProjectUpsertInput{
		Mode:      UpsertUpdate,
		Key:       env.proj.Key,
		IfVersion: intPtrLocal(1),
		Settings:  &ProjectSettings{Coordinator: &self},
	})
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeForbidden {
		t.Fatalf("write-scope coordinator appointment: got %v, want forbidden", err)
	}

	reader := Actor{TokenID: "tok-r", Name: "reader", Scopes: domain.Scopes{domain.ScopeRead}}
	_, err = env.svc.ProjectUpsert(ctx, reader, ProjectUpsertInput{
		Mode:      UpsertUpdate,
		Key:       env.proj.Key,
		IfVersion: intPtrLocal(1),
		Settings:  &ProjectSettings{Coordinator: &self},
	})
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeForbidden {
		t.Fatalf("read-scope coordinator appointment: got %v, want forbidden", err)
	}

	// The admin path works and the appointment is visible.
	seedToken(t, env, "tok-agent", "agent", domain.Scopes{domain.ScopeWrite})
	setCoordinator(t, env, env.actor, "tok-agent")
	board, err := env.svc.BoardGet(ctx, env.actor, BoardGetInput{ProjectKey: env.proj.Key, View: ViewSummary})
	if err != nil {
		t.Fatalf("board_get: %v", err)
	}
	if board.Projects[0].Coordinator == nil || board.Projects[0].Coordinator.TokenID != "tok-agent" {
		t.Fatalf("coordinator after admin appointment = %+v, want tok-agent", board.Projects[0].Coordinator)
	}
}

// TestProjectUpsert_CoordinatorValidation: a bad appointment is refused and
// rolls the whole upsert back; an empty string clears; absence leaves the
// stored value untouched.
func TestProjectUpsert_CoordinatorValidation(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	seedToken(t, env, "tok-agent", "agent", domain.Scopes{domain.ScopeWrite})
	seedToken(t, env, "tok-foreign", "foreign", domain.Scopes{domain.ScopeWrite}, "OTHER")
	seedToken(t, env, "tok-dead", "dead", domain.Scopes{domain.ScopeWrite})
	revokeToken(t, env, "dead")

	cases := []struct {
		name      string
		tokenID   string
		wantField string
	}{
		{"unknown token", "tok-missing", "settings.coordinator"},
		{"revoked token", "tok-dead", "settings.coordinator"},
		{"token without project access", "tok-foreign", "settings.coordinator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var version int
			if err := env.Read(ctx, func(tx store.Tx) error {
				p, err := env.Projects().GetByKey(tx, env.proj.Key)
				if err != nil {
					return err
				}
				version = p.Version
				return nil
			}); err != nil {
				t.Fatalf("read version: %v", err)
			}
			id := tc.tokenID
			_, err := env.svc.ProjectUpsert(ctx, env.actor, ProjectUpsertInput{
				Mode:      UpsertUpdate,
				Key:       env.proj.Key,
				IfVersion: &version,
				Settings:  &ProjectSettings{Coordinator: &id},
			})
			de := domain.AsError(err)
			if de == nil || de.Code != domain.CodeValidation || de.Field != tc.wantField {
				t.Fatalf("appointment of %q: got %v, want validation error on %s", tc.tokenID, err, tc.wantField)
			}
		})
	}

	// Valid appointment, then an explicit clear, then a no-op update leaves
	// the (cleared) value alone.
	setCoordinator(t, env, env.actor, "tok-agent")
	empty := ""
	setCoordinator(t, env, env.actor, empty)
	board, err := env.svc.BoardGet(ctx, env.actor, BoardGetInput{ProjectKey: env.proj.Key, View: ViewSummary})
	if err != nil {
		t.Fatalf("board_get: %v", err)
	}
	if board.Projects[0].Coordinator != nil {
		t.Fatalf("coordinator after clear = %+v, want nil", board.Projects[0].Coordinator)
	}

	// A settings update that does not mention coordinator keeps it.
	setCoordinator(t, env, env.actor, "tok-agent")
	var version int
	if err := env.Read(ctx, func(tx store.Tx) error {
		p, err := env.Projects().GetByKey(tx, env.proj.Key)
		if err != nil {
			return err
		}
		version = p.Version
		return nil
	}); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if _, err := env.svc.ProjectUpsert(ctx, env.actor, ProjectUpsertInput{
		Mode:      UpsertUpdate,
		Key:       env.proj.Key,
		IfVersion: &version,
		Settings:  &ProjectSettings{},
	}); err != nil {
		t.Fatalf("unrelated settings update: %v", err)
	}
	board, err = env.svc.BoardGet(ctx, env.actor, BoardGetInput{ProjectKey: env.proj.Key, View: ViewSummary})
	if err != nil {
		t.Fatalf("board_get: %v", err)
	}
	if board.Projects[0].Coordinator == nil || board.Projects[0].Coordinator.TokenID != "tok-agent" {
		t.Fatalf("coordinator after unrelated update = %+v, want tok-agent", board.Projects[0].Coordinator)
	}
}

// TestBoardGet_ParticipantsDerivedFromTokens: the participant list is DERIVED
// from the tokens that have access to the project — active tokens only, no
// second catalogue, no secrets.
func TestBoardGet_ParticipantsDerivedFromTokens(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	// env.actor ("test-admin") is all-projects; add a scoped-in token, a
	// scoped-out token and a revoked one.
	seedToken(t, env, "tok-agent", "agent", domain.Scopes{domain.ScopeWrite}, env.proj.Key)
	seedToken(t, env, "tok-outsider", "outsider", domain.Scopes{domain.ScopeWrite}, "OTHER")
	seedToken(t, env, "tok-revoked", "revoked", domain.Scopes{domain.ScopeWrite})
	revokeToken(t, env, "revoked")
	setCoordinator(t, env, env.actor, "tok-agent")

	board, err := env.svc.BoardGet(ctx, env.actor, BoardGetInput{ProjectKey: env.proj.Key, View: ViewSummary})
	if err != nil {
		t.Fatalf("board_get: %v", err)
	}
	bp := board.Projects[0]

	if bp.Coordinator == nil || bp.Coordinator.TokenID != "tok-agent" || bp.Coordinator.Name != "agent" {
		t.Fatalf("coordinator = %+v, want {tok-agent agent}", bp.Coordinator)
	}
	want := []Participant{
		{TokenID: "tok-agent", Name: "agent"},
		{TokenID: env.actor.TokenID, Name: env.actor.Name},
	}
	if !reflect.DeepEqual(bp.Participants, want) {
		t.Fatalf("participants = %+v, want %+v (active tokens with access, sorted by name; revoked and scoped-out excluded)", bp.Participants, want)
	}

	// No secrets ever travel with a participant: the shape is id + name, and
	// its JSON form must contain neither a hash nor any other credential
	// field.
	b, err := json.Marshal(bp.Participants)
	if err != nil {
		t.Fatalf("marshal participants: %v", err)
	}
	if string(b) != `[{"TokenID":"tok-agent","Name":"agent"},{"TokenID":"tok-admin","Name":"test-admin"}]` {
		t.Fatalf("participants JSON = %s, want exactly token id and name", b)
	}
	var pt Participant
	if got := reflect.TypeOf(pt).NumField(); got != 2 {
		t.Fatalf("Participant has %d fields, want 2 (TokenID, Name) — a secret field would be a contract break", got)
	}
}

// TestCoordinator_IdentitySurvivesRotation: rotation is UPDATE tokens SET
// hash = ? WHERE id = ? — the row, its id and its name survive. The
// coordinator appointment, keyed on tokens.id, therefore survives too
// (KANB-44 acceptance 3).
func TestCoordinator_IdentitySurvivesRotation(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	seedToken(t, env, "tok-agent", "agent", domain.Scopes{domain.ScopeWrite})
	setCoordinator(t, env, env.actor, "tok-agent")

	// Rotate exactly the way kanban token rotate does: replace the hash in
	// place, keeping the row and its id.
	if err := env.Write(ctx, func(tx store.Tx) error {
		return env.Tokens().UpdateHash(tx, "tok-agent", []byte("rotated-hash"))
	}); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	board, err := env.svc.BoardGet(ctx, env.actor, BoardGetInput{ProjectKey: env.proj.Key, View: ViewSummary})
	if err != nil {
		t.Fatalf("board_get after rotation: %v", err)
	}
	c := board.Projects[0].Coordinator
	if c == nil || c.TokenID != "tok-agent" || c.Name != "agent" {
		t.Fatalf("coordinator after rotation = %+v, want the same {tok-agent agent} — identity is tokens.id, not the secret", c)
	}
}
