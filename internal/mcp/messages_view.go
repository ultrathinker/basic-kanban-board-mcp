package mcp

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// The messages view of board_get (KANB-45): a forward, cursor-paged read of
// one project's communication feed. It is a VIEW of the existing board_get
// tool, not a tenth tool — the response carries messages only, never the
// board or task bodies, so reading the feed never pays for the board.

// feedMessageOut is the wire form of one feed message. Token ids are the
// addressing contract (author_token_id, recipient, resolved_executor carry
// tokens.id values or the literal "all"); the *_name fields are resolved
// display names for humans reading the JSON.
type feedMessageOut struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at" jsonschema:"server time (RFC3339) — the ordering authority of the feed"`
	// Author is the display name the sender signed with; AuthorTokenID is the
	// AUTHORIZED source: the tokens.id of the token that posted, which the
	// client never chooses. The two can differ and the id is the truth.
	Author      string   `json:"author"`
	AuthorToken string   `json:"author_token_id,omitempty"`
	Kind        string   `json:"kind" jsonschema:"update | scope_change | question | command"`
	Recipient   string   `json:"recipient,omitempty" jsonschema:"tokens.id the sender addressed, or \"all\"; empty = the coordinator default"`
	RecipientNm string   `json:"recipient_name,omitempty"`
	Executor    string   `json:"resolved_executor,omitempty" jsonschema:"tokens.id fixed at send time; a later coordinator change never readdresses an old command"`
	ExecutorNm  string   `json:"resolved_executor_name,omitempty"`
	ReplyTo     string   `json:"reply_to,omitempty" jsonschema:"id of the message this one answers, same project"`
	Body        string   `json:"body"`
	TaskKeys    []string `json:"task_keys,omitempty" jsonschema:"tasks created by accepting this command message"`
}

// boardMessagesData is the data block board_get returns for view:"messages".
type boardMessagesData struct {
	Project  string           `json:"project"`
	Messages []feedMessageOut `json:"messages"`
	// NextCursor deliberately carries no omitempty: a polling consumer does
	// `cursor = next_cursor` and re-asks. If the field vanished on an empty
	// page, the consumer would read "" and restart from the beginning of the
	// history, re-delivering everything it already saw. On an empty page the
	// service echoes the requested after back; "" means only "the feed is
	// empty from the requested position on" (an empty first page).
	NextCursor string `json:"next_cursor" jsonschema:"opaque position: pass back as after for the next page; present on every page — on an empty page it echoes the requested after so a polling consumer never loses its position; empty only before the first message"`
	HasMore    bool   `json:"has_more"`
}

func feedMessagesOut(res *service.ChatFeedResult) []feedMessageOut {
	out := make([]feedMessageOut, 0, len(res.Messages))
	for i := range res.Messages {
		fm := &res.Messages[i]
		out = append(out, feedMessageOut{
			ID:          fm.Message.ID,
			CreatedAt:   formatTime(fm.Message.CreatedAt),
			Author:      fm.Message.Author,
			AuthorToken: fm.Message.AuthorTokenID,
			Kind:        string(fm.Message.Kind),
			Recipient:   fm.Message.Recipient,
			RecipientNm: fm.RecipientName,
			Executor:    fm.Message.ResolvedExecutor,
			ExecutorNm:  fm.ResolvedExecutorName,
			ReplyTo:     fm.Message.ReplyToID,
			Body:        fm.Message.Body,
			TaskKeys:    fm.TaskKeys,
		})
	}
	return out
}

// renderMessagesFeed is the text rendering for view:"messages": one line per
// message, oldest first, plus a footer carrying the cursor so a text-only
// consumer can page without parsing the structured form. It is deliberately
// NOT the compact board grammar — there are no task lines here — but it keeps
// the same conventions: line-oriented, ` · ` separators, labelled suffixes.
func renderMessagesFeed(d *boardMessagesData) string {
	var sb strings.Builder
	sb.WriteString(versionHeader())
	sb.WriteByte('\n')
	tail := ""
	if d.HasMore {
		tail = " · more"
	}
	fmt.Fprintf(&sb, "# %s messages %d%s\n", d.Project, len(d.Messages), tail)
	for i := range d.Messages {
		m := &d.Messages[i]
		fmt.Fprintf(&sb, "- %s %s %s: %s", m.CreatedAt, m.Kind, m.Author, feedBodyExcerpt(m.Body))
		if m.Recipient != "" {
			label := m.Recipient
			if m.RecipientNm != "" {
				label = m.RecipientNm
			}
			fmt.Fprintf(&sb, " · to %s", label)
		}
		if m.Executor != "" {
			label := m.Executor
			if m.ExecutorNm != "" {
				label = m.ExecutorNm
			}
			fmt.Fprintf(&sb, " · executor %s", label)
		}
		if m.ReplyTo != "" {
			fmt.Fprintf(&sb, " · reply_to %s", m.ReplyTo)
		}
		if len(m.TaskKeys) > 0 {
			fmt.Fprintf(&sb, " · tasks %s", strings.Join(m.TaskKeys, ","))
		}
		sb.WriteByte('\n')
	}
	// The footer prints whenever a position exists. It is absent only on an
	// empty first page — the one case where "no cursor" cannot lose data,
	// because there is nothing after the start to re-read. Once the service
	// echoes the position back on empty pages (see boardMessagesData), a
	// consumer that already holds a cursor always sees it again here.
	if d.NextCursor != "" {
		fmt.Fprintf(&sb, "next_cursor %s\n", d.NextCursor)
	}
	return sb.String()
}

// feedBodyExcerpt collapses a body onto one lexable line and clips it, so a
// long message cannot blow up the text form's budget. The structured form
// always carries the body whole.
func feedBodyExcerpt(body string) string {
	if body == "" {
		return "(empty)"
	}
	collapsed := strings.Join(strings.FieldsFunc(body, unicode.IsSpace), " ")
	const max = 160
	if len(collapsed) > max {
		return collapsed[:max] + "…"
	}
	return collapsed
}
