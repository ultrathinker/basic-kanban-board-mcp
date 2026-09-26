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
	Actual         *float64       `json:"actual,omitempty" jsonschema:"effort actually spent, same unit as estimate"`
	Tags           []string       `json:"tags,omitempty"`
	Assignee       string         `json:"assignee,omitempty" jsonschema:"a participant of the project, exactly as board_get lists it (a token or executor key name); anything else is refused"`
	Reviewer       string         `json:"reviewer,omitempty" jsonschema:"free text: who checks the work"`
	Column         string         `json:"column,omitempty" jsonschema:"column name; default is the project's first backlog column"`
	Parent         string         `json:"parent,omitempty" jsonschema:"an existing task key such as \"BMB-14\", or another item of this same batch written as a single @ followed by that item's ref value: an item declaring ref:\"scaffold\" is written here as \"@scaffold\"."`
	BlockedBy      []string       `json:"blocked_by,omitempty" jsonschema:"tasks that must be done first: existing keys and/or items of this same batch, e.g. [\"BMB-14\", \"@scaffold\"] where another item in the batch declares ref:\"scaffold\"."`
	Blocks         []string       `json:"blocks,omitempty" jsonschema:"tasks this one must finish before (blocked_by seen from the other end): existing keys and/or @ref items of this batch"`
	Acceptance     []string       `json:"acceptance,omitempty" jsonschema:"acceptance criteria, unchecked"`
	Outcome        string         `json:"outcome,omitempty" jsonschema:"where the result stands, for a card created already judged (e.g. found and fixed on the way); default open"`
	Conclusion     string         `json:"conclusion,omitempty" jsonschema:"post-hoc takeaway / verdict, distinct from body; lets a card created straight into done say what it concluded"`
	DueAt          string         `json:"due_at,omitempty" jsonschema:"RFC3339 timestamp"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	Ref            string         `json:"ref,omitempty" jsonschema:"name this item so other items in the batch can point at it: ref:\"scaffold\" here is written \"@scaffold\" in their parent or blocked_by."`
	IdempotencyKey string         `json:"idempotency_key,omitempty" jsonschema:"replay-safe for 24h: the same (token, key) returns the original response"`
}

type taskCreateInput struct {
	// Project must NOT be set here — it is a per-item field. It is declared
	// only so that a caller who puts it at the top level (a common mistake:
	// "project":"I6","tasks":[...]) gets the precise "move it inside each item"
	// error below, instead of the SDK's terse "unexpected additional property".
	Project *string     `json:"project,omitempty" jsonschema:"do NOT set this: project is a per-item field — put it inside each element of tasks[]"`
	Tasks   []newTaskIn `json:"tasks" jsonschema:"all-or-nothing: either every task is created, or none are"`
	// SourceMessage accepts a kind:command feed message ATOMICALLY: the tasks
	// and the "command -> tasks -> you" link commit together or not at all.
	// Only the command's resolved_executor may accept (read it from
	// board_get(view:messages)); a repeat returns the original task keys with
	// meta.already_accepted=true and creates nothing. The whole batch must
	// belong to the command's project.
	SourceMessage string `json:"source_message,omitempty" jsonschema:"id of a kind:command message you (its resolved_executor) are accepting; omit for an ordinary create"`
	Echo          string `json:"echo,omitempty"`
}

type taskCreateData struct {
	// Tasks holds taskAckOut by default and taskOut for echo:"full" — no
	// output schema is published any more (KANB-58), so the element type is
	// free to follow the caller's choice.
	Tasks []any `json:"tasks"`
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
	setEcho(s)
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
	setEnum(prop(item, "outcome"), outcomeNames()...)
	setDefault(prop(item, "priority"), domain.PriorityNone.String())
	setMaxLen(prop(item, "assignee"), domain.MaxAssigneeLen)
	setMaxLen(prop(item, "reviewer"), domain.MaxAssigneeLen)
	tags := prop(item, "tags")
	setMaxItems(tags, domain.MaxTags)
	setMaxLen(tags.Items, domain.MaxTagLen)
	acc := prop(item, "acceptance")
	setMaxItems(acc, domain.MaxAcceptance)
	setMaxLen(acc.Items, domain.MaxAcceptanceText)
	// The wire struct infers "project" as required (no `omitempty`), but
	// the friendly "put project INSIDE each item" remediation belongs on
	// our envelope, not on the SDK's terse "missing required property".
	dropRequired(item, "project")

	return &gomcp.Tool{
		Name: opTaskCreate,
		Description: "Create one or more tasks in a single atomic batch (all-or-nothing). " +
			"To link items of the same batch, give one a `ref` and point at it from another by prefixing that name with a single @: " +
			"an item created as `{\"ref\": \"scaffold\", ...}` is referenced as `\"blocked_by\": [\"@scaffold\"]`.\n" +
			"Pass `source_message` to ACCEPT a command from the project feed: only the command's resolved_executor may, " +
			"the tasks and the acceptance link commit atomically, and a repeat returns the original task keys with " +
			"meta.already_accepted=true instead of creating a second batch. " +
			"NOTE: this acceptance guarantee is about the BOARD only — it does not prevent an external command, deploy or " +
			"side effect from running twice; guard those separately. " +
			"A pure question needs no task at all — answer it with `project_post(reply_to: ...)`.\n" +
			"`assignee` must be a participant of the project (board_get lists them; executor_key_issue adds one) — the executor key of that name then works the card. " +
			"Executor keys cannot create tasks.\n" +
			"Returns `data.tasks[]`, in request order: `{key, version, column}` per created task by default, the whole task with `echo:\"full\"`. " +
			versionEchoRule,
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
	if strings.TrimSpace(in.Project) == "" {
		return service.NewTask{}, domain.Invalid("project",
			"project is required on each task item",
			"Put \"project\" INSIDE each item of tasks[], not at the top level: {\"tasks\":[{\"project\":\"I6\",\"title\":\"...\"}]}")
	}
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
	out.Actual = in.Actual
	if in.Assignee != "" {
		a := in.Assignee
		out.Assignee = &a
	}
	if in.Reviewer != "" {
		r := in.Reviewer
		out.Reviewer = &r
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
	for _, b := range in.Blocks {
		bk, derr := keyOrRef("blocks", b)
		if derr != nil {
			return service.NewTask{}, derr
		}
		out.Blocks = append(out.Blocks, bk)
	}
	out.Acceptance = in.Acceptance
	if in.Outcome != "" {
		o := domain.Outcome(in.Outcome)
		out.Outcome = &o
	}
	out.Conclusion = in.Conclusion
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
	addTool(s, tool, func(ctx context.Context, req *gomcp.CallToolRequest, in taskCreateInput) (*gomcp.CallToolResult, taskCreateOutput, error) {
		actor, aerr := actorFromContext(ctx)
		if aerr != nil {
			return errorResult(opTaskCreate, aerr), taskCreateOutput{OK: false, Op: opTaskCreate, Error: newErrorEnvelope(aerr)}, nil
		}

		// A top-level "project" is the shape mistake that motivated the
		// per-item error too: catch it here with the same remediation, since
		// the SDK would otherwise reject the extra key with a generic message
		// that never says where "project" belongs.
		if in.Project != nil {
			derr := domain.Invalid("project",
				"project is a per-item field, not a top-level one",
				"Move \"project\" INSIDE each item of tasks[]: {\"tasks\":[{\"project\":\"I6\",\"title\":\"...\"}]}")
			return errorResult(opTaskCreate, derr), taskCreateOutput{OK: false, Op: opTaskCreate, Error: newErrorEnvelope(derr)}, nil
		}

		tasks := make([]service.NewTask, len(in.Tasks))
		for i, t := range in.Tasks {
			nt, derr := newTaskToService(t)
			if derr != nil {
				return errorResult(opTaskCreate, derr), taskCreateOutput{OK: false, Op: opTaskCreate, Error: newErrorEnvelope(derr)}, nil
			}
			tasks[i] = nt
		}

		res, err := svc.TaskCreate(ctx, actor, service.TaskCreateInput{
			Tasks:         tasks,
			SourceMessage: strings.TrimSpace(in.SourceMessage),
		})
		if err != nil {
			derr := asDomainError(err)
			return errorResult(opTaskCreate, derr), taskCreateOutput{OK: false, Op: opTaskCreate, Error: newErrorEnvelope(derr)}, nil
		}

		includes := service.Includes{service.IncludeBody, service.IncludeAcceptance}
		full := taskViewOutList(res.Tasks, service.FullProjection(includes))
		created := make([]any, len(full))
		for i := range full {
			if in.Echo == echoFull {
				created[i] = full[i]
			} else {
				created[i] = ackOf(full[i])
			}
		}
		out := taskCreateOutput{
			OK: true, Op: opTaskCreate,
			Data: &taskCreateData{Tasks: created},
			Meta: &toolMeta{Count: len(res.Tasks), Replayed: res.Replayed && !res.AlreadyAccepted, AlreadyAccepted: res.AlreadyAccepted},
		}
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: jsonText(out)}}}, out, nil
	})
}
