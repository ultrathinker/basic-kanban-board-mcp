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

// TestBoardGet_CompactFormat_CallsRender is the dispatch test for the
// default / "compact" format. We don't duplicate the grammar tests from
// compact_test.go; we just confirm this package picks the Render branch
// (text starts with the version header) and that Render was called with a
// non-nil *service.Board.
func TestBoardGet_CompactFormat_CallsRender(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultBoardGet = &service.Board{
		Projects: []service.BoardProject{{
			Key:     "BMB",
			Name:    "BeeMemoryBank",
			Columns: []service.BoardColumn{{Name: "Backlog", Kind: domain.KindBacklog, Count: 0}},
		}},
	}

	// Default format (no format field) and explicit "compact" must both
	// route through compact.go's Render.
	for _, args := range []map[string]any{
		{"project": "BMB"},
		{"project": "BMB", "format": "compact"},
	} {
		res, _ := callTool(t, cs, "board_get", args)
		tc, ok := res.Content[0].(*gomcp.TextContent)
		if !ok {
			t.Fatalf("args=%v: content[0] is %T", args, res.Content[0])
		}
		if !strings.HasPrefix(tc.Text, "compact_version=") {
			t.Errorf("args=%v: text %q does not start with the compact grammar version header", args, tc.Text[:min(len(tc.Text), 80)])
		}
	}

	// The service was actually invoked with the input we sent — Render had
	// a real *service.Board to render, not nil.
	if svc.LastBoardGet.ProjectKey != "BMB" {
		t.Errorf("LastBoardGet.ProjectKey = %q, want BMB", svc.LastBoardGet.ProjectKey)
	}
}

// TestBoardGet_JSONFormat_ProducesJSONText confirms the format:"json"
// branch returns pretty JSON text, not the version header. We don't
// re-validate the structuredContent shape — that's covered by
// TestRoundTrip_BoardGet_Success.
func TestBoardGet_JSONFormat_ProducesJSONText(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultBoardGet = &service.Board{
		Projects: []service.BoardProject{{
			Key:     "BMB",
			Name:    "BeeMemoryBank",
			Columns: []service.BoardColumn{{Name: "Backlog", Kind: domain.KindBacklog, Count: 0}},
		}},
	}

	res, _ := callTool(t, cs, "board_get", map[string]any{
		"project": "BMB", "format": "json",
	})
	tc, ok := res.Content[0].(*gomcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T", res.Content[0])
	}
	if strings.HasPrefix(tc.Text, "compact_version=") {
		t.Errorf("format=json must not call Render: %q", tc.Text)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &parsed); err != nil {
		t.Fatalf("format=json text is not JSON: %v\n%s", err, tc.Text)
	}
	if _, ok := parsed["data"]; !ok {
		t.Errorf("JSON text missing data envelope: %v", parsed)
	}
	if op, _ := parsed["op"].(string); op != "board_get" {
		t.Errorf("op = %v, want board_get", op)
	}
}

// TestBoardGet_InvalidFormat is the negative case: an unknown format value
// must be rejected by the schema enum (additionalProperties:false plus the
// format field's enum), so a caller that mistypes "json " or "Json" learns
// about it without the handler running. The schema enforces this before we
// see the request, returning a CallToolResult with IsError=true and a
// validation message in the text content.
func TestBoardGet_InvalidFormat(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultBoardGet = &service.Board{}

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "board_get",
		Arguments: map[string]any{"project": "BMB", "format": "yaml"},
	})
	if err != nil {
		t.Fatalf("CallTool returned Go error (schema validation should surface as IsError): %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("IsError = %v, want true", res)
	}
	tc, ok := res.Content[0].(*gomcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T", res.Content[0])
	}
	if !strings.Contains(tc.Text, "yaml") || !strings.Contains(tc.Text, "enum") {
		t.Errorf("text %q should mention the offending value and the enum constraint", tc.Text)
	}
	// The handler never ran.
	if svc.LastBoardGet.ProjectKey != "" {
		t.Errorf("board_get handler ran despite invalid format: %+v", svc.LastBoardGet)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
