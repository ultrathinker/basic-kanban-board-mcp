package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// TestProjectUpsert_WIPLimitIsRefusedWithTheReason (KANB-59): the board has
// no WIP limits since 26.09.2026. wip_limit is gone from the schema, so an
// agent reading it is not invited to send it; one that sends it anyway (an
// old schema, an old habit) must learn why — not a bare "unexpected
// property", not a silent success — and nothing may reach the service.
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
	for _, want := range []string{"wip_limit", "no WIP limits"} {
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

func TestProjectUpsert_SchemaNoLongerOffersWIPLimit(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		raw, _ := json.Marshal(tool.InputSchema)
		if strings.Contains(string(raw), "wip_limit") {
			t.Errorf("%s still publishes wip_limit in its input schema", tool.Name)
		}
	}
}
