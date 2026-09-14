package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ---------------------------------------------------------------------------
// KANB-45 — board_get(view:messages): cursor reading of the feed, forward
// from the beginning of history.
// ---------------------------------------------------------------------------

// postFeedMessage appends a message through the service so the rows carry
// everything a real send does.
func postFeedMessage(t *testing.T, env *testEnv, a Actor, body string) *domain.ChatMessage {
	t.Helper()
	m, err := env.svc.ChatAdd(context.Background(), a, ChatAddInput{ProjectKey: env.proj.Key, Author: a.Name, Body: body})
	if err != nil {
		t.Fatalf("ChatAdd(%q): %v", body, err)
	}
	return m
}

// seedFeedMessageAt inserts a row with an EXPLICIT timestamp, so tests can
// force identical created_at values across several messages — the boundary
// case the cursor's (created_at, id) tie-break exists for.
func seedFeedMessageAt(t *testing.T, env *testEnv, id string, ts time.Time, body string) {
	t.Helper()
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		return env.Chat().Add(tx, &domain.ChatMessage{
			ID: id, ProjectID: env.proj.ID, Author: env.actor.Name, Body: body, CreatedAt: ts,
		})
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// feedBodies extracts the ordered bodies of a feed page.
func feedBodies(msgs []ChatFeedMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Message.Body)
	}
	return out
}

// TestChatFeed_StartsAtBeginning: with no `after`, the first page is the
// OLDEST messages in chronological order — not the newest ones, not "from
// now". A consumer that launched after a command was written must still see
// that command on its first read (KANB-45 acceptance 1).
//
// Timestamps are distinct and explicit so the assertion is about CHRONOLOGY,
// not about the (created_at, id) tie-break, which the dedicated tie test
// below covers. One message goes through the service's own ChatAdd path to
// prove real posts are readable the same way.
func TestChatFeed_StartsAtBeginning(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)
	seedFeedMessageAt(t, env, "m-old-1", base, "written before the consumer started")
	seedFeedMessageAt(t, env, "m-old-2", base.Add(time.Second), "second")
	posted := postFeedMessage(t, env, env.actor, "posted through the service")
	seedFeedMessageAt(t, env, "m-old-3", base.Add(2*time.Second), "third")

	res, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("ChatFeed: %v", err)
	}
	got := feedBodies(res.Messages)
	// The service-posted message carries the real clock, so it is the NEWEST
	// of the four regardless of insertion order.
	want := []string{"written before the consumer started", "second", "third", "posted through the service"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("feed = %v, want oldest-first %v", got, want)
	}
	if res.Messages[0].Message.ID != "m-old-1" {
		t.Fatalf("first message id = %s, want the oldest message m-old-1", res.Messages[0].Message.ID)
	}
	if posted.ID == "" || posted.Kind != domain.MessageUpdate {
		t.Fatalf("service-posted message = %+v, want a real id and the default update kind", posted)
	}
	if res.Messages[len(res.Messages)-1].Message.ID != posted.ID {
		t.Fatal("the message posted through the service is not the newest feed entry")
	}
	if res.HasMore {
		t.Fatal("full feed reported has_more, want false — everything fit in one page")
	}
	// The cursor is present even on the final page: a consumer polls with it
	// to pick up messages that arrive later, so "no more right now" must not
	// mean "nowhere to continue from".
	if res.NextCursor == "" {
		t.Fatal("final page carried no next_cursor — a consumer could not resume polling for new arrivals")
	}
}

// TestChatFeed_IdenticalTimestampsNoLossNoDuplicates: many messages share
// one created_at; paging through with a limit that cuts INSIDE the tie must
// return every message exactly once (KANB-45 acceptance 2).
func TestChatFeed_IdenticalTimestampsNoLossNoDuplicates(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	ts := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	const total = 7
	for i := 0; i < total; i++ {
		seedFeedMessageAt(t, env, "m-00"+string(rune('1'+i)), ts, "same-instant")
	}

	var seen []string
	after := ""
	pages := 0
	for {
		res, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key, After: after, Limit: 3})
		if err != nil {
			t.Fatalf("ChatFeed page %d: %v", pages+1, err)
		}
		for _, m := range res.Messages {
			seen = append(seen, m.Message.ID)
		}
		pages++
		if !res.HasMore {
			break
		}
		after = res.NextCursor
		if pages > total {
			t.Fatal("pagination does not terminate")
		}
	}
	if len(seen) != total {
		t.Fatalf("paged over %d messages, want %d (loss or duplication at the tie boundary): %v", len(seen), total, seen)
	}
	dups := map[string]int{}
	for _, id := range seen {
		dups[id]++
		if dups[id] > 1 {
			t.Fatalf("message %s returned twice: %v", id, seen)
		}
	}
}

// TestChatFeed_MessageBetweenPagesVisible: a message posted after a page was
// read is not skipped — the cursor marks a position, so everything after it,
// including newcomers, arrives on the next page (KANB-45 acceptance 3).
func TestChatFeed_MessageBetweenPagesVisible(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)
	seedFeedMessageAt(t, env, "m-a", base, "old 1")
	seedFeedMessageAt(t, env, "m-b", base.Add(time.Second), "old 2")
	seedFeedMessageAt(t, env, "m-b2", base.Add(2*time.Second), "old 3")

	page1, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key, Limit: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if !page1.HasMore {
		t.Fatal("page 1 should report has_more with a limit below the history size")
	}

	seedFeedMessageAt(t, env, "m-c", base.Add(3*time.Second), "arrived between pages")

	page2, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key, After: page1.NextCursor})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	got := feedBodies(page2.Messages)
	if strings.Join(got, "|") != "old 3|arrived between pages" {
		t.Fatalf("page 2 = %v, want [old 3, arrived between pages] — the message that arrived after page 1 was read must be visible", got)
	}
}

// TestChatFeed_RereadingSamePageSafe: replaying a page with the same cursor
// returns the same answer — the read has no side effects (KANB-45
// acceptance 4).
func TestChatFeed_RereadingSamePageSafe(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	ts := time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)
	for i := 1; i <= 4; i++ {
		seedFeedMessageAt(t, env, "m-0"+string(rune('0'+i)), ts, "body")
	}

	first, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key, Limit: 2})
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	again, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key, Limit: 2})
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if strings.Join(feedBodies(again.Messages), "|") != strings.Join(feedBodies(first.Messages), "|") {
		t.Fatalf("reread differs: %v vs %v", feedBodies(again.Messages), feedBodies(first.Messages))
	}
	if again.NextCursor != first.NextCursor || again.HasMore != first.HasMore {
		t.Fatal("reread paging metadata differs — the read must be a pure function of (project, cursor, limit)")
	}

	// And the cursor itself replays: asking for the page AFTER a stale cursor
	// again returns the same continuation.
	cont1, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key, After: first.NextCursor})
	if err != nil {
		t.Fatalf("continuation 1: %v", err)
	}
	cont2, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key, After: first.NextCursor})
	if err != nil {
		t.Fatalf("continuation 2: %v", err)
	}
	if strings.Join(feedBodies(cont2.Messages), "|") != strings.Join(feedBodies(cont1.Messages), "|") {
		t.Fatalf("cursor continuation differs across calls: %v vs %v", feedBodies(cont2.Messages), feedBodies(cont1.Messages))
	}
}

// TestChatFeed_ForeignCursorRejected: a cursor from another project (or one
// naming no message at all, or an unparseable string) is a loud validation
// error, never a silent empty page (KANB-45 acceptance 5).
func TestChatFeed_ForeignCursorRejected(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	// A second project with its own feed.
	p2 := &domain.Project{ID: "proj-2-id", Key: "OTHER", Name: "Other", EstimateUnit: "h", EnforceDependencies: true, ClaimTTLSeconds: 3600}
	if err := env.Write(ctx, func(tx store.Tx) error {
		if err := env.Projects().Create(tx, p2); err != nil {
			return err
		}
		return env.Chat().Add(tx, &domain.ChatMessage{
			ID: "foreign-msg", ProjectID: p2.ID, Author: "someone", Body: "other project",
			CreatedAt: time.Date(2026, 9, 14, 7, 0, 0, 0, time.UTC),
		})
	}); err != nil {
		t.Fatalf("seed second project: %v", err)
	}
	foreignCursor := (&domain.ChatCursor{
		CreatedAt: time.Date(2026, 9, 14, 7, 0, 0, 0, time.UTC),
		ID:        "foreign-msg",
	}).String()

	cases := []struct {
		name   string
		after  string
		field  string
		substr string
	}{
		{"cursor of another project", foreignCursor, "after", "different project"},
		{"cursor naming a missing message", "2026-09-14T07:00:00Z/no-such-message", "after", "does not exist"},
		{"malformed cursor", "not-a-cursor", "cursor", "malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key, After: tc.after})
			de := domain.AsError(err)
			if de == nil {
				t.Fatalf("cursor %q accepted, want rejection", tc.after)
			}
			if de.Field != tc.field || !strings.Contains(de.Message, tc.substr) {
				t.Fatalf("error = %s — %s (field %q), want field %q mentioning %q", de.Code, de.Message, de.Field, tc.field, tc.substr)
			}
		})
	}
}

// TestChatFeed_EmptyPageKeepsPosition: the documented polling loop is
// `cursor = next_cursor`; an empty page must therefore echo the requested
// `after` back as next_cursor, never return an empty string — an empty string
// would reset the consumer to the beginning of the history and re-deliver
// everything it already saw. Written against the cursor as an OPAQUE string
// on purpose: the wire contract is "the position survives an empty page",
// whatever the cursor's internal format is.
func TestChatFeed_EmptyPageKeepsPosition(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	postFeedMessage(t, env, env.actor, "first")
	postFeedMessage(t, env, env.actor, "second")

	page, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if page.NextCursor == "" {
		t.Fatal("non-empty page carried no next_cursor")
	}
	held := page.NextCursor

	// Poll with the held cursor until nothing new arrives; the position must
	// survive every empty page.
	for poll := 0; poll < 3; poll++ {
		res, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key, After: held})
		if err != nil {
			t.Fatalf("poll %d: %v", poll, err)
		}
		if len(res.Messages) != 0 {
			t.Fatalf("poll %d returned %d messages over a static feed", poll, len(res.Messages))
		}
		if res.NextCursor != held {
			t.Fatalf("empty poll %d returned next_cursor %q, want the requested position %q echoed back", poll, res.NextCursor, held)
		}
	}

	// The held position still works as an insertion point: a message that
	// arrives afterwards is delivered exactly once, and the page after that
	// is empty again WITH the position kept.
	postFeedMessage(t, env, env.actor, "arrived later")
	res, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key, After: held})
	if err != nil {
		t.Fatalf("poll after arrival: %v", err)
	}
	if got := feedBodies(res.Messages); len(got) != 1 || got[0] != "arrived later" {
		t.Fatalf("poll after arrival delivered %v, want exactly [arrived later]", got)
	}
	if res.NextCursor == "" || res.NextCursor == held {
		t.Fatalf("page after arrival returned next_cursor %q, want a new position past the new message", res.NextCursor)
	}
	advanced := res.NextCursor

	empty, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key, After: advanced})
	if err != nil {
		t.Fatalf("final poll: %v", err)
	}
	if len(empty.Messages) != 0 || empty.NextCursor != advanced {
		t.Fatalf("final poll = %v, cursor %q; want an empty page echoing %q", feedBodies(empty.Messages), empty.NextCursor, advanced)
	}

	// An empty FIRST page has no position to keep: "" is the honest
	// "start from the top" answer there.
	fresh := seedSecondProject(t, env)
	emptyFirst, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: fresh.Key})
	if err != nil {
		t.Fatalf("empty first page: %v", err)
	}
	if len(emptyFirst.Messages) != 0 || emptyFirst.NextCursor != "" {
		t.Fatalf("empty first page = %v, cursor %q; want an empty page and no position", feedBodies(emptyFirst.Messages), emptyFirst.NextCursor)
	}
}

// TestChatFeed_Scoping: the feed is project-scoped and the actor's project
// scope applies; a token scoped away from the project cannot read its feed,
// and another project's messages never leak into the page.
func TestChatFeed_Scoping(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	postFeedMessage(t, env, env.actor, "bmb message")

	p2 := &domain.Project{ID: "proj-2-id", Key: "OTHER", Name: "Other", EstimateUnit: "h", EnforceDependencies: true, ClaimTTLSeconds: 3600}
	if err := env.Write(ctx, func(tx store.Tx) error {
		if err := env.Projects().Create(tx, p2); err != nil {
			return err
		}
		return env.Chat().Add(tx, &domain.ChatMessage{
			ID: "other-msg", ProjectID: p2.ID, Author: "someone", Body: "other project",
			CreatedAt: time.Date(2026, 9, 14, 7, 0, 0, 0, time.UTC),
		})
	}); err != nil {
		t.Fatalf("seed second project: %v", err)
	}

	res, err := env.svc.ChatFeed(ctx, env.actor, ChatFeedInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("ChatFeed: %v", err)
	}
	if n := len(res.Messages); n != 1 || res.Messages[0].Message.ProjectID != env.proj.ID {
		t.Fatalf("feed held %d messages, want exactly the 1 message of the requested project", n)
	}

	outsider := Actor{TokenID: "tok-out", Name: "outsider", Scopes: domain.Scopes{domain.ScopeRead}, ProjectKeys: []string{"OTHER"}}
	_, err = env.svc.ChatFeed(ctx, outsider, ChatFeedInput{ProjectKey: env.proj.Key})
	if de := domain.AsError(err); de == nil || de.Code != domain.CodeForbidden {
		t.Fatalf("outsider read of the feed: got %v, want forbidden", err)
	}
}
