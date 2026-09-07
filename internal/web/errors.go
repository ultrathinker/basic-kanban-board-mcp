package web

import (
	"encoding/json"
	"errors"
	"html"
	"net/http"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ErrServiceUnavailable is returned by internal/app's placeholder
// service.Service for every method call. It is declared here, not in
// internal/app, so this package can
// recognize it and render a clear 503 without importing internal/app (which
// would create an import cycle: app already imports web to build the
// handler tree).
var ErrServiceUnavailable = errors.New("web: the task engine is not wired into this build yet")

// httpStatusForCode maps a domain.Code to the HTTP status every surface
// (fragments, pages) reports it as. Centralized so the mapping cannot drift
// between handlers (AGENTS.md house style: "map it once, centrally").
func httpStatusForCode(c domain.Code) int {
	switch c {
	case domain.CodeNotFound:
		return http.StatusNotFound
	case domain.CodeValidation:
		return http.StatusBadRequest
	case domain.CodeConflict, domain.CodeBlocked, domain.CodeWIPExceeded, domain.CodeClaimed, domain.CodeIdempotencyMismatch, domain.CodeCycle:
		return http.StatusConflict
	case domain.CodeForbidden:
		return http.StatusForbidden
	case domain.CodeRateLimited:
		return http.StatusTooManyRequests
	case domain.CodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	default:
		return http.StatusInternalServerError
	}
}

// errInfo is what both apiError and pageError render; message/remediation
// are never dropped, per AGENTS.md ("never lose the remediation text").
type errInfo struct {
	status      int
	code        string
	message     string
	remediation string
	current     any
}

func mapError(err error) errInfo {
	if err == nil {
		return errInfo{status: http.StatusOK}
	}
	if errors.Is(err, ErrServiceUnavailable) {
		return errInfo{
			status:  http.StatusServiceUnavailable,
			code:    "service_unavailable",
			message: "the task engine is not wired into this build yet",
			remediation: "internal/service has no implementation in this build. Health, login, " +
				"static assets, agent-setup and token administration all work; board and task " +
				"operations will work once the service layer lands.",
		}
	}
	if de := domain.AsError(err); de != nil {
		return errInfo{
			status:      httpStatusForCode(de.Code),
			code:        string(de.Code),
			message:     de.Message,
			remediation: de.Remediation,
			current:     de.Current,
		}
	}
	return errInfo{
		status:      http.StatusInternalServerError,
		code:        "internal",
		message:     "internal error",
		remediation: "Try again; if this persists, check the server logs.",
	}
}

// apiError writes the JSON envelope PLAN §6 defines for tool errors, reused
// here so fragment/API responses look the same shape as MCP results.
func apiError(rw http.ResponseWriter, err error) {
	info := mapError(err)
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(info.status)
	_ = json.NewEncoder(rw).Encode(map[string]any{
		"ok": false,
		"error": map[string]any{
			"code":        info.code,
			"message":     info.message,
			"remediation": info.remediation,
			"current":     info.current,
		},
	})
}

// pageError renders a minimal, dependency-free HTML error page. It does not
// go through the templates engine: an error that fires because rendering
// itself failed must not depend on rendering succeeding.
func (w *Web) pageError(rw http.ResponseWriter, r *http.Request, err error) {
	info := mapError(err)
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(info.status)
	title := http.StatusText(info.status)
	if title == "" {
		title = "Error"
	}
	_, _ = rw.Write([]byte("<!doctype html><meta charset=\"utf-8\"><title>" +
		html.EscapeString(title) + "</title>" +
		"<main style=\"font-family:sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem\">" +
		"<h1>" + html.EscapeString(title) + "</h1>" +
		"<p>" + html.EscapeString(info.message) + "</p>" +
		"<p>" + html.EscapeString(info.remediation) + "</p>" +
		"<p><a href=\"/\">Back to overview</a></p>" +
		"</main>"))
}
