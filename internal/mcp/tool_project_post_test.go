package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// TestProjectPost_Success verifies that project_post creates a message in the
// project chat with the specified author and body, returning the canonical
// envelope with the message ID, project key, author, body, and timestamp.
func TestProjectPost_Success(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	now := time.Now().UTC().Truncate(time.Second)
	svc.DefaultChatAdd = &domain.ChatMessage{
		ID:        "msg-12345",
		ProjectID: "proj-uuid-1",
		Author:    "Claude",
		Body:      "Working on KANB-7: implementing MCP tool project_post",
		CreatedAt: now,
	}

	res, sc := callTool(t, cs, "project_post", map[string]any{
		"project": "kanb",
		"author":  "Claude",
		"body":    "Working on KANB-7: implementing MCP tool project_post",
	})

	if res.IsError {
		t.Fatalf("callTool failed: %v", sc)
	}
	expectOK(t, sc, "project_post")

	// Verify input forwarded to service
	if svc.LastChatAdd.ProjectKey != "KANB" {
		t.Errorf("service ProjectKey = %q, want KANB", svc.LastChatAdd.ProjectKey)
	}
	if svc.LastChatAdd.Author != "Claude" {
		t.Errorf("service Author = %q, want Claude", svc.LastChatAdd.Author)
	}
	if svc.LastChatAdd.Body != "Working on KANB-7: implementing MCP tool project_post" {
		t.Errorf("service Body = %q, want exact body", svc.LastChatAdd.Body)
	}

	// Verify structured output envelope
	data, ok := sc["data"].(map[string]any)
	if !ok {
		t.Fatalf("data is %T, want map[string]any: %v", sc["data"], sc)
	}
	if data["id"] != "msg-12345" {
		t.Errorf("data.id = %v, want msg-12345", data["id"])
	}
	if data["project"] != "KANB" {
		t.Errorf("data.project = %v, want KANB", data["project"])
	}
	if data["author"] != "Claude" {
		t.Errorf("data.author = %v, want Claude", data["author"])
	}
	if data["body"] != "Working on KANB-7: implementing MCP tool project_post" {
		t.Errorf("data.body = %v, want exact body", data["body"])
	}
	if data["created_at"] != formatTime(now) {
		t.Errorf("data.created_at = %v, want %v", data["created_at"], formatTime(now))
	}
}

// TestProjectPost_AppearsInProjectChat simulates a project chat store to prove
// that messages posted via project_post accumulate in project chat and preserve
// author identities, including non-ASCII free-form names.
func TestProjectPost_AppearsInProjectChat(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	var chatHistory []domain.ChatMessage
	svc.NextChatAdd = func(_ context.Context, _ service.Actor, in service.ChatAddInput) (*domain.ChatMessage, error) {
		msg := domain.ChatMessage{
			ID:        "msg-chat-1",
			ProjectID: "proj-uuid",
			Author:    in.Author,
			Body:      in.Body,
			CreatedAt: time.Now().UTC(),
		}
		chatHistory = append(chatHistory, msg)
		return &msg, nil
	}

	authorName := "Zoë-Ω"
	bodyText := "Still on KANB-9, tests written."

	res, sc := callTool(t, cs, "project_post", map[string]any{
		"project": "KANB",
		"author":  authorName,
		"body":    bodyText,
	})
	if res.IsError {
		t.Fatalf("callTool failed: %v", sc)
	}
	expectOK(t, sc, "project_post")

	if len(chatHistory) != 1 {
		t.Fatalf("chat history length = %d, want 1", len(chatHistory))
	}
	if chatHistory[0].Author != authorName {
		t.Errorf("chat author = %q, want %q", chatHistory[0].Author, authorName)
	}
	if chatHistory[0].Body != bodyText {
		t.Errorf("chat body = %q, want %q", chatHistory[0].Body, bodyText)
	}
}

// TestProjectPost_EmptyBodyRejected verifies that empty or whitespace-only bodies
// are rejected with a clear validation error.
func TestProjectPost_EmptyBodyRejected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"empty_string", ""},
		{"whitespace_spaces", "    "},
		{"whitespace_mixed", "  \t\n  \r\n  "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cs, svc := roundtripServer(t, NewServer)
			svc.DefaultChatAdd = &domain.ChatMessage{ID: "should-not-reach-here"}

			res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
				Name: "project_post",
				Arguments: map[string]any{
					"project": "KANB",
					"author":  "Claude",
					"body":    tc.body,
				},
			})
			if err != nil {
				t.Fatalf("unexpected protocol error: %v", err)
			}
			env := errorEnvelopeOf(t, res)
			if env["code"] != string(domain.CodeValidation) {
				t.Errorf("code = %v, want %v", env["code"], domain.CodeValidation)
			}
			msg, _ := env["message"].(string)
			if !strings.Contains(strings.ToLower(msg), "empty") && !strings.Contains(strings.ToLower(msg), "body") {
				t.Errorf("message %q should mention empty/body", msg)
			}
			// Service must not have recorded an add
			if svc.LastChatAdd.Body != "" {
				t.Errorf("service was invoked despite empty body: %+v", svc.LastChatAdd)
			}
		})
	}
}

// TestProjectPost_UnknownProjectRejected verifies that targeting an unknown
// project returns a clear not_found error.
func TestProjectPost_UnknownProjectRejected(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.NextChatAdd = func(_ context.Context, _ service.Actor, in service.ChatAddInput) (*domain.ChatMessage, error) {
		return nil, domain.NotFound("project", in.ProjectKey)
	}

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "project_post",
		Arguments: map[string]any{
			"project": "UNKNOWN",
			"author":  "Claude",
			"body":    "Hello to nowhere",
		},
	})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	env := errorEnvelopeOf(t, res)
	if env["code"] != string(domain.CodeNotFound) {
		t.Errorf("code = %v, want %v", env["code"], domain.CodeNotFound)
	}
	msg, _ := env["message"].(string)
	if !strings.Contains(msg, "UNKNOWN") {
		t.Errorf("message %q does not contain project key UNKNOWN", msg)
	}
	rem, _ := env["remediation"].(string)
	if rem == "" {
		t.Errorf("remediation should not be empty")
	}
}

// TestProjectPost_UnknownFieldRejected verifies that unexpected properties in
// the arguments are rejected by the schema validator.
func TestProjectPost_UnknownFieldRejected(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultChatAdd = &domain.ChatMessage{ID: "unexpected"}

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "project_post",
		Arguments: map[string]any{
			"project":       "KANB",
			"author":        "Claude",
			"body":          "Hello world",
			"extra_field":   "should_be_rejected",
			"another_stray": 42,
		},
	})
	if err != nil {
		t.Fatalf("schema rejection should be a CallToolResult, not a Go error: %v", err)
	}
	env := errorEnvelopeOf(t, res)
	if env["code"] != string(domain.CodeValidation) {
		t.Errorf("code = %v, want %v", env["code"], domain.CodeValidation)
	}
	msg, _ := env["message"].(string)
	if !strings.Contains(msg, "extra_field") && !strings.Contains(msg, "another_stray") {
		t.Errorf("error message should name unknown fields: %q", msg)
	}
	// Handler must not have run
	if svc.LastChatAdd.ProjectKey != "" {
		t.Errorf("handler was invoked despite schema rejection: %+v", svc.LastChatAdd)
	}
}

// TestProjectPost_ReadOnlyServerLacksTool_AndFullServerHasIt verifies that
// project_post is NOT published on the read-only MCP server, but IS published
// on the full MCP server.
func TestProjectPost_ReadOnlyServerLacksTool_AndFullServerHasIt(t *testing.T) {
	t.Parallel()

	// 1. Read-only server lacks project_post
	roCS, _ := roundtripServer(t, NewReadOnlyServer)
	roTools, err := roCS.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("roCS.ListTools: %v", err)
	}
	for _, tl := range roTools.Tools {
		if tl.Name == "project_post" {
			t.Fatalf("read-only server MUST NOT list project_post, but found it")
		}
	}

	// Calling project_post on read-only server must fail
	roRes, roErr := roCS.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "project_post",
		Arguments: map[string]any{
			"project": "KANB",
			"author":  "Claude",
			"body":    "testing read-only",
		},
	})
	if roErr == nil && (roRes == nil || !roRes.IsError) {
		t.Fatalf("calling project_post on read-only server should fail; got res=%v, err=%v", roRes, roErr)
	}

	// 2. Full server has project_post
	fullCS, fullSvc := roundtripServer(t, NewServer)
	fullSvc.DefaultChatAdd = &domain.ChatMessage{
		ID:        "msg-full-1",
		ProjectID: "proj-1",
		Author:    "Claude",
		Body:      "testing full server",
		CreatedAt: time.Now().UTC(),
	}

	fullTools, err := fullCS.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("fullCS.ListTools: %v", err)
	}
	found := false
	for _, tl := range fullTools.Tools {
		if tl.Name == "project_post" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("full server must list project_post, but did not")
	}

	// Calling project_post on full server succeeds
	fullRes, fullSC := callTool(t, fullCS, "project_post", map[string]any{
		"project": "KANB",
		"author":  "Claude",
		"body":    "testing full server",
	})
	if fullRes.IsError {
		t.Fatalf("callTool on full server failed: %v", fullSC)
	}
	expectOK(t, fullSC, "project_post")
}

// TestProjectPost_EmptyAuthorRejected verifies that an empty or whitespace author
// is rejected.
func TestProjectPost_EmptyAuthorRejected(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultChatAdd = &domain.ChatMessage{ID: "never"}

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "project_post",
		Arguments: map[string]any{
			"project": "KANB",
			"author":  "   ",
			"body":    "Valid body",
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	env := errorEnvelopeOf(t, res)
	if env["code"] != string(domain.CodeValidation) {
		t.Errorf("code = %v, want %v", env["code"], domain.CodeValidation)
	}
	if !strings.Contains(env["message"].(string), "author") {
		t.Errorf("message = %q, want mention of author", env["message"])
	}
}

// TestProjectPost_DescriptionContent verifies that the published description
// conveys all essential operational context: short messages, periodic broadcasting,
// task key linking, and real-time view rather than a final report.
func TestProjectPost_DescriptionContent(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)
	tool := toolByName(t, cs, "project_post")
	desc := tool.Description

	requiredElements := []string{
		"chat feed",
		"real-time broadcast",
		"posterity",
		"periodically",
		"KANB-7",
		"clickable",
		"`data`",
	}

	for _, elem := range requiredElements {
		if !strings.Contains(desc, elem) {
			t.Errorf("project_post description does not contain %q:\n%s", elem, desc)
		}
	}
}
