package mcp

import (
	"context"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

const opTaskGet = "task_get"

// taskGetDefaultInclude matches PLAN §6.3: body, acceptance and links by
// default; notes are opt-in (they can be large and are rarely needed on a
// routine read). Subtask summary, version and lease are never gated — they
// are always present on taskOut.
var taskGetDefaultInclude = []string{string(service.IncludeBody), string(service.IncludeAcceptance), string(service.IncludeLinks)}

type taskGetInput struct {
	Keys    []string `json:"keys" jsonschema:"task keys, case-insensitive"`
	Include []string `json:"include,omitempty" jsonschema:"widen the per-task fields returned"`
}

// taskGetItem is a per-key result. Missing keys are never dropped (PLAN
// §6.3): they appear here with ok:false and a not_found error, in the same
// position the key held in the request.
type taskGetItem struct {
	Key   string         `json:"key"`
	OK    bool           `json:"ok"`
	Task  *taskOut       `json:"task,omitempty"`
	Error *errorEnvelope `json:"error,omitempty"`
}

type taskGetData struct {
	Items []taskGetItem `json:"items"`
}

type taskGetOutput struct {
	OK    bool           `json:"ok"`
	Op    string         `json:"op"`
	Data  *taskGetData   `json:"data,omitempty"`
	Meta  *toolMeta      `json:"meta,omitempty"`
	Error *errorEnvelope `json:"error,omitempty"`
}

func taskGetTool() *gomcp.Tool {
	s := schemaFor[taskGetInput]()
	setMinItems(prop(s, "keys"), 1)
	setMaxItems(prop(s, "keys"), domain.MaxGetKeys)
	inc := prop(s, "include")
	setEnum(inc.Items, includeEnumValues...)
	setDefault(inc, taskGetDefaultInclude)

	return &gomcp.Tool{
		Name: opTaskGet,
		Description: "Fetch tasks by key. Returns `data.items[]`: one `{key, ok, task|error}` entry per requested key, " +
			"in request order — a key that does not exist is reported in place with ok:false, never silently dropped. " +
			"(task_next and task_create return a flat `data.tasks[]` instead, because neither answers per requested key.)",
		InputSchema: s,
	}
}

func registerTaskGet(s *gomcp.Server, svc service.Service) {
	tool := taskGetTool()
	gomcp.AddTool(s, tool, func(ctx context.Context, req *gomcp.CallToolRequest, in taskGetInput) (*gomcp.CallToolResult, taskGetOutput, error) {
		actor, aerr := actorFromContext(ctx)
		if aerr != nil {
			return errorResult(opTaskGet, aerr), taskGetOutput{OK: false, Op: opTaskGet, Error: newErrorEnvelope(aerr)}, nil
		}

		includes := toIncludes(in.Include)
		if len(includes) == 0 {
			includes = toIncludes(taskGetDefaultInclude)
		}

		// A malformed key is a per-item validation failure, not a whole-call
		// abort: PLAN §6.3 promises request order is preserved and no key is
		// ever silently dropped, malformed or merely absent.
		items := make([]taskGetItem, len(in.Keys))
		validKeys := make([]string, 0, len(in.Keys))
		for i, raw := range in.Keys {
			nk, err := domain.NormalizeTaskKey(raw)
			if err != nil {
				items[i] = taskGetItem{Key: raw, OK: false, Error: newErrorEnvelope(domain.AsError(err))}
				continue
			}
			items[i] = taskGetItem{Key: nk}
			validKeys = append(validKeys, nk)
		}

		notFoundList := []string{}
		if len(validKeys) > 0 {
			res, err := svc.TaskGet(ctx, actor, service.TaskGetInput{Keys: validKeys, Include: includes})
			if err != nil {
				derr := asDomainError(err)
				return errorResult(opTaskGet, derr), taskGetOutput{OK: false, Op: opTaskGet, Error: newErrorEnvelope(derr)}, nil
			}
			found := make(map[string]*domain.TaskView, len(res.Tasks))
			for i := range res.Tasks {
				found[res.Tasks[i].Key] = &res.Tasks[i]
			}
			notFoundList = append(notFoundList, res.NotFound...)
			for i := range items {
				if items[i].Error != nil {
					continue // already a validation failure
				}
				if tv, ok := found[items[i].Key]; ok {
					out := taskViewOut(tv, includes)
					items[i].OK = true
					items[i].Task = &out
					continue
				}
				items[i].Error = newErrorEnvelope(domain.NotFound("task", items[i].Key))
			}
		}

		out := taskGetOutput{
			OK: true, Op: opTaskGet,
			Data: &taskGetData{Items: items},
			Meta: &toolMeta{Count: len(items), NotFound: notFoundList},
		}
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: jsonText(out)}}}, out, nil
	})
}
