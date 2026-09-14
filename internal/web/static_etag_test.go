package web

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// KANB-40, review debt item 1: the "no-cache + ETag" fix (commit 1ccd35a) was
// guarded only by "Cache-Control is non-empty" — the exact regression that
// caused the owner to see yesterday's CSS for days (a revert to
// max-age=86400) stayed green. These tests pin the fix's actual contract.
// ---------------------------------------------------------------------------

// expectedStaticETag is the contract the handler implements: a strong ETag,
// quoted, base64 (raw url) of SHA-256 over exactly the bytes served.
func expectedStaticETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + base64.RawURLEncoding.EncodeToString(sum[:]) + `"`
}

func TestStatic_ETagIsTheHashOfTheServedBytes(t *testing.T) {
	w := newTestWeb(t)
	rw := do(w, "GET", "/static/app.css", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d", rw.Code)
	}
	if got, want := rw.Header().Get("Cache-Control"), "no-cache"; got != want {
		t.Fatalf("Cache-Control = %q, want exactly %q — anything longer lets a browser serve yesterday's CSS", got, want)
	}
	tag := rw.Header().Get("ETag")
	if tag == "" {
		t.Fatal("no ETag: a revalidating browser would re-download the whole file every time")
	}
	if want := expectedStaticETag(rw.Body.Bytes()); tag != want {
		t.Fatalf("ETag = %s, want the SHA-256 of the served bytes (%s)", tag, want)
	}
	if !strings.HasPrefix(tag, `"`) || !strings.HasSuffix(tag, `"`) {
		t.Errorf("ETag %s is not a quoted strong validator", tag)
	}
}

func TestStatic_IfNoneMatchAnswers304WithoutABody(t *testing.T) {
	w := newTestWeb(t)
	first := do(w, "GET", "/static/app.css", nil)
	tag := first.Header().Get("ETag")
	if tag == "" {
		t.Fatal("the first response carried no ETag, so there is nothing to revalidate against")
	}

	rw := do(w, "GET", "/static/app.css", map[string]string{"If-None-Match": tag})
	if rw.Code != http.StatusNotModified {
		t.Fatalf("a matching If-None-Match answered %d, want 304\n%s", rw.Code, rw.Body.String())
	}
	if rw.Body.Len() != 0 {
		t.Fatalf("the 304 carried %d bytes of body — the whole point of revalidation is not sending it", rw.Body.Len())
	}
	if got := rw.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("the 304 lost the Cache-Control contract: %q", got)
	}
}

func TestStatic_AStaleValidatorGetsTheFreshBody(t *testing.T) {
	w := newTestWeb(t)
	rw := do(w, "GET", "/static/app.css", map[string]string{"If-None-Match": `"a-validator-from-last-deploy"`})
	if rw.Code != http.StatusOK {
		t.Fatalf("a stale validator answered %d, want 200 with the fresh bytes", rw.Code)
	}
	if rw.Body.Len() == 0 {
		t.Fatal("a stale validator got an empty body")
	}
}
