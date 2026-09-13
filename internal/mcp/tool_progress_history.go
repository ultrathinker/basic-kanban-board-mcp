package mcp

import (
	"context"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

const opProgressHistory = "progress_history"

type progressHistoryInput struct {
	Task    *string `json:"task,omitempty" jsonschema:"task key (e.g. KANB-3); exactly one of task or project must be provided"`
	Project *string `json:"project,omitempty" jsonschema:"project key; exactly one of task or project must be provided"`
	Limit   int     `json:"limit,omitempty" jsonschema:"how many of the most recent marks to return; older marks beyond this cap are simply not included. Defaults to the maximum."`
}

// progressHistoryMarkOut is deliberately leaner than progress_set's
// progressMarkOut: every mark in one progress_history response shares the
// same (project, task) scope — it is carried once on progressHistoryData —
// so repeating it per mark would only be stutter.
type progressHistoryMarkOut struct {
	ID        string  `json:"id" jsonschema:"unique mark identifier"`
	Assessor  string  `json:"assessor" jsonschema:"agent identity / display name"`
	Percent   int     `json:"percent" jsonschema:"recorded progress percent 0..100"`
	ETA       *string `json:"eta,omitempty" jsonschema:"RFC3339 ETA forecast if the assessor gave one"`
	CreatedAt string  `json:"created_at" jsonschema:"RFC3339 creation timestamp"`
}

type progressHistoryData struct {
	Project string                   `json:"project" jsonschema:"canonical uppercase project key"`
	Task    *string                  `json:"task,omitempty" jsonschema:"canonical task key (e.g. KANB-3); omitted for a project-level history"`
	Marks   []progressHistoryMarkOut `json:"marks" jsonschema:"marks in chronological order (oldest first); capped to the most recent 'limit' entries when the full history is longer"`
	Total   int                      `json:"total" jsonschema:"the scope's full mark count before the 'limit' cap; larger than len(marks) when older marks were left out"`
}

type progressHistoryOutput struct {
	OK    bool                 `json:"ok"`
	Op    string               `json:"op"`
	Data  *progressHistoryData `json:"data,omitempty"`
	Meta  *toolMeta            `json:"meta,omitempty"`
	Error *errorEnvelope       `json:"error,omitempty"`
}

const progressHistoryDescription = "Read the full progress-mark history behind one metric: a project's manual estimate (project only) or one task's summary estimate (task). " +
	"This is the same append-only calibration history progress_set writes — nothing here is averaged, rounded or decimated, so a revision like 70% rolled back to 45% is visible exactly as it happened, in the order it happened. " +
	"Specify exactly one target: either `task` (e.g. KANB-3) to read one task's history, or `project` (e.g. KANB) to read the project's own manual history. " +
	"Returns `data.marks[]` in chronological order (oldest first) — each with its assessor, percent, timestamp and eta forecast if one was given — plus `data.total`, the full mark count before the `limit` cap."

func progressHistoryTool() *gomcp.Tool {
	s := schemaFor[progressHistoryInput]()
	setMin(prop(s, "limit"), 1)
	setMax(prop(s, "limit"), float64(domain.MaxProgressHistoryLimit))
	setDefault(prop(s, "limit"), domain.MaxProgressHistoryLimit)

	return &gomcp.Tool{
		Name:        opProgressHistory,
		Description: progressHistoryDescription,
		InputSchema: s,
	}
}

// registerProgressHistory is a thin read wrapper over the already-existing
// service.Service.ProgressHistory (internal/service/progress.go), the same
// method internal/web/pages.go's handleProgressChart calls to draw the
// browser's progress-history chart. No new service or store logic: this
// layer only resolves the caller's task-or-project target into the
// (ProjectKey, TaskKey) scope ProgressHistory already accepts, and caps the
// result to the most recent `limit` marks.
func registerProgressHistory(s *gomcp.Server, svc service.Service) {
	tool := progressHistoryTool()
	gomcp.AddTool(s, tool, func(ctx context.Context, req *gomcp.CallToolRequest, in progressHistoryInput) (*gomcp.CallToolResult, progressHistoryOutput, error) {
		actor, aerr := actorFromContext(ctx)
		if aerr != nil {
			return errorResult(opProgressHistory, aerr), progressHistoryOutput{OK: false, Op: opProgressHistory, Error: newErrorEnvelope(aerr)}, nil
		}

		hasTask := in.Task != nil && strings.TrimSpace(*in.Task) != ""
		hasProject := in.Project != nil && strings.TrimSpace(*in.Project) != ""

		if hasTask && hasProject {
			derr := domain.Invalid("task", "both task and project were provided", "Provide exactly one of 'task' (e.g. KANB-3) or 'project' (e.g. KANB), not both.")
			return errorResult(opProgressHistory, derr), progressHistoryOutput{OK: false, Op: opProgressHistory, Error: newErrorEnvelope(derr)}, nil
		}
		if !hasTask && !hasProject {
			derr := domain.Invalid("task", "neither task nor project was provided", "Provide exactly one of 'task' (e.g. KANB-3) or 'project' (e.g. KANB).")
			return errorResult(opProgressHistory, derr), progressHistoryOutput{OK: false, Op: opProgressHistory, Error: newErrorEnvelope(derr)}, nil
		}

		// ProgressHistoryInput always needs a ProjectKey (it resolves the
		// project row first, then optionally a task inside it) — unlike
		// progress_set, it has no "derive the project from the task key"
		// path of its own, so a task-only call resolves it here the same
		// way resolveTask does internally: parse the "PROJ-N" prefix.
		var projectKey, taskKey string
		if hasTask {
			nk, err := domain.NormalizeTaskKey(*in.Task)
			if err != nil {
				derr := domain.AsError(err)
				return errorResult(opProgressHistory, derr), progressHistoryOutput{OK: false, Op: opProgressHistory, Error: newErrorEnvelope(derr)}, nil
			}
			taskKey = nk
			pk, _, _ := domain.ParseTaskKey(nk) // safe: NormalizeTaskKey already validated the shape
			projectKey = pk
		} else {
			pk, err := domain.ValidateProjectKey(*in.Project)
			if err != nil {
				derr := domain.AsError(err)
				return errorResult(opProgressHistory, derr), progressHistoryOutput{OK: false, Op: opProgressHistory, Error: newErrorEnvelope(derr)}, nil
			}
			projectKey = pk
		}

		res, err := svc.ProgressHistory(ctx, actor, service.ProgressHistoryInput{
			ProjectKey: projectKey,
			TaskKey:    taskKey,
		})
		if err != nil {
			derr := asDomainError(err)
			return errorResult(opProgressHistory, derr), progressHistoryOutput{OK: false, Op: opProgressHistory, Error: newErrorEnvelope(derr)}, nil
		}

		// Cap to the most recent `limit` marks: keep the tail (newest) and
		// drop the head (oldest), same direction board_get's done_limit
		// truncates in ("most recently done first"). Unlike done_limit this
		// stays in the service's own chronological (oldest-first) order —
		// the same order the browser's progress chart already consumes —
		// rather than flipping to newest-first, which would invent a second,
		// undocumented order convention for the same data.
		limit := in.Limit
		if limit <= 0 || limit > domain.MaxProgressHistoryLimit {
			limit = domain.MaxProgressHistoryLimit
		}
		total := len(res.Marks)
		marks := res.Marks
		if total > limit {
			marks = marks[total-limit:]
		}

		marksOut := make([]progressHistoryMarkOut, len(marks))
		for i, m := range marks {
			marksOut[i] = progressHistoryMarkOut{
				ID:        m.ID,
				Assessor:  m.Assessor,
				Percent:   m.Percent,
				ETA:       formatTimePtr(m.ETA),
				CreatedAt: formatTime(m.CreatedAt),
			}
		}

		var taskKeyPtr *string
		if res.TaskKey != "" {
			taskKeyPtr = &res.TaskKey
		}

		out := progressHistoryOutput{
			OK: true,
			Op: opProgressHistory,
			Data: &progressHistoryData{
				Project: res.ProjectKey,
				Task:    taskKeyPtr,
				Marks:   marksOut,
				Total:   total,
			},
			Meta: &toolMeta{Count: len(marksOut)},
		}
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: jsonText(out)}}}, out, nil
	})
}
