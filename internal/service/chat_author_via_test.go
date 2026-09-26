package service

import (
	"context"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

func TestAuthorVia(t *testing.T) {
	names := map[string]string{"tok-x3": "opus-x3"}
	cases := []struct {
		name string
		msg  domain.ChatMessage
		want string
	}{
		{"signature differs", domain.ChatMessage{Author: "owner", AuthorTokenID: "tok-x3"}, "opus-x3"},
		{"signature is the token name", domain.ChatMessage{Author: "opus-x3", AuthorTokenID: "tok-x3"}, ""},
		{"differs only by case and space", domain.ChatMessage{Author: " Opus-X3 ", AuthorTokenID: "tok-x3"}, ""},
		{"row predates token attribution", domain.ChatMessage{Author: "owner"}, ""},
		{"token no longer resolves", domain.ChatMessage{Author: "owner", AuthorTokenID: "tok-gone"}, ""},
	}
	for _, c := range cases {
		if got := AuthorVia(c.msg, names); got != c.want {
			t.Errorf("%s: AuthorVia = %q, want %q", c.name, got, c.want)
		}
	}
}

// Both reads a surface renders the feed from carry the posting token's name
// next to a signature that is not it, and nothing next to one that is.
func TestChatReads_CarryAuthorViaWhenSignatureDiffers(t *testing.T) {
	e := openExecEnv(t)
	ctx := context.Background()
	post := func(author string) string {
		t.Helper()
		m, err := e.svc.ChatAdd(ctx, e.exec, ChatAddInput{ProjectKey: "BMB", Author: author, Body: "approve the release"})
		if err != nil {
			t.Fatalf("post as %q: %v", author, err)
		}
		return m.ID
	}
	signedAdmin := post("admin")
	signedSelf := post("exec-1")

	list, err := e.svc.ChatList(ctx, e.actor, ChatListInput{ProjectKey: "BMB"})
	if err != nil {
		t.Fatalf("ChatList: %v", err)
	}
	if got := list.Meta[signedAdmin].AuthorVia; got != "exec-1" {
		t.Errorf("ChatList: message signed admin by exec-1 has AuthorVia %q, want exec-1", got)
	}
	if got := list.Meta[signedSelf].AuthorVia; got != "" {
		t.Errorf("ChatList: message signed with its own token name has AuthorVia %q, want empty", got)
	}

	reply, err := e.svc.ChatAdd(ctx, e.actor, ChatAddInput{ProjectKey: "BMB", Author: "test-admin", Body: "who is this?", ReplyTo: signedAdmin})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	list, err = e.svc.ChatList(ctx, e.actor, ChatListInput{ProjectKey: "BMB"})
	if err != nil {
		t.Fatalf("ChatList: %v", err)
	}
	if got := list.Meta[reply.ID].ParentAuthorVia; got != "exec-1" {
		t.Errorf("reply quoting the admin-signed message has ParentAuthorVia %q, want exec-1", got)
	}

	feed, err := e.svc.ChatFeed(ctx, e.actor, ChatFeedInput{ProjectKey: "BMB"})
	if err != nil {
		t.Fatalf("ChatFeed: %v", err)
	}
	via := map[string]string{}
	for _, fm := range feed.Messages {
		via[fm.Message.ID] = fm.AuthorVia
	}
	if via[signedAdmin] != "exec-1" || via[signedSelf] != "" {
		t.Errorf("ChatFeed AuthorVia = %v, want exec-1 for %s and empty for %s", via, signedAdmin, signedSelf)
	}
}
