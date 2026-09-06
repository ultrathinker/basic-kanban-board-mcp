package web

import "net/http"

// handleHealthz is a pure liveness probe: it never touches the database, so
// a slow disk or a busy writer never flips it to unhealthy. Docker's
// HEALTHCHECK and load balancers hit this one.
func (w *Web) handleHealthz(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write([]byte("ok"))
}

// handleReadyz is the readiness probe. It never leaks database detail to an
// anonymous caller (PLAN §9): the body is one of two fixed words regardless
// of who is asking; only the status code depends on the underlying check.
// The check itself (auth.Manager.Tokens.Count) is a cheap, already-open
// read through the token store — enough to prove the database connection is
// alive without importing internal/store into this package.
func (w *Web) handleReadyz(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if _, err := w.d.Auth.Tokens.Count(r.Context()); err != nil {
		rw.WriteHeader(http.StatusServiceUnavailable)
		_, _ = rw.Write([]byte("not ready"))
		return
	}
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write([]byte("ready"))
}
