package web

import (
	"context"
	"net/http"
	"strings"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// handleProgressTrackDelete is "POST /fragments/progress/delete": the owner
// permanently removes one assessor's whole progress track — every mark that
// assessor logged for one task, or for the project as a whole when the
// "task" field is empty. This is a real DELETE at the store layer (see
// store.ProgressRepo.DeleteTrack); nothing is hidden-but-remembered.
//
// Deliberately gated by requireOwnerSession, NOT requireAPIAuth: an AI agent
// authenticates through an MCP bearer token or an API key, never a browser
// session cookie, and this is the one board mutation an AI must never reach
// — there is no MCP method for it and there must never be one. Only a
// signed-in owner, through the browser session, may delete a track.
//
// CSRF protection is verifyCSRF, the same double-submit check
// handleFragmentMove uses for "POST /fragments/move" — copied rather than
// reinvented, per the brief. Because requireOwnerSession only ever resolves
// the session cookie (never Authorization/X-API-Key), every request that
// reaches verifyCSRF here is a session request, so the check is always live,
// never skipped the way it would be for a bearer-authenticated fragment.
//
// A GET cannot reach this handler at all: it is registered only as
// "POST /fragments/progress/delete" (see web.go), and Go's ServeMux answers
// a GET to that exact path with 405, never routing it here.
func (w *Web) handleProgressTrackDelete(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireOwnerSession(rw, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, domain.MaxRequestBodyBytes)
	if err := w.verifyCSRF(r); err != nil {
		apiError(rw, err)
		return
	}
	projectKey, err := domain.ValidateProjectKey(r.PostFormValue("project"))
	if err != nil {
		apiError(rw, err)
		return
	}
	assessor := strings.TrimSpace(r.PostFormValue("assessor"))
	if assessor == "" {
		apiError(rw, domain.Invalid("assessor", "assessor is required", "Pass the assessor whose track to remove."))
		return
	}
	// Empty means "the project-level track" — service.ProgressTrackDelete's
	// own convention (ProgressTrackDeleteInput.TaskKey doc).
	taskKey := strings.TrimSpace(r.PostFormValue("task"))

	actor := actorFor(tok)
	if _, err := w.d.Service.ProgressTrackDelete(r.Context(), actor, service.ProgressTrackDeleteInput{
		ProjectKey: projectKey, TaskKey: taskKey, Assessor: assessor,
	}); err != nil {
		apiError(rw, err)
		return
	}

	// The mean, the painted squares and the remaining tracks all change
	// together once a track is gone, so the client gets a freshly recomputed
	// metric back — never a locally patched copy of the old one.
	pv, err := w.progressViewAfterDelete(r.Context(), actor, projectKey, taskKey)
	if err != nil {
		apiError(rw, err)
		return
	}
	w.renderFragment(rw, r, http.StatusOK, "progress-bar", map[string]any{"Progress": pv})
}

// progressViewAfterDelete rebuilds the one progress-bar fragment the client
// needs after a delete: the project-level (manual) metric when taskKey is
// empty, one task's summary metric otherwise. It is deliberately the only
// place outside attachProgress that turns a service progress result into a
// view.ProgressView, so the two stay in agreement about what "Tracks" means
// — and, since KANB-7 landed a forecast badge on the same metric, about what
// "Forecast" means too: deleting a track can change which assessor's ETA is
// now the freshest standing one, so the forecast is recomputed here exactly
// like the mean and the tracks list, never carried over from before the
// delete.
func (w *Web) progressViewAfterDelete(ctx context.Context, a service.Actor, projectKey, taskKey string) (*view.ProgressView, error) {
	if taskKey == "" {
		pp, err := w.d.Service.ProjectProgress(ctx, a, service.ProjectProgressInput{ProjectKey: projectKey})
		if err != nil {
			return nil, err
		}
		return view.NewAssessedProgress(pp.Manual, pp.ManualAssessors, pp.ManualForecastETA, pp.ManualForecastBy).
			WithTracks(projectKey, "", tracksFromService(pp.ManualTracks)), nil
	}
	key, err := domain.NormalizeTaskKey(taskKey)
	if err != nil {
		return nil, err
	}
	tp, err := w.d.Service.TaskProgress(ctx, a, service.TaskProgressInput{ProjectKey: projectKey, Keys: []string{key}})
	if err != nil {
		return nil, err
	}
	if len(tp.Items) == 0 {
		return nil, domain.NotFound("task", key)
	}
	item := tp.Items[0]
	return view.NewAssessedProgress(item.Percent, item.Assessors, item.ForecastETA, item.ForecastBy).
		WithTracks(projectKey, item.Key, tracksFromService(item.Tracks)), nil
}
