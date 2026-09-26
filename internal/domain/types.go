// Package domain holds the entities, enums and pure rules of the board.
// It has no I/O: no database, no HTTP, no MCP. Everything here must be testable
// with plain Go values so the rules that define the product can be verified fast.
package domain

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Kind classifies a column and drives task_next candidacy, "done" semantics and the
// hide-done toggle in the UI.
type Kind string

const (
	KindBacklog Kind = "backlog"
	KindActive  Kind = "active"
	KindDone    Kind = "done"
	// KindWaiting marks a column as parked rather than active work (KANB-52):
	// visible and counted as open, but not a task_next candidate, and not a source of "done" for anything
	// it blocks. It exists so a project can name that state explicitly
	// instead of overloading an active column for it (the workaround this
	// card replaces). No project gets one by default, and no existing
	// column's kind changes because of this constant: a column becomes
	// "waiting" only when a caller sets it explicitly.
	KindWaiting Kind = "waiting"
)

// AllKinds is every valid column kind, in the order the wire enum should
// list them. This is the single source of truth Kind.Valid() reduces to and
// the MCP schema's "kind" enum (internal/mcp/tool_project_upsert.go) is
// built from — the project already lived through "the docs say nine tools,
// there are twelve" once (KANB-... the tool-count drift), and a column kind
// enum copied by hand into a schema is the exact same disease: add a kind
// here and forget the other list, and the new kind is valid everywhere
// except the one surface that would let a caller actually set it.
var AllKinds = []Kind{KindBacklog, KindActive, KindDone, KindWaiting}

func (k Kind) Valid() bool {
	for _, v := range AllKinds {
		if v == k {
			return true
		}
	}
	return false
}

// Type is the task type. One spelling everywhere: the API, the compact
// rendering and the UI all use these exact strings, so an agent can echo back
// what it read without hitting a validation error.
type Type string

const (
	TypeTask     Type = "task"
	TypeBug      Type = "bug"
	TypeFeat     Type = "feat"
	TypeChore    Type = "chore"
	TypeDoc      Type = "doc"
	TypePerf     Type = "perf"
	TypeResearch Type = "research"
)

var AllTypes = []Type{TypeTask, TypeBug, TypeFeat, TypeChore, TypeDoc, TypePerf, TypeResearch}

func (t Type) Valid() bool {
	for _, v := range AllTypes {
		if v == t {
			return true
		}
	}
	return false
}

// Priority is stored as an int for cheap sorting and threshold filtering, but
// every external surface (MCP params, compact text, UI) speaks the names.
type Priority int

const (
	PriorityNone Priority = iota
	PriorityLow
	PriorityMedium
	PriorityHigh
	PriorityCritical
)

var priorityNames = [...]string{"none", "low", "medium", "high", "critical"}

func (p Priority) Valid() bool { return p >= PriorityNone && p <= PriorityCritical }

// String returns the canonical name used on every external surface.
func (p Priority) String() string {
	if !p.Valid() {
		return "none"
	}
	return priorityNames[p]
}

// ParsePriority accepts the canonical names case-insensitively.
func ParsePriority(s string) (Priority, bool) {
	for i, n := range priorityNames {
		if equalFold(s, n) {
			return Priority(i), true
		}
	}
	return PriorityNone, false
}

// Outcome records the epistemic status of a task's result — kept separate from
// the column the task sits in. A task can be Done yet have an outcome of
// "refuted" (the conclusion turned out wrong) or "moot" (the work was not
// needed after all). This is the signal a research agent asked for: "done"
// alone cannot say whether what was done still holds. "open" is the default
// until the result is judged, so every existing task is valid without backfill.
type Outcome string

const (
	OutcomeOpen       Outcome = "open"       // not yet judged
	OutcomeHolds      Outcome = "holds"      // the result stands / was confirmed
	OutcomeRefuted    Outcome = "refuted"    // the conclusion turned out wrong
	OutcomeSuperseded Outcome = "superseded" // replaced by a better result
	OutcomeMoot       Outcome = "moot"       // the work turned out unneeded
)

var AllOutcomes = []Outcome{OutcomeOpen, OutcomeHolds, OutcomeRefuted, OutcomeSuperseded, OutcomeMoot}

func (o Outcome) Valid() bool {
	for _, v := range AllOutcomes {
		if v == o {
			return true
		}
	}
	return false
}

// ParseOutcome accepts the canonical names case-insensitively.
func ParseOutcome(s string) (Outcome, bool) {
	for _, v := range AllOutcomes {
		if equalFold(s, string(v)) {
			return v, true
		}
	}
	return OutcomeOpen, false
}

// LinkType is the typed dependency edge. v1 ships "blocks" only; the column
// exists so adding "relates"/"duplicates" later is data, not a migration of
// meaning.
type LinkType string

const LinkBlocks LinkType = "blocks"

func (l LinkType) Valid() bool { return l == LinkBlocks }

// Scope is a token capability. write implies read; admin implies both.
//
// executor sits between read and write (KANB-60): it reads everything read
// does, but writes only to the cards assigned to the token's own name, and
// never across the done boundary. It exists so an orchestrator can hand each
// agent it launches a key of its own without handing it the power to accept
// its own work — the reviewer closes, not the executor. write implies it:
// every executor capability is a write capability narrowed to one's own cards.
type Scope string

const (
	ScopeRead     Scope = "read"
	ScopeExecutor Scope = "executor"
	ScopeWrite    Scope = "write"
	ScopeAdmin    Scope = "admin"
)

func (s Scope) Valid() bool {
	switch s {
	case ScopeRead, ScopeExecutor, ScopeWrite, ScopeAdmin:
		return true
	}
	return false
}

// Scopes is the set carried by a token.
type Scopes []Scope

func (ss Scopes) Has(want Scope) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
		if s == ScopeAdmin {
			return true // admin implies everything
		}
		if s == ScopeWrite && (want == ScopeRead || want == ScopeExecutor) {
			return true // write implies read and every executor capability
		}
		if s == ScopeExecutor && want == ScopeRead {
			return true // executor implies read
		}
	}
	return false
}

// OwnCardsOnly reports whether the set confines writes to the caller's own
// cards: it carries the executor capability but nothing wider. A token that
// also holds write is a writer, not an executor.
func (ss Scopes) OwnCardsOnly() bool {
	return ss.Has(ScopeExecutor) && !ss.Has(ScopeWrite)
}

// Project settings and identity. Key is immutable: task keys derive from it.
type Project struct {
	ID                  string
	Key                 string // [A-Z][A-Z0-9]{1,7}
	Name                string
	Description         string
	Version             int // bumps on project/column configuration changes
	NextTaskSeq         int
	FocusTaskID         *string
	EstimateUnit        string // default "h"
	EnforceDependencies bool
	StrictDone          bool
	ClaimTTLSeconds     int // clamped to [ClaimTTLMin, ClaimTTLMax]
	// IdleAfterSeconds is how long a card in an active column may go without
	// movement before board_get's attention line names it (KANB-67). 0 = not
	// set: the threshold is then the claim TTL, as it was before the setting
	// existed. A lease and "this card looks abandoned" are different
	// questions — a review cycle measured in days would otherwise light the
	// line up every hour.
	IdleAfterSeconds int
	// CoordinatorTokenID names the project's coordinator (KANB-44). It is a
	// tokens.id, never a display name: the id is the stable identity that
	// survives secret rotation, while a name is a label that can be reused by
	// a different token. Empty = no coordinator appointed.
	CoordinatorTokenID string
	ArchivedAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Column belongs to a project. Names are unique per project because tools
// address columns by name.
type Column struct {
	ID        string
	ProjectID string
	Name      string
	Position  int
	Kind      Kind
	// There is no WIP limit: the owner removed limits from the board on
	// 26.09.2026 (KANB-59). A limit only ever caught "the board is lying"
	// for AI agents, and a hard refusal blocked an honest move to protest
	// OTHER, stale cards — idle time does that job now. The wip_limit column
	// is still in the database, unread: dropping it would mean rebuilding the
	// table on a live board for nothing.
}

// AcceptanceItem is one checkbox on a task.
type AcceptanceItem struct {
	Text string
	Done bool
}

// Task is the unit of work. Key (PROJ-N) is the handle everywhere: agents and
// humans reference it in prose, so no surface exposes the UUID as primary.
type Task struct {
	ID        string
	Key       string // PROJ-N, immutable
	ProjectID string
	ColumnID  string
	ParentID  *string // subtask; depth capped at MaxSubtaskDepth
	Rank      int64   // sparse integer, step RankStep; ties broken by Key

	Title    string
	Body     string
	Type     Type
	Priority Priority
	Estimate *float64
	// Actual is the effort a task really took, in the same unit as Estimate.
	// It is recorded after the fact (usually via task_update when the work is
	// done), so it is separate from Estimate rather than overwriting it: the
	// gap between the two is the calibration signal the board exists to expose.
	Actual   *float64
	Tags     []string // lowercase, no spaces
	Assignee *string  // free text: human or agent name
	// Reviewer is who is expected to check the work, kept distinct from
	// Assignee (who does it): a research task moving to Done through a review
	// column needs both to be nameable independently.
	Reviewer *string
	// Outcome is the epistemic status of the task's result (see Outcome). It is
	// independent of ColumnKind: a Done task may still be refuted or moot.
	// Defaults to OutcomeOpen.
	Outcome Outcome
	// Conclusion is the post-hoc takeaway — what was actually learned or
	// decided — kept distinct from Body (the brief written up front) and from
	// Notes (the running log an agent appends as it works). Empty until written.
	Conclusion string

	ClaimedBy       *string
	ClaimedAt       *time.Time
	ClaimExpiresAt  *time.Time
	Acceptance      []AcceptanceItem
	DueAt           *time.Time
	ColumnEnteredAt time.Time
	StartedAt       *time.Time
	DoneAt          *time.Time

	Version  int // optimistic concurrency; see VersionBumping in rules.go
	Metadata map[string]any

	CreatedAt time.Time
	UpdatedAt time.Time
	CreatedBy string // actor = token name
	UpdatedBy string

	ArchivedAt *time.Time
}

// TaskView is a Task plus everything derived that callers need but the store
// does not persist. Read paths return this; write paths accept Task fields.
type TaskView struct {
	Task
	ProjectKey string
	// EstimateUnit is the owning project's unit (e.g. "h"), copied onto the
	// view so a per-task read can render "3.5h" without the caller having to
	// fetch the project separately. Read paths populate it; it is never stored
	// on the task row.
	EstimateUnit string
	ColumnName   string
	ColumnKind   Kind
	BlockedBy    []string // keys of OPEN blockers only, sorted
	Blocks       []string // keys this task blocks, sorted
	SubDone      int
	SubTotal     int
	Ready        bool // no open blockers, no incomplete subtasks, claimable
	LeaseRemain  *time.Duration
	Notes        []Note // populated only when explicitly included
}

// Link is a typed dependency edge: Blocker must be done before Blocked may
// enter an active column (when the project enforces dependencies).
type Link struct {
	BlockerID string
	BlockedID string
	Type      LinkType
	CreatedAt time.Time
	CreatedBy string
}

// Note is an agent's working log entry. Append-only; never bumps task Version.
type Note struct {
	ID        string
	TaskID    string
	Author    string
	Body      string
	CreatedAt time.Time
}

// ProgressMark is one progress estimate of a task, or of the whole project
// when TaskID is nil. Append-only: an assessor revises its answer by adding
// a new mark, never by editing an old one, and nothing prunes the history —
// the trend is the data. Assessor is the estimating agent's self-declared
// name; it is not checked against any registry, because any agent may assess.
type ProgressMark struct {
	ID        string
	ProjectID string
	TaskID    *string // nil = an estimate of the project as a whole
	Assessor  string
	Percent   int // 0..100; a CHECK in migration 0005 is the backstop
	// ETA is the optional forecast of when the assessed work will finish.
	ETA       *time.Time
	CreatedAt time.Time
}

// MessageKind classifies a chat message for the communication protocol
// (KANB-45..47): plain updates, scope changes, questions and commands. The
// default everywhere is update — a caller that sends none of the new
// arguments behaves exactly as before the protocol existed.
type MessageKind string

const (
	MessageUpdate      MessageKind = "update"
	MessageScopeChange MessageKind = "scope_change"
	MessageQuestion    MessageKind = "question"
	MessageCommand     MessageKind = "command"
)

var AllMessageKinds = []MessageKind{MessageUpdate, MessageScopeChange, MessageQuestion, MessageCommand}

func (k MessageKind) Valid() bool {
	for _, v := range AllMessageKinds {
		if v == k {
			return true
		}
	}
	return false
}

// ParseMessageKind accepts the canonical names case-insensitively, the same
// concession every other enum in this package makes to hand-typed callers.
func ParseMessageKind(s string) (MessageKind, bool) {
	for _, v := range AllMessageKinds {
		if equalFold(s, string(v)) {
			return v, true
		}
	}
	return MessageUpdate, false
}

// ChatMessage represents a project-scoped AI conversation entry.
// Messages are append-only and never pruned.
//
// Author is the human-facing display name — a signature, not an identity. The
// authorized source is AuthorTokenID: the tokens.id of the token that posted,
// taken from the authenticated caller and never chosen by the client. The
// addressing fields (Recipient, ResolvedExecutor) also hold tokens.id values;
// ResolvedExecutor is fixed at send time and deliberately frozen afterwards.
type ChatMessage struct {
	ID        string
	ProjectID string
	Author    string
	Body      string
	CreatedAt time.Time

	Kind          MessageKind
	AuthorTokenID string
	// Recipient is what the sender asked for: a tokens.id or "all". Empty
	// means unset — for question/command that resolves to the project's
	// coordinator AT SEND TIME (see ResolvedExecutor).
	Recipient string
	// ResolvedExecutor is the tokens.id fixed at send time for a question or
	// command addressed to exactly one executor; empty for update,
	// scope_change, "all" and unanswered-addressing refusals. A coordinator
	// change later must not readdress old commands: this value is written
	// once and never recomputed.
	ResolvedExecutor string
	// ReplyToID names the message this one answers, always within the same
	// project.
	ReplyToID string
	// IdempotencyKey deduplicates retries of one send, per authorized sender,
	// for the message's whole lifetime.
	IdempotencyKey string
	// Seq is the message's position in the feed: the rowid chat_messages
	// assigned at INSERT, filled by the store and never rewritten. It is the
	// feed's ordering authority. CreatedAt cannot serve: it has millisecond
	// precision and arrivals routinely share one millisecond (200 sequential
	// posts measured at 36 distinct milliseconds), so ordering ties inside a
	// bucket would fall to the random UUID id — and a message could then sort
	// BEFORE an already-issued cursor and be lost to every later page. Seq is
	// monotonic for as long as the feed exists (chat rows are never pruned;
	// the anchor check in the feed read catches any freed-and-reused position
	// loudly instead of silently skipping it).
	Seq int64
}

// CommandAcceptance is the durable link "command message -> created tasks ->
// accepting agent" (KANB-47). One command can be accepted exactly once per
// project: the uniqueness lives here, keyed by the message, not in any
// per-token idempotency table — so the binding survives consumer restarts and
// outlives every retry window.
type CommandAcceptance struct {
	ID                string
	MessageID         string
	ProjectID         string
	AcceptedByTokenID string
	// TaskIDs and TaskKeys name the batch the acceptance created. Keys are
	// denormalized beside ids because task keys are immutable (PROJ-N): the
	// acceptance can answer "which tasks came of this command" without a
	// join, even after a restart.
	TaskIDs  []string
	TaskKeys []string
	// RequestHash fingerprints the accepted batch content, so a retried
	// acceptance with DIFFERENT content is a loud conflict, not a silent
	// second package.
	RequestHash string
	CreatedAt   time.Time
}

// ChatCursor identifies a point in chat history for backward pagination
// (fetching older messages). It carries both CreatedAt and ID so that
// pagination across identical timestamps breaks ties deterministically
// without gaps or duplicates.
type ChatCursor struct {
	CreatedAt time.Time
	ID        string
}

// String encodes the cursor into a string of the form "<timestamp>/<id>".
func (c ChatCursor) String() string {
	if c.CreatedAt.IsZero() && c.ID == "" {
		return ""
	}
	return c.CreatedAt.UTC().Format(time.RFC3339Nano) + "/" + c.ID
}

// ParseChatCursor parses a cursor string formatted as "<timestamp>/<id>".
func ParseChatCursor(s string) (*ChatCursor, error) {
	if s == "" {
		return nil, nil
	}
	tsStr, id, ok := strings.Cut(s, "/")
	if !ok || id == "" {
		return nil, Invalid("cursor", "malformed chat cursor", "Cursor format is <timestamp>/<id>.")
	}
	t, err := time.Parse(time.RFC3339Nano, tsStr)
	if err != nil {
		t, err = time.Parse(time.RFC3339, tsStr)
	}
	if err != nil {
		return nil, Invalid("cursor", fmt.Sprintf("invalid cursor timestamp %q", tsStr), "Cursor format is <timestamp>/<id>.")
	}
	return &ChatCursor{CreatedAt: t.UTC(), ID: id}, nil
}

// FeedCursor identifies a position in a project's forward feed. It carries
// the message's Seq — the monotonic insertion key the feed is ordered and
// paged by — plus the message id as an integrity check: if the row at that
// position is not the named message (possible only if rows were deleted
// underneath a held cursor, which the product never does), the reader must
// refuse loudly instead of silently paging past history.
type FeedCursor struct {
	Seq int64
	ID  string
}

// String encodes the cursor as "<seq>/<id>". The cursor is opaque to
// consumers by contract — the encoding exists only so the position survives
// as a plain string across restarts, and it may change between releases for
// exactly that reason.
func (c FeedCursor) String() string {
	if c.Seq == 0 && c.ID == "" {
		return ""
	}
	return strconv.FormatInt(c.Seq, 10) + "/" + c.ID
}

// ParseFeedCursor parses "<seq>/<id>" as issued by FeedCursor.String. A
// cursor the server cannot parse is a loud validation error, never an empty
// page: guessing at a malformed position would silently skip or repeat
// history.
func ParseFeedCursor(s string) (*FeedCursor, error) {
	if s == "" {
		return nil, nil
	}
	seqStr, id, ok := strings.Cut(s, "/")
	if !ok || id == "" {
		return nil, Invalid("cursor", fmt.Sprintf("malformed feed cursor %q", s),
			"The cursor is issued by the server; pass back a next_cursor you were given, or omit after to read from the beginning.")
	}
	seq, err := strconv.ParseInt(seqStr, 10, 64)
	if err != nil || seq <= 0 {
		return nil, Invalid("cursor", fmt.Sprintf("malformed feed cursor %q", s),
			"The cursor is issued by the server; pass back a next_cursor you were given, or omit after to read from the beginning.")
	}
	return &FeedCursor{Seq: seq, ID: id}, nil
}

// EventType enumerates everything that can appear in the activity feed.
type EventType string

const (
	EventProjectCreated  EventType = "project.created"
	EventProjectUpdated  EventType = "project.updated"
	EventProjectArchived EventType = "project.archived"
	EventTaskCreated     EventType = "task.created"
	EventTaskUpdated     EventType = "task.updated"
	EventTaskMoved       EventType = "task.moved"
	EventTaskStarted     EventType = "task.started"
	EventTaskClaimed     EventType = "task.claimed"
	EventTaskRenewed     EventType = "task.renewed"
	EventTaskReleased    EventType = "task.released"
	EventTaskArchived    EventType = "task.archived"
	EventTaskRestored    EventType = "task.restored"
	EventLinkAdded       EventType = "link.added"
	EventLinkRemoved     EventType = "link.removed"
	EventNoteAdded       EventType = "note.added"
	EventFocusChanged    EventType = "focus.changed"
	// EventProgressRecorded fires after a new progress mark (task- or
	// project-scoped) commits. It carries only enough to identify the scope,
	// same as every other event — the mark itself lives in progress_marks and
	// is read from there, never from the event payload (KANB-12).
	EventProgressRecorded EventType = "progress.recorded"
	// EventProgressTrackDeleted fires after an assessor's whole progress
	// track (every mark that assessor logged for a scope) is permanently
	// removed. Like EventProgressRecorded, it carries only enough to
	// identify the scope — never the assessor's name or the marks that were
	// lost, which are gone from progress_marks and must not resurface via
	// the event payload. Other open tabs on this board have no other way to
	// learn the metric they are showing is now stale (KANB-12's live refresh
	// re-reads the real aggregate; this event is only what tells it to).
	EventProgressTrackDeleted EventType = "progress.track_deleted"
	// EventChatPosted fires after a new chat message commits. The message
	// body is never carried in the payload: the event is a change signal,
	// the chat_messages table is the only source of truth for what was said.
	EventChatPosted EventType = "chat.posted"
	// EventExecutorKeyIssued fires once per project an executor key was
	// issued (or re-issued) for (KANB-60): the project's participant list
	// just changed, and every open board has to learn it. The payload names
	// the key and its expiry only — the secret exists in exactly one place,
	// the issuing response, and an event is replayed to every subscriber.
	EventExecutorKeyIssued EventType = "executor_key.issued"
)

// Event is append-only. It feeds SSE, the activity view and any future audit.
type Event struct {
	ID        int64
	TS        time.Time
	Actor     string
	Type      EventType
	ProjectID string
	TaskID    *string
	Payload   map[string]any
}

// Token authenticates a caller. Name is the actor identity used everywhere:
// events, claims, notes, CreatedBy. There is no separate user model.
type Token struct {
	ID          string
	Name        string // unique
	Hash        []byte // SHA-256 of a 32-byte random secret (see SECURITY.md invariant)
	Scopes      Scopes
	ProjectKeys []string // empty = all projects
	CreatedAt   time.Time
	LastUsedAt  *time.Time
	RevokedAt   *time.Time
	// ExpiresAt ends the token's life without anyone having to revoke it
	// (KANB-60). nil means the token never expires, which is every token
	// minted before migration 0010 and every token the CLI creates. Executor
	// keys always carry one, so a forgotten key stops working on its own.
	ExpiresAt *time.Time
}

// Active reports only that the token was not revoked. It cannot see expiry,
// because expiry needs a clock: every authentication and participation
// decision must use ActiveAt instead.
func (t *Token) Active() bool { return t != nil && t.RevokedAt == nil }

// ActiveAt reports whether the token may authenticate at now: not revoked
// and not expired. The expiry instant itself is already dead — a key issued
// for 24 hours does not get a 24h+1ns grace.
func (t *Token) ActiveAt(now time.Time) bool {
	if !t.Active() {
		return false
	}
	return t.ExpiresAt == nil || now.Before(*t.ExpiresAt)
}

// MayAccessProject reports whether the token is allowed to touch a project key.
func (t *Token) MayAccessProject(key string) bool {
	if t == nil {
		return false
	}
	if len(t.ProjectKeys) == 0 {
		return true
	}
	for _, k := range t.ProjectKeys {
		if equalFold(k, key) {
			return true
		}
	}
	return false
}

// Session is a browser session created by pasting a token.
type Session struct {
	ID         string
	TokenID    string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// IdempotencyRecord makes a retried create safe: the same key replays the
// original response; the same key with different content is an error.
type IdempotencyRecord struct {
	TokenID     string
	Key         string
	RequestHash string
	Response    []byte
	ExpiresAt   time.Time
}
