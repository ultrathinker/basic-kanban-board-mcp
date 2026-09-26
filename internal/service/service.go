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

// OwnCardsOnly reports an executor key (KANB-60): it writes only to cards
// assigned to its own name, never across the done boundary. Every write
// use-case that admits executors checks this and narrows accordingly.
func (a Actor) OwnCardsOnly() bool { return a.Scopes.OwnCardsOnly() }

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

	TaskProgress(ctx context.Context, a Actor, in TaskProgressInput) (*TaskProgressResult, error)
	ProjectProgress(ctx context.Context, a Actor, in ProjectProgressInput) (*ProjectProgressResult, error)
	ProgressHistory(ctx context.Context, a Actor, in ProgressHistoryInput) (*ProgressHistoryResult, error)
	ProgressTrackDelete(ctx context.Context, a Actor, in ProgressTrackDeleteInput) (*ProgressTrackDeleteResult, error)
	ProgressSet(ctx context.Context, a Actor, in ProgressSetInput) (*ProgressSetResult, error)
	ChatAdd(ctx context.Context, a Actor, in ChatAddInput) (*domain.ChatMessage, error)
	ChatList(ctx context.Context, a Actor, in ChatListInput) (*ChatListResult, error)
	// ChatFeed reads a project's message feed FORWARD, from the beginning of
	// history (KANB-45) — the read a consumer uses to find commands that were
	// written before it started.
	ChatFeed(ctx context.Context, a Actor, in ChatFeedInput) (*ChatFeedResult, error)

	// ExecutorKeyIssue mints a named, expiring executor key for one or more
	// projects (KANB-60). The issuer is an admin or the coordinator of every
	// listed project. The secret is returned here and nowhere else.
	ExecutorKeyIssue(ctx context.Context, a Actor, in ExecutorKeyIssueInput) (*ExecutorKeyIssueResult, error)

	// Export builds the full backup document (KANB-70): every readable
	// project with all its cards, archived ones included, and every history
	// stream. `kanban export` and both web Export buttons call it, so they
	// produce the same document.
	Export(ctx context.Context, a Actor, in ExportInput) (*ExportDocument, error)
	// Import restores an export document in one transaction, all or nothing,
	// and refuses before writing anything if one of its projects already
	// exists. Admin only.
	Import(ctx context.Context, a Actor, doc *ExportDocument) (*ImportResult, error)
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
	// IncludeProgress is board_get-only (KANB-61): it annotates every card in
	// an active column with its acceptance count, its assessed percent and
	// how long it has been idle, so a progress report is one read instead of
	// a scripted walk over task_get. It is not a task field, so task_get and
	// task_next do not publish it.
	IncludeProgress Include = "progress"
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
// Response projection
// ---------------------------------------------------------------------------

// FieldDetail says how much of a *selected* field a result actually carries.
//
// It exists because `include` was doing two incompatible jobs (architecture
// review finding #21): it selected which fields to return AND was read, one
// layer down, as permission to return them in full. That made "included but
// bounded" unrepresentable — a caller could have the whole 64 KiB body or
// nothing at all — and it made task_next's default answer more expensive than
// reading the entire board. Selection and detail are now separate axes.
type FieldDetail string

const (
	// DetailFull means the field carries the stored value unaltered.
	DetailFull FieldDetail = "full"
	// DetailBounded means the field was clipped to the limit the Projection
	// reports. A bounded body ends in the "… +N chars" marker; a bounded
	// acceptance list reports its true length in AcceptanceTotals.
	DetailBounded FieldDetail = "bounded"
)

// Projection is the explicit, returned description of the shaping a read
// applied. It is part of the result, not an agreement each layer re-derives
// from the request: the service decides once, and every surface that renders
// the result reads that decision instead of guessing at it.
type Projection struct {
	// Fields are the optional fields actually populated on every task in the
	// result. A field absent here is absent from the tasks — a renderer must
	// not emit it, and must not assume it was merely empty.
	Fields Includes

	// Body / Acceptance report how much of those fields survived, and the
	// limit applied when the answer is DetailBounded.
	Body            FieldDetail
	BodyLimitBytes  int
	Acceptance      FieldDetail
	AcceptanceLimit int

	// AcceptanceTotals gives the true item count for each task whose
	// acceptance list was clipped, keyed by task key. A key is absent when
	// nothing was cut, so an empty map means the lists are complete. Bounding
	// a list without saying how much is missing would be exactly the silent
	// downgrade AGENTS.md forbids.
	AcceptanceTotals map[string]int
}

// Has reports whether the projection populated a field.
func (p Projection) Has(w Include) bool { return p.Fields.Has(w) }

// Apply shapes one already-hydrated view to this projection, in place: it
// clears every optional field the projection did not select and clips the ones
// it bounded, recording in AcceptanceTotals whatever it had to cut.
//
// Notes are the one field Apply cannot handle — they are a separate query, so
// the caller loads them inside its own transaction — but it does clear them,
// so an unselected note list can never leak through.
//
// It is exported because the response shaping is a contract, not an
// implementation detail: a surface measuring or asserting what a bounded read
// costs must be able to run the real rule rather than a copy of it that drifts.
func (p *Projection) Apply(tv *domain.TaskView) {
	if !p.Has(IncludeBody) {
		tv.Body = ""
	} else if p.Body == DetailBounded {
		tv.Body = truncateBody(tv.Body, p.BodyLimitBytes)
	}

	if !p.Has(IncludeAcceptance) {
		tv.Acceptance = nil
	} else if p.Acceptance == DetailBounded && len(tv.Acceptance) > p.AcceptanceLimit {
		if p.AcceptanceTotals == nil {
			p.AcceptanceTotals = map[string]int{}
		}
		p.AcceptanceTotals[tv.Key] = len(tv.Acceptance)
		tv.Acceptance = tv.Acceptance[:p.AcceptanceLimit]
	}

	if !p.Has(IncludeLinks) {
		tv.Blocks = nil
	}
	if !p.Has(IncludeMetadata) {
		tv.Metadata = nil
	}
	if !p.Has(IncludeNotes) {
		tv.Notes = nil
	}
}

// FullProjection describes a read that returned every selected field whole.
// It is the shape of every read except task_next, which bounds by default;
// naming it keeps that difference visible instead of implied.
func FullProjection(fields Includes) Projection {
	return Projection{Fields: fields, Body: DetailFull, Acceptance: DetailFull}
}

// ---------------------------------------------------------------------------
// board_get
// ---------------------------------------------------------------------------

type BoardView string

const (
	ViewTasks    BoardView = "tasks"
	ViewSummary  BoardView = "summary"
	ViewMessages BoardView = "messages"
)

type BoardGetInput struct {
	ProjectKey string // empty = all accessible projects
	View       BoardView
	DoneLimit  int
	Filter     BoardFilter
	Include    Includes
}

type BoardFilter struct {
	// Columns names columns to include (OR). A name that no column of the
	// boards being read carries is a validation error listing the real
	// names, never a silently empty board (KANB-59).
	Columns []string
	// ColumnKinds includes columns by kind (OR). With Columns set too, a
	// column must satisfy both.
	ColumnKinds  []domain.Kind
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

// BoardProject carries the project's full configuration, not just its
// identity. Version in particular is not decoration: project_upsert
// (mode:"update") requires if_version, and board_get is the only read that
// publishes a project at all — without it an administrator cannot reconfigure
// an existing project through the tool surface at all, which is the pressure
// that invents a tenth tool (architecture review finding #10).
type BoardProject struct {
	Key         string
	Name        string
	Description string
	Version     int    // echo back as project_upsert's if_version
	FocusKey    string // "" when unset

	// Settings, as project_upsert's `settings` object accepts them, so a
	// caller can read the current configuration and send back a modified one
	// without inventing values it never saw.
	EstimateUnit        string // "h" unless the project configured otherwise
	EnforceDependencies bool
	StrictDone          bool
	ClaimTTLSeconds     int
	// IdleAfterSeconds is the project's own idle threshold, 0 when not set
	// (the attention line then uses ClaimTTLSeconds).
	IdleAfterSeconds int
	Archived         bool

	// Coordinator is the appointed coordinator, nil when none is set, and
	// Participants is everyone who MAY participate — every active token with
	// access to the project (KANB-44). Having access says "may participate",
	// not "is working right now"; nothing here tracks presence. No secrets
	// travel with either: a Participant is an id and a display name only.
	Coordinator  *Participant
	Participants []Participant

	Columns   []BoardColumn
	DoneTotal int
	DoneShown int

	// Attention names the cards in active columns that have not moved for
	// longer than IdleAfterSeconds, or ClaimTTLSeconds when that is not set
	// (KANB-61, KANB-67) — the signal that replaced WIP
	// limits. nil when no card qualifies, so a healthy board pays nothing.
	// It follows the board filter, like the column counts do.
	Attention *Attention
	// Activity is keyed by task key and populated only for
	// include:["progress"], for the cards in active columns.
	Activity map[string]TaskActivity
}

// Attention is the bounded "look at these" list of a project: Count is the
// number of idle cards, Sample the AttentionSampleSize most idle of them,
// most idle first.
type Attention struct {
	Count  int
	Sample []IdleTask
}

// IdleTask is one card of an Attention sample.
type IdleTask struct {
	Key  string
	Idle time.Duration
}

// TaskActivity is the per-card progress line of include:["progress"].
//
// AcceptanceDone/AcceptanceTotal are the "X of N": acceptance criteria are
// the only countable, checkable progress a card stores — a progress mark is
// a free percentage, one per assessor, with no N behind it. Percent is that
// assessed mean when anyone assessed the card (nil otherwise, never 0).
// LastActivityAt is the card's last movement by anyone — the same instant
// the idle check measures from.
type TaskActivity struct {
	AcceptanceDone  int
	AcceptanceTotal int
	Percent         *int
	LastActivityAt  time.Time
	Idle            time.Duration
}

// Participant names one actor who may take part in a project's
// communication. TokenID is the stable identity (tokens.id); Name is the
// display label. The token's secret never appears in this shape.
type Participant struct {
	TokenID string
	Name    string
}

type BoardColumn struct {
	Name  string
	Kind  domain.Kind
	Count int
	Tasks []domain.TaskView // empty in ViewSummary
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

// NextDetail chooses how much of each selected field task_next returns. It is
// deliberately separate from Include: `include` says *which* fields, `detail`
// says *how much*. task_next is the tool an agent calls to choose a piece of
// work, so the default is the cheap one — full cards are what task_get is for.
type NextDetail string

const (
	// NextDetailSummary is the default: body clipped to NextSummaryBodyBytes
	// and acceptance to NextSummaryAcceptanceItems, enough to pick between
	// candidates without paying for all of them.
	NextDetailSummary NextDetail = "summary"
	// NextDetailFull widens to PLAN §6.2's stated bounds — body ≤
	// domain.NextBodyTruncate, acceptance ≤ domain.NextAcceptanceItems. It is
	// still bounded: task_next never returns a whole 64 KiB body, because a
	// candidate list is the wrong place to spend a context window.
	NextDetailFull NextDetail = "full"
)

// Bounds of the two task_next detail levels. The summary numbers live here
// rather than in domain because they describe one tool's response shaping,
// not a rule about what a task may contain; domain's Next* limits remain the
// ceiling that NextDetailFull uses.
//
// The split between them is not arbitrary. Measured on the wire, one
// acceptance item costs about as much as 2 KB of body excerpt does per
// candidate — roughly 40 tokens against 4 — while an excerpt tells you far
// more about whether a card is the one you want. So the summary budget goes
// almost entirely to the excerpt, and the criteria are represented by their
// count (`acceptance_total`), which is the part that signals size. The text of
// criteria 3..50 is what task_get, and detail:"full", are for.
const (
	NextSummaryBodyBytes       = 256
	NextSummaryAcceptanceItems = 2
)

type TaskNextInput struct {
	ProjectKey string
	Action     NextAction
	Limit      int
	Include    Includes
	Detail     NextDetail // "" = NextDetailSummary
}

type NextResult struct {
	Tasks []domain.TaskView
	// Projection states exactly what shaping produced Tasks. Rendering
	// surfaces must read it instead of re-deriving the answer from the
	// request they sent.
	Projection Projection
	// ClaimedKey / StartedKey name the task actually taken, if any.
	ClaimedKey string
	StartedKey string
	// Reasons explains why the remaining candidates are not ready. An agent
	// that gets an empty list must never have to guess.
	Reasons NextReasons
	// BlockedTop samples the most relevant blocked tasks (max
	// domain.NextBlockedTopSample).
	BlockedTop []BlockedSample
}

type NextReasons struct {
	BlockedDependency int
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
	// NotesBefore pages backwards through a task's notes: only notes created
	// strictly before it are returned, newest first. Without it the newest
	// domain.MaxNotesPerRead are all an agent could ever reach, so a
	// long-running task's own working history became unreadable through the
	// tool surface as soon as it crossed the cap (PLAN §6.3).
	NotesBefore *time.Time
}

// TaskGetResult preserves the requested order and reports missing keys
// explicitly rather than silently dropping them.
type TaskGetResult struct {
	Tasks    []domain.TaskView
	NotFound []string
	// NotesNext is the cursor to send back as NotesBefore for the next, older
	// page of notes, keyed by task key. A key is absent when that task has no
	// older notes — the cursor is per task because each task's history ends at
	// a different point.
	NotesNext map[string]time.Time
	// Subtasks breaks a parent's live subtasks down by the kind of column
	// each sits in (KANB-61), keyed by task key. A key is absent for a task
	// without subtasks.
	Subtasks map[string]SubtaskKinds
}

// SubtaskKinds counts a parent's live (unarchived) subtasks per column kind.
type SubtaskKinds struct {
	Backlog int
	Active  int
	Waiting int
	Done    int
}

// ---------------------------------------------------------------------------
// task_create  (all-or-nothing)
// ---------------------------------------------------------------------------

type TaskCreateInput struct {
	Tasks []NewTask
	// SourceMessage optionally names a kind:command chat message this batch
	// ACCEPTS (KANB-47). Empty means the call behaves exactly as before.
	//
	// When set, the whole call becomes an atomic acceptance: only the
	// command's resolved executor (fixed at send time) may accept, the batch
	// must belong to the command's project, and the "command -> tasks ->
	// acceptor" link is written in the SAME transaction as the tasks — either
	// the tasks exist AND the message is accepted, or nothing happened. A
	// repeated acceptance returns the original task_keys with
	// AlreadyAccepted=true and creates nothing; a repeated acceptance with
	// different content is a loud conflict. The guarantee is durable (it
	// survives restarts), and it covers the BOARD only: it cannot prevent an
	// external command or deploy from running twice.
	SourceMessage string
	// Restore marks a batch that recreates cards which already existed — the
	// `kanban import` path — rather than new work (KANB-60). Only then is an
	// assignee accepted without being a participant of the project: the
	// value is a historical fact carried over from an export, and the
	// executor key it once named may long have expired. Admin scope only;
	// no MCP tool sets it.
	Restore bool
}

// NewTask uses symbolic refs, not positional indices: an LLM building a batch
// miscounts array offsets far more often than it mistypes a name it chose.
// Ref names the item; Parent/BlockedBy may point at "@name" within the batch.
type NewTask struct {
	ProjectKey string
	Title      string
	Body       string
	Type       domain.Type
	Priority   domain.Priority
	Estimate   *float64
	Actual     *float64
	Tags       []string
	Assignee   *string
	Reviewer   *string
	Column     string // empty = first backlog column
	Parent     string // task key or "@ref"
	BlockedBy  []string
	// Blocks is BlockedBy seen from the other end: tasks (existing keys or
	// "@ref" items of this batch) that this new task must finish before.
	Blocks     []string
	Acceptance []string
	// Outcome and Conclusion let a card be created already judged: the
	// legitimate "found it and fixed it on the way" card that goes straight
	// to Done (KANB-59). nil Outcome = open.
	Outcome        *domain.Outcome
	Conclusion     string
	DueAt          *time.Time
	Metadata       map[string]any
	Ref            string
	IdempotencyKey string
}

type TaskCreateResult struct {
	Tasks []domain.TaskView
	// Replayed is true when an idempotency key returned the original response.
	Replayed bool
	// AlreadyAccepted is true when a source_message acceptance replayed the
	// original acceptance: Tasks then carries the CURRENT views of the
	// originally created task keys, and nothing new was created (KANB-47).
	AlreadyAccepted bool
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

	Title *string
	Body  *string
	// BodyAppend adds text to the end of the current body instead of replacing
	// it, so an agent can grow a long body in pieces without resending (or
	// holding) the whole thing — and without a transport that clips a large
	// argument silently truncating the result. Mutually exclusive with Body.
	// A nil pointer means "leave the body alone"; a non-nil pointer appends its
	// value (which the service separates from the existing text with a blank
	// line when the body is non-empty).
	BodyAppend *string
	Type       *domain.Type
	Priority   *domain.Priority
	Estimate   FieldFloat // set or explicitly clear
	// Actual is recorded like Estimate (set or explicit clear). It does not
	// touch Estimate: the two coexist so the gap between them is legible.
	Actual   FieldFloat
	Assignee FieldString // set or explicitly clear
	Reviewer FieldString // set or explicitly clear; who checks the work
	// Outcome sets the epistemic status of the result (open/holds/refuted/
	// superseded/moot). A nil pointer leaves it unchanged; setting it to
	// domain.OutcomeOpen is how a caller resets a task back to "not judged".
	Outcome *domain.Outcome
	// Conclusion replaces the post-hoc takeaway text. nil leaves it unchanged;
	// an empty string clears it. Distinct from Body and from Note.
	Conclusion *string
	DueAt      FieldTime // set or explicitly clear

	Tags       []string // replace; mutually exclusive with TagsAdd/TagsRemove
	TagsAdd    []string
	TagsRemove []string

	Column string      // move; validated by domain.CheckMove inside the tx
	Rank   string      // "top" | "bottom"
	Parent FieldString // reparent, or clear to make top-level

	Acceptance      []domain.AcceptanceItem // replace whole list
	AcceptanceCheck []int                   // tick by index; requires IfVersion
	AcceptanceAdd   []string

	Note          string         // appended as a Note; does not bump version
	Focus         *bool          // project focus; bumps PROJECT version
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
	Items           []RemoveItem
	CascadeSubtasks bool
	Restore         bool
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
	Mode        UpsertMode
	Key         string
	Name        string
	Description *string
	// DescriptionAppend adds text to the end of the current description
	// instead of replacing it — the same idea as TaskPatch.BodyAppend, and
	// deliberately built the same way: mutually exclusive with Description
	// (checked in ProjectUpsert before either mode function runs), joined
	// with a blank line when the description is not empty, and if_version
	// already guards every mode:"update" call regardless of which field
	// changed, so an append is racesafe for free.
	DescriptionAppend *string
	IfVersion         *int // required for update
	Columns           []ColumnSpec
	RemoveColumns     []RemoveColumn
	Settings          *ProjectSettings
	Archived          *bool
}

type ColumnSpec struct {
	Name string
	Kind domain.Kind
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
	// IdleAfterSeconds sets the project's idle threshold for board_get's
	// attention line (KANB-67), inside [domain.IdleAfterMin,
	// domain.IdleAfterMax]; 0 clears it back to the claim TTL, nil leaves it
	// unchanged. Out of range is refused, never clamped.
	IdleAfterSeconds *int
	// Coordinator appoints the project's coordinator (KANB-44). The value is
	// a tokens.id — the identity that survives secret rotation — or an empty
	// string to clear the appointment. nil leaves it unchanged. There is
	// deliberately no participant list to set: participants are derived from
	// the tokens that have access to the project.
	Coordinator *string
}

type ProjectUpsertResult struct {
	Project domain.Project
	Columns []domain.Column
}

// ---------------------------------------------------------------------------
// chat
// ---------------------------------------------------------------------------

// ChatAddInput posts one message to a project feed. Project/Author/Body are
// the original trio and behave exactly as before; the remaining fields are
// the communication protocol (KANB-46). Their names and types are a frozen
// wire contract — the next stage's adapter and external clients bind to
// them.
type ChatAddInput struct {
	ProjectKey string
	Author     string // optional: defaults to Actor.Name
	Body       string
	// Kind is one of update | scope_change | question | command. Empty
	// means update — a call that predates the protocol is indistinguishable
	// from one that chose its default.
	Kind string
	// Recipient is a participant's tokens.id or the literal "all". Empty is
	// legal and, for question/command, means "the project's coordinator" —
	// resolved ONCE, at send time.
	Recipient string
	// ReplyTo names the message this one answers. It must exist in the same
	// project; a reply across projects is refused.
	ReplyTo string
	// IdempotencyKey deduplicates retries of one send, per authorized
	// sender. The same key with the same content returns the existing
	// message; with different content it is an explicit error. Unlike
	// task_create's 24h idempotency window, the key lives as long as the
	// message does.
	IdempotencyKey string
}

type ChatMessageAddInput = ChatAddInput

type ChatListInput struct {
	ProjectKey string // optional: empty = all accessible projects
	Limit      int    // optional: <= 0 defaults to 50
	Before     *domain.ChatCursor
	Cursor     string // optional string-encoded cursor; used if Before is nil
}

type ChatMessageListInput = ChatListInput

type ChatListResult struct {
	Messages   []domain.ChatMessage
	NextCursor *domain.ChatCursor
	Cursor     string // string-encoded NextCursor, or empty if nil
	// Meta resolves, per message id, the display data the raw rows only
	// reference (KANB-48): who a recipient id is, what a reply answers, and
	// whether a command has been accepted. Ids absent from the map have
	// nothing to resolve — a plain update that addresses nobody and replies
	// to nothing never appears here. Filled in the same read transaction as
	// Messages, so a page can never show a quote or an acceptance that the
	// listed messages do not have.
	Meta map[string]ChatListEntryMeta
}

// ChatListEntryMeta is the resolved display data for ONE listed message.
// Every field is optional: RecipientName/ExecutorName are "" when the row
// names no token, Parent is nil when the message is not a reply, Acceptance
// is nil until the command has actually been accepted (KANB-47) — an
// unaccepted command must not look accepted.
type ChatListEntryMeta struct {
	RecipientName string              // display name of Recipient, "" for unset/"all"
	ExecutorName  string              // display name of ResolvedExecutor, "" when none
	Parent        *domain.ChatMessage // the message this one replies to
	Acceptance    *domain.CommandAcceptance
}

type ChatMessageListResult = ChatListResult

// ---------------------------------------------------------------------------
// chat feed — forward reading (KANB-45)
// ---------------------------------------------------------------------------

// ChatFeedInput reads a project's feed chronologically from the beginning.
// After is the opaque cursor a previous page returned; WITHOUT it the read
// starts at the OLDEST available message, never at "now" — a consumer that
// starts at the current moment would silently miss every command written
// before it launched, which is exactly the message class the feed exists to
// deliver.
type ChatFeedInput struct {
	ProjectKey string
	After      string
	Limit      int // <= 0 defaults to 50; capped at 100
}

// ChatFeedMessage is one feed entry with the display names resolved against
// the token registry. Token ids stay canonical (they are the addressing
// contract); the *Name fields are conveniences for humans.
type ChatFeedMessage struct {
	Message              domain.ChatMessage
	RecipientName        string // "" when the recipient is unset or "all"
	ResolvedExecutorName string // "" when no single executor was fixed
	// TaskKeys are the tasks created by accepting this command (KANB-47).
	// Empty for every other message and for an unaccepted command.
	TaskKeys []string
}

type ChatFeedResult struct {
	Messages []ChatFeedMessage
	// NextCursor is the position of the last message returned; pass it back
	// as `after` for the next page. On an empty page past a position it
	// echoes that position back, so the documented polling loop
	// (`cursor = next_cursor`) never loses its place; it is empty only when
	// the feed holds no message at or after the request (an empty first page
	// included).
	NextCursor string
	HasMore    bool
}

// ---------------------------------------------------------------------------
// progress — the one place the metrics arithmetic lives
//
// Web and MCP render these results; they must never re-derive them, or the
// two surfaces drift apart (rounding first, scope next). See progress.go for
// the rules and the rounding.
// ---------------------------------------------------------------------------

// TaskProgressInput asks for the summary progress of specific tasks of one
// project. Keys that do not parse, do not exist in this project, or fall
// outside the actor's scope are reported in NotFound — the caller asked for N
// keys and gets N answers, never a silent drop (task_get's rule).
type TaskProgressInput struct {
	ProjectKey string
	Keys       []string // task keys, PROJ-N; up to domain.MaxGetKeys
}

// TaskProgressResult preserves the requested order.
type TaskProgressResult struct {
	ProjectKey string
	Items      []TaskProgressItem
	NotFound   []string
}

// TaskProgressItem is one task's summary progress. Percent is nil when nobody
// has assessed the task. It must stay distinguishable from an assessed 0%:
// a bare int cannot — zero claims the work has not started, and a renderer
// draws a 0% bar while it hides "no data" altogether.
type TaskProgressItem struct {
	Key    string
	TaskID string
	// Percent is the mean of the assessors' latest marks, rounded halves up.
	// nil = no assessor has spoken yet.
	Percent *int
	// Assessors is how many latest marks the mean used. A track with fifty
	// revisions still counts once: one mark per assessor.
	Assessors int
	// ForecastETA is the freshest standing finish-date promise among this
	// task's assessors. For each assessor, their own most recent mark is
	// their current standing answer for both percent AND forecast, since
	// ETA rides along on the same append-only mark; an assessor whose
	// latest mark carries no ETA is not currently offering one (the
	// append-only, no-second-opinion model has no honest way to say "my
	// old promise still holds" instead). Among the assessors who ARE
	// currently offering one, this is the LATEST (most pessimistic) date —
	// never an average, which would be meaningless for dates. Nil when
	// nobody currently has a standing forecast.
	ForecastETA *time.Time
	// ForecastBy is the assessor whose forecast ForecastETA actually is —
	// guaranteed to name the mark that produced it, never a different
	// assessor's name.
	ForecastBy string
	// Tracks is the per-assessor breakdown behind Percent: one entry per
	// assessor contributing to this task's summary, each carrying that
	// assessor's latest percent and how many marks make up their whole
	// track. Nothing recomputes Percent from this — meanPercent already did
	// that — it exists purely so the owner's delete-track control can name
	// an assessor and say how many history points a click would remove.
	// Empty when nobody has assessed the task.
	Tracks []AssessorTrack
}

// AssessorTrack is one assessor's contribution to a progress metric.
type AssessorTrack struct {
	Assessor string
	Percent  int
	Count    int
}

// ProjectProgressInput asks for both project-level progress views.
type ProjectProgressInput struct {
	ProjectKey string
}

// ProjectProgressResult carries the manual and the automatic progress side by
// side. They never overwrite each other: manual is what assessors said about
// the project as a whole (task_id IS NULL marks only), auto is what the board
// itself says. The gap between them is the point of the pair.
type ProjectProgressResult struct {
	ProjectKey string
	// Manual is nil when nobody assessed the project as a whole.
	Manual          *int
	ManualAssessors int
	// ManualForecastETA / ManualForecastBy mirror TaskProgressItem's
	// ForecastETA/ForecastBy pair, but computed over the project-level
	// ("Manual") track only — the same track Manual itself is computed
	// from. Nil/"" when nobody currently has a standing forecast for the
	// project as a whole.
	ManualForecastETA *time.Time
	ManualForecastBy  string
	// Auto is nil when the project has no unarchived tasks: an empty board
	// has no measured progress, and 0% would claim work not started.
	Auto       *int
	DoneTasks  int
	TotalTasks int
	// ManualTracks is the per-assessor breakdown behind Manual — same
	// purpose as TaskProgressItem.Tracks, for the project-level scope. Empty
	// when nobody assessed the project as a whole.
	ManualTracks []AssessorTrack
	// Readiness is the estimate-weighted readiness of the board right now
	// (KANB-35): finished effort over all estimated effort, over the
	// unarchived LEAVES of the tree, with its coverage and basis attached.
	// It sits beside Auto rather than replacing it — Auto counts cards, this
	// weighs them — and both are computed from the same counts, so they
	// cannot end up describing different boards.
	Readiness EstimateReadiness
	// HistoryStartsAt is the instant from which the board's lifecycle journal
	// is trustworthy — the moment migration 0006 ran (KANB-30). Everything
	// before it happened while nothing was recording: cards moved, were
	// finished, reopened and archived, and none of that was written down. It
	// is returned so a renderer can draw NOTHING before this instant instead
	// of a line reconstructed from created_at. Half a truth on a chart is
	// worse than an honest gap, because the reader cannot see which half.
	HistoryStartsAt time.Time
}

// ProgressHistoryInput asks for the full mark history behind one progress
// metric: the project's manual scope (TaskKey empty) or one task's summary
// scope (TaskKey set) — the same (project, task) scoping ProgressTrackDelete
// and the Tracks breakdown already use. It powers the progress-history chart
// (internal/web/view/chart.go), fetched once when a bar is first clicked
// open rather than rendered for every metric on the page: see KANB-13's
// REPORT.md for the measured argument. This method only fetches the marks;
// the chart itself is drawn entirely by view.RenderProgressChart, which the
// web layer calls with the Marks this returns.
type ProgressHistoryInput struct {
	ProjectKey string
	TaskKey    string
	// Limit bounds the read to the newest Limit marks (still returned
	// oldest-first). Zero or less means "the whole scope", which is what the
	// browser's chart asks for. A caller that wants a page MUST set this
	// rather than trimming the result: the bound is applied in SQL, so
	// trimming afterwards would read every row the limit was meant to skip.
	Limit int
	// IncludeReplay asks for the project's history REPLAYED FROM THE
	// LIFECYCLE JOURNAL (Result.Replay). This is the ONE historical curve of
	// the item counts (KANB-34): it sees reopenings, archivals and estimates
	// as they stood at each instant, none of which tasks.done_at can
	// remember, and the count chart is built from it — there is no second,
	// created_at-based curve beside it. Project-scope only (a single task
	// has no item count) and off by default: it is a second read, and the
	// only caller that wants it is the browser's chart.
	IncludeReplay bool
}

// ProgressHistoryResult carries the marks in chronological order (the store's
// own History order); nothing here re-sorts, de-duplicates or decimates —
// that is chart.go's job, once, on the render path.
type ProgressHistoryResult struct {
	ProjectKey string
	TaskKey    string
	Marks      []domain.ProgressMark
	// Total is how many marks the scope holds, which is not len(Marks) when
	// Limit trimmed the read. It is what tells a caller that older history
	// exists beyond the page it asked for.
	Total int
	// Replay is the project's history rebuilt from the lifecycle journal,
	// present only when the caller set IncludeReplay on a project-scope read.
	// Oldest first, one point per recorded instant. It carries the item
	// counts AND the estimate sums, all as of that instant.
	Replay []HistoryPoint
	// HistoryStartsAt is the instant from which the journal is trustworthy
	// (KANB-30). Replay never reaches back before it, and nothing should be
	// drawn before it either: that history was not recorded and cannot be
	// honestly recovered.
	HistoryStartsAt time.Time
}

// ProgressTrackDeleteInput names one (project, task, assessor) track. An
// empty TaskKey selects the project-level track (task_id IS NULL).
type ProgressTrackDeleteInput struct {
	ProjectKey string
	TaskKey    string
	Assessor   string
}

// ProgressTrackDeleteResult reports how many marks the store removed.
type ProgressTrackDeleteResult struct {
	Removed int64
}

// ---------------------------------------------------------------------------
// executor_key_issue (KANB-60)
// ---------------------------------------------------------------------------

// ExecutorKeyIssueInput names the executor and where it works. Name becomes
// the key's actor identity — the string a card's assignee must equal for the
// key to write to it — so it is taken verbatim, never normalised.
type ExecutorKeyIssueInput struct {
	Name        string
	ProjectKeys []string
	// TTLSeconds is the key's lifetime; 0 = domain.ExecutorKeyDefaultTTL,
	// anything outside [ExecutorKeyMinTTL, ExecutorKeyMaxTTL] is refused.
	TTLSeconds int
	// ParticipantOnly makes Name a participant of ProjectKeys without a key
	// (KANB-68): no secret is minted or stored, the name cannot sign in, and
	// it never expires. Calling it again for the same participant adds
	// projects to it.
	ParticipantOnly bool
	// Renew extends the executor key already named Name to now + TTLSeconds
	// and keeps its secret (KANB-68), live or expired, never revoked.
	// ProjectKeys must be empty: a renewal keeps the key's projects.
	Renew bool
}

// ExecutorKeyIssueResult carries the secret exactly once. Nothing else in
// the product can show it again: only its hash is stored.
type ExecutorKeyIssueResult struct {
	TokenID     string
	Name        string
	ProjectKeys []string
	// ExpiresAt is zero for a participant without a key: it never expires.
	ExpiresAt time.Time
	// Secret is set only by a plain issue. A renewal keeps the secret the
	// executor already has, and a participant without a key has none.
	Secret string
	// Reissued is true when the name belonged to an expired executor key and
	// that row was given a new secret and expiry instead of a new row.
	Reissued bool
	// Renewed is true for a renewal: same row, same secret, new expiry.
	Renewed bool
	// ParticipantOnly is true for a participant without a key; ProjectKeys
	// is then every project it participates in after the call, and
	// AddedProjects the ones this call added (empty on a repeat).
	ParticipantOnly bool
	AddedProjects   []string
}
