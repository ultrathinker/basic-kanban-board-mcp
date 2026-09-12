package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ---------------------------------------------------------------------------
// Progress aggregation.
//
// Every number a surface renders about progress is computed here, once. Web
// and MCP must copy results, not arithmetic: two places deriving "the mean of
// the latest marks" WILL drift — rounding first, scope next — and then the UI
// and the API disagree about the same task.
//
// Three metrics, deliberately independent:
//
//   - a task's summary progress is the mean of the latest mark of every
//     assessor of that task, one mark per assessor no matter how long the
//     track is;
//   - a project's manual progress is the same mean over the project-level
//     marks only (task_id IS NULL) — task marks never bleed into it;
//   - a project's automatic progress is the share of its tasks sitting in
//     done-kind columns, computed from the board, not from the marks. It is
//     the honest anchor the manual number is judged against, and the reason
//     the manual number must not contaminate it.
//
// "Nobody assessed" is a nil *int everywhere here, never 0: zero claims the
// work has not started, and an empty track is a different fact.
// ---------------------------------------------------------------------------

// RoundMeanHalfUp rounds sum/n to the nearest integer, halves up (2.5 -> 3,
// 0.5 -> 1). Pure integer arithmetic: a float64 mean can sit an ulp below the
// exact half and round the wrong way, which is exactly the kind of drift this
// package exists to prevent. n must be positive; the zero return for n <= 0
// keeps a misuse from dividing by zero, but callers report "no value" through
// meanPercent instead of reaching here.
//
// Exported because the web and MCP renderers must reuse this exact rule
// rather than grow a second one.
func RoundMeanHalfUp(sum, n int) int {
	if n <= 0 {
		return 0
	}
	return (2*sum + n) / (2 * n)
}

// meanPercent averages percents already reduced to one value per assessor.
// ok is false for an empty input — the caller turns that into a nil Percent,
// not a zero.
func meanPercent(percents []int) (int, bool) {
	if len(percents) == 0 {
		return 0, false
	}
	sum := 0
	for _, p := range percents {
		sum += p
	}
	return RoundMeanHalfUp(sum, len(percents)), true
}

// latestPercents reduces latest-per-assessor marks to their percent values.
// The store guarantees one row per assessor here (LatestByTask,
// LatestByAssessor); the 0..100 clamp is not re-checked because the schema
// CHECK on percent already enforces it at the write boundary.
func latestPercents(marks []domain.ProgressMark) []int {
	out := make([]int, 0, len(marks))
	for _, m := range marks {
		out = append(out, m.Percent)
	}
	return out
}

// taskPercent summarizes one task's latest marks. A nil percent means nobody
// assessed the task.
func taskPercent(latest []domain.ProgressMark) (*int, int) {
	percents := latestPercents(latest)
	if mean, ok := meanPercent(percents); ok {
		return &mean, len(percents)
	}
	return nil, 0
}

// latestForecast picks the current, most pessimistic standing forecast out
// of a set of "latest mark per assessor" rows — exactly what
// store.Progress().LatestByTask / LatestByAssessor already return, and what
// taskPercent/meanPercent above are built from. An assessor's own most
// recent mark is their current standing answer for both percent AND
// forecast, since ETA rides along on the same append-only mark; a latest
// mark with no ETA means that assessor is not currently offering one — the
// append-only, no-second-opinion model has no honest way to say "my old
// promise still holds" instead, so this does not go looking further back in
// that assessor's history for one.
//
// Among the assessors who ARE currently offering a forecast, this returns
// the LATEST (most pessimistic) date, never an average: averaging promised
// dates is meaningless, and the later one is the honest one. Ties (two
// assessors naming the exact same instant) fall to the alphabetically first
// assessor, so the result is deterministic. Returns (nil, "") when nobody in
// marks currently has a forecast.
func latestForecast(marks []domain.ProgressMark) (*time.Time, string) {
	var bestETA *time.Time
	var bestBy string
	for _, m := range marks {
		if m.ETA == nil {
			continue
		}
		if bestETA == nil || m.ETA.After(*bestETA) || (m.ETA.Equal(*bestETA) && m.Assessor < bestBy) {
			eta := *m.ETA
			bestETA = &eta
			bestBy = m.Assessor
		}
	}
	return bestETA, bestBy
}

// buildTracks pairs each assessor's latest mark with how many marks make up
// their whole track in this scope, for the delete-track control: it needs an
// assessor's name and a point count, never the mean itself. counts may be
// nil or missing an assessor (defaulting to 0) without panicking — every
// assessor with a latest mark has logged at least one, so 0 never happens in
// practice, but a caller with a stale or partial count map must not crash.
func buildTracks(latest []domain.ProgressMark, counts map[string]int) []AssessorTrack {
	if len(latest) == 0 {
		return nil
	}
	out := make([]AssessorTrack, 0, len(latest))
	for _, m := range latest {
		out = append(out, AssessorTrack{Assessor: m.Assessor, Percent: m.Percent, Count: counts[m.Assessor]})
	}
	return out
}

// countByAssessor tallies a flat history read into per-assessor mark counts.
// Used only for the project-level scope, which is read once per page render
// (never once per card), so a full History() plus this Go-side tally costs
// nothing worth a dedicated batched store method — that batching only
// matters for TaskProgress, which is CountsByTask's job instead.
func countByAssessor(marks []domain.ProgressMark) map[string]int {
	out := make(map[string]int, len(marks))
	for _, m := range marks {
		out[m.Assessor]++
	}
	return out
}

// autoPercent is done/total as a 0..100 share, rounded halves up like every
// other percent here. ok is false when the project has no tasks: there is
// nothing measured, and 0% would claim work not started (and 100% would
// claim it finished).
func autoPercent(done, total int) (int, bool) {
	if total <= 0 {
		return 0, false
	}
	return RoundMeanHalfUp(done*100, total), true
}

// TaskProgress returns the summary progress of 1..50 tasks of one project in
// the caller's order, in a fixed number of store reads regardless of the key
// count: the board calls this for every card it renders.
func (s *svc) TaskProgress(ctx context.Context, a Actor, in TaskProgressInput) (*TaskProgressResult, error) {
	if err := requireRead(a); err != nil {
		return nil, err
	}
	if in.ProjectKey == "" {
		return nil, domain.Invalid("project_key", "project key is required",
			"Pass the project key, e.g. BMB.")
	}
	if len(in.Keys) == 0 {
		return nil, domain.Invalid("keys", "at least one key is required", "Pass 1-50 task keys.")
	}
	if len(in.Keys) > domain.MaxGetKeys {
		return nil, domain.Invalid("keys",
			fmt.Sprintf("%d keys, the limit is %d", len(in.Keys), domain.MaxGetKeys),
			"Split the request into multiple calls.")
	}
	if err := requireProjectAccess(a, in.ProjectKey); err != nil {
		return nil, err
	}
	ctx = store.WithActor(ctx, a.Name)

	var result TaskProgressResult
	err := s.store.Read(ctx, func(tx store.Tx) error {
		p, err := s.store.Projects().GetByKey(tx, in.ProjectKey)
		if err != nil {
			return err
		}
		result.ProjectKey = p.Key

		// Three store reads in total — project, tasks, latest marks — none of
		// them per key. The marks read covers every task track of the project
		// at once; the map simply has no entry for a task nobody assessed.
		tasks, err := s.store.Tasks().GetManyByKeys(tx, in.Keys)
		if err != nil {
			return err
		}
		latest, err := s.store.Progress().LatestByTask(tx, p.ID)
		if err != nil {
			return err
		}
		// Batched like LatestByTask: one query for the whole project, so the
		// delete-track control's point counts cost nothing extra per card.
		counts, err := s.store.Progress().CountsByTask(tx, p.ID)
		if err != nil {
			return err
		}

		result.Items = make([]TaskProgressItem, 0, len(in.Keys))
		for _, rawKey := range in.Keys {
			t, ok := tasks[strings.ToUpper(rawKey)]
			if !ok || t.ProjectID != p.ID {
				// The key does not parse, does not exist, or names a task of
				// another project. Either way the caller asked about it and
				// gets an explicit answer, not a silent drop.
				canon, cErr := domain.NormalizeTaskKey(rawKey)
				if cErr != nil {
					canon = strings.ToUpper(rawKey)
				}
				result.NotFound = append(result.NotFound, canon)
				continue
			}
			percent, assessors := taskPercent(latest[t.ID])
			forecastETA, forecastBy := latestForecast(latest[t.ID])
			result.Items = append(result.Items, TaskProgressItem{
				Key:         t.Key,
				TaskID:      t.ID,
				Percent:     percent,
				Assessors:   assessors,
				ForecastETA: forecastETA,
				ForecastBy:  forecastBy,
				Tracks:      buildTracks(latest[t.ID], counts[t.ID]),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// ProjectProgress returns the manual and the automatic project progress. The
// two never overwrite each other — they are computed from disjoint sources
// (project-level marks vs. column membership) and are both reported, nil
// separately, whenever their source has nothing to say.
func (s *svc) ProjectProgress(ctx context.Context, a Actor, in ProjectProgressInput) (*ProjectProgressResult, error) {
	if err := requireRead(a); err != nil {
		return nil, err
	}
	if err := requireProjectAccess(a, in.ProjectKey); err != nil {
		return nil, err
	}
	ctx = store.WithActor(ctx, a.Name)

	var result ProjectProgressResult
	err := s.store.Read(ctx, func(tx store.Tx) error {
		p, err := s.store.Projects().GetByKey(tx, in.ProjectKey)
		if err != nil {
			return err
		}
		result.ProjectKey = p.Key

		// Manual: the project-level track only. Task marks are a different
		// metric; letting them bleed in would average the assessors' opinion
		// of individual cards into the opinion about the whole project.
		latest, err := s.store.Progress().LatestByAssessor(tx, p.ID, nil)
		if err != nil {
			return err
		}
		if mean, ok := meanPercent(latestPercents(latest)); ok {
			result.Manual = &mean
		}
		result.ManualAssessors = len(latest)
		result.ManualForecastETA, result.ManualForecastBy = latestForecast(latest)
		// The project-level scope is read once per page render (never once
		// per card), so a full History() plus a Go-side tally is cheap here
		// — CountsByTask's batching is what TaskProgress needs, not this.
		hist, err := s.store.Progress().History(tx, p.ID, nil)
		if err != nil {
			return err
		}
		result.ManualTracks = buildTracks(latest, countByAssessor(hist))

		// Auto: the board's own verdict, from column membership. IncludeDone
		// is required — the filter's default hides done columns, which is a
		// display rule and would make this metric count nothing. Archived
		// tasks are excluded on both sides of the fraction, consistent with
		// how the board and the subtask tally treat them.
		cols, err := s.store.Columns().ListByProject(tx, p.ID)
		if err != nil {
			return err
		}
		doneCols := make(map[string]bool, len(cols))
		for _, c := range cols {
			if c.Kind == domain.KindDone {
				doneCols[c.ID] = true
			}
		}
		tasks, err := s.store.Tasks().List(tx, store.TaskFilter{
			ProjectIDs:  []string{p.ID},
			IncludeDone: true,
		})
		if err != nil {
			return err
		}
		result.TotalTasks = len(tasks)
		for _, t := range tasks {
			if doneCols[t.ColumnID] {
				result.DoneTasks++
			}
		}
		if share, ok := autoPercent(result.DoneTasks, result.TotalTasks); ok {
			result.Auto = &share
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// ProgressTrackDelete removes one assessor's whole track. It is the store's
// DeleteTrack with keys resolved to ids and the usual write-scope checks —
// no rules of its own: which tracks may be deleted and what it means is
// already decided at the storage boundary.
func (s *svc) ProgressTrackDelete(ctx context.Context, a Actor, in ProgressTrackDeleteInput) (*ProgressTrackDeleteResult, error) {
	if err := requireWrite(a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Assessor) == "" {
		return nil, domain.Invalid("assessor", "assessor is empty",
			"Pass the assessor whose track to remove.")
	}
	ctx = store.WithActor(ctx, a.Name)

	var result ProgressTrackDeleteResult
	err := s.store.Write(ctx, func(tx store.Tx) error {
		p, err := s.resolveProject(tx, a, in.ProjectKey)
		if err != nil {
			return err
		}
		var taskID *string
		if in.TaskKey != "" {
			t, err := s.store.Tasks().GetByKey(tx, in.TaskKey)
			if err != nil {
				return err
			}
			if t.ProjectID != p.ID {
				return domain.NotFound("task", in.TaskKey)
			}
			taskID = &t.ID
		}
		removed, err := s.store.Progress().DeleteTrack(tx, p.ID, taskID, in.Assessor)
		if err != nil {
			return err
		}
		result.Removed = removed
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}
