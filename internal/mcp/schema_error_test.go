package mcp

import (
	"context"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// toolByName returns the published tool definition a client sees in
// tools/list — descriptions and schemas exactly as a model receives them.
func toolByName(t *testing.T, cs *gomcp.ClientSession, name string) *gomcp.Tool {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not published", name)
	return nil
}

// errorEnvelopeOf pulls the {code, message, remediation} envelope out of a
// failed call's structuredContent, failing the test if there is none — the
// envelope is a contract, not a best effort (PLAN §6).
func errorEnvelopeOf(t *testing.T, res *gomcp.CallToolResult) map[string]any {
	t.Helper()
	if !res.IsError {
		t.Fatalf("IsError = false, want true")
	}
	if res.StructuredContent == nil {
		t.Fatalf("structuredContent is nil; every failure must carry the envelope")
	}
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent is %T, want an object", res.StructuredContent)
	}
	if ok, _ := sc["ok"].(bool); ok {
		t.Errorf("ok = true on a failure: %v", sc)
	}
	env, ok := sc["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope in structuredContent: %v", sc)
	}
	return env
}

// TestProjectUpsert_MissingMode_TeachesTheCall is LIVE-FINDINGS §1: the first
// call any agent makes at a tool named "upsert" is the one that omits `mode`,
// and it used to come back as a bare validator sentence
// (`missing properties: ["mode"]`) with no envelope and no hint.
//
// PLAN §6.9 makes `mode` required on purpose — a typo in an immutable project
// key must not fork the board into a second project — so the fix is not to
// default it away but to make the refusal teach the correct call, which is
// AGENTS.md's "fail loud" applied to the schema gate.
func TestProjectUpsert_MissingMode_TeachesTheCall(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultProjectUpsert = &service.ProjectUpsertResult{}

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "project_upsert",
		Arguments: map[string]any{"key": "TEST", "name": "Smoke Test"},
	})
	if err != nil {
		t.Fatalf("a schema rejection must be a CallToolResult, not a protocol error: %v", err)
	}
	env := errorEnvelopeOf(t, res)

	if env["code"] != string(domain.CodeValidation) {
		t.Errorf("code = %v, want %v", env["code"], domain.CodeValidation)
	}
	msg, _ := env["message"].(string)
	if !strings.Contains(msg, "mode") {
		t.Errorf("message %q does not name the missing property", msg)
	}
	rem, _ := env["remediation"].(string)
	for _, want := range []string{"mode", "create", "update", "key"} {
		if !strings.Contains(rem, want) {
			t.Errorf("remediation %q does not mention %q; it must say what to send instead", rem, want)
		}
	}
	// The handler must not have run: a rejected call changes nothing.
	if svc.LastProjectUpsert.Key != "" {
		t.Errorf("service was invoked despite the rejection: %+v", svc.LastProjectUpsert)
	}
}

// TestProjectUpsert_DescriptionCarriesBothCalls pins the other half of the
// fix: the model should get the call right from tools/list alone, without
// spending a round trip on a rejection first.
func TestProjectUpsert_DescriptionCarriesBothCalls(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)
	desc := toolByName(t, cs, "project_upsert").Description

	for _, want := range []string{
		`"mode":"create"`, // a literal example, not prose about a mode field
		`"mode":"update"`, // …for both branches
		`"if_version"`,    // the other thing an update call needs
		"fork",            // why it is required at all
		"Backlog",         // what create produces when columns are omitted
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("project_upsert description does not contain %q:\n%s", want, desc)
		}
	}
}

// TestSchemaRejection_KeepsTheValidatorsOwnSentence guards the repair from
// becoming a downgrade: wrapping the SDK's message in an envelope must not
// lose it. The unknown-field tests rely on that text naming the bad property.
func TestSchemaRejection_KeepsTheValidatorsOwnSentence(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "task_get",
		Arguments: map[string]any{
			"keys":     []string{"BMB-1"},
			"surprise": "not in the schema",
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	env := errorEnvelopeOf(t, res)
	if msg, _ := env["message"].(string); !strings.Contains(msg, "surprise") {
		t.Errorf("message %q lost the offending property name", msg)
	}
	// The text content must still carry it too — some clients only read that.
	tc, _ := res.Content[0].(*gomcp.TextContent)
	if tc == nil || !strings.Contains(tc.Text, "surprise") {
		t.Errorf("text content lost the offending property name: %+v", res.Content)
	}
}

// TestSchemaRejection_LeavesHandlerEnvelopesAlone: a domain failure already
// carries its own envelope, and the middleware must not rewrite it into a
// generic validation error.
func TestSchemaRejection_LeavesHandlerEnvelopesAlone(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskGet = nil
	svc.NextTaskGet = func(_ context.Context, _ service.Actor, _ service.TaskGetInput) (*service.TaskGetResult, error) {
		return nil, domain.NotFound("project", "TEST")
	}

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "task_get",
		Arguments: map[string]any{"keys": []string{"BMB-1"}},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	env := errorEnvelopeOf(t, res)
	if env["code"] != string(domain.CodeNotFound) {
		t.Errorf("code = %v, want not_found; the handler's envelope was overwritten", env["code"])
	}
	if msg, _ := env["message"].(string); !strings.Contains(msg, "TEST") {
		t.Errorf("message = %q, want the key the caller asked for", msg)
	}
}

// TestSchemaRemediation_ListsWhatEachToolRequires proves the remediation is
// read off the published schema rather than hand-copied, so it cannot drift
// from what the validator enforces.
func TestSchemaRemediation_ListsWhatEachToolRequires(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tool string
		want []string
	}{
		{"project_upsert", []string{"mode", "key", "create", "update"}},
		{"task_get", []string{"keys"}},
		{"task_create", []string{"tasks"}},
		{"task_update", []string{"patches"}},
		{"task_claim", []string{"key", "action"}},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			t.Parallel()
			got := schemaRemediation(tc.tool)
			if !strings.HasPrefix(got, tc.tool+" requires:") {
				t.Errorf("remediation %q does not open by naming the tool and its requirements", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("remediation %q does not mention %q", got, want)
				}
			}
		})
	}
}

// TestToolInputSchemas_CoverEveryPublishedTool keeps the remediation source of
// truth complete: a tool that is registered but missing from the map would
// silently fall back to the generic sentence.
func TestToolInputSchemas_CoverEveryPublishedTool(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 9 {
		t.Errorf("published %d tools, want the nine of PLAN §6", len(res.Tools))
	}
	for _, tool := range res.Tools {
		if toolInputSchemas[tool.Name] == nil {
			t.Errorf("no input schema recorded for %q; its rejections lose their remediation", tool.Name)
		}
	}
}
