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

	TaskProgress(ctx context.Context, a Actor, in TaskProgressInput) (*TaskProgressResult, error)
	ProjectProgress(ctx context.Context, a Actor, in ProjectProgressInput) (*ProjectProgressResult, error)
	ProgressHistory(ctx context.Context, a Actor, in ProgressHistoryInput) (*ProgressHistoryResult, error)
	ProgressTrackDelete(ctx context.Context, a Actor, in ProgressTrackDeleteInput) (*ProgressTrackDeleteResult, error)
	ProgressSet(ctx context.Context, a Actor, in ProgressSetInput) (*ProgressSetResult, error)
	ChatAdd(ctx context.Context, a Actor, in ChatAddInput) (*domain.ChatMessage, error)
	ChatList(ctx context.Context, a Actor, in ChatListInput) (*ChatListResult, error)
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
	Archived            bool

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
	Actual         *float64
	Tags           []string
	Assignee       *string
	Reviewer       *string
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

// ---------------------------------------------------------------------------
// chat
// ---------------------------------------------------------------------------

type ChatAddInput struct {
	ProjectKey string
	Author     string // optional: defaults to Actor.Name
	Body       string
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
}

type ChatMessageListResult = ChatListResult

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
}

// ProgressHistoryResult carries the marks in chronological order (the store's
// own History order); nothing here re-sorts, de-duplicates or decimates —
// that is chart.go's job, once, on the render path.
type ProgressHistoryResult struct {
	ProjectKey string
	TaskKey    string
	Marks      []domain.ProgressMark
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
