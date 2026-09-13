package mcp

import (
	"context"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// TestProjectUpsert_DescriptionAppend_Forwarded verifies the wire field
// reaches the service untouched, the same way BodyAppend does for
// task_update — the MCP layer does no joining or validation of its own,
// that is entirely the service's job (internal/service/project.go
// applyDescription).
func TestProjectUpsert_DescriptionAppend_Forwarded(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultProjectUpsert = &service.ProjectUpsertResult{
		Project: domain.Project{Key: "BMB", Name: "BeeMemoryBank", Version: 2},
	}

	_, sc := callTool(t, cs, "project_upsert", map[string]any{
		"mode": "update", "key": "bmb", "if_version": 1,
		"description_append": "one more line",
	})
	expectOK(t, sc, "project_upsert")

	if svc.LastProjectUpsert.DescriptionAppend == nil || *svc.LastProjectUpsert.DescriptionAppend != "one more line" {
		t.Errorf("service DescriptionAppend = %v, want \"one more line\"", svc.LastProjectUpsert.DescriptionAppend)
	}
	if svc.LastProjectUpsert.Description != nil {
		t.Errorf("service Description = %v, want nil (only description_append was sent)", svc.LastProjectUpsert.Description)
	}
}

// TestProjectUpsert_DescriptionAndDescriptionAppend_BothForwarded checks the
// wire shape allows sending both in one call (the schema does not declare
// them mutually exclusive — task_update's body/body_append precedent does
// not either) and lets the service be the one that refuses it; this test
// pins that the MCP layer does forward both through untouched rather than
// silently dropping one.
func TestProjectUpsert_DescriptionAndDescriptionAppend_BothForwarded(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultProjectUpsertEr = domain.Invalid("description_append",
		"description and description_append are mutually exclusive",
		"Send either description (replace) or description_append (append), not both.")

	res, sc := callTool(t, cs, "project_upsert", map[string]any{
		"mode": "update", "key": "BMB", "if_version": 1,
		"description":        "replace",
		"description_append": "extra",
	})
	if !res.IsError {
		t.Fatalf("expected the service's mutual-exclusion refusal to surface, got success: %v", sc)
	}
	if svc.LastProjectUpsert.Description == nil || *svc.LastProjectUpsert.Description != "replace" {
		t.Errorf("service Description = %v, want \"replace\" (both fields forwarded, not dropped)", svc.LastProjectUpsert.Description)
	}
	if svc.LastProjectUpsert.DescriptionAppend == nil || *svc.LastProjectUpsert.DescriptionAppend != "extra" {
		t.Errorf("service DescriptionAppend = %v, want \"extra\"", svc.LastProjectUpsert.DescriptionAppend)
	}
	env := errorEnvelopeOf(t, res)
	if env["code"] != string(domain.CodeValidation) {
		t.Errorf("code = %v, want validation", env["code"])
	}
}

// TestProjectUpsert_DescriptionAppend_PublishedInSchema proves
// description_append is a real, typed property of the published input
// schema (not just a Go-side field the SDK happens to decode): a wrong
// JSON type for it must be rejected before the handler ever runs.
func TestProjectUpsert_DescriptionAppend_PublishedInSchema(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "project_upsert",
		Arguments: map[string]any{
			"mode": "update", "key": "BMB", "if_version": 1,
			"description_append": 123,
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatalf("a non-string description_append was accepted")
	}
	if svc.LastProjectUpsert.DescriptionAppend != nil {
		t.Errorf("service was invoked despite the schema rejection: %+v", svc.LastProjectUpsert)
	}
}
