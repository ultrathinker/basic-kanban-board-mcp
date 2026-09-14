package mcp

import (
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// KANB-46 — project_post's kind, recipient, reply_to, idempotency_key on the
// MCP surface.
// ---------------------------------------------------------------------------

// TestProjectPost_ProtocolFieldsForwarded: the four new parameters reach the
// service verbatim (kind canonicalized), the response echoes them, and the
// schema teaches the enum.
func TestProjectPost_ProtocolFieldsForwarded(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	now := time.Now().UTC().Truncate(time.Millisecond)
	svc.DefaultChatAdd = &domain.ChatMessage{
		ID: "msg-9", ProjectID: "p", Author: "lead", Body: "do it", CreatedAt: now,
		Kind:             domain.MessageCommand,
		Recipient:        "tok-agent",
		ResolvedExecutor: "tok-agent",
		ReplyToID:        "msg-1",
		IdempotencyKey:   "send-9",
	}

	res, sc := callTool(t, cs, "project_post", map[string]any{
		"project":         "kanb",
		"author":          "lead",
		"body":            "do it",
		"kind":            "command",
		"recipient":       "tok-agent",
		"reply_to":        "msg-1",
		"idempotency_key": "send-9",
	})
	if res.IsError {
		t.Fatalf("callTool failed: %v", sc)
	}
	expectOK(t, sc, "project_post")

	in := svc.LastChatAdd
	if in.Kind != "command" {
		t.Errorf("service Kind = %q, want the canonicalized command", in.Kind)
	}
	if in.Recipient != "tok-agent" || in.ReplyTo != "msg-1" || in.IdempotencyKey != "send-9" {
		t.Errorf("service forwarding = recipient %q reply_to %q key %q", in.Recipient, in.ReplyTo, in.IdempotencyKey)
	}

	data, _ := sc["data"].(map[string]any)
	if data["kind"] != "command" {
		t.Errorf("data.kind = %v", data["kind"])
	}
	if data["resolved_executor"] != "tok-agent" {
		t.Errorf("data.resolved_executor = %v", data["resolved_executor"])
	}
	if data["reply_to"] != "msg-1" {
		t.Errorf("data.reply_to = %v", data["reply_to"])
	}
	if data["idempotency_key"] != "send-9" {
		t.Errorf("data.idempotency_key = %v", data["idempotency_key"])
	}
}

// TestProjectPost_UnsafeWireTextRejected prevents a message or replay key
// from being silently rewritten when it crosses JSON or storage boundaries.
func TestProjectPost_UnsafeWireTextRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		field string
		value string
	}{
		{name: "body NUL", field: "body", value: "before\x00after"},
		{name: "key NUL", field: "idempotency_key", value: "retry\x00one"},
		{name: "key newline", field: "idempotency_key", value: "retry\none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cs, svc := roundtripServer(t, NewServer)
			args := map[string]any{"project": "kanb", "author": "a", "body": "safe"}
			args[tc.field] = tc.value
			res, sc := callTool(t, cs, "project_post", args)
			if !res.IsError {
				t.Fatalf("unsafe %s accepted: %v", tc.field, sc)
			}
			errBlock, _ := sc["error"].(map[string]any)
			if errBlock["code"] != string(domain.CodeValidation) {
				t.Errorf("error = %v, want validation", errBlock)
			}
			if svc.LastChatAdd.Body != "" {
				t.Errorf("service called for unsafe %s: %+v", tc.field, svc.LastChatAdd)
			}
		})
	}

	if err := validateChatWireText("body", string([]byte{0xff, 'x'}), true); err == nil {
		t.Fatal("invalid UTF-8 body accepted")
	}
	if err := validateChatWireText("idempotency_key", string([]byte{0xff, 'x'}), false); err == nil {
		t.Fatal("invalid UTF-8 idempotency key accepted")
	}
	if err := validateChatWireText("body", "before\x00after", true); err == nil {
		t.Fatal("NUL body accepted by wire validation")
	}
	if err := validateChatWireText("idempotency_key", "retry\none", false); err == nil {
		t.Fatal("newline idempotency key accepted by wire validation")
	}
}

// TestProjectPost_DefaultKindAndUpdate: without kind the call is an update —
// the pre-protocol shape of the tool keeps working unchanged.
func TestProjectPost_DefaultKindAndUpdate(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultChatAdd = &domain.ChatMessage{ID: "m", Author: "a", Body: "b", Kind: domain.MessageUpdate}

	res, sc := callTool(t, cs, "project_post", map[string]any{
		"project": "kanb", "author": "a", "body": "b",
	})
	if res.IsError {
		t.Fatalf("legacy call failed: %v", sc)
	}
	if svc.LastChatAdd.Kind != "update" {
		t.Errorf("service Kind = %q, want update", svc.LastChatAdd.Kind)
	}
	data, _ := sc["data"].(map[string]any)
	if data["kind"] != "update" {
		t.Errorf("data.kind = %v, want update", data["kind"])
	}
	if _, has := data["resolved_executor"]; has {
		t.Errorf("data.resolved_executor = %v on an update — only question/command resolve one", data["resolved_executor"])
	}
}

// TestProjectPost_UnknownKindRejectedBySchemaAndHandler: the enum is taught
// by the schema (a wrong JSON type never reaches the handler) and a bogus
// kind name is refused whichever layer sees it.
func TestProjectPost_UnknownKindRejectedBySchemaAndHandler(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	// kind is a real typed property of the published schema: a non-string
	// value is rejected before the handler runs.
	res, sc := callTool(t, cs, "project_post", map[string]any{
		"project": "kanb", "author": "a", "body": "b", "kind": 123,
	})
	if !res.IsError {
		t.Fatalf("a non-string kind was accepted: %v", sc)
	}
	if svc.LastChatAdd.Body != "" {
		t.Errorf("service was invoked despite the schema rejection: %+v", svc.LastChatAdd)
	}

	res, sc = callTool(t, cs, "project_post", map[string]any{
		"project": "kanb", "author": "a", "body": "b", "kind": "memo",
	})
	if !res.IsError {
		t.Fatalf("unknown kind accepted: %v", sc)
	}
	errBlock, _ := sc["error"].(map[string]any)
	if errBlock["code"] != "validation" {
		t.Fatalf("error = %v, want validation", errBlock)
	}
}

// TestProjectPost_IdempotencyKeyForwardedVerbatimAndBounded: the key is the
// service's to judge, so the MCP layer forwards it untouched (trimming here
// would turn a refused whitespace key into a silently unprotected send) and
// the published schema carries the same 255-character bound the service
// enforces.
func TestProjectPost_IdempotencyKeyForwardedVerbatimAndBounded(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultChatAdd = &domain.ChatMessage{ID: "msg-x", Author: "a", Kind: domain.MessageUpdate}

	props := schemaNode(t, toolByName(t, cs, "project_post").InputSchema, "properties")
	prop, ok := props["idempotency_key"].(map[string]any)
	if !ok {
		t.Fatalf("project_post schema has no idempotency_key property: %v", props)
	}
	maxLen, ok := prop["maxLength"].(float64)
	if !ok || maxLen != float64(domain.MaxChatIdempotencyKeyLen) {
		t.Fatalf("idempotency_key maxLength = %v, want %d", prop["maxLength"], domain.MaxChatIdempotencyKeyLen)
	}

	callTool(t, cs, "project_post", map[string]any{
		"project": "kanb", "author": "a", "body": "b", "idempotency_key": "  send-x  ",
	})
	if svc.LastChatAdd.IdempotencyKey != "  send-x  " {
		t.Fatalf("service received idempotency_key %q, want it forwarded verbatim", svc.LastChatAdd.IdempotencyKey)
	}
}
