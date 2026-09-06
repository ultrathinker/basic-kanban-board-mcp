package mcp

import (
	"context"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

const opTaskCreate = "task_create"

type newTaskIn struct {
	Project        string         `json:"project" jsonschema:"project key, case-insensitive"`
	Title          string         `json:"title" jsonschema:"short imperative title"`
	Body           string         `json:"body,omitempty" jsonschema:"markdown detail"`
	Type           string         `json:"type,omitempty"`
	Priority       string         `json:"priority,omitempty"`
	Estimate       *float64       `json:"estimate,omitempty" jsonschema:"in the project's estimate_unit"`
	Tags           []string       `json:"tags,omitempty"`
	Assignee       string         `json:"assignee,omitempty" jsonschema:"free text: a human or agent name"`
	Column         string         `json:"column,omitempty" jsonschema:"column name; default is the project's first backlog column"`
	Parent         string         `json:"parent,omitempty" jsonschema:"an existing task key such as \"BMB-14\", or another item of this same batch written as a single @ followed by that item's ref value: an item declaring ref:\"scaffold\" is written here as \"@scaffold\"."`
	BlockedBy      []string       `json:"blocked_by,omitempty" jsonschema:"tasks that must be done first: existing keys and/or items of this same batch, e.g. [\"BMB-14\", \"@scaffold\"] where another item in the batch declares ref:\"scaffold\"."`
	Acceptance     []string       `json:"acceptance,omitempty" jsonschema:"acceptance criteria, unchecked"`
	DueAt          string         `json:"due_at,omitempty" jsonschema:"RFC3339 timestamp"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	Ref            string         `json:"ref,omitempty" jsonschema:"name this item so other items in the batch can point at it: ref:\"scaffold\" here is written \"@scaffold\" in their parent or blocked_by."`
	IdempotencyKey string         `json:"idempotency_key,omitempty" jsonschema:"replay-safe for 24h: the same (token, key) returns the original response"`
}

type taskCreateInput struct {
	Tasks []newTaskIn `json:"tasks" jsonschema:"all-or-nothing: either every task is created, or none are"`
}

type taskCreateData struct {
	Tasks []taskOut `json:"tasks"`
}

type taskCreateOutput struct {
	OK    bool            `json:"ok"`
	Op    string          `json:"op"`
	Data  *taskCreateData `json:"data,omitempty"`
	Meta  *toolMeta       `json:"meta,omitempty"`
	Error *errorEnvelope  `json:"error,omitempty"`
}

func taskCreateTool() *gomcp.Tool {
	s := schemaFor[taskCreateInput]()
	tasks := prop(s, "tasks")
	setMinItems(tasks, 1)
	setMaxItems(tasks, domain.MaxBatchTasks)

	item := tasks.Items
	setMinLen(prop(item, "title"), 1)
	setMaxLen(prop(item, "title"), domain.MaxTitleLen)
	setMaxLen(prop(item, "body"), domain.MaxBodyBytes)
	setEnum(prop(item, "type"), typeNames()...)
	setDefault(prop(item, "type"), string(domain.TypeTask))
	setEnum(prop(item, "priority"), priorityNames()...)
	setDefault(prop(item, "priority"), domain.PriorityNone.String())
	setMaxLen(prop(item, "assignee"), domain.MaxAssigneeLen)
	tags := prop(item, "tags")
	setMaxItems(tags, domain.MaxTags)
	setMaxLen(tags.Items, domain.MaxTagLen)
	acc := prop(item, "acceptance")
	setMaxItems(acc, domain.MaxAcceptance)
	setMaxLen(acc.Items, domain.MaxAcceptanceText)

	return &gomcp.Tool{
		Name: opTaskCreate,
		Description: "Create one or more tasks in a single atomic batch (all-or-nothing). " +
			"To link items of the same batch, give one a `ref` and point at it from another by prefixing that name with a single @: " +
			"an item created as `{\"ref\": \"scaffold\", ...}` is referenced as `\"blocked_by\": [\"@scaffold\"]`. " +
			"Returns `data.tasks[]`: the created tasks, in request order, each carrying its assigned `key` and `version`.",
		InputSchema: s,
	}
}

// keyOrRef passes a symbolic "@ref" through unchanged (it is resolved inside
// the same batch by the service) and normalizes anything else as a task
// key. This is presentation-layer disambiguation, not a business rule.
func keyOrRef(field, s string) (string, *domain.Error) {
	if s == "" {
		return "", nil
	}
	if strings.HasPrefix(s, "@") {
		return s, nil
	}
	nk, err := domain.NormalizeTaskKey(s)
	if err != nil {
		return "", domain.Invalid(field, err.Error(), "Use a task key such as BMB-14, or \"@name\" to reference another item in this batch.")
	}
	return nk, nil
}

func newTaskToService(in newTaskIn) (service.NewTask, *domain.Error) {
	out := service.NewTask{
		ProjectKey:     domain.NormalizeProjectKey(in.Project),
		Title:          in.Title,
		Body:           in.Body,
		Column:         in.Column,
		Tags:           in.Tags,
		Metadata:       in.Metadata,
		Ref:            in.Ref,
		IdempotencyKey: in.IdempotencyKey,
	}
	if in.Type != "" {
		out.Type = domain.Type(in.Type)
	} else {
		out.Type = domain.TypeTask
	}
	if in.Priority != "" {
		p, derr := parsePriorityName("priority", in.Priority)
		if derr != nil {
			return service.NewTask{}, derr
		}
		out.Priority = p
	}
	out.Estimate = in.Estimate
	if in.Assignee != "" {
		a := in.Assignee
		out.Assignee = &a
	}
	if in.Parent != "" {
		p, derr := keyOrRef("parent", in.Parent)
		if derr != nil {
			return service.NewTask{}, derr
		}
		out.Parent = p
	}
	if len(in.BlockedBy) > 0 {
		out.BlockedBy = make([]string, len(in.BlockedBy))
		for i, b := range in.BlockedBy {
			bk, derr := keyOrRef("blocked_by", b)
			if derr != nil {
				return service.NewTask{}, derr
			}
			out.BlockedBy[i] = bk
		}
	}
	out.Acceptance = in.Acceptance
	if in.DueAt != "" {
		t, derr := parseRFC3339("due_at", in.DueAt)
		if derr != nil {
			return service.NewTask{}, derr
		}
		out.DueAt = &t
	}
	return out, nil
}

func registerTaskCreate(s *gomcp.Server, svc service.Service) {
	tool := taskCreateTool()
	gomcp.AddTool(s, tool, func(ctx context.Context, req *gomcp.CallToolRequest, in taskCreateInput) (*gomcp.CallToolResult, taskCreateOutput, error) {
		actor, aerr := actorFromContext(ctx)
		if aerr != nil {
			return errorResult(opTaskCreate, aerr), taskCreateOutput{OK: false, Op: opTaskCreate, Error: newErrorEnvelope(aerr)}, nil
		}

		tasks := make([]service.NewTask, len(in.Tasks))
		for i, t := range in.Tasks {
			nt, derr := newTaskToService(t)
			if derr != nil {
				return errorResult(opTaskCreate, derr), taskCreateOutput{OK: false, Op: opTaskCreate, Error: newErrorEnvelope(derr)}, nil
			}
			tasks[i] = nt
		}

		res, err := svc.TaskCreate(ctx, actor, service.TaskCreateInput{Tasks: tasks})
		if err != nil {
			derr := asDomainError(err)
			return errorResult(opTaskCreate, derr), taskCreateOutput{OK: false, Op: opTaskCreate, Error: newErrorEnvelope(derr)}, nil
		}

		includes := service.Includes{service.IncludeBody, service.IncludeAcceptance}
		out := taskCreateOutput{
			OK: true, Op: opTaskCreate,
			Data: &taskCreateData{Tasks: taskViewOutList(res.Tasks, service.FullProjection(includes))},
			Meta: &toolMeta{Count: len(res.Tasks), Replayed: res.Replayed},
		}
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: jsonText(out)}}}, out, nil
	})
}
