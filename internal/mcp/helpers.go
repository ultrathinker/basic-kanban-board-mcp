package mcp

import (
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

// errorText renders a *domain.Error as the human-readable line every failed
// tool call carries in its text content, alongside the identical
// information in structuredContent.error (PLAN §6 envelope rule).
func errorText(op string, e *domain.Error) string {
	if e == nil {
		return op + ": ok"
	}
	msg := fmt.Sprintf("%s: %s — %s", op, e.Code, e.Message)
	if e.Remediation != "" {
		msg += "\nremediation: " + e.Remediation
	}
	return msg
}

// errorResult builds the failure-side CallToolResult: IsError set, text
// carrying the same code/message/remediation that structuredContent.error
// will carry. Callers still return their own Output value (with OK:false
// and Error populated) alongside this so structuredContent is set too — see
// toolForErr in the go-sdk: it only skips structuredContent when the
// handler returns a non-nil Go error, which this package never does for a
// domain failure.
func errorResult(op string, e *domain.Error) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{Text: errorText(op, e)}},
	}
}

// jsonText pretty-prints v for the `format:"json"` text-content branch of
// read tools. It is the caller's job to pass the same value that becomes
// structuredContent, so the two forms can never disagree.
func jsonText(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("error rendering JSON: %v", err)
	}
	return string(b)
}
