package service

import (
	"context"
	"testing"
)

// ---------------------------------------------------------------------------
// KANB-48 — ChatList resolves the display data the web panel renders:
// who a recipient is, what a reply quotes, and whether a command was
// accepted. The feed read (ChatFeed) already carried this; the panel reads
// newest-first through ChatList, and without it no UI could show the
// acceptance mark the card requires.
// ---------------------------------------------------------------------------

// TestChatList_MetaResolvesRecipientReplyAndAcceptance walks one page through
// the whole protocol: a plain update, a command with an explicit recipient,
// a reply to that command, and the acceptance that turns the command into
// tasks. The meta must show each of the states the UI distinguishes —
// plain, addressed, accepted — and must never show an acceptance where
// there is none.
func TestChatList_MetaResolvesRecipientReplyAndAcceptance(t *testing.T) {
	env := openAcceptanceEnv(t)
	ctx := context.Background()

	if _, err := env.svc.ChatAdd(ctx, env.executor, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "executor", Body: "plain note to the room",
	}); err != nil {
		t.Fatalf("post update: %v", err)
	}
	reply, err := env.svc.ChatAdd(ctx, env.executor, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "executor", Body: "on it",
		ReplyTo: env.command.ID,
	})
	if err != nil {
		t.Fatalf("post reply: %v", err)
	}

	// Before the acceptance: the command's meta has names and the reply has
	// its parent, but the command has NO acceptance — received is not
	// accepted.
	before, err := env.svc.ChatList(ctx, env.actor, ChatListInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("ChatList: %v", err)
	}
	cmdMeta, ok := before.Meta[env.command.ID]
	if !ok {
		t.Fatalf("command %s missing from meta: %v", env.command.ID, before.Meta)
	}
	if cmdMeta.RecipientName != "executor" {
		t.Errorf("command recipient name = %q, want executor", cmdMeta.RecipientName)
	}
	if cmdMeta.ExecutorName != "executor" {
		t.Errorf("command executor name = %q, want executor", cmdMeta.ExecutorName)
	}
	if cmdMeta.Acceptance != nil {
		t.Errorf("unaccepted command carries an acceptance: %+v", cmdMeta.Acceptance)
	}
	replyMeta, ok := before.Meta[reply.ID]
	if !ok {
		t.Fatalf("reply %s missing from meta", reply.ID)
	}
	if replyMeta.Parent == nil || replyMeta.Parent.ID != env.command.ID {
		t.Errorf("reply parent = %+v, want the command", replyMeta.Parent)
	}
	if replyMeta.Parent != nil && replyMeta.Parent.Body != env.command.Body {
		t.Errorf("reply parent body = %q, want the command's own body", replyMeta.Parent.Body)
	}
	// The plain update addresses nobody and accepts nothing: its meta is
	// the zero value. Find it by body — the page is newest first, so
	// index arithmetic would couple the test to posting order.
	var plain ChatListEntryMeta
	found := false
	for _, m := range before.Messages {
		if m.Body == "plain note to the room" {
			plain = before.Meta[m.ID]
			found = true
		}
	}
	if !found {
		t.Fatal("the plain update is not on the page")
	}
	if plain.RecipientName != "" || plain.ExecutorName != "" || plain.Acceptance != nil || plain.Parent != nil {
		t.Errorf("plain update resolved display data it should not have: %+v", plain)
	}

	// Accept the command; the SAME read must now report the acceptance with
	// the task keys, so the panel can turn it into the mark with links.
	res, err := accept(t, env, env.executor, acceptBatch())
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	after, err := env.svc.ChatList(ctx, env.actor, ChatListInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("ChatList after: %v", err)
	}
	acc := after.Meta[env.command.ID].Acceptance
	if acc == nil {
		t.Fatal("accepted command still has no acceptance in meta")
	}
	if len(acc.TaskKeys) != len(res.Tasks) {
		t.Errorf("acceptance task keys = %v, want the batch's %d keys", acc.TaskKeys, len(res.Tasks))
	}
}

// TestChatList_MetaCoversEveryListedMessage: whatever the page holds, the
// meta map has an entry per message — the UI looks entries up by id and a
// missing one would silently drop a mark the message is entitled to.
func TestChatList_MetaCoversEveryListedMessage(t *testing.T) {
	env := openAcceptanceEnv(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := env.svc.ChatAdd(ctx, env.coordinator, ChatAddInput{
			ProjectKey: env.proj.Key, Author: "lead", Body: "note",
		}); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
	}
	page, err := env.svc.ChatList(ctx, env.actor, ChatListInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("ChatList: %v", err)
	}
	if len(page.Meta) != len(page.Messages) {
		t.Fatalf("meta has %d entries for %d messages", len(page.Meta), len(page.Messages))
	}
	for _, m := range page.Messages {
		if _, ok := page.Meta[m.ID]; !ok {
			t.Errorf("message %s has no meta entry", m.ID)
		}
	}
}
