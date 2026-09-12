package mcp

import (
	"context"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

const opProjectPost = "project_post"

type projectPostInput struct {
	Project string `json:"project" jsonschema:"project key, case-insensitive"`
	Author  string `json:"author" jsonschema:"agent identity / display name (e.g. Claude, Codex, Zoë)"`
	Body    string `json:"body" jsonschema:"message text; mention task keys like KANB-7 for clickable links"`
}

type projectPostData struct {
	ID        string `json:"id" jsonschema:"unique message identifier"`
	Project   string `json:"project" jsonschema:"canonical uppercase project key"`
	Author    string `json:"author" jsonschema:"agent identity / display name"`
	Body      string `json:"body" jsonschema:"message body as posted"`
	CreatedAt string `json:"created_at" jsonschema:"RFC3339 timestamp"`
}

type projectPostOutput struct {
	OK    bool             `json:"ok"`
	Op    string           `json:"op"`
	Data  *projectPostData `json:"data,omitempty"`
	Meta  *toolMeta        `json:"meta,omitempty"`
	Error *errorEnvelope   `json:"error,omitempty"`
}

const projectPostDescription = "Post a live progress update to the project chat feed. " +
	"This is a real-time broadcast for the human watching the board right now, not a post-mortem or final report for posterity. " +
	"Post short messages periodically during your work (approximately every 5 minutes), not just once at the end: " +
	"share what is being done now, what is planned next, and what went wrong or surprised you. " +
	"Reference tasks by their key directly in the text (e.g. KANB-7) — they become clickable links on the board. " +
	"Returns `data`: the posted message with its id, author, body and timestamp."

func projectPostTool() *gomcp.Tool {
	s := schemaFor[projectPostInput]()
	setMinLen(prop(s, "project"), domain.MinProjectKeyLen)
	setMaxLen(prop(s, "project"), domain.MaxProjectKeyLen)
	setMaxLen(prop(s, "author"), domain.MaxAssigneeLen)
	setMaxLen(prop(s, "body"), domain.MaxChatMessageLen)

	return &gomcp.Tool{
		Name:        opProjectPost,
		Description: projectPostDescription,
		InputSchema: s,
	}
}

func registerProjectPost(s *gomcp.Server, svc service.Service) {
	tool := projectPostTool()
	gomcp.AddTool(s, tool, func(ctx context.Context, req *gomcp.CallToolRequest, in projectPostInput) (*gomcp.CallToolResult, projectPostOutput, error) {
		actor, aerr := actorFromContext(ctx)
		if aerr != nil {
			return errorResult(opProjectPost, aerr), projectPostOutput{OK: false, Op: opProjectPost, Error: newErrorEnvelope(aerr)}, nil
		}

		key, err := domain.ValidateProjectKey(in.Project)
		if err != nil {
			derr := domain.AsError(err)
			return errorResult(opProjectPost, derr), projectPostOutput{OK: false, Op: opProjectPost, Error: newErrorEnvelope(derr)}, nil
		}

		author := strings.TrimSpace(in.Author)
		if author == "" {
			derr := domain.Invalid("author", "author is required", "Pass an author name (e.g. Claude, Codex, Zoë).")
			return errorResult(opProjectPost, derr), projectPostOutput{OK: false, Op: opProjectPost, Error: newErrorEnvelope(derr)}, nil
		}
		if err := domain.ValidateActorName("author", author); err != nil {
			derr := domain.AsError(err)
			return errorResult(opProjectPost, derr), projectPostOutput{OK: false, Op: opProjectPost, Error: newErrorEnvelope(derr)}, nil
		}

		if err := domain.ValidateChatMessageBody(in.Body); err != nil {
			derr := domain.AsError(err)
			return errorResult(opProjectPost, derr), projectPostOutput{OK: false, Op: opProjectPost, Error: newErrorEnvelope(derr)}, nil
		}

		msg, svcErr := svc.ChatAdd(ctx, actor, service.ChatAddInput{
			ProjectKey: key,
			Author:     author,
			Body:       in.Body,
		})
		if svcErr != nil {
			derr := asDomainError(svcErr)
			return errorResult(opProjectPost, derr), projectPostOutput{OK: false, Op: opProjectPost, Error: newErrorEnvelope(derr)}, nil
		}

		out := projectPostOutput{
			OK: true,
			Op: opProjectPost,
			Data: &projectPostData{
				ID:        msg.ID,
				Project:   key,
				Author:    msg.Author,
				Body:      msg.Body,
				CreatedAt: formatTime(msg.CreatedAt),
			},
		}
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: jsonText(out)}}}, out, nil
	})
}
