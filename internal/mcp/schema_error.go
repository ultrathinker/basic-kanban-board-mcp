package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// methodCallTool is the JSON-RPC method name for a tool call. The SDK keeps
// its own copy unexported, so the middleware below carries one.
const methodCallTool = "tools/call"

// schemaErrorOutput is the envelope a schema rejection carries. It is the
// {ok, op, error} subset every tool's declared output schema already allows
// (all nine share ok/op/data?/meta?/error?), so a client validating
// structuredContent against the tool's outputSchema still accepts it.
type schemaErrorOutput struct {
	OK    bool           `json:"ok"`
	Op    string         `json:"op"`
	Error *errorEnvelope `json:"error,omitempty"`
}

// toolInputSchemas is every tool's published input schema keyed by tool name,
// built once at package init from the very same constructors the servers
// register. Building it here (rather than mutating a map as tools are
// registered) keeps it immutable, so the two servers can be constructed
// concurrently without a data race.
var toolInputSchemas = buildToolInputSchemas()

func buildToolInputSchemas() map[string]*jsonschema.Schema {
	tools := []*gomcp.Tool{
		boardGetTool(), taskNextTool(), taskGetTool(), taskCreateTool(),
		taskUpdateTool(), taskLinkTool(), taskClaimTool(), taskRemoveTool(),
		projectUpsertTool(),
	}
	m := make(map[string]*jsonschema.Schema, len(tools))
	for _, t := range tools {
		// Tool.InputSchema is `any` on the wire type; every tool in this
		// package sets it from schemaFor, so the assertion cannot fail —
		// and if a future tool sets something else, the missing entry
		// degrades to the generic remediation rather than panicking.
		if s, ok := t.InputSchema.(*jsonschema.Schema); ok {
			m[t.Name] = s
		}
	}
	return m
}

// schemaRemediation names the properties the tool actually requires, read
// from its published schema so the sentence can never drift from what the
// validator enforces.
func schemaRemediation(tool string) string {
	s := toolInputSchemas[tool]
	if s == nil || len(s.Required) == 0 {
		return "Send only the properties this tool's inputSchema declares — tools/list publishes it, and nothing outside it is accepted."
	}
	parts := make([]string, 0, len(s.Required))
	for _, name := range s.Required {
		part := name
		if p := s.Properties[name]; p != nil && len(p.Enum) > 0 {
			part += " (one of: " + enumValues(p) + ")"
		}
		parts = append(parts, part)
	}
	return fmt.Sprintf(
		"%s requires: %s. Send exactly the properties its inputSchema declares — tools/list publishes it, and the tool description carries a complete example call.",
		tool, strings.Join(parts, ", "))
}

// enumValues renders a property's enum as a comma-separated list.
func enumValues(p *jsonschema.Schema) string {
	out := make([]string, 0, len(p.Enum))
	for _, v := range p.Enum {
		out = append(out, fmt.Sprint(v))
	}
	return strings.Join(out, ", ")
}

// schemaErrorEnvelope repairs the one hole in the envelope contract this
// package otherwise honours everywhere.
//
// The SDK validates `arguments` against the tool's input schema *before* the
// typed handler runs, and answers a violation with a bare CallToolResult:
// IsError set, a raw validator sentence such as
// `validating "arguments": ... missing properties: ["mode"]` as its only
// content, and no structuredContent at all. That contradicts two promises the
// server itself makes at initialize — that every failure carries
// {code, message, remediation}, and that remediation names the concrete next
// action — and it is the first thing an agent hits when it guesses a call
// wrong (LIVE-FINDINGS §1: project_upsert without `mode`).
//
// So: when a tool call comes back flagged as an error but with no envelope,
// the typed handler never ran, and this middleware builds the envelope the
// handler would have. The validator's own sentence is preserved verbatim as
// the message — it names the offending property — and the remediation is
// derived from the tool's published schema.
func schemaErrorEnvelope(next gomcp.MethodHandler) gomcp.MethodHandler {
	return func(ctx context.Context, method string, req gomcp.Request) (gomcp.Result, error) {
		res, err := next(ctx, method, req)
		if err != nil || method != methodCallTool {
			return res, err
		}
		ctr, ok := res.(*gomcp.CallToolResult)
		// A non-error result, or one that already carries an envelope, is a
		// handler's own answer: leave it exactly as it is.
		if !ok || ctr == nil || !ctr.IsError || ctr.StructuredContent != nil {
			return res, err
		}
		op := "tool_call"
		if ctReq, ok := req.(*gomcp.CallToolRequest); ok && ctReq.Params != nil {
			op = ctReq.Params.Name
		}
		derr := &domain.Error{
			Code:        domain.CodeValidation,
			Message:     schemaErrorMessage(ctr),
			Remediation: schemaRemediation(op),
		}
		ctr.Content = []gomcp.Content{&gomcp.TextContent{Text: errorText(op, derr)}}
		ctr.StructuredContent = schemaErrorOutput{OK: false, Op: op, Error: newErrorEnvelope(derr)}
		return ctr, nil
	}
}

// schemaErrorMessage recovers the validator's sentence: SetError stashes the
// error itself, and the text content is the fallback for any future SDK path
// that sets IsError without it.
func schemaErrorMessage(ctr *gomcp.CallToolResult) string {
	if e := ctr.GetError(); e != nil {
		return e.Error()
	}
	for _, c := range ctr.Content {
		if tc, ok := c.(*gomcp.TextContent); ok && tc.Text != "" {
			return tc.Text
		}
	}
	return "the arguments did not match this tool's input schema"
}
