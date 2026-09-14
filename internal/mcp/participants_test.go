package mcp

import (
	"context"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// ---------------------------------------------------------------------------
// KANB-44 — coordinator and participants on the MCP surface.
// ---------------------------------------------------------------------------

// TestBoardGet_CoordinatorAndParticipantsPublished: board_get's structured
// output carries the appointed coordinator and the derived participant list —
// ids and display names, never a secret.
func TestBoardGet_CoordinatorAndParticipantsPublished(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultBoardGet = &service.Board{Projects: []service.BoardProject{{
		Key: "KANB", Name: "Track B",
		Coordinator: &service.Participant{TokenID: "tok-lead", Name: "lead"},
		Participants: []service.Participant{
			{TokenID: "tok-lead", Name: "lead"},
			{TokenID: "tok-agent", Name: "agent"},
		},
	}}}

	_, sc := callTool(t, cs, "board_get", map[string]any{"project": "kanb", "view": "summary"})
	expectOK(t, sc, "board_get")

	data, _ := sc["data"].(map[string]any)
	projects, _ := data["projects"].([]any)
	if len(projects) != 1 {
		t.Fatalf("projects = %v, want 1", projects)
	}
	p := projects[0].(map[string]any)

	coord, ok := p["coordinator"].(map[string]any)
	if !ok || coord["token_id"] != "tok-lead" || coord["name"] != "lead" {
		t.Fatalf("coordinator = %v, want {token_id: tok-lead, name: lead}", p["coordinator"])
	}
	parts, ok := p["participants"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("participants = %v, want 2 entries", p["participants"])
	}
	first := parts[0].(map[string]any)
	if first["token_id"] != "tok-lead" || first["name"] != "lead" {
		t.Fatalf("participants[0] = %v, want the coordinator", first)
	}
	if _, has := first["hash"]; has {
		t.Fatal("participant output carries a hash field — secrets must never travel")
	}
}

// TestProjectUpsert_CoordinatorForwarded: settings.coordinator reaches the
// service untouched, so the tokens.id contract survives the tool boundary.
func TestProjectUpsert_CoordinatorForwarded(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.NextProjectUpsert = func(_ context.Context, _ service.Actor, in service.ProjectUpsertInput) (*service.ProjectUpsertResult, error) {
		if in.Settings == nil || in.Settings.Coordinator == nil || *in.Settings.Coordinator != "tok-lead" {
			t.Errorf("settings.coordinator forwarded = %+v, want tok-lead", in.Settings)
		}
		return &service.ProjectUpsertResult{}, nil
	}

	_, sc := callTool(t, cs, "project_upsert", map[string]any{
		"mode":       "update",
		"key":        "kanb",
		"if_version": 4,
		"settings":   map[string]any{"coordinator": "tok-lead"},
	})
	expectOK(t, sc, "project_upsert")
}
