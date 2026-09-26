package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// opBoardGuide is the read-only MCP tool an agent calls when it asks "how
// does this board work?" — the counterpart to instructionsText (which the
// SDK serves at initialize). the two answer the same question; one is the
// 8 KB the agent reads on every session open, the other is the 1–2 KB a
// confused mid-task agent asks for when something is not clear. board_guide
// also carries the project's settings when project is given — the things
// a human re-reads each session because the estimate unit and lease TTL
// are per-project, not global.
//
// board_guide is the FIRST tool that derives its own count and parameter
// list from the registry instead of a hand-kept constant. PLAN §11's
// "nine tools" rule and the ten-times-the-doc-said-nine bug are why:
// discipline alone failed silently. The spec slice this package hands
// to registerBoardGuide is built by walking the toolXTool() functions
// — the same factories that back every gomcp.AddTool call in server.go
// — so a new tool appears in board_guide the moment registerBoardGet /
// registerTaskNext / etc. append it.
const opBoardGuide = "board_guide"

type boardGuideInput struct {
	Project *string `json:"project,omitempty" jsonschema:"optional project key; when present, the response also carries that project's estimate unit, claim TTL, strict_done and enforce_dependencies settings"`
}

type boardGuideToolSpec struct {
	Name        string `json:"name" jsonschema:"tool name as it appears on the wire"`
	Description string `json:"description" jsonschema:"one-line summary the tool published"`
	// Parameters is a flat list of property names with their declared
	// description. The full schema is available through tools/list, so
	// this is the short form an agent can scan in a single read.
	Parameters []boardGuideParamSpec `json:"parameters,omitempty"`
}

type boardGuideParamSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type boardGuideProjectInfo struct {
	Key                 string `json:"key"`
	Name                string `json:"name"`
	Description         string `json:"description,omitempty"`
	Version             int    `json:"version"`
	FocusKey            string `json:"focus_key,omitempty"`
	EstimateUnit        string `json:"estimate_unit"`
	EnforceDependencies bool   `json:"enforce_dependencies"`
	StrictDone          bool   `json:"strict_done"`
	ClaimTTLSeconds     int    `json:"claim_ttl_seconds"`
	Archived            bool   `json:"archived"`
	// Coordinator and Participants answer "who else is on this project?"
	// (KANB-44). Both carry the stable tokens.id and the display name and
	// nothing else: participantOut has no field a secret or a hash could
	// travel in. Access is not presence -- a name here means the token may
	// take part, never that it is working right now.
	Coordinator  *participantOut  `json:"coordinator,omitempty" jsonschema:"the project's appointed coordinator: tokens.id and display name, never a secret"`
	Participants []participantOut `json:"participants,omitempty" jsonschema:"tokens with access to this project: tokens.id and display name, never a secret; access means may take part, not is working now"`
}

type boardGuideData struct {
	Guide        string                 `json:"guide" jsonschema:"the common operating policy: identity, the canonical loop, lease rules, compact grammar, error envelope"`
	ToolCount    int                    `json:"tool_count" jsonschema:"number of tools registered on this server, computed from the registry"`
	Tools        []boardGuideToolSpec   `json:"tools" jsonschema:"registered tools, ordered by registration"`
	Project      *boardGuideProjectInfo `json:"project,omitempty" jsonschema:"project settings, present only when 'project' is given"`
	CompactToken int                    `json:"compact_token_budget" jsonschema:"token budget for the default compact board_get format (PLAN §11); not derived from any project"`
	CompactTasks int                    `json:"compact_task_sample" jsonschema:"the task count the budget is measured against"`
}

type boardGuideOutput struct {
	OK    bool            `json:"ok"`
	Op    string          `json:"op"`
	Data  *boardGuideData `json:"data,omitempty"`
	Meta  *toolMeta       `json:"meta,omitempty"`
	Error *errorEnvelope  `json:"error,omitempty"`
}

const boardGuideDescription = "Read the operating guide for this kanban board: identity rules, the canonical read-modify-write loop, lease behaviour, the compact grammar version, and the error envelope. " +
	"Without a `project`, returns the common guide plus the list of tools registered on this server (their count is computed, not a constant — adding a tool appears here automatically). " +
	"With `project`, appends that project's settings (estimate unit, claim TTL, strict_done, enforce_dependencies, archive state) so an agent can read-modify-write against the project's own rules without guessing, plus who is on it: the appointed coordinator and the participants derived from the tokens with access, each as a tokens.id and a display name and never a secret. Access means a token MAY take part, not that it is working right now. " +
	"This tool is read-only and exists on every server, including /mcp/readonly. Returns `data` with `guide`, `tool_count`, `tools[]`, and (optionally) `project`."

func boardGuideTool() *gomcp.Tool {
	s := schemaFor[boardGuideInput]()
	setMinLen(prop(s, "project"), domain.MinProjectKeyLen)
	setMaxLen(prop(s, "project"), domain.MaxProjectKeyLen)
	return &gomcp.Tool{
		Name:        opBoardGuide,
		Description: boardGuideDescription,
		InputSchema: s,
	}
}

// specsFromTools turns a slice of tool factories into boardGuideToolSpec
// entries. Walking the factories (rather than hand-listing "board_get",
// "task_next", ...) is what makes the count and parameter list derive
// from the registry: the next tool added with a `xxxTool() *gomcp.Tool`
// factory appears here as soon as the factory is added to the slice
// passed into registerBoardGuide.
//
// Description is rendered as the tool's published one-liner. The
// parameter list is flattened to property names plus their declared
// description — the full schema is one tools/list call away, so the
// short form here is the form an agent scans in a single read.
func specsFromTools(factories []func() *gomcp.Tool) []boardGuideToolSpec {
	return appendNamedSpecs(nil, factories...)
}

// appendNamedSpecs is the explicit helper used by specsFromTools. Inlined
// here rather than inlined into specsFromTools to keep the JSON-tagged
// shape definition close to the schema extraction.
func appendNamedSpecs(dst []boardGuideToolSpec, factories ...func() *gomcp.Tool) []boardGuideToolSpec {
	for _, f := range factories {
		t := f()
		dst = append(dst, boardGuideToolSpec{
			Name:        t.Name,
			Description: oneLineDescription(t.Description),
			Parameters:  paramsFromSchema(t.InputSchema),
		})
	}
	return dst
}

// oneLineDescription keeps only the first sentence of a tool's
// description, so the registry list fits in the same screen an agent
// uses to choose a tool. Multi-paragraph detail (which is what the
// tool descriptions carry for the schema-driven consumer) stays in
// tools/list, where the agent asks for it on demand.
func oneLineDescription(desc string) string {
	if i := strings.IndexAny(desc, "\n."); i > 0 {
		return strings.TrimSpace(desc[:i])
	}
	return strings.TrimSpace(desc)
}

// paramsFromSchema walks the published input schema and returns one
// boardGuideParamSpec per property. A schema-less tool returns nil —
// there is nothing to render.
func paramsFromSchema(s any) []boardGuideParamSpec {
	if s == nil {
		return nil
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	var node map[string]any
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil
	}
	props, _ := node["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := make([]boardGuideParamSpec, 0, len(props))
	for name, raw := range props {
		m, _ := raw.(map[string]any)
		desc, _ := m["description"].(string)
		out = append(out, boardGuideParamSpec{Name: name, Description: desc})
	}
	return out
}

// guideText is the common operating text board_guide serves. The same
// rules appear in instructionsText (the SDK's initialize response); both
// pull from this function so the "common source" the card asks for is
// real, not aspirational.
//
// It is a function, not a const, because the lease TTLs and compact
// version are runtime constants and Go does not let a string const
// interpolate a function call. The cost is one Sprintf per board_guide
// call — negligible next to the schema extraction the tool already does.
func guideText() string {
	return fmt.Sprintf(`Identity: your actor identity is the name of your bearer token. No tool accepts an "actor" parameter — whatever you do is attributed to your token, always.

Projects: you may not see every project on the board. Call board_get with no project to list every project your token can access.

The canonical loop:
  1. board_get(project: "KEY") at the start of a session — prefer the default compact text; it is cheap enough to call every session.
  2. task_next(project: "KEY", action: "start") to take the next unblocked, unclaimed task in one round trip.
  3. While working, task_update with only note set — notes are append-only and never bump a task's version.
  4. Whenever you change title, body, type, priority, estimate, tags, assignee, column, rank, parent, acceptance, due_at or metadata, send if_version. A conflict error carries the current state as current; merge into it and retry with the version it reports.
  5. The version on every returned task is the value AFTER the call and is authoritative — chain your next if_version from it. note, lease and focus do not move a task's version by design.

Leases: a default TTL of %s is granted on claim; project defaults can override; every lease is clamped to %s–%s. task_claim renews or releases explicitly; letting it expire hands the task back to task_next.

Compact grammar: board_get's default text output is compact_version=%d. Every task line ends in v<version>, so a read-modify-write needs no follow-up task_get just to learn the version to echo back.

Errors: every failure is {ok:false, error:{code, message, remediation}} with isError set on the result. remediation names the concrete next action — read it instead of guessing.`,
		formatDuration(domain.ClaimTTLDefault),
		formatDuration(domain.ClaimTTLMin),
		formatDuration(domain.ClaimTTLMax),
		domain.CompactVersion,
	)
}

// registerBoardGuide wires the read-only board_guide tool into s. The
// specs slice is the tool registry snapshot the response renders; the
// caller (NewServer / NewReadOnlyServer) builds it by walking each
// toolXTool() factory so the count and parameter list are derived, not
// hand-kept. The handler reads svc through BoardGet for the optional
// project-settings block — read-only is honoured because the tool itself
// never calls a mutating method.
func registerBoardGuide(s *gomcp.Server, svc service.Service, specs []boardGuideToolSpec) {
	tool := boardGuideTool()
	addTool(s, tool, func(ctx context.Context, req *gomcp.CallToolRequest, in boardGuideInput) (*gomcp.CallToolResult, boardGuideOutput, error) {
		actor, aerr := actorFromContext(ctx)
		if aerr != nil {
			return errorResult(opBoardGuide, aerr), boardGuideOutput{OK: false, Op: opBoardGuide, Error: newErrorEnvelope(aerr)}, nil
		}

		data := &boardGuideData{
			Guide:        guideText(),
			ToolCount:    len(specs),
			Tools:        specs,
			CompactToken: domain.CompactTokenBudget,
			CompactTasks: domain.CompactBudgetTasks,
		}

		if in.Project != nil {
			key := strings.TrimSpace(*in.Project)
			if key == "" {
				derr := domain.Invalid("project", "project key is empty", "Pass a non-empty project key or omit 'project'.")
				return errorResult(opBoardGuide, derr), boardGuideOutput{OK: false, Op: opBoardGuide, Error: newErrorEnvelope(derr)}, nil
			}
			projectInfo, err := loadBoardGuideProject(ctx, svc, actor, key)
			if err != nil {
				derr := asDomainError(err)
				return errorResult(opBoardGuide, derr), boardGuideOutput{OK: false, Op: opBoardGuide, Error: newErrorEnvelope(derr)}, nil
			}
			data.Project = projectInfo
		}

		out := boardGuideOutput{
			OK:   true,
			Op:   opBoardGuide,
			Data: data,
			Meta: &toolMeta{Count: len(specs)},
		}
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: jsonText(out)}}}, out, nil
	})
}

// loadBoardGuideProject fetches the single project board_guide renders
// in its optional `project` block. The handler does not need acceptance,
// links or notes; the cheapest read that carries the version, focus key
// and settings is the right one. ErrNotFound is surfaced as-is so the
// remediation string the agent sees names the offending key.
//
// board may be nil when the service layer returned a typed zero value
// without an error (a fake in tests, for example); the for-range over a
// nil slice is a no-op, so the NotFound path below is reached and the
// handler turns it into the same shape as a real miss.
func loadBoardGuideProject(ctx context.Context, svc service.Service, actor service.Actor, key string) (*boardGuideProjectInfo, error) {
	board, err := svc.BoardGet(ctx, actor, service.BoardGetInput{
		ProjectKey: key,
		View:       service.ViewSummary,
	})
	if err != nil {
		return nil, err
	}
	if board == nil {
		return nil, domain.NotFound("project", key)
	}
	for _, bp := range board.Projects {
		if strings.EqualFold(bp.Key, key) {
			return &boardGuideProjectInfo{
				Key:                 bp.Key,
				Name:                bp.Name,
				Description:         bp.Description,
				Version:             bp.Version,
				FocusKey:            bp.FocusKey,
				EstimateUnit:        bp.EstimateUnit,
				EnforceDependencies: bp.EnforceDependencies,
				StrictDone:          bp.StrictDone,
				ClaimTTLSeconds:     bp.ClaimTTLSeconds,
				Archived:            bp.Archived,
				Coordinator:         participantOutPtr(bp.Coordinator),
				Participants:        participantOuts(bp.Participants),
			}, nil
		}
	}
	return nil, domain.NotFound("project", key)
}
