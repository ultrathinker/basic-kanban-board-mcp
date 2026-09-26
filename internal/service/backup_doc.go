package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ExportDocument is the JSON wire format of a board backup: what `kanban
// export`, the web Export buttons and `kanban import` all speak. It lives in
// the service, not next to either surface, because it used to exist twice —
// once in cmd/kanban and once, hand-copied and already drifting, in the web
// package — and the web copy had quietly become a document the strict
// importer refused (KANB-70). One type and one builder (Service.Export) make
// "the web export is the CLI export" true by construction.
//
// It carries the same Projects payload as Board, plus the append-only streams
// Board never carries, all re-keyed to project and task KEYS (not internal
// ids) so they survive the re-keying import does. Board stays lean on
// purpose: those streams grow forever, and board_get is read every session.
type ExportDocument struct {
	Projects      []BoardProject       `json:"projects"`
	ProgressMarks []ExportProgressMark `json:"progress_marks"`
	// ChatMessages is every project's feed in feed order (oldest first, by
	// the source's Seq). Exports made before KANB-70 wrote it newest first
	// and without seq; import recognises those by the missing seq.
	ChatMessages []ExportChatMessage `json:"chat_messages"`
	// Journal is the task lifecycle journal (KANB-30), re-keyed: project and
	// task keys instead of ids, column names instead of column ids. Without it
	// a moved board would zero out every history chart.
	Journal []ExportJournalEntry `json:"journal"`
	// HistoryStartsAt is the instant from which the source's journal is
	// trustworthy. Import lowers the destination's origin to it, so a board
	// moved to a new machine does not claim its history began on moving day.
	HistoryStartsAt time.Time `json:"history_starts_at"`

	// ArchivedTasks carries every archived card of each exported project.
	// board_get never returns one, but the journal, a progress mark, a note or
	// a chat message may still name it (KANB-62).
	ArchivedTasks []ExportArchivedTask `json:"archived_tasks,omitempty"`

	// NextTaskSeq is each project's next card number, by project key, so a
	// number freed by a hard delete is not handed out again (KANB-66).
	// Absent in older exports: import then falls back to highest key + 1.
	NextTaskSeq map[string]int `json:"next_task_seq,omitempty"`

	// Notes carries every note of every exported card, live or archived,
	// with its original author and time (KANB-65).
	Notes []ExportNote `json:"notes,omitempty"`

	// CommandAcceptances records which commands were already turned into
	// tasks, and by whom (KANB-47). Without it a restored command that was
	// accepted long ago would be open for acceptance a second time.
	CommandAcceptances []ExportCommandAcceptance `json:"command_acceptances,omitempty"`
}

// ExportNote is one card note, re-keyed like the other history streams.
type ExportNote struct {
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	Task      string    `json:"task"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// ExportArchivedTask is one archived card, in the same shape board_get uses
// for a live one so import restores both through the same code.
type ExportArchivedTask struct {
	Project string          `json:"project"`
	Task    domain.TaskView `json:"task"`
}

// ExportProgressMark keeps the mark's own id, so the restored mark has the
// same primary key as the source and a diff between the two is trivial.
type ExportProgressMark struct {
	ID        string     `json:"id"`
	Project   string     `json:"project"`
	Task      string     `json:"task,omitempty"`
	Assessor  string     `json:"assessor"`
	Percent   int        `json:"percent"`
	ETA       *time.Time `json:"eta,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// ExportChatMessage is one feed message with its whole communication
// protocol (KANB-45..47). Before KANB-70 only id, author, body and time
// travelled, so a restored command had lost its kind and its addressee and
// could no longer be accepted by anyone.
//
// The token ids are carried verbatim. Recipient and ResolvedExecutor are
// plain values, so they are restored as they were: a command stays addressed
// to the executor it was frozen on at send time, and becomes acceptable again
// the moment that executor's key exists on the destination board.
// AuthorTokenID is a foreign key, so import keeps it only when that token
// exists on the destination.
type ExportChatMessage struct {
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`

	Kind             domain.MessageKind `json:"kind,omitempty"`
	AuthorTokenID    string             `json:"author_token_id,omitempty"`
	Recipient        string             `json:"recipient,omitempty"`
	ResolvedExecutor string             `json:"resolved_executor,omitempty"`
	// ReplyTo names the answered message by its id. Message ids are kept on
	// import, so the reference needs no translation; import only checks that
	// the answered message comes earlier in the same project.
	ReplyTo        string `json:"reply_to,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// Seq is the message's position in the source feed. It orders the import
	// (a fresh board assigns fresh positions, in insertion order) and marks
	// the document as written after KANB-70.
	Seq int64 `json:"seq,omitempty"`
}

// ExportCommandAcceptance is one "command -> tasks -> acceptor" record.
// AcceptedBy is a tokens.id; the tasks are named by key and re-resolved.
type ExportCommandAcceptance struct {
	ID          string    `json:"id"`
	Project     string    `json:"project"`
	Message     string    `json:"message"`
	AcceptedBy  string    `json:"accepted_by"`
	TaskKeys    []string  `json:"task_keys"`
	RequestHash string    `json:"request_hash"`
	CreatedAt   time.Time `json:"created_at"`
}

// ExportJournalEntry is one row of the task lifecycle journal (KANB-30). The
// row id is not carried: nothing references it, so the destination assigns
// its own.
type ExportJournalEntry struct {
	Project       string    `json:"project"`
	Task          string    `json:"task,omitempty"`
	Actor         string    `json:"actor"`
	Kind          string    `json:"kind"`
	TS            time.Time `json:"ts"`
	Reconstructed bool      `json:"reconstructed"`
	Archived      *bool     `json:"archived,omitempty"`
	FromColumn    string    `json:"from_column,omitempty"`
	FromKind      string    `json:"from_kind,omitempty"`
	ToColumn      string    `json:"to_column,omitempty"`
	ToKind        string    `json:"to_kind,omitempty"`
	OldEstimate   *float64  `json:"old_estimate,omitempty"`
	NewEstimate   *float64  `json:"new_estimate,omitempty"`
	OldParent     string    `json:"old_parent,omitempty"`
	NewParent     string    `json:"new_parent,omitempty"`
}

// ImportResult reports what an import restored and what it could not.
type ImportResult struct {
	// Projects are the keys created, in document order.
	Projects []string
	// Warnings name every value the destination board could not take as it
	// was — a coordinator whose token does not exist here, a message author
	// token that is unknown here — each saying what was restored instead.
	// The import still succeeded; these are the honest footnotes.
	Warnings []string
}

// DecodeExportDocument parses an export the way `kanban import` must: the
// retired per-column WIP limit is stripped first (so every backup taken
// before its removal still imports), then the document is decoded as a
// closed contract. dropped is the number of columns that carried a real WIP
// limit, which the caller reports rather than dropping silently.
func DecodeExportDocument(raw []byte) (doc ExportDocument, dropped int, err error) {
	raw, dropped, err = StripLegacyWIPLimits(raw)
	if err != nil {
		return ExportDocument{}, 0, fmt.Errorf("import parse: %w", err)
	}
	doc, err = DecodeStrictExportDocument(raw)
	return doc, dropped, err
}

// DecodeStrictExportDocument treats an export as a closed wire contract.
// Missing newer fields remain compatible with old exports; an unknown field
// is a typo and must not silently discard history.
func DecodeStrictExportDocument(raw []byte) (ExportDocument, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc ExportDocument
	if err := dec.Decode(&doc); err != nil {
		return ExportDocument{}, fmt.Errorf("import parse: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return ExportDocument{}, fmt.Errorf("import parse: multiple JSON documents are not allowed")
		}
		return ExportDocument{}, fmt.Errorf("import parse: %w", err)
	}
	return doc, nil
}

// StripLegacyWIPLimits removes the retired per-column WIP limit from an
// export made before 26.09.2026, when the board still had WIP limits
// (KANB-59). The strict decoder refuses unknown fields, so without this every
// backup taken before the removal would stop importing. Old exports spell the
// field the way encoding/json wrote an untagged struct field ("WIPLimit",
// null on every column without a limit); the match ignores case and
// underscores so "wip_limit" is covered too. The returned count is the
// columns that carried a real (non-null) limit.
func StripLegacyWIPLimits(raw []byte) ([]byte, int, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Not an object at all: let the strict decoder produce the error.
		return raw, 0, nil
	}
	projKey, ok := foldKey(doc, "projects")
	if !ok {
		return raw, 0, nil
	}
	var projects []map[string]json.RawMessage
	if json.Unmarshal(doc[projKey], &projects) != nil {
		return raw, 0, nil
	}
	stripped, limited := 0, 0
	for _, proj := range projects {
		colKey, ok := foldKey(proj, "columns")
		if !ok {
			continue
		}
		var cols []map[string]json.RawMessage
		if json.Unmarshal(proj[colKey], &cols) != nil {
			continue
		}
		changed := false
		for _, col := range cols {
			for k, v := range col {
				if strings.ReplaceAll(strings.ToLower(k), "_", "") != "wiplimit" {
					continue
				}
				if s := strings.TrimSpace(string(v)); s != "" && s != "null" {
					limited++
				}
				delete(col, k)
				stripped++
				changed = true
			}
		}
		if changed {
			b, err := json.Marshal(cols)
			if err != nil {
				return nil, 0, err
			}
			proj[colKey] = b
		}
	}
	if stripped == 0 {
		return raw, 0, nil
	}
	b, err := json.Marshal(projects)
	if err != nil {
		return nil, 0, err
	}
	doc[projKey] = b
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, 0, err
	}
	return out, limited, nil
}

// foldKey finds the key in m that encoding/json would match to name
// (case-insensitively), the way the strict decoder resolves it.
func foldKey(m map[string]json.RawMessage, name string) (string, bool) {
	if _, ok := m[name]; ok {
		return name, true
	}
	for k := range m {
		if strings.EqualFold(k, name) {
			return k, true
		}
	}
	return "", false
}
