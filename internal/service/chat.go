package service

import (
	"context"
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
		out = *m
		return nil
	})
	if err != nil {
		return nil, err
	}
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
