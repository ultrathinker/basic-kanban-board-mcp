package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ChatAdd appends one message to a project chat. The body is validated for
// length and non-emptiness, but stored verbatim without escaping.
//
// The communication protocol fields (KANB-46) are validated and resolved
// INSIDE the write transaction together with the insert, so addressing can
// never race a settings change: resolved_executor is computed from the
// project configuration as of this send and then frozen on the row.
func (s *svc) ChatAdd(ctx context.Context, a Actor, in ChatAddInput) (*domain.ChatMessage, error) {
	if err := requireCardWrite(a); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(in.ProjectKey)
	if key == "" {
		return nil, domain.Invalid("project_key", "project key is required", "Pass the project key.")
	}
	canonicalKey, err := domain.ValidateProjectKey(key)
	if err != nil {
		return nil, err
	}
	if err := requireProjectAccess(a, canonicalKey); err != nil {
		return nil, err
	}
	if err := domain.ValidateChatMessageBody(in.Body); err != nil {
		return nil, err
	}

	kind := domain.MessageKind(strings.TrimSpace(in.Kind))
	if kind == "" {
		kind = domain.MessageUpdate
	}
	if !kind.Valid() {
		return nil, domain.Invalid("kind",
			fmt.Sprintf("message kind %q is invalid", in.Kind),
			"Use one of: update, scope_change, question, command.")
	}
	recipient := strings.TrimSpace(in.Recipient)
	replyTo := strings.TrimSpace(in.ReplyTo)
	idemKey := strings.TrimSpace(in.IdempotencyKey)
	// A key that is only whitespace would trim to "" and silently disable
	// the deduplication the caller believes it enabled — the quiet downgrade
	// AGENTS.md forbids, refused here the same way as any other invalid key.
	if idemKey == "" && in.IdempotencyKey != "" {
		return nil, domain.Invalid("idempotency_key",
			"idempotency_key is empty after trimming",
			"Pass a non-blank key, or omit idempotency_key to send without retry protection.")
	}
	// The key is a lookup handle the client invents, not content — capped so
	// one argument cannot grow without bound; the schema publishes the same
	// number.
	if len(idemKey) > domain.MaxChatIdempotencyKeyLen {
		return nil, domain.Invalid("idempotency_key",
			fmt.Sprintf("idempotency_key is %d characters, the limit is %d", len(idemKey), domain.MaxChatIdempotencyKeyLen),
			fmt.Sprintf("Use a key of at most %d characters.", domain.MaxChatIdempotencyKeyLen))
	}
	// A key deduplicates retries of ONE authorized sender; without a token
	// identity there is nothing to key on, and silently skipping the
	// deduplication would be the quiet downgrade AGENTS.md forbids.
	if idemKey != "" && a.TokenID == "" {
		return nil, domain.Invalid("idempotency_key",
			"an idempotency key requires an authenticated token",
			"Post through an authenticated surface; the key is scoped to the sender's token.")
	}

	author := strings.TrimSpace(in.Author)
	if author == "" {
		author = a.Name
	}
	if author == "" {
		return nil, domain.Invalid("author", "author is required", "Pass an author or authenticate.")
	}
	if err := domain.ValidateActorName("author", author); err != nil {
		return nil, err
	}

	ctx = store.WithActor(ctx, a.Name)
	var out domain.ChatMessage
	var pending []domain.Event
	err = s.store.Write(ctx, func(tx store.Tx) error {
		p, err := s.resolveProject(tx, a, canonicalKey)
		if err != nil {
			return err
		}

		// Retry deduplication: the same key of the same authorized sender
		// with the same content returns the existing message; different
		// content is an explicit conflict. Checked before anything else so a
		// replay costs nothing but the lookup — and inside the transaction,
		// so a concurrent twin send cannot slip past the check.
		if idemKey != "" {
			existing, err := s.store.Chat().GetBySenderKey(tx, a.TokenID, idemKey)
			if err != nil && !isNotFound(err) {
				return err
			}
			if existing != nil {
				// The key's scope is the sender's token, but the message's
				// destination is part of its content: the same key aimed at a
				// different project asks to store a different message under an
				// identity that is already taken. Replying ok with the first
				// project's message id would silently drop this publication —
				// the second project's feed would stay empty while the caller
				// believes it was delivered — so it is refused as a mismatch,
				// exactly like any other content difference.
				if existing.ProjectID != p.ID {
					other := existing.ProjectID
					if otherProject, err := s.store.Projects().GetByID(tx, existing.ProjectID); err == nil {
						other = otherProject.Key
					}
					return &domain.Error{
						Code: domain.CodeIdempotencyMismatch,
						Message: fmt.Sprintf(
							"idempotency key %q was already used for a message in project %s", idemKey, other),
						Remediation: "Send to the original project to receive the original message, or use a new key for this project.",
					}
				}
				if existing.Kind != kind || existing.Recipient != recipient ||
					existing.ReplyToID != replyTo || existing.Body != in.Body {
					return &domain.Error{
						Code: domain.CodeIdempotencyMismatch,
						Message: fmt.Sprintf(
							"idempotency key %q was already used with a different message", idemKey),
						Remediation: "Reuse the original content to receive the original message, or send a new message with a new key.",
					}
				}
				out = *existing
				return nil
			}
		}

		executor, aerr := s.resolveMessageAddressing(tx, p, kind, recipient)
		if aerr != nil {
			return aerr
		}
		if replyTo != "" {
			target, err := s.store.Chat().Get(tx, replyTo)
			if isNotFound(err) {
				return domain.Invalid("reply_to",
					fmt.Sprintf("reply_to message %s does not exist", replyTo),
					"Pass the id of a message in the same project, taken from the feed.")
			}
			if err != nil {
				return err
			}
			if target.ProjectID != p.ID {
				return domain.Invalid("reply_to",
					fmt.Sprintf("reply_to message %s belongs to a different project", replyTo),
					"Pass the id of a message in the same project, taken from the feed.")
			}
		}

		m := &domain.ChatMessage{
			ID:        newID(),
			ProjectID: p.ID,
			Author:    author,
			Body:      in.Body,

			Kind:             kind,
			AuthorTokenID:    a.TokenID,
			Recipient:        recipient,
			ResolvedExecutor: executor,
			ReplyToID:        replyTo,
			IdempotencyKey:   idemKey,
		}
		if err := s.store.Chat().Add(tx, m); err != nil {
			return err
		}
		// The message body is never carried in the event payload — it lives
		// in chat_messages and is read from there (KANB-12): this is only the
		// "something changed" signal.
		if err := s.emit(tx, &pending, a.Name, domain.EventChatPosted, p.ID, nil, nil); err != nil {
			return err
		}
		out = *m
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishAll(pending)
	return &out, nil
}

// RecipientAll is the literal broadcast recipient on every wire surface.
const RecipientAll = "all"

// resolveMessageAddressing validates the recipient the caller named and
// fixes the single executor of a question or command AT SEND TIME (KANB-46).
//
// The rules, in the order a caller trips over them:
//   - "all" broadcasts: legal for updates and questions (a question to the
//     room needs no single answerer), refused for a command — a command
//     nobody can accept is undeliverable work, not a broadcast.
//   - an explicit token id must name an active participant of THIS project:
//     addressing is checked against the project's configuration, not the
//     sender's say-so.
//   - an absent recipient on a question/command means the coordinator, read
//     from the project as of this send. No coordinator configured (or a
//     broken appointment) refuses the send loudly: an assignment without an
//     addressee must not go out as executable.
//
// resolved_executor is non-empty only for a question/command that resolved
// to exactly one token. Updates and scope changes never carry one, and the
// value, once written, is never recomputed — a coordinator change later must
// not silently readdress old commands.
func (s *svc) resolveMessageAddressing(tx store.Tx, p *domain.Project, kind domain.MessageKind, recipient string) (string, error) {
	needsExecutor := kind == domain.MessageQuestion || kind == domain.MessageCommand

	switch {
	case recipient == RecipientAll:
		if kind == domain.MessageCommand {
			return "", domain.Invalid("recipient",
				"a command addressed to \"all\" has no executor to accept it",
				fmt.Sprintf("Pass one participant's tokens.id as recipient, or appoint settings.coordinator on project %s and omit recipient.", p.Key))
		}
		return "", nil

	case recipient != "":
		tok, err := s.store.Tokens().GetByID(tx, recipient)
		if isNotFound(err) {
			return "", domain.Invalid("recipient",
				fmt.Sprintf("recipient %s is not a known token", recipient),
				"Pass a participant's tokens.id from board_get(view:summary), or \"all\".")
		}
		if err != nil {
			return "", err
		}
		why, err := tokenDeadReason(tx, tok)
		if err != nil {
			return "", err
		}
		if why != "" {
			return "", domain.Invalid("recipient",
				fmt.Sprintf("recipient %s (%s) is %s", recipient, tok.Name, why),
				"Address an active participant.")
		}
		if !tok.MayAccessProject(p.Key) {
			return "", domain.Invalid("recipient",
				fmt.Sprintf("recipient %s (%s) has no access to project %s", recipient, tok.Name, p.Key),
				"Address a participant of this project, taken from board_get(view:summary).")
		}
		if needsExecutor {
			return recipient, nil
		}
		return "", nil

	default:
		if !needsExecutor {
			return "", nil
		}
		if p.CoordinatorTokenID == "" {
			return "", domain.Invalid("recipient",
				fmt.Sprintf("project %s has no coordinator, so a %s without a recipient has nowhere to go", p.Key, kind),
				"Pass an explicit recipient, or ask an admin to appoint settings.coordinator on the project.")
		}
		tok, err := s.store.Tokens().GetByID(tx, p.CoordinatorTokenID)
		if isNotFound(err) {
			return "", domain.Invalid("recipient",
				fmt.Sprintf("project %s's coordinator token no longer exists", p.Key),
				"Ask an admin to re-appoint settings.coordinator on the project.")
		}
		if err != nil {
			return "", err
		}
		why, err := tokenDeadReason(tx, tok)
		if err != nil {
			return "", err
		}
		if why != "" {
			return "", domain.Invalid("recipient",
				fmt.Sprintf("project %s's coordinator %s is %s", p.Key, tok.Name, why),
				"Ask an admin to re-appoint settings.coordinator on the project.")
		}
		if !tok.MayAccessProject(p.Key) {
			return "", domain.Invalid("recipient",
				fmt.Sprintf("project %s's coordinator %s has no access to the project", p.Key, tok.Name),
				"Ask an admin to re-appoint settings.coordinator on the project.")
		}
		return p.CoordinatorTokenID, nil
	}
}

// ChatList returns a page of chat messages, newest first. If in.ProjectKey is
// empty, messages across all accessible projects are returned. When in.Before
// or in.Cursor is given, it fetches older messages without skips or duplicates.
func (s *svc) ChatList(ctx context.Context, a Actor, in ChatListInput) (*ChatListResult, error) {
	if err := requireRead(a); err != nil {
		return nil, err
	}

	cursor := in.Before
	if cursor == nil && in.Cursor != "" {
		parsed, err := domain.ParseChatCursor(in.Cursor)
		if err != nil {
			return nil, err
		}
		cursor = parsed
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}

	ctx = store.WithActor(ctx, a.Name)
	var result ChatListResult
	err := s.store.Read(ctx, func(tx store.Tx) error {
		filter := store.ChatFilter{
			Limit:  limit + 1,
			Before: cursor,
		}

		if in.ProjectKey != "" {
			p, err := s.resolveProject(tx, a, in.ProjectKey)
			if err != nil {
				return err
			}
			filter.ProjectID = &p.ID
		} else if len(a.ProjectKeys) > 0 {
			var projectIDs []string
			for _, pk := range a.ProjectKeys {
				p, err := s.store.Projects().GetByKey(tx, pk)
				if err == nil && p != nil {
					projectIDs = append(projectIDs, p.ID)
				}
			}
			if len(projectIDs) == 0 {
				result.Messages = make([]domain.ChatMessage, 0)
				return nil
			}
			filter.ProjectIDs = projectIDs
		}

		msgs, err := s.store.Chat().List(tx, filter)
		if err != nil {
			return err
		}

		if len(msgs) > limit {
			msgs = msgs[:limit]
			last := msgs[len(msgs)-1]
			result.NextCursor = &domain.ChatCursor{
				CreatedAt: last.CreatedAt,
				ID:        last.ID,
			}
			result.Cursor = result.NextCursor.String()
		}
		if msgs == nil {
			msgs = make([]domain.ChatMessage, 0)
		}
		result.Messages = msgs
		// KANB-48: resolve the display data the raw rows only reference,
		// inside the same read so a page never names a token that vanished
		// or quotes a parent it does not have. Best-effort per entry the
		// same way the rest of the read path is: a resolution that cannot
		// complete simply leaves the field zero, never fakes a value.
		result.Meta = s.resolveChatListMeta(tx, msgs)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// ChatFeed reads a project's message feed FORWARD, oldest first, starting at
// the beginning of the available history (KANB-45). Without `after` the first
// page is the OLDEST messages — a consumer starting mid-history would miss the
// very commands written before it launched, which is the failure this read
// exists to prevent.
//
// The cursor is an opaque position, not a timestamp: the feed is ordered and
// paged by the insertion key (rowid), because created_at has millisecond
// precision and many messages land in the same millisecond — a
// timestamp-based cursor would depend on the random UUID id for tie-breaking
// and could permanently hide a message that arrived after the cursor was
// issued. Because chat history is never pruned, a cursor cannot outlive the
// history it points into — there is deliberately no expiry machinery here. A
// cursor that does not belong to this project, names a position that no
// longer holds its message, or does not parse, is rejected with a remediation
// instead of silently returning an empty page.
func (s *svc) ChatFeed(ctx context.Context, a Actor, in ChatFeedInput) (*ChatFeedResult, error) {
	if err := requireRead(a); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(in.ProjectKey)
	if key == "" {
		return nil, domain.Invalid("project", "project key is required for the messages view",
			"Pass the project whose feed you are reading.")
	}
	canonicalKey, err := domain.ValidateProjectKey(key)
	if err != nil {
		return nil, err
	}

	var cursor *domain.FeedCursor
	if in.After != "" {
		parsed, err := domain.ParseFeedCursor(in.After)
		if err != nil {
			return nil, err
		}
		cursor = parsed
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}

	ctx = store.WithActor(ctx, a.Name)
	var result ChatFeedResult
	err = s.store.Read(ctx, func(tx store.Tx) error {
		p, err := s.resolveProject(tx, a, canonicalKey)
		if err != nil {
			return err
		}
		// A foreign or stale cursor must be refused loudly: silently
		// returning an empty page would read as "no more messages" and the
		// consumer would stop, having skipped history it was entitled to.
		// The cursor's id names a real message in THIS project's feed — chat
		// is never pruned, so a missing one can only be a cursor from
		// somewhere else.
		if cursor != nil {
			anchor, err := s.store.Chat().GetBySeq(tx, cursor.Seq)
			if err != nil {
				return domain.Invalid("after",
					fmt.Sprintf("cursor message %s does not exist in project %s", cursor.ID, p.Key),
					"Use the next_cursor returned by a previous page of this project's feed, or omit after to read from the beginning.")
			}
			// The position exists but holds a different message: rows were
			// deleted and the position reused underneath this cursor. Chat is
			// never pruned in normal operation, so this is a stale or forged
			// position — refuse it instead of silently paging from the wrong
			// place.
			if anchor.ID != cursor.ID {
				return domain.Invalid("after",
					fmt.Sprintf("cursor is stale: position %d holds message %s, not %s", cursor.Seq, anchor.ID, cursor.ID),
					"Omit after to read this project's feed from the beginning.")
			}
			if anchor.ProjectID != p.ID {
				return domain.Invalid("after",
					fmt.Sprintf("cursor message %s belongs to a different project", cursor.ID),
					"Use the next_cursor returned by a previous page of this project's feed, or omit after to read from the beginning.")
			}
		}

		msgs, err := s.store.Chat().List(tx, store.ChatFilter{
			ProjectID: &p.ID,
			Ascending: true,
			After:     cursor,
			Limit:     limit + 1,
		})
		if err != nil {
			return err
		}
		if len(msgs) > limit {
			result.HasMore = true
			msgs = msgs[:limit]
		}
		if len(msgs) > 0 {
			last := msgs[len(msgs)-1]
			result.NextCursor = (&domain.FeedCursor{Seq: last.Seq, ID: last.ID}).String()
		} else if in.After != "" {
			// Empty page past a position: echo the requested cursor back.
			// The documented polling loop is `cursor = next_cursor`; leaving
			// the field empty here would reset the consumer to the beginning
			// of the history on its next poll and re-deliver everything
			// already seen. An empty FIRST page (in.After == "") has no
			// position to keep, and "" stays the honest "start from the top"
			// value.
			result.NextCursor = in.After
		}

		names := make(map[string]string)
		toks, err := s.store.Tokens().List(tx)
		if err != nil {
			return err
		}
		for _, t := range toks {
			names[t.ID] = t.Name
		}

		ids := make([]string, 0, len(msgs))
		for _, m := range msgs {
			ids = append(ids, m.ID)
		}
		acceptances, err := s.store.Acceptances().ByMessages(tx, ids)
		if err != nil {
			return err
		}

		result.Messages = make([]ChatFeedMessage, 0, len(msgs))
		for _, m := range msgs {
			fm := ChatFeedMessage{
				Message:  m,
				TaskKeys: acceptances[m.ID].TaskKeys,
			}
			if m.Recipient != "" && m.Recipient != RecipientAll {
				fm.RecipientName = names[m.Recipient]
			}
			if m.ResolvedExecutor != "" {
				fm.ResolvedExecutorName = names[m.ResolvedExecutor]
			}
			fm.AuthorVia = AuthorVia(m, names)
			result.Messages = append(result.Messages, fm)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// AuthorVia is the name of the token that posted m when it differs from the
// signature m.Author, and "" otherwise. The signature is free text a sender
// chooses — an orchestrator relaying the owner's words legitimately signs as
// the owner — so every surface that shows it shows this next to it: a reader
// must never take "admin" for the admin when some other key wrote it. ""
// also covers a row that predates token attribution and a token that no
// longer resolves, where there is nothing true to add. names maps tokens.id
// to the token's name. Case alone is not a difference worth flagging.
func AuthorVia(m domain.ChatMessage, names map[string]string) string {
	if m.AuthorTokenID == "" {
		return ""
	}
	name := names[m.AuthorTokenID]
	if name == "" || strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(m.Author)) {
		return ""
	}
	return name
}

// ChatMessageAdd is an alias for ChatAdd.
func (s *svc) ChatMessageAdd(ctx context.Context, a Actor, in ChatAddInput) (*domain.ChatMessage, error) {
	return s.ChatAdd(ctx, a, in)
}

// resolveChatListMeta builds ChatListResult.Meta for one page: the display
// names of every recipient/executor the page addresses, the parent message
// behind every reply (the short quote the UI shows), and the acceptance of
// every command that HAS one (KANB-48). All lookups are batched — one token
// list, one acceptance query for the whole page, one point lookup per
// distinct parent — and everything happens inside the caller's read
// transaction so the page cannot mix rows from two moments.
//
// Zero values are information here, not failures: a recipient id that no
// longer resolves (a revoked-and-deleted token) leaves RecipientName empty,
// and the UI shows the raw id rather than inventing a name.
func (s *svc) resolveChatListMeta(tx store.Tx, msgs []domain.ChatMessage) map[string]ChatListEntryMeta {
	meta := make(map[string]ChatListEntryMeta, len(msgs))
	if len(msgs) == 0 {
		return meta
	}
	for _, m := range msgs {
		meta[m.ID] = ChatListEntryMeta{}
	}

	names := make(map[string]string)
	toks, err := s.store.Tokens().List(tx)
	if err == nil {
		for _, t := range toks {
			names[t.ID] = t.Name
		}
	}

	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	acceptances, err := s.store.Acceptances().ByMessages(tx, ids)
	if err != nil {
		acceptances = nil
	}

	parents := make(map[string]*domain.ChatMessage)
	for _, m := range msgs {
		if m.ReplyToID == "" {
			continue
		}
		if _, seen := parents[m.ReplyToID]; seen {
			continue
		}
		var p *domain.ChatMessage
		if parent, err := s.store.Chat().Get(tx, m.ReplyToID); err == nil && parent != nil {
			p = parent
		}
		parents[m.ReplyToID] = p
	}

	for _, m := range msgs {
		e := meta[m.ID]
		if m.Recipient != "" && m.Recipient != RecipientAll {
			e.RecipientName = names[m.Recipient]
		}
		if m.ResolvedExecutor != "" {
			e.ExecutorName = names[m.ResolvedExecutor]
		}
		e.AuthorVia = AuthorVia(m, names)
		e.Parent = parents[m.ReplyToID]
		if e.Parent != nil {
			e.ParentAuthorVia = AuthorVia(*e.Parent, names)
		}
		if a, ok := acceptances[m.ID]; ok {
			acc := a
			e.Acceptance = &acc
		}
		meta[m.ID] = e
	}
	return meta
}

// ChatMessageList is an alias for ChatList.
func (s *svc) ChatMessageList(ctx context.Context, a Actor, in ChatListInput) (*ChatListResult, error) {
	return s.ChatList(ctx, a, in)
}

// ChatGet is an alias for ChatList.
func (s *svc) ChatGet(ctx context.Context, a Actor, in ChatListInput) (*ChatListResult, error) {
	return s.ChatList(ctx, a, in)
}
