package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// A signature is free text, so the feed shows the posting key next to one
// that is not it — "owner · via opus-x3" — and a signature that IS the key's
// name, or an old row with no key, exactly as before.
func TestChatFeed_ShowsPostingKeyNextToADifferentSignature(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	svc := &chatOlderStubService{
		result: &service.ChatListResult{
			Messages: []domain.ChatMessage{
				{ID: "m4", Author: "opus-x9", AuthorTokenID: "tok-x9", Body: "re: approval", ReplyToID: "m3", CreatedAt: now.Add(-time.Minute)},
				{ID: "m3", Author: "owner", AuthorTokenID: "tok-x3", Body: "approve the release", CreatedAt: now.Add(-2 * time.Minute)},
				{ID: "m2", Author: "opus-x3", AuthorTokenID: "tok-x3", Body: "signed as itself", CreatedAt: now.Add(-3 * time.Minute)},
				{ID: "m1", Author: "legacy-bot", Body: "from before tokens were recorded", CreatedAt: now.Add(-4 * time.Minute)},
			},
			Meta: map[string]service.ChatListEntryMeta{
				"m4": {Parent: &domain.ChatMessage{ID: "m3", Author: "owner", Body: "approve the release"}, ParentAuthorVia: "opus-x3"},
				"m3": {AuthorVia: "opus-x3"},
				"m2": {},
			},
		},
	}
	w, sess := newChatOlderTestWeb(t, svc)
	rw := do(w, "GET", "/p/BMB/chat/older?before=2026-09-26T12%3A00%3A00Z%2Fm5", nil, sessionCookie(sess))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", rw.Code, rw.Body.String())
	}
	entries := map[string]string{}
	for _, part := range strings.Split(rw.Body.String(), "<li ")[1:] {
		for _, id := range []string{"m1", "m2", "m3", "m4"} {
			if strings.Contains(part, `data-chat-id="`+id+`"`) {
				entries[id] = part
			}
		}
	}
	if len(entries) != 4 {
		t.Fatalf("rendered %d entries, want 4:\n%s", len(entries), rw.Body.String())
	}
	if !strings.Contains(entries["m3"], ">owner</span>") || !strings.Contains(entries["m3"], "&#183; via opus-x3</span>") {
		t.Errorf("message signed owner by opus-x3 does not show both names:\n%s", entries["m3"])
	}
	for _, id := range []string{"m2", "m1"} {
		if strings.Contains(entries[id], "via ") {
			t.Errorf("entry %s shows a via name it must not:\n%s", id, entries[id])
		}
	}
	if !strings.Contains(entries["m4"], "owner &#183; via opus-x3:") {
		t.Errorf("reply quote of the owner-signed message hides the posting key:\n%s", entries["m4"])
	}
	if strings.Contains(strings.SplitN(entries["m4"], "data-chat-quote", 2)[0], "via ") {
		t.Errorf("reply signed with its own key name shows a via name:\n%s", entries["m4"])
	}
}
