package mcp

import (
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// TestBoardGuide_CoordinatorAndParticipants is KANB-44's second surface:
// the card asks for the lead and the participants on BOTH board_get(view:
// summary) and board_guide. board_get is covered by
// TestBoardGet_CoordinatorAndParticipantsPublished; this is the guide.
//
// The negative half matters as much as the positive one. The card's fourth
// acceptance criterion is "secrets are handed out nowhere", so the test
// walks the whole participant object and fails on any key that is not
// token_id or name -- a future field added to the wire type cannot slip a
// hash past a test that only checks the two fields it expects.
func TestBoardGuide_CoordinatorAndParticipants(t *testing.T) {
	t.Parallel()
	for name, newSrv := range map[string]func(service.Service, string) *gomcp.Server{
		"full":      NewServer,
		"read-only": NewReadOnlyServer,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cs, svc := roundtripServer(t, newSrv)
			board := configuredBoard()
			board.Projects[0].Coordinator = &service.Participant{TokenID: "tok-lead", Name: "lead"}
			board.Projects[0].Participants = []service.Participant{
				{TokenID: "tok-lead", Name: "lead"},
				{TokenID: "tok-agent", Name: "agent"},
			}
			svc.DefaultBoardGet = board

			_, sc := callTool(t, cs, "board_guide", map[string]any{"project": "BMB"})
			expectOK(t, sc, "board_guide")
			proj, ok := sc["data"].(map[string]any)["project"].(map[string]any)
			if !ok {
				t.Fatalf("data.project missing: %v", sc["data"])
			}

			coord, ok := proj["coordinator"].(map[string]any)
			if !ok {
				t.Fatalf("data.project.coordinator missing: %v", proj)
			}
			assertParticipantShape(t, "coordinator", coord, "tok-lead", "lead")

			parts, ok := proj["participants"].([]any)
			if !ok || len(parts) != 2 {
				t.Fatalf("data.project.participants = %v, want 2 entries", proj["participants"])
			}
			assertParticipantShape(t, "participants[0]", parts[0].(map[string]any), "tok-lead", "lead")
			assertParticipantShape(t, "participants[1]", parts[1].(map[string]any), "tok-agent", "agent")
		})
	}
}

// TestBoardGuide_NoParticipantsOmitsTheFields keeps the guide honest for a
// project nobody has configured: an absent coordinator must be absent, not
// an empty object that reads like "somebody, name unknown".
func TestBoardGuide_NoParticipantsOmitsTheFields(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultBoardGet = configuredBoard()

	_, sc := callTool(t, cs, "board_guide", map[string]any{"project": "BMB"})
	expectOK(t, sc, "board_guide")
	proj := sc["data"].(map[string]any)["project"].(map[string]any)
	if _, present := proj["coordinator"]; present {
		t.Errorf("coordinator should be omitted when unset: %v", proj["coordinator"])
	}
	if _, present := proj["participants"]; present {
		t.Errorf("participants should be omitted when empty: %v", proj["participants"])
	}
}

// assertParticipantShape fails unless the object is exactly the published
// pair. Any extra key is a leak by definition: participantOut is the only
// shape a participant travels in, and it is the shape that deliberately has
// no room for a secret.
func assertParticipantShape(t *testing.T, where string, got map[string]any, wantID, wantName string) {
	t.Helper()
	if got["token_id"] != wantID || got["name"] != wantName {
		t.Errorf("%s = %v, want {token_id: %s, name: %s}", where, got, wantID, wantName)
	}
	for k := range got {
		if k != "token_id" && k != "name" {
			t.Errorf("%s carries unexpected field %q (%v) -- participants publish an id and a name, never anything else", where, k, got[k])
		}
	}
}
