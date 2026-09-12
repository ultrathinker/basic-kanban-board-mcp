package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

const opProgressSet = "progress_set"

type progressSetInput struct {
	Assessor string  `json:"assessor" jsonschema:"agent self-declared name / identity (free-form text)"`
	Percent  int     `json:"percent" jsonschema:"progress percentage, integer 0..100"`
	Task     *string `json:"task,omitempty" jsonschema:"task key (e.g. KANB-3); exactly one of task or project must be provided"`
	Project  *string `json:"project,omitempty" jsonschema:"project key; exactly one of task or project must be provided"`
	ETA      *string `json:"eta,omitempty" jsonschema:"RFC3339 timestamp forecasting target completion date and time (not duration remaining)"`
}

type progressMarkOut struct {
	ID        string  `json:"id" jsonschema:"unique mark identifier"`
	Project   string  `json:"project" jsonschema:"canonical uppercase project key"`
	Task      *string `json:"task,omitempty" jsonschema:"canonical task key (e.g. KANB-3); omitted for project-level marks"`
	Assessor  string  `json:"assessor" jsonschema:"agent identity / display name"`
	Percent   int     `json:"percent" jsonschema:"recorded progress percent 0..100"`
	ETA       *string `json:"eta,omitempty" jsonschema:"RFC3339 ETA forecast if specified"`
	CreatedAt string  `json:"created_at" jsonschema:"RFC3339 creation timestamp"`
}

type progressSummaryOut struct {
	Percent   *int `json:"percent" jsonschema:"current aggregated progress percent for the scope (mean of latest mark per assessor, rounded half-up)"`
	Assessors int  `json:"assessors" jsonschema:"number of distinct assessors contributing to the latest summary"`
}

type progressSetData struct {
	Mark    progressMarkOut    `json:"mark" jsonschema:"the recorded progress mark"`
	Summary progressSummaryOut `json:"summary" jsonschema:"current summary progress for the assessed scope"`
}

type progressSetOutput struct {
	OK    bool             `json:"ok"`
	Op    string           `json:"op"`
	Data  *progressSetData `json:"data,omitempty"`
	Meta  *toolMeta        `json:"meta,omitempty"`
	Error *errorEnvelope   `json:"error,omitempty"`
}

const progressSetDescription = "Record a progress assessment and completion forecast for a task or an entire project. " +
	"Provide periodic assessments as work proceeds (e.g. every few steps or significant discoveries), not just at the start or finish. " +
	"Progress marks form an append-only calibration history: revisions never overwrite prior marks. " +
	"Overestimating and underestimating are expected and harmless; rolling your progress estimate backward (e.g. from 70% down to 45%) is a normal and valuable signal reflecting discovered complexity, not an admission of defeat. " +
	"Specify exactly one target: either `task` (e.g. KANB-3) to assess a specific task, or `project` (e.g. KANB) to assess the project as a whole. " +
	"`eta` is an RFC3339 timestamp forecasting the expected finish date and time (e.g. 2026-09-12T18:00:00Z), not a remaining duration. " +
	"Returns `data` containing the recorded mark and the updated summary progress for the assessed scope."

func progressSetTool() *gomcp.Tool {
	s := schemaFor[progressSetInput]()
	setMin(prop(s, "percent"), 0)
	setMax(prop(s, "percent"), 100)
	setMaxLen(prop(s, "assessor"), domain.MaxAssigneeLen)

	return &gomcp.Tool{
		Name:        opProgressSet,
		Description: progressSetDescription,
		InputSchema: s,
	}
}

func registerProgressSet(s *gomcp.Server, svc service.Service) {
	tool := progressSetTool()
	gomcp.AddTool(s, tool, func(ctx context.Context, req *gomcp.CallToolRequest, in progressSetInput) (*gomcp.CallToolResult, progressSetOutput, error) {
		actor, aerr := actorFromContext(ctx)
		if aerr != nil {
			return errorResult(opProgressSet, aerr), progressSetOutput{OK: false, Op: opProgressSet, Error: newErrorEnvelope(aerr)}, nil
		}

		if in.Percent < 0 || in.Percent > 100 {
			derr := domain.Invalid("percent", fmt.Sprintf("percent %d is outside 0..100", in.Percent), "Send an integer between 0 and 100.")
			return errorResult(opProgressSet, derr), progressSetOutput{OK: false, Op: opProgressSet, Error: newErrorEnvelope(derr)}, nil
		}

		var eta *time.Time
		if in.ETA != nil {
			rawETA := strings.TrimSpace(*in.ETA)
			if rawETA == "" {
				derr := domain.Invalid("eta", "eta timestamp cannot be empty", "Pass a valid RFC3339 timestamp (e.g. 2026-09-12T18:00:00Z) or omit eta.")
				return errorResult(opProgressSet, derr), progressSetOutput{OK: false, Op: opProgressSet, Error: newErrorEnvelope(derr)}, nil
			}
			t, err := time.Parse(time.RFC3339Nano, rawETA)
			if err != nil {
				t, err = time.Parse(time.RFC3339, rawETA)
			}
			if err != nil {
				derr := domain.Invalid("eta", fmt.Sprintf("invalid eta timestamp %q", *in.ETA), "Pass an RFC3339 timestamp forecasting completion date and time (e.g. 2026-09-12T18:00:00Z).")
				return errorResult(opProgressSet, derr), progressSetOutput{OK: false, Op: opProgressSet, Error: newErrorEnvelope(derr)}, nil
			}
			tUTC := t.UTC()
			eta = &tUTC
		}

		hasTask := in.Task != nil && strings.TrimSpace(*in.Task) != ""
		hasProject := in.Project != nil && strings.TrimSpace(*in.Project) != ""

		if hasTask && hasProject {
			derr := domain.Invalid("task", "both task and project were provided", "Provide exactly one of 'task' (e.g. KANB-3) or 'project' (e.g. KANB), not both.")
			return errorResult(opProgressSet, derr), progressSetOutput{OK: false, Op: opProgressSet, Error: newErrorEnvelope(derr)}, nil
		}
		if !hasTask && !hasProject {
			derr := domain.Invalid("task", "neither task nor project was provided", "Provide exactly one of 'task' (e.g. KANB-3) or 'project' (e.g. KANB).")
			return errorResult(opProgressSet, derr), progressSetOutput{OK: false, Op: opProgressSet, Error: newErrorEnvelope(derr)}, nil
		}

		assessor := strings.TrimSpace(in.Assessor)
		if assessor == "" {
			derr := domain.Invalid("assessor", "assessor is required", "Pass an assessor name representing your agent identity.")
			return errorResult(opProgressSet, derr), progressSetOutput{OK: false, Op: opProgressSet, Error: newErrorEnvelope(derr)}, nil
		}

		var taskKey, projectKey string
		if hasTask {
			taskKey = strings.TrimSpace(*in.Task)
		} else {
			projectKey = strings.TrimSpace(*in.Project)
		}

		res, svcErr := svc.ProgressSet(ctx, actor, service.ProgressSetInput{
			Assessor:   assessor,
			Percent:    in.Percent,
			TaskKey:    taskKey,
			ProjectKey: projectKey,
			ETA:        eta,
		})
		if svcErr != nil {
			derr := asDomainError(svcErr)
			return errorResult(opProgressSet, derr), progressSetOutput{OK: false, Op: opProgressSet, Error: newErrorEnvelope(derr)}, nil
		}

		var taskKeyPtr *string
		if res.TaskKey != "" {
			taskKeyPtr = &res.TaskKey
		}

		out := progressSetOutput{
			OK: true,
			Op: opProgressSet,
			Data: &progressSetData{
				Mark: progressMarkOut{
					ID:        res.Mark.ID,
					Project:   res.ProjectKey,
					Task:      taskKeyPtr,
					Assessor:  res.Mark.Assessor,
					Percent:   res.Mark.Percent,
					ETA:       formatTimePtr(res.Mark.ETA),
					CreatedAt: formatTime(res.Mark.CreatedAt),
				},
				Summary: progressSummaryOut{
					Percent:   res.SummaryPercent,
					Assessors: res.Assessors,
				},
			},
		}
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: jsonText(out)}}}, out, nil
	})
}
