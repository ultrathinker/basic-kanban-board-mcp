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
	Name            string   `json:"name" jsonschema:"the executor's name, taken verbatim: it becomes the key's identity and a participant of the listed projects — assign the executor's cards to exactly this name. No spaces; unique across all tokens"`
	Projects        []string `json:"projects,omitempty" jsonschema:"project keys the executor works in, case-insensitive; you must be an admin or the coordinator of each. Required, except with renew, which keeps the key's own projects and refuses this field"`
	TTLSeconds      int      `json:"ttl_seconds,omitempty" jsonschema:"key lifetime in seconds, counted from now (also for renew); omit for 24h. Refused with participant_only"`
	ParticipantOnly bool     `json:"participant_only,omitempty" jsonschema:"make name a participant of projects WITHOUT a key: no secret exists, nobody can sign in as it, it never expires. Again for the same name adds projects"`
	Renew           bool     `json:"renew,omitempty" jsonschema:"extend the existing executor key named name to now + ttl_seconds, keeping its secret; works on a live or expired key, never a revoked one"`
}

type executorKeyIssueData struct {
	TokenID         string   `json:"token_id"`
	Name            string   `json:"name"`
	Projects        []string `json:"projects"`
	ExpiresAt       string   `json:"expires_at,omitempty"`
	Secret          string   `json:"secret,omitempty"`
	Reissued        bool     `json:"reissued,omitempty"`
	Renewed         bool     `json:"renewed,omitempty"`
	ParticipantOnly bool     `json:"participant_only,omitempty"`
	AddedProjects   []string `json:"added_projects,omitempty"`
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
	"hand it straight to the executor's launcher and never post it to the feed, a note or a card.\n" +
	"renew:true extends the executor key already called name to now + ttl_seconds WITHOUT a new secret, so an agent resuming after a long pause keeps the secret its launcher holds. " +
	"It works on a live or an expired key, omits projects (the key keeps its own), and returns renewed:true with the new expires_at and no secret. " +
	"A revoked key, or a name that is not an executor key, is refused. Same authority: admin, or the coordinator of every project on the key.\n" +
	"participant_only:true makes name a participant of projects without any key — for someone cards are assigned to or reviewed by who never calls the board under that name (the owner, a person, an agent run from outside). " +
	"No secret is created or stored, nobody can sign in as it, and it never expires (ttl_seconds is refused). " +
	"Calling it again for the same name adds the listed projects to it; the response lists all its projects, added_projects the new ones, and has no secret. " +
	"A name that already belongs to a key or a standing token is refused."

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
			Name:            in.Name,
			ProjectKeys:     in.Projects,
			TTLSeconds:      in.TTLSeconds,
			ParticipantOnly: in.ParticipantOnly,
			Renew:           in.Renew,
		})
		if svcErr != nil {
			derr := asDomainError(svcErr)
			return errorResult(opExecutorKeyIssue, derr), executorKeyIssueOutput{OK: false, Op: opExecutorKeyIssue, Error: newErrorEnvelope(derr)}, nil
		}
		out := executorKeyIssueOutput{
			OK: true, Op: opExecutorKeyIssue,
			Data: &executorKeyIssueData{
				TokenID:         res.TokenID,
				Name:            res.Name,
				Projects:        res.ProjectKeys,
				Secret:          res.Secret,
				Reissued:        res.Reissued,
				Renewed:         res.Renewed,
				ParticipantOnly: res.ParticipantOnly,
				AddedProjects:   res.AddedProjects,
			},
		}
		// A participant without a key never expires: no expires_at at all,
		// rather than a zero time that reads like a date.
		if !res.ExpiresAt.IsZero() {
			out.Data.ExpiresAt = formatTime(res.ExpiresAt)
		}
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: jsonText(out)}}}, out, nil
	})
}
