package mcp

import (
	"context"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// TestProjectUpsert_WIPLimitIsRefusedWithTheReason (KANB-59): the board has
// no WIP limits since 26.09.2026. A caller that still sends wip_limit must
// learn that — neither a bare "unexpected property" from the schema nor a
// silent success that lets it believe a limit is in force — and nothing may
// reach the service.
func TestProjectUpsert_WIPLimitIsRefusedWithTheReason(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultProjectUpsert = &service.ProjectUpsertResult{}

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "project_upsert",
		Arguments: map[string]any{
			"mode": "create", "key": "WIP", "name": "Old habits",
			"columns": []any{
				map[string]any{"name": "Backlog", "kind": "backlog"},
				map[string]any{"name": "Doing", "kind": "active", "wip_limit": 3},
				map[string]any{"name": "Done", "kind": "done"},
			},
		},
	})
	if err != nil {
		t.Fatalf("the refusal must be a CallToolResult, not a protocol error: %v", err)
	}
	env := errorEnvelopeOf(t, res)
	if env["code"] != string(domain.CodeValidation) {
		t.Errorf("code = %v, want %v", env["code"], domain.CodeValidation)
	}
	msg, _ := env["message"].(string)
	for _, want := range []string{"Doing", "no WIP limits"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not say %q", msg, want)
		}
	}
	rem, _ := env["remediation"].(string)
	if !strings.Contains(rem, "Drop wip_limit") || !strings.Contains(rem, "idle time") {
		t.Errorf("remediation %q must say what to do and what replaced the limit", rem)
	}
	if svc.LastProjectUpsert.Key != "" {
		t.Errorf("service was invoked despite the refusal: %+v", svc.LastProjectUpsert)
	}
}
