package view_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// ---------------------------------------------------------------------------
// KANB-48 — the feed shows the communication protocol and the composer.
// The mapping half (ChatEntriesNewestFirst over per-message meta) and the
// markup half (the rendered board page) are pinned separately: the mapping
// can drift while the template still renders, and vice versa.
// ---------------------------------------------------------------------------

func metaFixtureMsg(id, author, body, kind string, at time.Time) domain.ChatMessage {
	return domain.ChatMessage{ID: id, Author: author, Body: body, CreatedAt: at, Kind: domain.MessageKind(kind)}
}

// TestChatEntries_MetaBecomesEntryFields pins the mapping: kind, addressee,
// quote, and the command's state. This is where "received / accepted / done"
// are decided — an accepted command differs from a merely delivered one by
// the acceptance the caller resolved, never by anything the view guesses.
func TestChatEntries_MetaBecomesEntryFields(t *testing.T) {
	now := chatFixtureNow
	msgs := []domain.ChatMessage{
		metaFixtureMsg("m1", "lead", "ship the report", "command", now),
		metaFixtureMsg("m2", "agent", "on it", "update", now.Add(-time.Minute)),
		metaFixtureMsg("m3", "agent", "wide question to the room", "question", now.Add(-2*time.Minute)),
	}
	meta := map[string]view.ChatEntryMeta{
		"m1": {
			RecipientName: "",
			ExecutorName:  "executor",
			Accepted:      true,
			Tasks: []view.ChatEntryTask{
				{Key: "BMB-1", Done: false},
				{Key: "BMB-2", Done: true},
			},
		},
		"m2": {ParentAuthor: "lead", ParentBody: "ship the report"},
	}
	entries := view.ChatEntriesNewestFirst(msgs, now, nil, meta)
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}

	cmd := entries[0]
	if cmd.Kind != "command" {
		t.Errorf("kind = %q, want command", cmd.Kind)
	}
	// The fixed executor is the addressee a reader needs to see — the one
	// who must accept — so it wins over the raw recipient naming.
	if cmd.Recipient != "executor" {
		t.Errorf("recipient = %q, want the executor's name", cmd.Recipient)
	}
	if !cmd.Accepted || cmd.AwaitingAccept {
		t.Errorf("accepted command state = accepted:%v awaiting:%v", cmd.Accepted, cmd.AwaitingAccept)
	}
	if len(cmd.Tasks) != 2 || cmd.Tasks[0].Key != "BMB-1" || cmd.Tasks[1].Done != true {
		t.Errorf("tasks = %+v, want both keys with BMB-2 done", cmd.Tasks)
	}

	reply := entries[1]
	if reply.Kind != "update" {
		t.Errorf("update kind = %q, want update", reply.Kind)
	}
	if reply.Accepted || reply.AwaitingAccept {
		t.Errorf("a plain update claimed a command state: accepted:%v awaiting:%v", reply.Accepted, reply.AwaitingAccept)
	}
	if reply.ReplyAuthor != "lead" || !strings.HasPrefix(reply.ReplyText, "ship the report") {
		t.Errorf("reply quote = %q by %q, want an excerpt of the command", reply.ReplyText, reply.ReplyAuthor)
	}

	question := entries[2]
	if question.Kind != "question" || question.AwaitingAccept {
		t.Errorf("question = %+v, want kind question and no command state", question)
	}
}

// TestChatEntries_UnacceptedCommandIsAwaiting: the second of the three
// states. A command without an acceptance is DELIVERED work, not accepted
// work — the difference the card says the user must see.
func TestChatEntries_UnacceptedCommandIsAwaiting(t *testing.T) {
	msgs := []domain.ChatMessage{metaFixtureMsg("m1", "lead", "ship the report", "command", chatFixtureNow)}
	entries := view.ChatEntriesNewestFirst(msgs, chatFixtureNow, nil, nil)
	e := entries[0]
	if !e.AwaitingAccept || e.Accepted || len(e.Tasks) != 0 {
		t.Errorf("unaccepted command = accepted:%v awaiting:%v tasks:%v, want awaiting only", e.Accepted, e.AwaitingAccept, e.Tasks)
	}
	// With a meta entry present but no acceptance either — the shape the
	// service produces for an addressed command — the state must not flip.
	entries = view.ChatEntriesNewestFirst(msgs, chatFixtureNow, nil, map[string]view.ChatEntryMeta{
		"m1": {RecipientName: "executor"},
	})
	if !entries[0].AwaitingAccept || entries[0].Accepted {
		t.Errorf("addressed but unaccepted command flipped state: %+v", entries[0])
	}
	if entries[0].Recipient != "executor" {
		t.Errorf("recipient = %q, want executor", entries[0].Recipient)
	}
}

// TestChatEntries_BroadcastReadsAll: an update addressed to "all" shows it —
// a reader must be able to tell a room-wide post from one aimed at nobody
// in particular.
func TestChatEntries_BroadcastReadsAll(t *testing.T) {
	msgs := []domain.ChatMessage{metaFixtureMsg("m1", "lead", "to everyone", "update", chatFixtureNow)}
	msgs[0].Recipient = "all"
	entries := view.ChatEntriesNewestFirst(msgs, chatFixtureNow, nil, nil)
	if entries[0].Recipient != "all" {
		t.Errorf("recipient = %q, want all", entries[0].Recipient)
	}
}

// TestChatEntries_QuoteIsCutNotCarried: a long parent is excerpted, never
// re-posted whole in the reply's bubble. The cut counts runes, not bytes, so
// the fixture is a three-byte rune: a byte-based cut would produce a
// different rune count and these assertions would fail.
func TestChatEntries_QuoteIsCutNotCarried(t *testing.T) {
	long := strings.Repeat("漢", 120)
	msgs := []domain.ChatMessage{metaFixtureMsg("m1", "agent", "answer", "update", chatFixtureNow)}
	entries := view.ChatEntriesNewestFirst(msgs, chatFixtureNow, nil, map[string]view.ChatEntryMeta{
		"m1": {ParentAuthor: "lead", ParentBody: long},
	})
	q := entries[0].ReplyText
	if !strings.HasSuffix(q, "…") {
		t.Fatalf("quote %q is not cut", q)
	}
	// 80 runes of source plus the ellipsis.
	if n := len([]rune(q)); n != 81 {
		t.Errorf("quote is %d runes, want 81", n)
	}
	if strings.Contains(q, strings.Repeat("漢", 81)) {
		t.Error("quote kept more of the parent than the cut allows")
	}
}

// TestChatComposer_BuildsRecipientOrder: coordinator first (the default the
// service resolves at send time), then the participants by id, then the
// broadcast; no write scope, no composer at all.
func TestChatComposer_BuildsRecipientOrder(t *testing.T) {
	if c := view.NewChatComposer(false, "lead", []view.ChatRecipient{{ID: "tok-exec", Label: "executor"}}); c != nil {
		t.Errorf("read-only session got a composer: %+v", c)
	}
	c := view.NewChatComposer(true, "lead", []view.ChatRecipient{{ID: "tok-exec", Label: "executor"}})
	if c == nil || !c.CanPost {
		t.Fatal("posting session got no composer")
	}
	if len(c.Recipients) != 3 {
		t.Fatalf("recipients = %+v, want coordinator, participant, all", c.Recipients)
	}
	if c.Recipients[0].ID != "" || c.Recipients[0].Label != "coordinator (lead)" {
		t.Errorf("first recipient = %+v, want the coordinator default", c.Recipients[0])
	}
	if c.Recipients[1] != (view.ChatRecipient{ID: "tok-exec", Label: "executor"}) {
		t.Errorf("second recipient = %+v, want the participant", c.Recipients[1])
	}
	if c.Recipients[2] != (view.ChatRecipient{ID: "all", Label: "all"}) {
		t.Errorf("third recipient = %+v, want the broadcast", c.Recipients[2])
	}
	// No coordinator appointed: the default option must still exist and
	// simply not name one — the send-time resolution error is the service's
	// to report, not a reason to hide the option.
	c2 := view.NewChatComposer(true, "", nil)
	if c2.Recipients[0].Label != "coordinator" {
		t.Errorf("coordinator-less label = %q, want plain coordinator", c2.Recipients[0].Label)
	}
}

// TestChatComposer_RendersVisibleTypeSelect is acceptance 1: the editor with
// an addressee and a VISIBLE type choice. The type select is real markup
// with all three options rendered — not a hidden input, not a remembered
// mode — because a neutral post must not be able to quietly become a
// command.
func TestChatComposer_RendersVisibleTypeSelect(t *testing.T) {
	chat := view.NewChatPanel(chatFixtureMsgs, "", chatFixtureNow, nil, nil)
	chat.Compose = view.NewChatComposer(true, "lead", []view.ChatRecipient{{ID: "tok-exec", Label: "executor"}})
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", boardWithChat(chat, true)))

	for _, want := range []string{
		`data-chat-compose`,
		`data-chat-compose-text`,
		`data-chat-compose-recipient`,
		`data-chat-compose-kind`,
		`data-chat-compose-send`,
		`<option value="update">message</option>`,
		`<option value="question">question</option>`,
		`<option value="command">command</option>`,
		`<option value="">coordinator (lead)</option>`,
		`<option value="tok-exec">executor</option>`,
		`<option value="all">all</option>`,
		// The section announces it accepts posting, which is what shows the
		// entries' reply affordances.
		`data-can-post`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("composer markup is missing %q", want)
		}
	}
}

// TestChatComposer_ReadOnlySessionRendersNoComposer: a session that cannot
// write must not be shown an editor that cannot work — the form is absent,
// not merely disabled, and nothing announces posting.
func TestChatComposer_ReadOnlySessionRendersNoComposer(t *testing.T) {
	chat := view.NewChatPanel(chatFixtureMsgs, "", chatFixtureNow, nil, nil)
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", boardWithChat(chat, true)))
	for _, banned := range []string{"data-chat-compose", "data-can-post"} {
		if strings.Contains(html, banned) {
			t.Errorf("read-only page carries %q", banned)
		}
	}
}

// TestChatEntry_RendersProtocolAndStates walks one rendered feed through the
// acceptance markup: kind badges, the quote, the addressee line, and the
// three command states with the tasks' own done state inline.
func TestChatEntry_RendersProtocolAndStates(t *testing.T) {
	now := chatFixtureNow
	msgs := []domain.ChatMessage{
		metaFixtureMsg("done1", "lead", "already finished", "command", now),
		metaFixtureMsg("acc1", "lead", "ship it", "command", now),
		metaFixtureMsg("wait1", "lead", "please do this", "command", now),
		metaFixtureMsg("q1", "agent", "which column?", "question", now),
		metaFixtureMsg("rep1", "agent", "doing it now", "update", now),
	}
	msgs[0].Recipient = "all"
	msgs[3].AuthorTokenID = "tok-exec" // the question carries its author's token for the reply affordance
	meta := map[string]view.ChatEntryMeta{
		"done1": {Accepted: true, Tasks: []view.ChatEntryTask{{Key: "BMB-9", Done: true}}},
		"acc1":  {ExecutorName: "executor", Accepted: true, Tasks: []view.ChatEntryTask{{Key: "BMB-1", Done: false}, {Key: "BMB-2", Done: false}}},
		"wait1": {RecipientName: "executor"},
		"rep1":  {ParentAuthor: "lead", ParentBody: "please do this"},
	}
	chat := view.NewChatPanel(msgs, "", now, nil, meta)
	chat.Compose = view.NewChatComposer(true, "", []view.ChatRecipient{})
	html := renderProgress(t, "page-board", view.SamplePage("Test", "board", boardWithChat(chat, true)))

	for _, want := range []string{
		// The kind badge: command and question are labelled, an update is not
		// (a neutral post must not wear a directive's badge).
		`<span class="chat-kind">command</span>`,
		`<span class="chat-kind">question</span>`,
		// The three states, distinguishable in words.
		`awaiting acceptance`,
		`accepted &#8594; <a href="/t/BMB-1">BMB-1</a>`,
		`<a href="/t/BMB-9">BMB-9</a> (done)`,
		// The quote a reply carries.
		`&#8627; lead: &#8220;please do this&#8221;`,
		// The addressee lines: a broadcast and a named executor.
		`<div class="chat-addr text-subtle text-xs">&rarr; all</div>`,
		`&rarr; executor`,
		// The reply affordance carries whom it replies to and the author's
		// token, so the composer can preselect the addressee.
		`data-chat-reply="rep1" data-chat-reply-author="agent"`,
		`data-author-token=`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("feed markup is missing %q", want)
		}
	}
	// The accepted command shows its executor, not just "accepted": the
	// reader must see WHO owes the acceptance without decoding token ids.
	if !strings.Contains(html, "accepted &#8594; <a href=\"/t/BMB-1\">BMB-1</a>") {
		t.Error("accepted command does not link its tasks")
	}
}
