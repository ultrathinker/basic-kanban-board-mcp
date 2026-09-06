package mcp

import (
	"context"
	"fmt"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

const opTaskRemove = "task_remove"

type removeItemIn struct {
	Key       string `json:"key" jsonschema:"task key, case-insensitive"`
	IfVersion *int   `json:"if_version,omitempty"`
}

type taskRemoveInput struct {
	Items           []removeItemIn `json:"items"`
	CascadeSubtasks *bool          `json:"cascade_subtasks,omitempty" jsonschema:"also archive subtasks of an archived parent"`
	Restore         bool           `json:"restore,omitempty" jsonschema:"restore instead of archive"`
}

type taskRemoveData struct {
	Items []taskPatchResultOut `json:"items"`
}

type taskRemoveOutput struct {
	OK    bool            `json:"ok"`
	Op    string          `json:"op"`
	Data  *taskRemoveData `json:"data,omitempty"`
	Meta  *toolMeta       `json:"meta,omitempty"`
	Error *errorEnvelope  `json:"error,omitempty"`
}

func taskRemoveTool() *gomcp.Tool {
	s := schemaFor[taskRemoveInput]()
	items := prop(s, "items")
	setMinItems(items, 1)
	setMaxItems(items, domain.MaxBatchTasks)
	setDefault(prop(s, "cascade_subtasks"), true)

	return &gomcp.Tool{
		Name: opTaskRemove,
		Description: "Archive (or restore) tasks. Archiving clears claim and focus and keeps links (hidden while archived). " +
			"Hard delete is never available over MCP — use `kanban task purge` (admin CLI).",
		InputSchema: s,
	}
}

func registerTaskRemove(s *gomcp.Server, svc service.Service) {
	tool := taskRemoveTool()
	gomcp.AddTool(s, tool, func(ctx context.Context, req *gomcp.CallToolRequest, in taskRemoveInput) (*gomcp.CallToolResult, taskRemoveOutput, error) {
		actor, aerr := actorFromContext(ctx)
		if aerr != nil {
			return errorResult(opTaskRemove, aerr), taskRemoveOutput{OK: false, Op: opTaskRemove, Error: newErrorEnvelope(aerr)}, nil
		}

		type slot struct {
			key  string
			err  *domain.Error
			task *taskOut
			ok   bool
		}
		slots := make([]slot, len(in.Items))
		var toSend []service.RemoveItem
		var sendIdx []int
		for i, it := range in.Items {
			nk, err := domain.NormalizeTaskKey(it.Key)
			if err != nil {
				slots[i] = slot{key: it.Key, err: domain.AsError(err)}
				continue
			}
			toSend = append(toSend, service.RemoveItem{Key: nk, IfVersion: it.IfVersion})
			sendIdx = append(sendIdx, i)
			slots[i] = slot{key: nk}
		}

		if len(toSend) > 0 {
			cascade := true
			if in.CascadeSubtasks != nil {
				cascade = *in.CascadeSubtasks
			}
			res, err := svc.TaskRemove(ctx, actor, service.TaskRemoveInput{
				Items:           toSend,
				CascadeSubtasks: cascade,
				Restore:         in.Restore,
			})
			if err != nil {
				derr := asDomainError(err)
				return errorResult(opTaskRemove, derr), taskRemoveOutput{OK: false, Op: opTaskRemove, Error: newErrorEnvelope(derr)}, nil
			}
			includes := service.Includes{}
			for j, ir := range res.Items {
				if j >= len(sendIdx) {
					break
				}
				i := sendIdx[j]
				if ir.OK {
					out := taskViewOut(ir.Task, service.FullProjection(includes))
					slots[i] = slot{key: ir.Key, ok: true, task: &out}
				} else {
					slots[i] = slot{key: ir.Key, err: ir.Err}
				}
			}
		}

		items := make([]taskPatchResultOut, len(slots))
		warnings := []string{}
		for i, sl := range slots {
			if sl.ok {
				items[i] = taskPatchResultOut{Key: sl.key, OK: true, Task: sl.task}
			} else {
				items[i] = taskPatchResultOut{Key: sl.key, OK: false, Error: newErrorEnvelope(sl.err)}
				warnings = append(warnings, fmt.Sprintf("%s: %s", sl.key, sl.err.Code))
			}
		}

		out := taskRemoveOutput{
			OK: true, Op: opTaskRemove,
			Data: &taskRemoveData{Items: items},
			Meta: &toolMeta{Count: len(items), Warnings: warnings},
		}
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: jsonText(out)}}}, out, nil
	})
}
