package mcp

import (
	"context"
	"strconv"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

const opTaskNext = "task_next"

// nextIncludeEnumValues is narrower than board_get/task_get's: task_next has
// no "metadata" widening per PLAN §6.2.
var nextIncludeEnumValues = []string{
	string(service.IncludeBody),
	string(service.IncludeAcceptance),
	string(service.IncludeNotes),
	string(service.IncludeLinks),
}

type taskNextInput struct {
	Project string   `json:"project,omitempty" jsonschema:"project key; omitted = every accessible project"`
	Action  string   `json:"action,omitempty" jsonschema:"peek looks without taking, claim leases the top candidate, start leases and moves it into the first active column"`
	Limit   int      `json:"limit,omitempty" jsonschema:"how many ready candidates to consider/return"`
	Include []string `json:"include,omitempty" jsonschema:"WHICH per-task fields come back. How much of each is a separate choice: see detail."`
	Detail  string   `json:"detail,omitempty" jsonschema:"HOW MUCH of each included field: summary clips body and acceptance to an excerpt, full widens them. Both are bounded — call task_get for a whole card."`
}

// projectionOut publishes the shaping the service applied, so a caller never
// has to infer from a short body whether the task is short or the answer was
// clipped. Fields are omitempty: a full projection costs almost nothing here.
type projectionOut struct {
	Body            string `json:"body,omitempty"`
	BodyLimitBytes  int    `json:"body_limit_bytes,omitempty"`
	Acceptance      string `json:"acceptance,omitempty"`
	AcceptanceLimit int    `json:"acceptance_limit,omitempty"`
}

func newProjectionOut(p service.Projection) *projectionOut {
	out := &projectionOut{}
	if p.Has(service.IncludeBody) {
		out.Body = string(p.Body)
		if p.Body == service.DetailBounded {
			out.BodyLimitBytes = p.BodyLimitBytes
		}
	}
	if p.Has(service.IncludeAcceptance) {
		out.Acceptance = string(p.Acceptance)
		if p.Acceptance == service.DetailBounded {
			out.AcceptanceLimit = p.AcceptanceLimit
		}
	}
	return out
}

type nextReasonsOut struct {
	BlockedDependency int `json:"blocked_dependency"`
	WIPFull           int `json:"wip_full"`
	ClaimedByOther    int `json:"claimed_by_other"`
	ParentIncomplete  int `json:"parent_incomplete"`
	NotLeaf           int `json:"not_leaf"`
}

type blockedSampleOut struct {
	Key       string   `json:"key"`
	BlockedBy []string `json:"blocked_by"`
}

type taskNextData struct {
	Tasks []taskOut `json:"tasks"`
}

type taskNextOutput struct {
	OK    bool           `json:"ok"`
	Op    string         `json:"op"`
	Data  *taskNextData  `json:"data,omitempty"`
	Meta  *toolMeta      `json:"meta,omitempty"`
	Error *errorEnvelope `json:"error,omitempty"`
}

func taskNextTool() *gomcp.Tool {
	s := schemaFor[taskNextInput]()
	setEnum(prop(s, "action"), string(service.NextPeek), string(service.NextClaim), string(service.NextStart))
	setDefault(prop(s, "action"), string(service.NextPeek))
	setMin(prop(s, "limit"), 1)
	setMax(prop(s, "limit"), float64(domain.MaxNextLimit))
	setDefault(prop(s, "limit"), 3)
	inc := prop(s, "include")
	setEnum(inc.Items, nextIncludeEnumValues...)
	setDefault(inc, []string{string(service.IncludeBody), string(service.IncludeAcceptance)})
	setEnum(prop(s, "detail"), string(service.NextDetailSummary), string(service.NextDetailFull))
	setDefault(prop(s, "detail"), string(service.NextDetailSummary))

	return &gomcp.Tool{
		Name: opTaskNext,
		Description: "Find, claim or start the next ready task. `peek` never takes anything (even when WIP is full); " +
			"`claim` leases the top candidate without moving it; `start` leases it and moves it into the first active column atomically. " +
			"Returns `data.tasks[]`: a flat list of task objects, best candidate first (task_get returns `data.items[]` instead, " +
			"because it answers per requested key). `meta.reasons` counts why the rest were not offered.\n" +
			"This is a chooser, so it answers cheaply: bodies come back as a " + strconv.Itoa(service.NextSummaryBodyBytes) +
			"-byte excerpt ending in \"… +N chars\" and acceptance as the first " + strconv.Itoa(service.NextSummaryAcceptanceItems) +
			" items with `acceptance_total` when there are more. `detail:\"full\"` widens that to " +
			strconv.Itoa(domain.NextBodyTruncate) + " bytes and " + strconv.Itoa(domain.NextAcceptanceItems) +
			" items; `meta.projection` always states which bounds were applied. Once you have chosen, task_get returns the whole card.",
		InputSchema: s,
	}
}

// registerTaskNext registers task_next. readOnly gates the mutating actions
// so /mcp/readonly can share every line of translation logic with /mcp and
// still refuse loudly per PLAN §6 rule 4, instead of silently downgrading
// claim/start to peek.
func registerTaskNext(s *gomcp.Server, svc service.Service, readOnly bool) {
	tool := taskNextTool()
	gomcp.AddTool(s, tool, func(ctx context.Context, req *gomcp.CallToolRequest, in taskNextInput) (*gomcp.CallToolResult, taskNextOutput, error) {
		actor, aerr := actorFromContext(ctx)
		if aerr != nil {
			return errorResult(opTaskNext, aerr), taskNextOutput{OK: false, Op: opTaskNext, Error: newErrorEnvelope(aerr)}, nil
		}

		action := service.NextAction(in.Action)
		if action == "" {
			action = service.NextPeek
		}
		if readOnly && (action == service.NextClaim || action == service.NextStart) {
			derr := domain.Forbidden(
				"claim and start are disabled on the read-only endpoint",
				"Use action:\"peek\" here, or call this tool against /mcp with a write-scope token to claim or start work.",
			)
			return errorResult(opTaskNext, derr), taskNextOutput{OK: false, Op: opTaskNext, Error: newErrorEnvelope(derr)}, nil
		}

		limit := in.Limit
		if limit == 0 {
			limit = 3
		}
		includes := toIncludes(in.Include)
		if len(includes) == 0 {
			includes = service.Includes{service.IncludeBody, service.IncludeAcceptance}
		}

		res, err := svc.TaskNext(ctx, actor, service.TaskNextInput{
			ProjectKey: in.Project,
			Action:     action,
			Limit:      limit,
			Include:    includes,
			Detail:     service.NextDetail(in.Detail),
		})
		if err != nil {
			derr := asDomainError(err)
			return errorResult(opTaskNext, derr), taskNextOutput{OK: false, Op: opTaskNext, Error: newErrorEnvelope(derr)}, nil
		}

		blockedTop := make([]blockedSampleOut, len(res.BlockedTop))
		for i, b := range res.BlockedTop {
			blockedTop[i] = blockedSampleOut{Key: b.Key, BlockedBy: b.BlockedBy}
		}

		// The service decided the shaping and said so; rendering obeys that
		// statement rather than re-deriving it from the include list we sent.
		// Those two derivations disagreeing is what made "included but
		// bounded" impossible to express (architecture review finding #21).
		out := taskNextOutput{
			OK: true, Op: opTaskNext,
			Data: &taskNextData{Tasks: taskViewOutList(res.Tasks, res.Projection)},
			Meta: &toolMeta{
				Count:      len(res.Tasks),
				Projection: newProjectionOut(res.Projection),
				WIPFull:    res.WIPFull,
				ClaimedKey: res.ClaimedKey,
				StartedKey: res.StartedKey,
				Reasons: &nextReasonsOut{
					BlockedDependency: res.Reasons.BlockedDependency,
					WIPFull:           res.Reasons.WIPFull,
					ClaimedByOther:    res.Reasons.ClaimedByOther,
					ParentIncomplete:  res.Reasons.ParentIncomplete,
					NotLeaf:           res.Reasons.NotLeaf,
				},
				BlockedTop: blockedTop,
			},
		}
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: jsonText(out)}}}, out, nil
	})
}
