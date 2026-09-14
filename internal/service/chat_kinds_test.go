package service

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ---------------------------------------------------------------------------
// KANB-46 — project_post: kind, recipient, reply_to, idempotency_key.
// ---------------------------------------------------------------------------

// seedSecondProject creates a second project so cross-project refusals have
// something real to point at.
func seedSecondProject(t *testing.T, env *testEnv) *domain.Project {
	t.Helper()
	p := &domain.Project{ID: "proj-other-id", Key: "OTHER", Name: "Other", EstimateUnit: "h",
		EnforceDependencies: true, ClaimTTLSeconds: 3600}
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		return env.Projects().Create(tx, p)
	}); err != nil {
		t.Fatalf("seed second project: %v", err)
	}
	return p
}

// postKind posts through ChatAdd and fails the test on any error.
func postKind(t *testing.T, env *testEnv, a Actor, in ChatAddInput) *domain.ChatMessage {
	t.Helper()
	m, err := env.svc.ChatAdd(context.Background(), a, in)
	if err != nil {
		t.Fatalf("ChatAdd(kind=%s recipient=%s): %v", in.Kind, in.Recipient, err)
	}
	return m
}

// TestChatAdd_KindDefaultsAndValues: all four kinds are accepted, empty
// means update, and anything else is refused by name (KANB-46 acceptance 1).
func TestChatAdd_KindDefaultsAndValues(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	// question/command without a recipient address the coordinator, so the
	// loop below needs one appointed.
	seedToken(t, env, "tok-coord", "coord", domain.Scopes{domain.ScopeWrite})
	setCoordinator(t, env, env.actor, "tok-coord")

	m := postKind(t, env, env.actor, ChatAddInput{ProjectKey: env.proj.Key, Author: "a", Body: "no kind given"})
	if m.Kind != domain.MessageUpdate {
		t.Fatalf("absent kind = %q, want the update default", m.Kind)
	}
	for _, k := range domain.AllMessageKinds {
		m := postKind(t, env, env.actor, ChatAddInput{ProjectKey: env.proj.Key, Author: "a", Body: "body", Kind: string(k)})
		if m.Kind != k {
			t.Fatalf("kind %q stored as %q", k, m.Kind)
		}
	}
	_, err := env.svc.ChatAdd(ctx, env.actor, ChatAddInput{ProjectKey: env.proj.Key, Author: "a", Body: "b", Kind: "directive"})
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeValidation || de.Field != "kind" {
		t.Fatalf("unknown kind: got %v, want a validation error on kind", err)
	}
}

// TestChatAdd_ResolvedExecutorFixedAtSend: the executor of a question or
// command is read from the project configuration AT SEND TIME and frozen; a
// coordinator change afterwards never readdresses what was already sent
// (KANB-46 acceptance 2).
func TestChatAdd_ResolvedExecutorFixedAtSend(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	seedToken(t, env, "tok-coord-1", "coordinator-one", domain.Scopes{domain.ScopeWrite})
	seedToken(t, env, "tok-coord-2", "coordinator-two", domain.Scopes{domain.ScopeWrite})
	seedToken(t, env, "tok-agent", "agent", domain.Scopes{domain.ScopeWrite}, env.proj.Key)
	setCoordinator(t, env, env.actor, "tok-coord-1")

	// Absent recipient: the coordinator at send time.
	cmd := postKind(t, env, env.actor, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "lead", Body: "do the thing", Kind: "command",
	})
	if cmd.ResolvedExecutor != "tok-coord-1" {
		t.Fatalf("command without recipient resolved to %q, want the coordinator tok-coord-1", cmd.ResolvedExecutor)
	}
	// Explicit recipient wins over the coordinator.
	direct := postKind(t, env, env.actor, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "lead", Body: "you specifically", Kind: "command", Recipient: "tok-agent",
	})
	if direct.ResolvedExecutor != "tok-agent" {
		t.Fatalf("explicit recipient command resolved to %q, want tok-agent", direct.ResolvedExecutor)
	}

	// Change the coordinator AFTER the sends: the old commands keep their
	// frozen executor; only new ones address the new coordinator. The
	// assertions look messages up by id so they hold whatever the feed
	// position of each post is.
	setCoordinator(t, env, env.actor, "tok-coord-2")
	fresh, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("ChatFeed: %v", err)
	}
	byID := map[string]string{}
	for _, fm := range fresh.Messages {
		byID[fm.Message.ID] = fm.Message.ResolvedExecutor
	}
	if len(byID) != 2 {
		t.Fatalf("feed held %d messages, want 2", len(byID))
	}
	if byID[cmd.ID] != "tok-coord-1" {
		t.Fatalf("old command's executor = %q after coordinator change, want the frozen tok-coord-1", byID[cmd.ID])
	}
	if byID[direct.ID] != "tok-agent" {
		t.Fatalf("direct command's executor = %q, want the frozen tok-agent", byID[direct.ID])
	}
	newCmd := postKind(t, env, env.actor, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "lead", Body: "after the change", Kind: "command",
	})
	if newCmd.ResolvedExecutor != "tok-coord-2" {
		t.Fatalf("post-change command resolved to %q, want the new coordinator tok-coord-2", newCmd.ResolvedExecutor)
	}
}

// TestChatAdd_QuestionAllHasNoExecutor: a question may address the whole
// room and then deliberately has NO single executor; a command may not
// address "all" at all — nobody could accept it.
func TestChatAdd_QuestionAllHasNoExecutor(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	q := postKind(t, env, env.actor, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "a", Body: "thoughts?", Kind: "question", Recipient: RecipientAll,
	})
	if q.ResolvedExecutor != "" {
		t.Fatalf("question to all resolved an executor %q — \"all\" must not assign one", q.ResolvedExecutor)
	}

	_, err := env.svc.ChatAdd(ctx, env.actor, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "a", Body: "someone do this", Kind: "command", Recipient: RecipientAll,
	})
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeValidation || de.Field != "recipient" {
		t.Fatalf("command to all: got %v, want a validation error on recipient", err)
	}
}

// TestChatAdd_NoCoordinatorRefusesDefaultAddressing: a question or command
// with no recipient and no appointed coordinator is refused, not sent as
// executable work into the void.
func TestChatAdd_NoCoordinatorRefusesDefaultAddressing(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	for _, kind := range []string{"question", "command"} {
		_, err := env.svc.ChatAdd(ctx, env.actor, ChatAddInput{
			ProjectKey: env.proj.Key, Author: "a", Body: "anyone?", Kind: kind,
		})
		de := domain.AsError(err)
		if de == nil || de.Code != domain.CodeValidation || de.Field != "recipient" ||
			!strings.Contains(de.Message, "no coordinator") {
			t.Fatalf("%s without recipient and coordinator: got %v, want a recipient validation error naming the missing coordinator", kind, err)
		}
	}

	// With a coordinator appointed the same call goes through; updates never
	// needed addressing in the first place.
	if _, err := env.svc.ChatAdd(ctx, env.actor, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "a", Body: "plain update", Kind: "update",
	}); err != nil {
		t.Fatalf("update without coordinator: %v", err)
	}
	seedToken(t, env, "tok-coord", "coord", domain.Scopes{domain.ScopeWrite})
	setCoordinator(t, env, env.actor, "tok-coord")
	postKind(t, env, env.actor, ChatAddInput{ProjectKey: env.proj.Key, Author: "a", Body: "now answerable", Kind: "question"})
}

// TestChatAdd_RecipientValidation: addressing is checked against the
// project's configuration — unknown, revoked and out-of-project tokens are
// refused with the field named.
func TestChatAdd_RecipientValidation(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	seedToken(t, env, "tok-agent", "agent", domain.Scopes{domain.ScopeWrite}, env.proj.Key)
	seedToken(t, env, "tok-outsider", "outsider", domain.Scopes{domain.ScopeWrite}, "OTHER")
	seedToken(t, env, "tok-dead", "dead", domain.Scopes{domain.ScopeWrite})
	revokeToken(t, env, "dead")

	cases := []struct {
		name      string
		recipient string
	}{
		{"unknown token", "tok-missing"},
		{"revoked token", "tok-dead"},
		{"token without project access", "tok-outsider"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.svc.ChatAdd(ctx, env.actor, ChatAddInput{
				ProjectKey: env.proj.Key, Author: "a", Body: "b", Kind: "command", Recipient: tc.recipient,
			})
			de := domain.AsError(err)
			if de == nil || de.Code != domain.CodeValidation || de.Field != "recipient" {
				t.Fatalf("recipient %q: got %v, want a validation error on recipient", tc.recipient, err)
			}
		})
	}

	// A participant of THIS project passes — and an update may carry a
	// recipient too (informational), without resolving an executor.
	m := postKind(t, env, env.actor, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "a", Body: "for the room's attention", Kind: "update", Recipient: "tok-agent",
	})
	if m.ResolvedExecutor != "" {
		t.Fatalf("update resolved an executor %q — updates are never assignments", m.ResolvedExecutor)
	}
}

// TestChatAdd_AuthorIsSignatureOnly: `author` is a display label a caller
// may set freely, but attribution never follows it — the authorized source
// stays the posting token (KANB-46 acceptance 5).
func TestChatAdd_AuthorIsSignatureOnly(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	seedToken(t, env, "tok-imp", "imp", domain.Scopes{domain.ScopeWrite})
	seedToken(t, env, "tok-alice", "alice", domain.Scopes{domain.ScopeWrite}, env.proj.Key)
	impersonator := Actor{TokenID: "tok-imp", Name: "imp", Scopes: domain.Scopes{domain.ScopeWrite}}

	// Posting under someone else's display name does not change who the
	// server records as the source.
	m, err := env.svc.ChatAdd(ctx, impersonator, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "alice", Body: "signed alice, sent by imp", Kind: "update",
	})
	if err != nil {
		t.Fatalf("ChatAdd: %v", err)
	}
	if m.Author != "alice" || m.AuthorTokenID != "tok-imp" {
		t.Fatalf("message author=%q author_token_id=%q — the signature may say alice, the source must stay tok-imp", m.Author, m.AuthorTokenID)
	}

	// And a command resolved to the real alice cannot be accepted by the
	// impersonator's token: acceptance compares tokens.ids, never names
	// (exercised end-to-end in KANB-47's tests; here the recorded identity
	// is what those checks will read).
	cmd := postKind(t, env, env.actor, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "lead", Body: "alice only", Kind: "command", Recipient: "tok-alice",
	})
	if cmd.ResolvedExecutor != "tok-alice" || cmd.AuthorTokenID != env.actor.TokenID {
		t.Fatalf("command attribution = executor %q source %q, want executor tok-alice source %s",
			cmd.ResolvedExecutor, cmd.AuthorTokenID, env.actor.TokenID)
	}
}

// TestChatAdd_ReplyToValidation: reply_to must name a message of the same
// project — a reply into another project is refused (KANB-46 acceptance 6).
func TestChatAdd_ReplyToValidation(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	other := seedSecondProject(t, env)

	own := postKind(t, env, env.actor, ChatAddInput{ProjectKey: env.proj.Key, Author: "a", Body: "original"})
	foreign := &domain.ChatMessage{ID: "msg-other-project", ProjectID: other.ID, Author: "x", Body: "elsewhere"}
	if err := env.Write(ctx, func(tx store.Tx) error {
		return env.Chat().Add(tx, foreign)
	}); err != nil {
		t.Fatalf("seed foreign message: %v", err)
	}

	ok := postKind(t, env, env.actor, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "a", Body: "an answer", Kind: "update", ReplyTo: own.ID,
	})
	if ok.ReplyToID != own.ID {
		t.Fatalf("reply_to stored as %q, want %q", ok.ReplyToID, own.ID)
	}

	cases := []struct {
		name    string
		replyTo string
		substr  string
	}{
		{"reply into another project", foreign.ID, "different project"},
		{"reply to a missing message", "no-such-message", "does not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.svc.ChatAdd(ctx, env.actor, ChatAddInput{
				ProjectKey: env.proj.Key, Author: "a", Body: "b", ReplyTo: tc.replyTo,
			})
			de := domain.AsError(err)
			if de == nil || de.Code != domain.CodeValidation || de.Field != "reply_to" ||
				!strings.Contains(de.Message, tc.substr) {
				t.Fatalf("reply_to %q: got %v, want a reply_to validation error mentioning %q", tc.replyTo, err, tc.substr)
			}
		})
	}
}

// TestChatAdd_IdempotencyKey: the same key of the same authorized sender
// with the same content returns the existing message; with different content
// it is an explicit error and never a second row; another sender's same key
// is an independent send (KANB-46 acceptance 3, 4).
func TestChatAdd_IdempotencyKey(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	seedToken(t, env, "tok-other", "other-sender", domain.Scopes{domain.ScopeWrite})
	other := Actor{TokenID: "tok-other", Name: "other-sender", Scopes: domain.Scopes{domain.ScopeWrite}}

	base := ChatAddInput{
		ProjectKey: env.proj.Key, Author: "a", Body: "one true send", Kind: "update", IdempotencyKey: "send-1",
	}
	first := postKind(t, env, env.actor, base)
	if first.IdempotencyKey != "send-1" {
		t.Fatalf("key not stored on the message: %+v", first.IdempotencyKey)
	}

	// Same sender, same key, same content: the same message, not a new one.
	again := postKind(t, env, env.actor, base)
	if again.ID != first.ID {
		t.Fatalf("replayed send produced a new message %s, want the original %s", again.ID, first.ID)
	}

	// Same key, different body: an explicit mismatch error.
	changed := base
	changed.Body = "different content"
	_, err := env.svc.ChatAdd(ctx, env.actor, changed)
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeIdempotencyMismatch {
		t.Fatalf("same key different content: got %v, want idempotency_mismatch", err)
	}

	// Another sender may independently use the same key string.
	theirs := postKind(t, env, other, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "b", Body: "their own send", IdempotencyKey: "send-1",
	})
	if theirs.ID == first.ID {
		t.Fatal("a different sender's same key returned the first sender's message — keys must be per authorized sender")
	}

	// Exactly two messages exist: the mismatch created nothing.
	res, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("ChatFeed: %v", err)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("feed held %d messages, want 2 (the mismatch and the replay created none)", len(res.Messages))
	}
}

// TestChatAdd_LegacyCallUnchanged: a call with only the original three
// parameters behaves exactly as before the protocol existed (KANB-46
// acceptance 7).
func TestChatAdd_LegacyCallUnchanged(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	m := postKind(t, env, env.actor, ChatAddInput{ProjectKey: env.proj.Key, Author: "old-style", Body: "as before"})
	if m.Kind != domain.MessageUpdate || m.Recipient != "" || m.ResolvedExecutor != "" ||
		m.ReplyToID != "" || m.IdempotencyKey != "" {
		t.Fatalf("legacy call produced a protocol-shaped message: %+v", m)
	}
	if m.AuthorTokenID != env.actor.TokenID {
		t.Fatalf("authorized source = %q, want the posting token %q", m.AuthorTokenID, env.actor.TokenID)
	}

	// The panel read (newest first, Before cursor) is untouched.
	page, err := env.svc.ChatList(ctx, env.actor, ChatListInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("ChatList: %v", err)
	}
	if len(page.Messages) != 1 || page.Messages[0].ID != m.ID {
		t.Fatalf("ChatList = %+v, want the one legacy message", page.Messages)
	}
}

// TestChatAdd_IdempotencyKeyIsPerProject: one token reusing one key across
// two projects must not be told "ok, here is your message" with the FIRST
// project's message id — that silently drops the second publication (its
// feed stays empty) while the response names a project the message is not
// in. The destination is part of the content, so a project mismatch is an
// idempotency_mismatch, same as a body difference. Inside ONE project the
// key keeps deduplicating as before.
func TestChatAdd_IdempotencyKeyIsPerProject(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	other := seedSecondProject(t, env)

	first, err := env.svc.ChatAdd(ctx, env.actor, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "a", Body: "same content", Kind: "update", IdempotencyKey: "K-X",
	})
	if err != nil {
		t.Fatalf("first post: %v", err)
	}

	_, err = env.svc.ChatAdd(ctx, env.actor, ChatAddInput{
		ProjectKey: other.Key, Author: "a", Body: "same content", Kind: "update", IdempotencyKey: "K-X",
	})
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeIdempotencyMismatch {
		t.Fatalf("same key in another project: got %v, want idempotency_mismatch", err)
	}
	if !strings.Contains(de.Message, env.proj.Key) {
		t.Fatalf("conflict message %q does not name the project where the key is already taken", de.Message)
	}

	// The refusal created nothing: the second project's feed is empty, and
	// exactly one message exists in the first.
	otherFeed, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: other.Key})
	if err != nil {
		t.Fatalf("other feed: %v", err)
	}
	if len(otherFeed.Messages) != 0 {
		t.Fatalf("the refused post leaked %d messages into %s", len(otherFeed.Messages), other.Key)
	}
	firstFeed, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("first feed: %v", err)
	}
	if len(firstFeed.Messages) != 1 || firstFeed.Messages[0].Message.ID != first.ID {
		t.Fatalf("first project feed = %+v, want exactly the original message", firstFeed.Messages)
	}

	// Within the original project the key still deduplicates: same content
	// returns the same message, and the response's project is where the
	// message actually lives.
	again, err := env.svc.ChatAdd(ctx, env.actor, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "a", Body: "same content", Kind: "update", IdempotencyKey: "K-X",
	})
	if err != nil {
		t.Fatalf("in-project replay: %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("in-project replay returned %s, want the original %s", again.ID, first.ID)
	}
	if again.ProjectID != env.proj.ID {
		t.Fatalf("replayed message project = %s, want the requested project %s", again.ProjectID, env.proj.ID)
	}
}

// TestChatAdd_BlankOrOversizedKeyRefused: a key of only whitespace would
// trim to "" and silently disable the deduplication the caller believes it
// enabled — refused loudly instead, like every other invalid key, and
// nothing is stored. Keys are also capped: a retry handle is a lookup
// identity, not content.
func TestChatAdd_BlankOrOversizedKeyRefused(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	_, err := env.svc.ChatAdd(ctx, env.actor, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "a", Body: "guard me", Kind: "update", IdempotencyKey: "   ",
	})
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeValidation || de.Field != "idempotency_key" {
		t.Fatalf("whitespace key: got %v, want a validation error on idempotency_key", err)
	}
	if !strings.Contains(de.Message, "empty after trimming") {
		t.Fatalf("whitespace key error = %q, want the trimming explanation", de.Message)
	}
	feed, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	if len(feed.Messages) != 0 {
		t.Fatalf("the refused key still stored %d message(s)", len(feed.Messages))
	}

	tooLong := strings.Repeat("k", domain.MaxChatIdempotencyKeyLen+1)
	_, err = env.svc.ChatAdd(ctx, env.actor, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "a", Body: "guard me", Kind: "update", IdempotencyKey: tooLong,
	})
	de = domain.AsError(err)
	if de == nil || de.Code != domain.CodeValidation || de.Field != "idempotency_key" {
		t.Fatalf("oversized key: got %v, want a validation error on idempotency_key", err)
	}
	if !strings.Contains(de.Message, fmt.Sprintf("%d", domain.MaxChatIdempotencyKeyLen)) {
		t.Fatalf("oversized key error = %q, want it to name the limit", de.Message)
	}

	// Exactly at the limit is accepted, surrounding whitespace is trimmed
	// BEFORE storage, and the trimmed form is what deduplicates.
	atLimit := strings.Repeat("k", domain.MaxChatIdempotencyKeyLen)
	m, err := env.svc.ChatAdd(ctx, env.actor, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "a", Body: "guard me", Kind: "update", IdempotencyKey: atLimit,
	})
	if err != nil {
		t.Fatalf("key at the limit refused: %v", err)
	}
	padded, err := env.svc.ChatAdd(ctx, env.actor, ChatAddInput{
		ProjectKey: env.proj.Key, Author: "a", Body: "guard me", Kind: "update", IdempotencyKey: "  " + atLimit + " ",
	})
	if err != nil {
		t.Fatalf("padded variant: %v", err)
	}
	if m.IdempotencyKey != atLimit || padded.ID != m.ID {
		t.Fatalf("trim/dedup mismatch: stored %q, padded variant returned %s (original %s)",
			m.IdempotencyKey, padded.ID, m.ID)
	}
}
