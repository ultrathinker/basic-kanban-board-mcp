package mcp

import (
	"context"
	"strconv"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

const opExecutorKeyIssue = "executor_key_issue"

type executorKeyIssueInput struct {
	Name       string   `json:"name" jsonschema:"the executor's name, taken verbatim: it becomes the key's identity and a participant of the listed projects — assign the executor's cards to exactly this name. No spaces; unique across all tokens"`
	Projects   []string `json:"projects" jsonschema:"project keys the executor works in, case-insensitive; you must be an admin or the coordinator of each"`
	TTLSeconds int      `json:"ttl_seconds,omitempty" jsonschema:"key lifetime in seconds; omit for 24h"`
}

type executorKeyIssueData struct {
	TokenID   string   `json:"token_id"`
	Name      string   `json:"name"`
	Projects  []string `json:"projects"`
	ExpiresAt string   `json:"expires_at"`
	Secret    string   `json:"secret"`
	Reissued  bool     `json:"reissued,omitempty"`
}

type executorKeyIssueOutput struct {
	OK    bool                  `json:"ok"`
	Op    string                `json:"op"`
	Data  *executorKeyIssueData `json:"data,omitempty"`
	Error *errorEnvelope        `json:"error,omitempty"`
}

var executorKeyIssueDescription = "Issue an executor key: a named, expiring bearer token for one agent you launch. " +
	"Only an admin or the coordinator of every listed project may issue one; an executor key can never issue keys. " +
	"The name becomes the executor's identity and a participant of the listed projects (board_get participants), " +
	"so assign its cards to exactly that name.\n" +
	"An executor key reads everything and writes only to cards assigned to its own name: " +
	"claim/renew/release (task_claim; task_next claim/start offers only its cards), " +
	"task_update with note, column and rank — any column except a done-kind one, so handing work over means moving it into the review column — " +
	"progress_set on its cards with assessor = its name, and project_post. " +
	"It cannot create, archive or link cards, change assignee, reviewer or any other field, move a card into or out of a done column, " +
	"use force, or touch project_upsert; each refusal is a forbidden error naming the rule. The reviewer closes the work.\n" +
	"ttl_seconds defaults to " + strconv.Itoa(int(domain.ExecutorKeyDefaultTTL.Seconds())) +
	" (24h) and must lie in " + strconv.Itoa(int(domain.ExecutorKeyMinTTL.Seconds())) + ".." +
	strconv.Itoa(int(domain.ExecutorKeyMaxTTL.Seconds())) + ". " +
	"An expired key stops authenticating and leaves the participant list on its own — nothing to clean up; " +
	"issuing the same name again after it expired re-issues that key (reissued:true) with a new secret. " +
	"A live key's name is refused.\n" +
	"Returns `data`: token_id, name, projects, expires_at and `secret`. The secret is shown ONLY in this response and is never stored or shown again: " +
	"hand it straight to the executor's launcher and never post it to the feed, a note or a card."

func executorKeyIssueTool() *gomcp.Tool {
	s := schemaFor[executorKeyIssueInput]()
	setMinLen(prop(s, "name"), 1)
	setMaxLen(prop(s, "name"), domain.MaxAssigneeLen)
	projects := prop(s, "projects")
	setMinItems(projects, 1)
	setMaxItems(projects, domain.MaxExecutorKeyProjects)
	setMinLen(projects.Items, domain.MinProjectKeyLen)
	setMaxLen(projects.Items, domain.MaxProjectKeyLen)
	ttl := prop(s, "ttl_seconds")
	setMin(ttl, 0)
	setMax(ttl, domain.ExecutorKeyMaxTTL.Seconds())

	return &gomcp.Tool{
		Name:        opExecutorKeyIssue,
		Description: executorKeyIssueDescription,
		InputSchema: s,
	}
}

func registerExecutorKeyIssue(s *gomcp.Server, svc service.Service) {
	tool := executorKeyIssueTool()
	addTool(s, tool, func(ctx context.Context, req *gomcp.CallToolRequest, in executorKeyIssueInput) (*gomcp.CallToolResult, executorKeyIssueOutput, error) {
		actor, aerr := actorFromContext(ctx)
		if aerr != nil {
			return errorResult(opExecutorKeyIssue, aerr), executorKeyIssueOutput{OK: false, Op: opExecutorKeyIssue, Error: newErrorEnvelope(aerr)}, nil
		}
		res, svcErr := svc.ExecutorKeyIssue(ctx, actor, service.ExecutorKeyIssueInput{
			Name:        in.Name,
			ProjectKeys: in.Projects,
			TTLSeconds:  in.TTLSeconds,
		})
		if svcErr != nil {
			derr := asDomainError(svcErr)
			return errorResult(opExecutorKeyIssue, derr), executorKeyIssueOutput{OK: false, Op: opExecutorKeyIssue, Error: newErrorEnvelope(derr)}, nil
		}
		out := executorKeyIssueOutput{
			OK: true, Op: opExecutorKeyIssue,
			Data: &executorKeyIssueData{
				TokenID:   res.TokenID,
				Name:      res.Name,
				Projects:  res.ProjectKeys,
				ExpiresAt: formatTime(res.ExpiresAt),
				Secret:    res.Secret,
				Reissued:  res.Reissued,
			},
		}
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: jsonText(out)}}}, out, nil
	})
}
