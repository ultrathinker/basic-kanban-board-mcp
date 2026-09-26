package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// nowFunc is the clock used for compact rendering's `age`/`lease …
// <remaining>` reference time. Tests override it for determinism; the
// server clock (Tx.Now()) already decided the underlying values, so this is
// purely a rendering reference, never used for a security decision.
var nowFunc = time.Now

func renderNow() time.Time { return nowFunc().UTC() }

// ONE REPRESENTATION ON THE WIRE (KANB-58). Every tool answers with its text
// content only: no outputSchema is declared and no structuredContent is sent.
//
// Why: an MCP client that finds structuredContent shows the MODEL that, not
// the text. Claude Code does exactly this, so the compact board grammar this
// server was built around never reached a single agent — measured 26.09.2026
// on one board_get through Patchbay: 525 characters of compact text next to
// 11 244 characters of structuredContent, and the model saw the 11 244. The
// JSON a caller asks for with format:"json" is still there; it is simply the
// text, not a second copy beside it.

// errorResult builds the failure-side CallToolResult: IsError set and the
// {ok:false, op, error:{code, message, remediation}} envelope as the text, in
// compact JSON. It is the same envelope the handler's own Output carries, so
// a caller parses errors and successes the same way.
func errorResult(op string, e *domain.Error) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{Text: jsonText(schemaErrorOutput{OK: false, Op: op, Error: newErrorEnvelope(e)})}},
	}
}

// jsonText renders v as COMPACT JSON for the text content. Not indented: the
// text is now what the model reads, and indentation was a fifth of it.
func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("error rendering JSON: %v", err)
	}
	return string(b)
}

// addTool registers a tool so that its answer travels as text only (see the
// note above). The handler keeps its typed Output — that is what the text is
// rendered from when the handler did not render its own (board_get renders
// the compact grammar; most tools leave the content empty) — but the Output
// is never handed to the SDK, so no outputSchema is derived from it and no
// structuredContent is attached.
func addTool[In, Out any](s *gomcp.Server, t *gomcp.Tool, h func(context.Context, *gomcp.CallToolRequest, In) (*gomcp.CallToolResult, Out, error)) {
	gomcp.AddTool(s, t, func(ctx context.Context, req *gomcp.CallToolRequest, in In) (*gomcp.CallToolResult, any, error) {
		res, out, err := h(ctx, req, in)
		if err != nil {
			return res, nil, err
		}
		if res == nil {
			res = &gomcp.CallToolResult{}
		}
		if len(res.Content) == 0 {
			res.Content = []gomcp.Content{&gomcp.TextContent{Text: jsonText(out)}}
		}
		return res, nil, nil
	})
}

// echoFull is the value of a write tool's `echo` argument that asks for the
// whole task back. Anything else — including omitting it — gets the short
// acknowledgement (KANB-58).
const echoFull = "full"

// taskAckOut is what a write returns per task by default: enough to chain the
// next write (key and the AFTER-version) and to see where the card landed.
// Closing seventeen cards used to echo every body, checklist and conclusion
// back — 32 000 characters for one call in the IAMT session.
type taskAckOut struct {
	Key     string `json:"key"`
	Version int    `json:"version"`
	Column  string `json:"column"`
}

func ackOf(t taskOut) taskAckOut {
	return taskAckOut{Key: t.Key, Version: t.Version, Column: t.Column}
}

// echoSchemaNote is the published description of the `echo` argument.
const echoSchemaNote = "ack (default) = key, version and column per task — the version is the AFTER-value, chain your next if_version from it; full = the whole task, body and checklist included"
