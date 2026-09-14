package mcp

import (
	"strings"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// ---------------------------------------------------------------------------
// KANB-45 — board_get(view:messages) on the MCP surface.
// ---------------------------------------------------------------------------

// TestBoardGet_MessagesView: the feed is a VIEW of board_get — one page of
// messages, a cursor, has_more, and nothing of the board itself.
func TestBoardGet_MessagesView(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	svc.DefaultChatFeed = &service.ChatFeedResult{
		Messages: []service.ChatFeedMessage{
			{
				Message: domain.ChatMessage{
					ID: "msg-1", Author: "lead", AuthorTokenID: "tok-lead",
					Kind: domain.MessageCommand, Recipient: "tok-agent", ResolvedExecutor: "tok-agent",
					Body: "Do the thing", CreatedAt: now,
				},
				RecipientName:        "agent",
				ResolvedExecutorName: "agent",
				TaskKeys:             []string{"KANB-9"},
			},
			{
				Message: domain.ChatMessage{
					ID: "msg-2", Author: "agent", AuthorTokenID: "tok-agent",
					Kind: domain.MessageUpdate, ReplyToID: "msg-1",
					Body: "On it", CreatedAt: now.Add(time.Second),
				},
			},
		},
		NextCursor: "2026-09-14T10:00:01.000Z/msg-2",
		HasMore:    true,
	}

	res, sc := callTool(t, cs, "board_get", map[string]any{
		"project": "kanb", "view": "messages", "after": "2026-09-14T09:59:00.000Z/msg-0", "limit": 30,
	})
	if res.IsError {
		t.Fatalf("callTool failed: %v", sc)
	}
	expectOK(t, sc, "board_get")

	if svc.LastChatFeed.ProjectKey != "KANB" {
		t.Errorf("service ProjectKey = %q, want KANB", svc.LastChatFeed.ProjectKey)
	}
	if svc.LastChatFeed.After != "2026-09-14T09:59:00.000Z/msg-0" {
		t.Errorf("service After = %q, want the opaque cursor passed through", svc.LastChatFeed.After)
	}
	if svc.LastChatFeed.Limit != 30 {
		t.Errorf("service Limit = %d, want 30", svc.LastChatFeed.Limit)
	}

	msgsBlock, ok := sc["messages"].(map[string]any)
	if !ok {
		t.Fatalf("messages block is %T, want an object: %v", sc["messages"], sc)
	}
	if msgsBlock["project"] != "KANB" {
		t.Errorf("messages.project = %v, want KANB", msgsBlock["project"])
	}
	if msgsBlock["next_cursor"] != "2026-09-14T10:00:01.000Z/msg-2" {
		t.Errorf("messages.next_cursor = %v", msgsBlock["next_cursor"])
	}
	if msgsBlock["has_more"] != true {
		t.Errorf("messages.has_more = %v, want true", msgsBlock["has_more"])
	}
	// The feed must not carry a board projection alongside the messages.
	if _, has := sc["data"]; has {
		t.Error("messages view returned a `data` board block — the feed must not carry the board")
	}

	msgs, _ := msgsBlock["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want 2", msgs)
	}
	first, _ := msgs[0].(map[string]any)
	if first["id"] != "msg-1" || first["kind"] != "command" || first["body"] != "Do the thing" {
		t.Errorf("messages[0] = %v", first)
	}
	if first["author"] != "lead" || first["author_token_id"] != "tok-lead" {
		t.Errorf("messages[0] author fields = %v / %v — display name AND authorized source must both travel", first["author"], first["author_token_id"])
	}
	if first["resolved_executor"] != "tok-agent" || first["resolved_executor_name"] != "agent" {
		t.Errorf("messages[0] executor fields = %v / %v", first["resolved_executor"], first["resolved_executor_name"])
	}
	taskKeys, _ := first["task_keys"].([]any)
	if len(taskKeys) != 1 || taskKeys[0] != "KANB-9" {
		t.Errorf("messages[0].task_keys = %v, want [KANB-9]", first["task_keys"])
	}
	second, _ := msgs[1].(map[string]any)
	if second["reply_to"] != "msg-1" {
		t.Errorf("messages[1].reply_to = %v, want msg-1", second["reply_to"])
	}

	// The text form is line-oriented and carries the cursor so a text-only
	// consumer can page.
	tc, ok := res.Content[0].(*gomcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want TextContent", res.Content[0])
	}
	text := tc.Text
	for _, want := range []string{"command lead: Do the thing", "executor agent", "tasks KANB-9", "next_cursor 2026-09-14T10:00:01.000Z/msg-2"} {
		if !strings.Contains(text, want) {
			t.Errorf("text rendering missing %q:\n%s", want, text)
		}
	}
}

// TestBoardGet_MessagesViewEmptyPageKeepsCursor: next_cursor must be present
// in the wire form on EVERY page, empty ones included. The polling consumer
// does `cursor = next_cursor`; an omitted field (the old omitempty) reads as
// an empty string and silently resets the consumer to the start of the
// history, re-delivering everything it already saw. On an empty page the
// service echoes the requested after back; on an empty FIRST page the value
// is "" but the key itself must still travel.
func TestBoardGet_MessagesViewEmptyPageKeepsCursor(t *testing.T) {
	t.Parallel()

	t.Run("empty page echoes the requested position", func(t *testing.T) {
		t.Parallel()
		cs, svc := roundtripServer(t, NewServer)
		svc.DefaultChatFeed = &service.ChatFeedResult{
			Messages:   []service.ChatFeedMessage{},
			NextCursor: "echoed-position",
		}
		_, sc := callTool(t, cs, "board_get", map[string]any{
			"project": "kanb", "view": "messages", "after": "held-position", "limit": 30,
		})
		expectOK(t, sc, "board_get")
		msgsBlock, ok := sc["messages"].(map[string]any)
		if !ok {
			t.Fatalf("messages block is %T: %v", sc["messages"], sc)
		}
		cursor, has := msgsBlock["next_cursor"]
		if !has {
			t.Fatal("empty page omitted next_cursor from the wire form — the polling consumer loses its position")
		}
		if cursor != "echoed-position" {
			t.Fatalf("empty page next_cursor = %v, want the requested position echoed back", cursor)
		}
	})

	t.Run("empty first page still carries the key", func(t *testing.T) {
		t.Parallel()
		cs, svc := roundtripServer(t, NewServer)
		svc.DefaultChatFeed = &service.ChatFeedResult{Messages: []service.ChatFeedMessage{}}
		res, sc := callTool(t, cs, "board_get", map[string]any{"project": "kanb", "view": "messages"})
		expectOK(t, sc, "board_get")
		msgsBlock, ok := sc["messages"].(map[string]any)
		if !ok {
			t.Fatalf("messages block is %T: %v", sc["messages"], sc)
		}
		cursor, has := msgsBlock["next_cursor"]
		if !has {
			t.Fatal("empty first page omitted next_cursor from the wire form — the client cannot tell this page apart from a lost position")
		}
		if cursor != "" {
			t.Fatalf("empty first page next_cursor = %v, want \"\"", cursor)
		}
		// The text form has no position to print here (nothing exists yet);
		// it must not crash and must still announce the empty feed.
		tc, ok := res.Content[0].(*gomcp.TextContent)
		if !ok {
			t.Fatalf("content[0] is %T, want TextContent", res.Content[0])
		}
		if !strings.Contains(tc.Text, "messages 0") {
			t.Fatalf("text form of an empty feed = %q, want the zero-count header", tc.Text)
		}
	})
}

// TestBoardGet_MessagesViewRequiresProject: the feed is project-scoped; a
// messages view without a project is a validation error that says so.
func TestBoardGet_MessagesViewRequiresProject(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)

	res, sc := callTool(t, cs, "board_get", map[string]any{"view": "messages"})
	if !res.IsError {
		t.Fatalf("view:messages without project succeeded: %v", sc)
	}
	errBlock, _ := sc["error"].(map[string]any)
	if errBlock["code"] != "validation" || !strings.Contains(errBlock["message"].(string), "project") {
		t.Fatalf("error = %v, want a validation error naming the project field", errBlock)
	}
	if svc := sc["ok"]; svc != false {
		t.Fatalf("ok = %v, want false", svc)
	}
}

// TestBoardGet_MessagesViewDefaultAndForeignCursorErrors: the service's
// cursor refusals surface as the standard error envelope.
func TestBoardGet_MessagesViewCursorErrors(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultChatFeedEr = domain.Invalid("after",
		"cursor message foreign belongs to a different project",
		"Use the next_cursor returned by a previous page of this project's feed, or omit after to read from the beginning.")

	res, sc := callTool(t, cs, "board_get", map[string]any{
		"project": "kanb", "view": "messages", "after": "2026-09-14T10:00:00.000Z/foreign",
	})
	if !res.IsError {
		t.Fatalf("foreign cursor accepted: %v", sc)
	}
	errBlock, _ := sc["error"].(map[string]any)
	if !strings.Contains(errBlock["message"].(string), "different project") {
		t.Fatalf("error message = %v, want the foreign-cursor explanation", errBlock["message"])
	}
	if _, has := errBlock["remediation"]; !has {
		t.Error("error carries no remediation — every refusal must name the next action")
	}
}
