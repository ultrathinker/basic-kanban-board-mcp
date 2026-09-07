package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// projectSearchLimit is the cap applied to the `limit` query parameter on
// GET /projects/search. Kept as a package-level constant so the cap is
// reviewable in one place, and so the README/prose about "never more than
// 20" has a single source of truth the test suite can pin.
const projectSearchLimit = 20

// handleProjectsSearch is "GET /projects/search?q=&limit=". It powers the
// project switcher combobox in the topbar.
//
// Authentication uses requireAPIAuth (not requireSessionPage) because this
// endpoint is JSON-shaped: on failure the client wants a non-2xx status,
// not a redirect to /login. It still accepts either a session cookie or a
// bearer token, exactly like the fragment endpoints it sits beside.
//
// The data path is identical to projectSummaries: BoardGet with ViewSummary
// re-applies the actor's project_keys, so the response is naturally scoped
// to the projects the caller may see. We never widen that — a token whose
// project_keys limits it to two projects will see two projects here, full
// stop.
//
// Filtering: `q` is trim+lowered and matched as a substring against either
// the project key or the project name (case-insensitive). An empty `q`
// returns the first ≤limit projects in board order. has_more reports
// whether more matches existed past the slice; clients render the
// "more results, narrow the query" hint based on it. Anything beyond has_more is not
// offered — the combobox is intentionally a "narrow then drill in" tool,
// not a paginated browse.
func (w *Web) handleProjectsSearch(rw http.ResponseWriter, r *http.Request) {
	tok, ok := w.requireAPIAuth(rw, r, domain.ScopeRead)
	if !ok {
		return
	}

	limit := projectSearchLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			apiError(rw, domain.Invalid("limit", "limit must be a positive integer", "Pass an integer between 1 and 20."))
			return
		}
		if n > projectSearchLimit {
			n = projectSearchLimit
		}
		limit = n
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))

	board, err := w.d.Service.BoardGet(r.Context(), actorFor(tok), service.BoardGetInput{View: service.ViewSummary})
	if err != nil {
		apiError(rw, err)
		return
	}

	// Collect in board.Projects order (the service's canonical order) so the
	// client renders rows in the same sequence as elsewhere on the page, and
	// stop as soon as one match past the page is seen: an installation with
	// thousands of matching projects must not build a slice of all of them
	// only to discard all but the first `limit`. The (limit+1)th match sets
	// has_more and ends the scan.
	items := make([]map[string]string, 0, limit)
	hasMore := false
	for _, p := range board.Projects {
		if q != "" {
			if !strings.Contains(strings.ToLower(p.Key), q) && !strings.Contains(strings.ToLower(p.Name), q) {
				continue
			}
		}
		if len(items) >= limit {
			hasMore = true
			break
		}
		items = append(items, map[string]string{"key": p.Key, "name": p.Name})
	}

	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(map[string]any{
		"items":    items,
		"has_more": hasMore,
	})
}
