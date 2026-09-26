package mcp

import (
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// The feed an agent reads names the posting key next to a signature that is
// not it, the same way the web feed does.
func TestBoardGet_MessagesViewNamesPostingKeyNextToADifferentSignature(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultChatFeed = &service.ChatFeedResult{Messages: []service.ChatFeedMessage{
		{Message: domain.ChatMessage{ID: "m1", Author: "owner", AuthorTokenID: "tok-x3", Kind: domain.MessageUpdate, Body: "approve it"}, AuthorVia: "opus-x3"},
		{Message: domain.ChatMessage{ID: "m2", Author: "opus-x3", AuthorTokenID: "tok-x3", Kind: domain.MessageUpdate, Body: "signed as itself"}},
	}}

	res := callToolRaw(t, cs, "board_get", map[string]any{"project": "kanb", "view": "messages"})
	text := res.Content[0].(*gomcp.TextContent).Text
	if !strings.Contains(text, "update owner (via opus-x3): approve it") {
		t.Errorf("text feed hides the posting key:\n%s", text)
	}
	if !strings.Contains(text, "update opus-x3: signed as itself") {
		t.Errorf("self-signed message rendered differently:\n%s", text)
	}

	_, sc := callTool(t, cs, "board_get", map[string]any{"project": "kanb", "view": "messages", "format": "json"})
	msgs, _ := sc["messages"].(map[string]any)["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("json feed has %d messages, want 2: %v", len(msgs), sc)
	}
	if got := msgs[0].(map[string]any)["author_via"]; got != "opus-x3" {
		t.Errorf("messages[0].author_via = %v, want opus-x3", got)
	}
	if _, present := msgs[1].(map[string]any)["author_via"]; present {
		t.Errorf("messages[1] carries author_via for a self-signed message: %v", msgs[1])
	}
}
