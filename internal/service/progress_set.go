package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ProgressSetInput specifies the parameters to record a progress assessment.
type ProgressSetInput struct {
	Assessor   string
	Percent    int
	TaskKey    string // empty if project-level assessment
	ProjectKey string // empty if task-level assessment
	ETA        *time.Time
}

// ProgressSetResult carries the recorded mark and updated summary progress for the scope.
type ProgressSetResult struct {
	Mark           domain.ProgressMark
	TaskKey        string // PROJ-N, or empty for project-level marks
	ProjectKey     string // canonical uppercase project key
	SummaryPercent *int   // aggregate progress percent for the scope (mean of latest marks, rounded half-up)
	Assessors      int    // count of distinct assessors contributing to the latest summary
}

// ProgressSet records an append-only progress mark for either a task or a project.
func (s *svc) ProgressSet(ctx context.Context, a Actor, in ProgressSetInput) (*ProgressSetResult, error) {
	if err := requireWrite(a); err != nil {
		return nil, err
	}

	assessor := strings.TrimSpace(in.Assessor)
	if assessor == "" {
		return nil, domain.Invalid("assessor", "assessor is required", "Pass an assessor name representing your agent identity.")
	}

	if in.Percent < 0 || in.Percent > 100 {
		return nil, domain.Invalid("percent", fmt.Sprintf("progress percent %d is outside 0..100", in.Percent), "Send a percent between 0 and 100.")
	}

	hasTask := strings.TrimSpace(in.TaskKey) != ""
	hasProject := strings.TrimSpace(in.ProjectKey) != ""

	if hasTask && hasProject {
		return nil, domain.Invalid("task", "both task and project were provided", "Provide exactly one of 'task' or 'project', not both.")
	}
	if !hasTask && !hasProject {
		return nil, domain.Invalid("task", "neither task nor project was provided", "Provide exactly one of 'task' or 'project'.")
	}

	ctx = store.WithActor(ctx, a.Name)

	var result ProgressSetResult
	err := s.store.Write(ctx, func(tx store.Tx) error {
		if hasTask {
			t, err := s.resolveTask(tx, a, in.TaskKey)
			if err != nil {
				return err
			}
			p, err := s.store.Projects().GetByID(tx, t.ProjectID)
			if err != nil {
				return err
			}

			m := &domain.ProgressMark{
				ID:        newID(),
				ProjectID: p.ID,
				TaskID:    &t.ID,
				Assessor:  assessor,
				Percent:   in.Percent,
				ETA:       in.ETA,
			}
			if err := s.store.Progress().Add(tx, m); err != nil {
				return err
			}

			latest, err := s.store.Progress().LatestByAssessor(tx, p.ID, &t.ID)
			if err != nil {
				return err
			}
			if mean, ok := meanPercent(latestPercents(latest)); ok {
				result.SummaryPercent = &mean
			}
			result.Assessors = len(latest)
			result.Mark = *m
			result.TaskKey = t.Key
			result.ProjectKey = p.Key
			return nil
		}

		pk, err := domain.ValidateProjectKey(in.ProjectKey)
		if err != nil {
			return err
		}
		p, err := s.resolveProject(tx, a, pk)
		if err != nil {
			return err
		}

		m := &domain.ProgressMark{
			ID:        newID(),
			ProjectID: p.ID,
			TaskID:    nil,
			Assessor:  assessor,
			Percent:   in.Percent,
			ETA:       in.ETA,
		}
		if err := s.store.Progress().Add(tx, m); err != nil {
			return err
		}

		latest, err := s.store.Progress().LatestByAssessor(tx, p.ID, nil)
		if err != nil {
			return err
		}
		if mean, ok := meanPercent(latestPercents(latest)); ok {
			result.SummaryPercent = &mean
		}
		result.Assessors = len(latest)
		result.Mark = *m
		result.ProjectKey = p.Key
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}
