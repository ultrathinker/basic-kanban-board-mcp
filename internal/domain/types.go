// Package domain holds the entities, enums and pure rules of the board.
// It has no I/O: no database, no HTTP, no MCP. Everything here must be testable
// with plain Go values so the rules that define the product can be verified fast.
package domain

import "time"

// Kind classifies a column and drives WIP checks, "done" semantics and the
// hide-done toggle in the UI.
type Kind string

const (
	KindBacklog Kind = "backlog"
	KindActive  Kind = "active"
	KindDone    Kind = "done"
)

func (k Kind) Valid() bool {
	switch k {
	case KindBacklog, KindActive, KindDone:
		return true
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

// LinkType is the typed dependency edge. v1 ships "blocks" only; the column
// exists so adding "relates"/"duplicates" later is data, not a migration of
// meaning.
type LinkType string

const LinkBlocks LinkType = "blocks"

func (l LinkType) Valid() bool { return l == LinkBlocks }

// Scope is a token capability. write implies read; admin implies both.
type Scope string

const (
	ScopeRead  Scope = "read"
	ScopeWrite Scope = "write"
	ScopeAdmin Scope = "admin"
)

func (s Scope) Valid() bool {
	switch s {
	case ScopeRead, ScopeWrite, ScopeAdmin:
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
		if s == ScopeWrite && want == ScopeRead {
			return true // write implies read
		}
	}
	return false
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
	ArchivedAt          *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Column belongs to a project. Names are unique per project because tools
// address columns by name.
type Column struct {
	ID        string
	ProjectID string
	Name      string
	Position  int
	Kind      Kind
	WIPLimit  *int // meaningful only for KindActive
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
	Tags     []string // lowercase, no spaces
	Assignee *string  // free text: human or agent name

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
	ProjectKey  string
	ColumnName  string
	ColumnKind  Kind
	BlockedBy   []string // keys of OPEN blockers only, sorted
	Blocks      []string // keys this task blocks, sorted
	SubDone     int
	SubTotal    int
	Ready       bool // no open blockers, no incomplete subtasks, claimable
	LeaseRemain *time.Duration
	Notes       []Note // populated only when explicitly included
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
}

func (t *Token) Active() bool { return t != nil && t.RevokedAt == nil }

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
