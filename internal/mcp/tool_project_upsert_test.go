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

// TestProjectUpsert_ColumnKindSchemaEnum_MatchesDomainAllKinds is KANB-52's
// drift canary. columns[].kind's published enum must be built from
// domain.AllKinds, not a hand-typed list — the exact disease this project
// already caught once with its tool count ("nine tools" in the docs,
// twelve real ones). A hand-typed enum here is invisible to every domain-
// level test: KindWaiting was added, CheckMove's exception was added and
// tested, and the schema still refused "waiting" before the handler ever
// ran, because nothing forced the wire enum to track the internal one.
//
// This test rebuilds its expectation directly from domain.AllKinds rather
// than calling columnKindNames() (the production helper under test), so it
// actually fails if a future edit reverts the wiring to a hand-typed list
// and someone later adds a fifth kind to domain.AllKinds without touching
// the schema.
func TestProjectUpsert_ColumnKindSchemaEnum_MatchesDomainAllKinds(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)
	tool := toolByName(t, cs, "project_upsert")

	// Walk the PUBLISHED schema (what tools/list actually sends), the same
	// discipline every test in tool_docs_test.go follows — not the Go
	// jsonschema.Schema value construction happens to produce internally.
	kindNode := schemaNode(t, tool.InputSchema, "properties", "columns", "items", "properties", "kind")
	rawEnum, ok := kindNode["enum"].([]any)
	if !ok {
		t.Fatalf("columns[].kind has no enum in the published schema: %v", kindNode)
	}

	want := make([]string, len(domain.AllKinds))
	for i, k := range domain.AllKinds {
		want[i] = string(k)
	}
	if len(rawEnum) != len(want) {
		t.Fatalf("published enum = %v, want exactly %v (domain.AllKinds)", rawEnum, want)
	}
	for i, wantKind := range want {
		got, _ := rawEnum[i].(string)
		if got != wantKind {
			t.Errorf("published enum[%d] = %v, want %q", i, rawEnum[i], wantKind)
		}
	}

	found := false
	for _, v := range rawEnum {
		if v == string(domain.KindWaiting) {
			found = true
		}
	}
	if !found {
		t.Fatalf("published enum %v does not include %q — an MCP caller could never create a waiting column", rawEnum, domain.KindWaiting)
	}
}

// TestProjectUpsert_ColumnKindWaiting_ReachesTheService is the end-to-end
// proof that KANB-52's Waiting kind is reachable THROUGH THE MCP LAYER, not
// only through a direct service call: the schema fix above is necessary but
// not sufficient on its own if something else on the wire path still
// mangled or rejected the value before it reached the service.
func TestProjectUpsert_ColumnKindWaiting_ReachesTheService(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultProjectUpsert = &service.ProjectUpsertResult{
		Project: domain.Project{Key: "BMB", Name: "BeeMemoryBank", Version: 2},
	}

	_, sc := callTool(t, cs, "project_upsert", map[string]any{
		"mode": "update", "key": "BMB", "if_version": 1,
		"columns": []map[string]any{
			{"name": "Backlog", "kind": "backlog"},
			{"name": "Doing", "kind": "active"},
			{"name": "Done", "kind": "done"},
			{"name": "Waiting", "kind": "waiting"},
		},
	})
	expectOK(t, sc, "project_upsert")

	cols := svc.LastProjectUpsert.Columns
	var waiting *service.ColumnSpec
	for i := range cols {
		if cols[i].Name == "Waiting" {
			waiting = &cols[i]
		}
	}
	if waiting == nil {
		t.Fatalf("service never received a Waiting column: %+v", cols)
	}
	if waiting.Kind != domain.KindWaiting {
		t.Fatalf("service Waiting column kind = %q, want %q", waiting.Kind, domain.KindWaiting)
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
