package web

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// KANB-14: the board's Show done / Hide done toggle used to live only in
// the "?hide_done=" query string, so a plain visit to /p/{key} always
// snapped back to the default. This file adds a per-browser, per-project
// cookie so the choice sticks without a redirect or a second round trip.
//
// The decision to use a cookie (not localStorage, not a database table) is
// the brief's, not this file's — see BRIEF.md. What lives here is only the
// encoding of a compact per-project set into one cookie, tolerant of
// garbage, plus the precedence rule: an explicit query parameter always
// wins and is written straight back to the cookie; with no parameter the
// cookie decides; with neither, the original default (done hidden) holds.

// doneCookieName holds the per-browser memory of which projects the owner
// has switched to "show done". A project's absence from the set means the
// unchanged default (done hidden) applies -- see resolveHideDone.
const doneCookieName = "kanban_done_shown"

// doneCookieMaxAge is deliberately long: the owner keeps this board open
// for hours/days and the whole point is that it stops forgetting. 400 days
// is the ceiling modern browsers honour for a cookie's lifetime, so this is
// as long-lived as the attribute can meaningfully be.
const doneCookieMaxAge = 400 * 24 * time.Hour

// maxDoneCookieBytes bounds how much of an incoming Cookie value we will
// ever split and validate. Browsers already cap a single cookie around 4KB,
// but a hand-crafted or corrupted header should not cost more than a fixed
// slice before parsing gives up on the rest.
const maxDoneCookieBytes = 4096

// parseDoneShownCookie decodes the cookie's wire format -- a comma-joined
// list of project keys -- into a set. It is deliberately forgiving: a
// cookie set by something else entirely, a value truncated mid-key, stray
// commas, blank fields, lower-case or otherwise malformed keys are all just
// dropped. A garbage cookie must degrade to "as if the cookie were absent",
// never to a broken page.
func parseDoneShownCookie(raw string) map[string]bool {
	out := map[string]bool{}
	if raw == "" {
		return out
	}
	if len(raw) > maxDoneCookieBytes {
		raw = raw[:maxDoneCookieBytes]
	}
	for _, part := range strings.Split(raw, ",") {
		key, err := domain.ValidateProjectKey(part)
		if err != nil {
			continue
		}
		out[key] = true
	}
	return out
}

// encodeDoneShownCookie renders the set back to the cookie's wire format: a
// sorted, comma-joined list of project keys. Sorting keeps the value (and
// therefore anything asserting on it) deterministic across runs.
func encodeDoneShownCookie(set map[string]bool) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// readDoneShown reads the caller's done-shown cookie. A missing cookie and
// a malformed one are indistinguishable on purpose: both come back as the
// empty set, which resolveHideDone turns into the unchanged default.
func (w *Web) readDoneShown(r *http.Request) map[string]bool {
	c, err := r.Cookie(doneCookieName)
	if err != nil {
		return map[string]bool{}
	}
	return parseDoneShownCookie(c.Value)
}

// writeDoneShown persists the set. HttpOnly (no JS ever needs to read this;
// the toggle links are plain "?hide_done=" anchors so the page keeps
// working with JavaScript off) and SameSite=Lax per the brief.
//
// Secure is not a second rule: it is the session cookie's own rule, reused.
// auth.DefaultCookiePolicy + CookiePolicy.Secure are the exact two exported
// building blocks internal/auth/sessions.go composes for the session
// cookie; the only piece that function call needs which is not already
// exported is the "is this deployment https by default" input
// (auth.Manager.cookieSecureBase, unexported). w.cookieSecureBase below
// recomputes that same two-line decision from Manager's own exported
// Insecure/BaseURL fields rather than duplicating a new policy.
func (w *Web) writeDoneShown(rw http.ResponseWriter, r *http.Request, set map[string]bool) {
	pol := auth.DefaultCookiePolicy(w.cookieSecureBase())
	c := &http.Cookie{
		Name:     doneCookieName,
		Value:    encodeDoneShownCookie(set),
		Path:     pol.Path,
		MaxAge:   int(doneCookieMaxAge / time.Second),
		HttpOnly: true,
		SameSite: pol.SameSite,
		Secure:   pol.Secure(r, w.d.Auth),
	}
	http.SetCookie(rw, c)
}

// cookieSecureBase mirrors auth.Manager.cookieSecureBase (internal/auth/sessions.go):
// BaseURL's https scheme forces Secure on, unless the deployment opted into
// --insecure-http (Manager.Insecure). It is copied rather than called
// because the source method is unexported outside internal/auth, but it
// reads only Manager's exported fields, so this is the same rule, not a
// second one.
func (w *Web) cookieSecureBase() bool {
	if w.d.Auth.Insecure {
		return false
	}
	return strings.HasPrefix(w.d.Auth.BaseURL, "https://")
}

// resolveHideDone applies the precedence rule for one board request: an
// explicit "?hide_done=" query parameter always wins and is written
// straight back to the per-project cookie so it becomes the new remembered
// value; with no parameter, the cookie's per-project set decides; with
// neither, the original default (done hidden) applies unchanged.
//
// The non-"0" branch covers both the toggle's own "?hide_done=1" link and
// any other stray value, matching the previous "!= 0 means hidden"
// behaviour exactly -- this file only adds the cookie, it does not change
// what the default is.
func (w *Web) resolveHideDone(rw http.ResponseWriter, r *http.Request, key string) bool {
	shown := w.readDoneShown(r)
	switch r.URL.Query().Get("hide_done") {
	case "":
		return !shown[key]
	case "0":
		shown[key] = true
		w.writeDoneShown(rw, r, shown)
		return false
	default:
		delete(shown, key)
		w.writeDoneShown(rw, r, shown)
		return true
	}
}
