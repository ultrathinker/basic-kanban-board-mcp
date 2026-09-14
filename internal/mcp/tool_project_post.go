package mcp

import (
	"context"
	"fmt"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

const opProjectPost = "project_post"

// projectPostInput is a frozen wire contract (KANB-46): kind, recipient,
// reply_to and idempotency_key bind the next stage's adapter and external
// clients, so their names and types do not move.
type projectPostInput struct {
	Project string `json:"project" jsonschema:"project key, case-insensitive"`
	Author  string `json:"author" jsonschema:"agent identity / display name (e.g. Claude, Codex, Zoë). Signature only — attribution always follows your token, this cannot change who you are."`
	Body    string `json:"body" jsonschema:"message text; mention task keys like KANB-7 for clickable links"`
	Kind    string `json:"kind,omitempty" jsonschema:"update = plain progress note (default), scope_change = the plan/scope changed, question = asks one recipient (default: the coordinator), command = an assignment one executor may accept via task_create(source_message)"`
	// Recipient is a participant's tokens.id (read them from
	// board_get(view:summary)) or the literal "all". Empty on a question or
	// command means the project's coordinator AT SEND TIME.
	Recipient      string `json:"recipient,omitempty" jsonschema:"tokens.id of one participant, or \"all\"; empty = the project coordinator for question/command"`
	ReplyTo        string `json:"reply_to,omitempty" jsonschema:"id of the message this one answers, same project"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"stable key of ONE send: the same key with the same content returns the existing message, different content (including a different project) is an error; lives as long as the message; not blank, at most 255 characters"`
}

type projectPostData struct {
	ID        string `json:"id" jsonschema:"unique message identifier"`
	Project   string `json:"project" jsonschema:"canonical uppercase project key"`
	Author    string `json:"author" jsonschema:"agent identity / display name"`
	Body      string `json:"body" jsonschema:"message body as posted"`
	CreatedAt string `json:"created_at" jsonschema:"RFC3339 timestamp"`
	Kind      string `json:"kind" jsonschema:"update | scope_change | question | command"`
	// Recipient is what the sender asked for (tokens.id or "all"); empty
	// means the coordinator default was applied at send time.
	Recipient string `json:"recipient,omitempty"`
	// ResolvedExecutor is the tokens.id fixed at send time for a question or
	// command addressed to exactly one executor. Empty for update,
	// scope_change and "all". A later coordinator change never rewrites it.
	ResolvedExecutor string `json:"resolved_executor,omitempty"`
	ReplyTo          string `json:"reply_to,omitempty"`
	IdempotencyKey   string `json:"idempotency_key,omitempty"`
}

type projectPostOutput struct {
	OK    bool             `json:"ok"`
	Op    string           `json:"op"`
	Data  *projectPostData `json:"data,omitempty"`
	Meta  *toolMeta        `json:"meta,omitempty"`
	Error *errorEnvelope   `json:"error,omitempty"`
}

const projectPostDescription = "Post a live message to the project chat feed. " +
	"This is a real-time broadcast for the human watching the board right now, not a post-mortem or final report for posterity. " +
	"Post short messages periodically during your work (approximately every 5 minutes), not just once at the end: " +
	"share what is being done now, what is planned next, and what went wrong or surprised you. " +
	"Reference tasks by their key directly in the text (e.g. KANB-7) — they become clickable links on the board.\n" +
	"kind picks the message class: \"update\" (the default — everything above), \"scope_change\", \"question\" and \"command\". " +
	"A question or command is addressed: pass recipient (a participant's tokens.id, or \"all\"), or omit it to address the project's coordinator " +
	"(both are resolved to a single resolved_executor AT SEND TIME — a later coordinator change never readdresses an old command). " +
	"A command's executor turns it into work atomically with task_create(source_message). " +
	"A reply references the message it answers with reply_to. " +
	"Send idempotency_key to make retries safe: the same key of yours with the same content returns the existing message.\n" +
	"Returns `data`: the posted message with its id, author, body, timestamp and, when addressed, its resolved_executor.\n" +
	"Keeping the feed: " + domain.FeedPostingRules

func projectPostTool() *gomcp.Tool {
	s := schemaFor[projectPostInput]()
	setMinLen(prop(s, "project"), domain.MinProjectKeyLen)
	setMaxLen(prop(s, "project"), domain.MaxProjectKeyLen)
	setMaxLen(prop(s, "author"), domain.MaxAssigneeLen)
	setMaxLen(prop(s, "body"), domain.MaxChatMessageLen)
	setEnum(prop(s, "kind"), messageKindNames()...)
	setDefault(prop(s, "kind"), string(domain.MessageUpdate))
	setMaxLen(prop(s, "idempotency_key"), domain.MaxChatIdempotencyKeyLen)

	return &gomcp.Tool{
		Name:        opProjectPost,
		Description: projectPostDescription,
		InputSchema: s,
	}
}

// messageKindNames is the kind enum published in schemas, built from
// domain.AllMessageKinds so the wire enum cannot drift from the internal one.
func messageKindNames() []string {
	names := make([]string, len(domain.AllMessageKinds))
	for i, k := range domain.AllMessageKinds {
		names[i] = string(k)
	}
	return names
}

func registerProjectPost(s *gomcp.Server, svc service.Service) {
	tool := projectPostTool()
	gomcp.AddTool(s, tool, func(ctx context.Context, req *gomcp.CallToolRequest, in projectPostInput) (*gomcp.CallToolResult, projectPostOutput, error) {
		actor, aerr := actorFromContext(ctx)
		if aerr != nil {
			return errorResult(opProjectPost, aerr), projectPostOutput{OK: false, Op: opProjectPost, Error: newErrorEnvelope(aerr)}, nil
		}

		key, err := domain.ValidateProjectKey(in.Project)
		if err != nil {
			derr := domain.AsError(err)
			return errorResult(opProjectPost, derr), projectPostOutput{OK: false, Op: opProjectPost, Error: newErrorEnvelope(derr)}, nil
		}

		author := strings.TrimSpace(in.Author)
		if author == "" {
			derr := domain.Invalid("author", "author is required", "Pass an author name (e.g. Claude, Codex, Zoë).")
			return errorResult(opProjectPost, derr), projectPostOutput{OK: false, Op: opProjectPost, Error: newErrorEnvelope(derr)}, nil
		}
		if err := domain.ValidateActorName("author", author); err != nil {
			derr := domain.AsError(err)
			return errorResult(opProjectPost, derr), projectPostOutput{OK: false, Op: opProjectPost, Error: newErrorEnvelope(derr)}, nil
		}

		if err := domain.ValidateChatMessageBody(in.Body); err != nil {
			derr := domain.AsError(err)
			return errorResult(opProjectPost, derr), projectPostOutput{OK: false, Op: opProjectPost, Error: newErrorEnvelope(derr)}, nil
		}

		kind := strings.TrimSpace(in.Kind)
		if kind == "" {
			kind = string(domain.MessageUpdate)
		}
		parsedKind, ok := domain.ParseMessageKind(kind)
		if !ok {
			derr := domain.Invalid("kind",
				fmt.Sprintf("message kind %q is not valid", in.Kind),
				"Use one of: update, scope_change, question, command.")
			return errorResult(opProjectPost, derr), projectPostOutput{OK: false, Op: opProjectPost, Error: newErrorEnvelope(derr)}, nil
		}
		// recipient passes through unchanged — the service checks it against
		// the project's participant list, which is the authoritative source;
		// duplicating a looser copy of that check here would only produce a
		// second, drift-prone error message.
		recipient := strings.TrimSpace(in.Recipient)

		// The idempotency key is forwarded VERBATIM: the service owns every
		// rule about it (blank after trimming, length, token requirement),
		// and trimming here would turn a refused "   " key into a silently
		// unprotected send.
		msg, svcErr := svc.ChatAdd(ctx, actor, service.ChatAddInput{
			ProjectKey:     key,
			Author:         author,
			Body:           in.Body,
			Kind:           string(parsedKind),
			Recipient:      recipient,
			ReplyTo:        strings.TrimSpace(in.ReplyTo),
			IdempotencyKey: in.IdempotencyKey,
		})
		if svcErr != nil {
			derr := asDomainError(svcErr)
			return errorResult(opProjectPost, derr), projectPostOutput{OK: false, Op: opProjectPost, Error: newErrorEnvelope(derr)}, nil
		}

		// `key` is the canonical form of the REQUESTED project, and the
		// service guarantees a returned message always lives in the project
		// it was asked for (a dedup hit in another project is refused as an
		// idempotency mismatch, not echoed) — so Project here is the factual
		// project of msg, not a request echo.
		out := projectPostOutput{
			OK: true,
			Op: opProjectPost,
			Data: &projectPostData{
				ID:               msg.ID,
				Project:          key,
				Author:           msg.Author,
				Body:             msg.Body,
				CreatedAt:        formatTime(msg.CreatedAt),
				Kind:             string(msg.Kind),
				Recipient:        msg.Recipient,
				ResolvedExecutor: msg.ResolvedExecutor,
				ReplyTo:          msg.ReplyToID,
				IdempotencyKey:   msg.IdempotencyKey,
			},
		}
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: jsonText(out)}}}, out, nil
	})
}
