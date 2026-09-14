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
func (s *svc) ChatAdd(ctx context.Context, a Actor, in ChatAddInput) (*domain.ChatMessage, error) {
	if err := requireWrite(a); err != nil {
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
		m := &domain.ChatMessage{
			ID:        newID(),
			ProjectID: p.ID,
			Author:    author,
			Body:      in.Body,
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
// The cursor is an opaque position, not a timestamp: several messages can
// share one created_at, so the cursor carries (created_at, id) and pages break
// ties deterministically without gaps or duplicates. Because chat history is
// never pruned, a cursor cannot outlive the history it points into — there is
// deliberately no expiry machinery here. A cursor that does not belong to
// this project (or does not parse) is rejected with a remediation instead of
// silently returning an empty page.
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

	var cursor *domain.ChatCursor
	if in.After != "" {
		parsed, err := domain.ParseChatCursor(in.After)
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
			anchor, err := s.store.Chat().Get(tx, cursor.ID)
			if err != nil {
				return domain.Invalid("after",
					fmt.Sprintf("cursor message %s does not exist in project %s", cursor.ID, p.Key),
					"Use the next_cursor returned by a previous page of this project's feed, or omit after to read from the beginning.")
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
			result.NextCursor = (&domain.ChatCursor{CreatedAt: last.CreatedAt, ID: last.ID}).String()
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
			if m.Recipient != "" && m.Recipient != recipientAll {
				fm.RecipientName = names[m.Recipient]
			}
			if m.ResolvedExecutor != "" {
				fm.ResolvedExecutorName = names[m.ResolvedExecutor]
			}
			result.Messages = append(result.Messages, fm)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// recipientAll is the literal broadcast recipient on the wire.
const recipientAll = "all"

// ChatMessageAdd is an alias for ChatAdd.
func (s *svc) ChatMessageAdd(ctx context.Context, a Actor, in ChatAddInput) (*domain.ChatMessage, error) {
	return s.ChatAdd(ctx, a, in)
}

// ChatMessageList is an alias for ChatList.
func (s *svc) ChatMessageList(ctx context.Context, a Actor, in ChatListInput) (*ChatListResult, error) {
	return s.ChatList(ctx, a, in)
}

// ChatGet is an alias for ChatList.
func (s *svc) ChatGet(ctx context.Context, a Actor, in ChatListInput) (*ChatListResult, error) {
	return s.ChatList(ctx, a, in)
}
