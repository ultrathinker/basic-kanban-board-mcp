package mcp

import (
	"context"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

const opTaskLink = "task_link"

type linkPairIn struct {
	Blocker string `json:"blocker" jsonschema:"task key that must be done first"`
	Blocked string `json:"blocked" jsonschema:"task key that waits on blocker"`
}

type taskLinkInput struct {
	Add    []linkPairIn `json:"add,omitempty"`
	Remove []linkPairIn `json:"remove,omitempty"`
}

type taskLinkData struct {
	Tasks []taskOut `json:"tasks"`
}

type taskLinkOutput struct {
	OK    bool           `json:"ok"`
	Op    string         `json:"op"`
	Data  *taskLinkData  `json:"data,omitempty"`
	Meta  *toolMeta      `json:"meta,omitempty"`
	Error *errorEnvelope `json:"error,omitempty"`
}

func taskLinkTool() *gomcp.Tool {
	s := schemaFor[taskLinkInput]()
	return &gomcp.Tool{
		Name:        opTaskLink,
		Description: "Add or remove `blocks` dependencies, atomically. Rejects self-links and cycles (including through parent chains).",
		InputSchema: s,
	}
}

func linkPairsToService(field string, pairs []linkPairIn) ([]service.LinkPair, *domain.Error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make([]service.LinkPair, len(pairs))
	for i, p := range pairs {
		blocker, err := domain.NormalizeTaskKey(p.Blocker)
		if err != nil {
			return nil, domain.AsError(err)
		}
		blocked, err := domain.NormalizeTaskKey(p.Blocked)
		if err != nil {
			return nil, domain.AsError(err)
		}
		out[i] = service.LinkPair{Blocker: blocker, Blocked: blocked}
	}
	return out, nil
}

func registerTaskLink(s *gomcp.Server, svc service.Service) {
	tool := taskLinkTool()
	gomcp.AddTool(s, tool, func(ctx context.Context, req *gomcp.CallToolRequest, in taskLinkInput) (*gomcp.CallToolResult, taskLinkOutput, error) {
		actor, aerr := actorFromContext(ctx)
		if aerr != nil {
			return errorResult(opTaskLink, aerr), taskLinkOutput{OK: false, Op: opTaskLink, Error: newErrorEnvelope(aerr)}, nil
		}
		if len(in.Add) == 0 && len(in.Remove) == 0 {
			derr := domain.Invalid("add", "at least one of add/remove must be non-empty", "Send one or more {blocker, blocked} pairs to add or remove.")
			return errorResult(opTaskLink, derr), taskLinkOutput{OK: false, Op: opTaskLink, Error: newErrorEnvelope(derr)}, nil
		}

		add, derr := linkPairsToService("add", in.Add)
		if derr != nil {
			return errorResult(opTaskLink, derr), taskLinkOutput{OK: false, Op: opTaskLink, Error: newErrorEnvelope(derr)}, nil
		}
		remove, derr := linkPairsToService("remove", in.Remove)
		if derr != nil {
			return errorResult(opTaskLink, derr), taskLinkOutput{OK: false, Op: opTaskLink, Error: newErrorEnvelope(derr)}, nil
		}

		res, err := svc.TaskLink(ctx, actor, service.TaskLinkInput{Add: add, Remove: remove})
		if err != nil {
			derr := asDomainError(err)
			return errorResult(opTaskLink, derr), taskLinkOutput{OK: false, Op: opTaskLink, Error: newErrorEnvelope(derr)}, nil
		}

		includes := service.Includes{service.IncludeLinks}
		out := taskLinkOutput{
			OK: true, Op: opTaskLink,
			Data: &taskLinkData{Tasks: taskViewOutList(res.Tasks, includes)},
			Meta: &toolMeta{Count: len(res.Tasks)},
		}
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: jsonText(out)}}}, out, nil
	})
}
