package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// These handlers back the htmx/fetch mutations the board and drawer pages
// perform. Every one of them requires write scope and a verified CSRF token
// (checked via header, matching what web/static/app.js actually sends).
//
// Move is the only one currently wired end-to-end by app.js (drag-and-drop
// and the keyboard move menu both POST here). Acceptance/notes/claim have no
// caller in the current templates yet — the drawer's checkboxes and the
// claim/release actions render but carry no hx-post/fetch wiring. These
// endpoints exist and are tested directly so the UI agent's next pass has a
// working seam to wire up; see the task report.

// handleFragmentMove is "POST /fragments/move": key, column, position
// ("top"|"bottom", optional). SortableJS and the move-menu both send only
// key+column+position — no if_version, because the card markup has no
// data-version attribute to read it from. task_update requires if_version
// for a column change, so this handler bridges the gap with a read-then-
// write: fetch the task's current version, then submit the patch with it.
// That leaves a small window between the two service calls where a
// concurrent edit could land; a genuine conflict still surfaces as a 409
// (task_update re-checks the version inside its own transaction) rather than
// silently overwriting — it just is not the same single-transaction
// guarantee the rest of the product provides. Closing it fully needs the
// board card to carry its version so the client can send if_version itself.
func (w *Web) handleFragmentMove(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireAPIAuth(rw, r, domain.ScopeWrite)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, domain.MaxRequestBodyBytes)
	if err := w.verifyCSRF(r); err != nil {
		apiError(rw, err)
		return
	}
	key, err := domain.NormalizeTaskKey(r.PostFormValue("key"))
	if err != nil {
		apiError(rw, err)
		return
	}
	column := strings.TrimSpace(r.PostFormValue("column"))
	if column == "" {
		apiError(rw, domain.Invalid("column", "column is required", "Pass the destination column name."))
		return
	}
	rank := strings.TrimSpace(r.PostFormValue("position"))
	if rank != "top" && rank != "bottom" {
		rank = "bottom"
	}

	actor := actorFor(tok)
	current, err := w.d.Service.TaskGet(r.Context(), actor, service.TaskGetInput{Keys: []string{key}})
	if err != nil {
		apiError(rw, err)
		return
	}
	if len(current.Tasks) == 0 {
		apiError(rw, domain.NotFound("task", key))
		return
	}
	version := current.Tasks[0].Version

	res, err := w.d.Service.TaskUpdate(r.Context(), actor, service.TaskUpdateInput{
		Atomic: true,
		Patches: []service.TaskPatch{{
			Key: key, IfVersion: &version, Column: column, Rank: rank,
		}},
	})
	if err != nil {
		apiError(rw, err)
		return
	}
	item := res.Items[0]
	if !item.OK {
		apiError(rw, item.Err)
		return
	}
	col := domain.Column{Name: item.Task.ColumnName, Kind: item.Task.ColumnKind}
	card := taskCardFrom(item.Task.ProjectKey, col, *item.Task)
	w.renderFragment(rw, r, http.StatusOK, "card", map[string]any{"Card": card, "CSRF": ""})
}

// handleFragmentAcceptance is "POST /fragments/acceptance": key, index,
// done ("true"|"false"). Ticking uses task_update's commutative
// AcceptanceCheck (no if_version needed); unticking replaces the whole list,
// which does require if_version, so it is read first the same way move is.
func (w *Web) handleFragmentAcceptance(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireAPIAuth(rw, r, domain.ScopeWrite)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, domain.MaxRequestBodyBytes)
	if err := w.verifyCSRF(r); err != nil {
		apiError(rw, err)
		return
	}
	key, err := domain.NormalizeTaskKey(r.PostFormValue("key"))
	if err != nil {
		apiError(rw, err)
		return
	}
	index, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("index")))
	if err != nil || index < 0 {
		apiError(rw, domain.Invalid("index", "index must be a non-negative integer", "Pass the zero-based acceptance item index."))
		return
	}
	done := strings.TrimSpace(r.PostFormValue("done")) == "true"

	actor := actorFor(tok)
	var patch service.TaskPatch
	if done {
		patch = service.TaskPatch{Key: key, AcceptanceCheck: []int{index}}
	} else {
		current, err := w.d.Service.TaskGet(r.Context(), actor, service.TaskGetInput{
			Keys: []string{key}, Include: service.Includes{service.IncludeAcceptance},
		})
		if err != nil {
			apiError(rw, err)
			return
		}
		if len(current.Tasks) == 0 {
			apiError(rw, domain.NotFound("task", key))
			return
		}
		t := current.Tasks[0]
		if index >= len(t.Acceptance) {
			apiError(rw, domain.Invalid("index", "index is out of range", "That acceptance item does not exist."))
			return
		}
		items := append([]domain.AcceptanceItem(nil), t.Acceptance...)
		items[index].Done = false
		version := t.Version
		patch = service.TaskPatch{Key: key, IfVersion: &version, Acceptance: items}
	}

	res, err := w.d.Service.TaskUpdate(r.Context(), actor, service.TaskUpdateInput{Atomic: true, Patches: []service.TaskPatch{patch}})
	if err != nil {
		apiError(rw, err)
		return
	}
	item := res.Items[0]
	if !item.OK {
		apiError(rw, item.Err)
		return
	}
	col := domain.Column{Name: item.Task.ColumnName, Kind: item.Task.ColumnKind}
	card := taskCardFrom(item.Task.ProjectKey, col, *item.Task)
	w.renderFragment(rw, r, http.StatusOK, "card", map[string]any{"Card": card, "CSRF": ""})
}

// handleFragmentNote is "POST /fragments/notes": key, body. Notes are
// append-only and never bump task.Version, so no if_version is needed.
func (w *Web) handleFragmentNote(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireAPIAuth(rw, r, domain.ScopeWrite)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, domain.MaxRequestBodyBytes)
	if err := w.verifyCSRF(r); err != nil {
		apiError(rw, err)
		return
	}
	key, err := domain.NormalizeTaskKey(r.PostFormValue("key"))
	if err != nil {
		apiError(rw, err)
		return
	}
	body := strings.TrimSpace(r.PostFormValue("body"))
	if body == "" {
		apiError(rw, domain.Invalid("body", "note body must not be empty", "Write something before submitting."))
		return
	}

	res, err := w.d.Service.TaskUpdate(r.Context(), actorFor(tok), service.TaskUpdateInput{
		Atomic:  true,
		Patches: []service.TaskPatch{{Key: key, Note: body}},
	})
	if err != nil {
		apiError(rw, err)
		return
	}
	item := res.Items[0]
	if !item.OK {
		apiError(rw, item.Err)
		return
	}
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write([]byte(`{"ok":true}`))
}

// handleFragmentClaim is "POST /fragments/claim": key, action
// ("claim"|"renew"|"release"), ttl_seconds (optional).
func (w *Web) handleFragmentClaim(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireAPIAuth(rw, r, domain.ScopeWrite)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, domain.MaxRequestBodyBytes)
	if err := w.verifyCSRF(r); err != nil {
		apiError(rw, err)
		return
	}
	key, err := domain.NormalizeTaskKey(r.PostFormValue("key"))
	if err != nil {
		apiError(rw, err)
		return
	}
	action := service.ClaimAction(strings.TrimSpace(r.PostFormValue("action")))
	switch action {
	case service.ClaimTake, service.ClaimRenew, service.ClaimRelease:
	default:
		apiError(rw, domain.Invalid("action", "action must be claim, renew or release", "Pass one of those three values."))
		return
	}
	ttl := 0
	if raw := strings.TrimSpace(r.PostFormValue("ttl_seconds")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			ttl = n
		}
	}

	res, err := w.d.Service.TaskClaim(r.Context(), actorFor(tok), service.TaskClaimInput{
		Key: key, Action: action, TTLSeconds: ttl,
	})
	if err != nil {
		apiError(rw, err)
		return
	}
	col := domain.Column{Name: res.Task.ColumnName, Kind: res.Task.ColumnKind}
	card := taskCardFrom(res.Task.ProjectKey, col, res.Task)
	w.renderFragment(rw, r, http.StatusOK, "card", map[string]any{"Card": card, "CSRF": ""})
}
