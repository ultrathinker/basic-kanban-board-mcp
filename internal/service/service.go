// Package service holds the use-cases. It is the ONLY layer that MCP, the web
// UI and the CLI call: every rule, every transaction boundary and every event
// lives here exactly once, so the three surfaces cannot drift apart.
package service

import (
	"context"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// Actor is the authenticated caller. Identity always comes from the token —
// no tool, endpoint or form accepts an actor parameter, because a write token
// that can forge an identity poisons attribution, the activity feed and claim
// ownership (PLAN §8).
type Actor struct {
	TokenID     string
	Name        string // the actor identity everywhere
	Scopes      domain.Scopes
	ProjectKeys []string // empty = all
}

func (a Actor) IsAdmin() bool  { return a.Scopes.Has(domain.ScopeAdmin) }
func (a Actor) CanWrite() bool { return a.Scopes.Has(domain.ScopeWrite) }
func (a Actor) CanRead() bool  { return a.Scopes.Has(domain.ScopeRead) }

// Service is the whole application surface. Nine MCP tools, the UI and the CLI
// all map onto these methods.
type Service interface {
	BoardGet(ctx context.Context, a Actor, in BoardGetInput) (*Board, error)
	TaskNext(ctx context.Context, a Actor, in TaskNextInput) (*NextResult, error)
	TaskGet(ctx context.Context, a Actor, in TaskGetInput) (*TaskGetResult, error)
	TaskCreate(ctx context.Context, a Actor, in TaskCreateInput) (*TaskCreateResult, error)
	TaskUpdate(ctx context.Context, a Actor, in TaskUpdateInput) (*TaskUpdateResult, error)
	TaskLink(ctx context.Context, a Actor, in TaskLinkInput) (*TaskLinkResult, error)
	TaskClaim(ctx context.Context, a Actor, in TaskClaimInput) (*TaskClaimResult, error)
	TaskRemove(ctx context.Context, a Actor, in TaskRemoveInput) (*TaskRemoveResult, error)
	ProjectUpsert(ctx context.Context, a Actor, in ProjectUpsertInput) (*ProjectUpsertResult, error)
}

// Include names an optional expansion on a read. One parameter name across all
// read tools: `include` (PLAN §6).
type Include string

const (
	IncludeBody       Include = "body"
	IncludeAcceptance Include = "acceptance"
	IncludeNotes      Include = "notes"
	IncludeLinks      Include = "links"
	IncludeMetadata   Include = "metadata"
)

type Includes []Include

func (is Includes) Has(w Include) bool {
	for _, i := range is {
		if i == w {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// board_get
// ---------------------------------------------------------------------------

type BoardView string

const (
	ViewTasks   BoardView = "tasks"
	ViewSummary BoardView = "summary"
)

type BoardGetInput struct {
	ProjectKey string // empty = all accessible projects
	View       BoardView
	DoneLimit  int
	Filter     BoardFilter
	Include    Includes
}

type BoardFilter struct {
	Columns      []string
	Types        []domain.Type
	PriorityMin  *domain.Priority
	Tags         []string
	Assignee     *string
	Claimed      string // "any" | "mine" | "unclaimed" | "other"
	Blocked      *bool
	Query        string
	UpdatedSince *time.Time
}

// Board is the structured form. The compact text rendering is produced from
// exactly this value by internal/mcp, so text and structuredContent can never
// disagree.
type Board struct {
	Projects []BoardProject
}

type BoardProject struct {
	Key       string
	Name      string
	FocusKey  string // "" when unset
	Columns   []BoardColumn
	DoneTotal int
	DoneShown int
}

type BoardColumn struct {
	Name     string
	Kind     domain.Kind
	WIPLimit *int
	Count    int
	Tasks    []domain.TaskView // empty in ViewSummary
}

// ---------------------------------------------------------------------------
// task_next
// ---------------------------------------------------------------------------

// NextAction distinguishes looking from taking. `start` exists because a claim
// that leaves the card in Backlog means the agent believes it took work while
// the human still sees it queued (PLAN §6.2).
type NextAction string

const (
	NextPeek  NextAction = "peek"
	NextClaim NextAction = "claim"
	NextStart NextAction = "start"
)

type TaskNextInput struct {
	ProjectKey string
	Action     NextAction
	Limit      int
	Include    Includes
}

type NextResult struct {
	Tasks []domain.TaskView
	// ClaimedKey / StartedKey name the task actually taken, if any.
	ClaimedKey string
	StartedKey string
	WIPFull    bool
	// Reasons explains why the remaining candidates are not ready. An agent
	// that gets an empty list must never have to guess.
	Reasons NextReasons
	// BlockedTop samples the most relevant blocked tasks (max
	// domain.NextBlockedTopSample).
	BlockedTop []BlockedSample
}

type NextReasons struct {
	BlockedDependency int
	WIPFull           int
	ClaimedByOther    int
	ParentIncomplete  int
	NotLeaf           int
}

type BlockedSample struct {
	Key       string
	BlockedBy []string
}

// ---------------------------------------------------------------------------
// task_get
// ---------------------------------------------------------------------------

type TaskGetInput struct {
	Keys    []string
	Include Includes
}

// TaskGetResult preserves the requested order and reports missing keys
// explicitly rather than silently dropping them.
type TaskGetResult struct {
	Tasks    []domain.TaskView
	NotFound []string
}

// ---------------------------------------------------------------------------
// task_create  (all-or-nothing)
// ---------------------------------------------------------------------------

type TaskCreateInput struct {
	Tasks []NewTask
}

// NewTask uses symbolic refs, not positional indices: an LLM building a batch
// miscounts array offsets far more often than it mistypes a name it chose.
// Ref names the item; Parent/BlockedBy may point at "@name" within the batch.
type NewTask struct {
	ProjectKey     string
	Title          string
	Body           string
	Type           domain.Type
	Priority       domain.Priority
	Estimate       *float64
	Tags           []string
	Assignee       *string
	Column         string // empty = first backlog column
	Parent         string // task key or "@ref"
	BlockedBy      []string
	Acceptance     []string
	DueAt          *time.Time
	Metadata       map[string]any
	Ref            string
	IdempotencyKey string
}

type TaskCreateResult struct {
	Tasks []domain.TaskView
	// Replayed is true when an idempotency key returned the original response.
	Replayed bool
}

// ---------------------------------------------------------------------------
// task_update  (per-item results; atomic on request)
// ---------------------------------------------------------------------------

type TaskUpdateInput struct {
	Patches []TaskPatch
	Atomic  bool
}

// TaskPatch is deliberately the richest schema in the product: consolidating
// mutations into parameters keeps the tool count low (PLAN §6).
//
// IfVersion is REQUIRED whenever any replacement-style field is set. Only
// commutative operations (Note, TagsAdd, TagsRemove) may omit it — optimistic
// concurrency a caller can casually skip is last-write-wins with better
// marketing.
type TaskPatch struct {
	Key       string
	IfVersion *int

	Title    *string
	Body     *string
	Type     *domain.Type
	Priority *domain.Priority
	Estimate FieldFloat  // set or explicitly clear
	Assignee FieldString // set or explicitly clear
	DueAt    FieldTime   // set or explicitly clear

	Tags       []string // replace; mutually exclusive with TagsAdd/TagsRemove
	TagsAdd    []string
	TagsRemove []string

	Column string       // move; validated by domain.CheckMove inside the tx
	Rank   string       // "top" | "bottom"
	Parent FieldString  // reparent, or clear to make top-level

	Acceptance      []domain.AcceptanceItem // replace whole list
	AcceptanceCheck []int                   // tick by index; requires IfVersion
	AcceptanceAdd   []string

	Note          string // appended as a Note; does not bump version
	Focus         *bool  // project focus; bumps PROJECT version
	MetadataMerge map[string]any // null value deletes the key

	Force  bool // admin scope only, requires Reason
	Reason string
}

// Field* express the three-state "absent / set / explicitly null" that a patch
// API needs to distinguish "leave alone" from "clear".
type FieldString struct {
	Set   bool
	Clear bool
	Value string
}

type FieldFloat struct {
	Set   bool
	Clear bool
	Value float64
}

type FieldTime struct {
	Set   bool
	Clear bool
	Value time.Time
}

type TaskUpdateResult struct {
	Items []ItemResult
}

// ItemResult is the per-item outcome of a batch. Err is a *domain.Error so the
// tool layer can surface the code, remediation and conflicting current state.
type ItemResult struct {
	Key  string
	OK   bool
	Task *domain.TaskView
	Err  *domain.Error
}

// ---------------------------------------------------------------------------
// task_link
// ---------------------------------------------------------------------------

type TaskLinkInput struct {
	Add    []LinkPair
	Remove []LinkPair
}

// LinkPair is named blocker/blocked rather than from/to: "from" and "to" are
// trivially reversed by a caller reasoning in prose.
type LinkPair struct {
	Blocker string
	Blocked string
}

type TaskLinkResult struct {
	// Tasks carries the endpoints with refreshed BlockedBy and Version, since a
	// link change alters readiness.
	Tasks []domain.TaskView
}

// ---------------------------------------------------------------------------
// task_claim
// ---------------------------------------------------------------------------

type ClaimAction string

const (
	ClaimTake    ClaimAction = "claim"
	ClaimRenew   ClaimAction = "renew"
	ClaimRelease ClaimAction = "release"
)

type TaskClaimInput struct {
	Key        string
	Action     ClaimAction
	TTLSeconds int  // 0 = project default; clamped to [60, 86400]
	Force      bool // admin only: steal a live lease
}

type TaskClaimResult struct {
	Task           domain.TaskView
	ClaimedBy      string
	ClaimExpiresAt *time.Time
	RemainSeconds  int
}

// ---------------------------------------------------------------------------
// task_remove  (archive only over MCP)
// ---------------------------------------------------------------------------

type TaskRemoveInput struct {
	Items            []RemoveItem
	CascadeSubtasks  bool
	Restore          bool
}

type RemoveItem struct {
	Key       string
	IfVersion *int
}

type TaskRemoveResult struct {
	Items []ItemResult
}

// ---------------------------------------------------------------------------
// project_upsert
// ---------------------------------------------------------------------------

// UpsertMode is required: a typo in an immutable project key must not silently
// fork the board into a second project.
type UpsertMode string

const (
	UpsertCreate UpsertMode = "create"
	UpsertUpdate UpsertMode = "update"
)

type ProjectUpsertInput struct {
	Mode          UpsertMode
	Key           string
	Name          string
	Description   *string
	IfVersion     *int // required for update
	Columns       []ColumnSpec
	RemoveColumns []RemoveColumn
	Settings      *ProjectSettings
	Archived      *bool
}

type ColumnSpec struct {
	Name     string
	Kind     domain.Kind
	WIPLimit *int
}

// RemoveColumn forces the caller to say where the orphans go; dropping a
// populated column without a destination is data loss by omission.
type RemoveColumn struct {
	Name        string
	MoveTasksTo string
}

type ProjectSettings struct {
	EstimateUnit        *string
	EnforceDependencies *bool
	StrictDone          *bool
	ClaimTTLSeconds     *int
}

type ProjectUpsertResult struct {
	Project domain.Project
	Columns []domain.Column
}
