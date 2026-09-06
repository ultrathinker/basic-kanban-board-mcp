package domain

import "time"

// Hard limits. These are contract, not taste: they appear in the published JSON
// Schemas and every layer enforces the same numbers.
const (
	MaxTitleLen       = 200
	MaxBodyBytes      = 64 * 1024
	MaxNoteBytes      = 16 * 1024
	MaxMetadataBytes  = 16 * 1024
	MaxTags           = 20
	MaxTagLen         = 40
	MaxAcceptance     = 50
	MaxAcceptanceText = 500
	MaxAssigneeLen    = 80

	MaxBatchTasks   = 100 // task_create / task_update / task_remove
	MaxGetKeys      = 50  // task_get
	MaxNextLimit    = 10  // task_next
	MaxDoneLimit    = 200 // board_get done_limit
	MaxNotesPerRead = 20

	MaxProjectKeyLen = 8
	MinProjectKeyLen = 2
	MaxColumnNameLen = 40
	MaxColumnsPerPrj = 12

	MaxSubtaskDepth = 2 // task -> subtask; a subtask may not have children

	// NextBodyTruncate bounds the body returned by task_next so a work loop
	// cannot blow the context the compact board_get exists to protect.
	NextBodyTruncate     = 2 * 1024
	NextAcceptanceItems  = 10
	NextBlockedTopSample = 5

	// RankStep is the gap left between neighbouring tasks so an insert between
	// two cards needs no renumbering. Renumber transactionally when a gap closes.
	RankStep int64 = 1024

	// CompactVersion is emitted as the first line of every compact rendering.
	// Bumping it is a breaking change to the grammar and requires new goldens.
	CompactVersion = 1

	// CompactTokenBudget is the release gate from PLAN §11: 30 active tasks must
	// render within this many tokens, approximated as len(text)/4.
	//
	// This is a regression gate, not a target. The honest fixture measures
	// ~1060 tokens; the ceiling sits above it with enough headroom to absorb
	// normal wording changes but not enough to hide the grammar growing a new
	// field. The original 600 was written into the plan before anything was
	// measured, and the first fixture was quietly bent to fit it — see PLAN §18
	// deviation 5.
	//
	// The number that actually matters to a user is the ratio, measured on the
	// same 30-task board: compact ~1060 tokens against ~5640 for minified JSON
	// and ~10650 for indented JSON — roughly 80% and 90% fewer tokens. Claims
	// in README/docs must quote that comparison, never a bare absolute.
	CompactTokenBudget = 1200
	CompactBudgetTasks = 30
)

// Lease and session bounds.
const (
	ClaimTTLDefault = time.Hour
	ClaimTTLMin     = time.Minute
	ClaimTTLMax     = 24 * time.Hour

	SessionIdleTTL     = 7 * 24 * time.Hour
	SessionAbsoluteTTL = 30 * 24 * time.Hour

	IdempotencyTTL = 24 * time.Hour

	// TicketTTL bounds the one-time SSE ticket: bearer tokens must never appear
	// in a URL, so token holders exchange one for a short-lived ticket.
	TicketTTL = time.Minute
)

// Rate limits. A batch of 100 items counts as one request, by design: batching
// is the product, so it must not be punished.
const (
	RateLimitPerTokenPerMin = 600
	RateLimitLoginPerIPMin  = 20
	MaxRequestBodyBytes     = 1 << 20 // 1 MB
)

// DefaultColumns is what project_upsert creates when no columns are given.
var DefaultColumns = []struct {
	Name     string
	Kind     Kind
	WIPLimit *int
}{
	{Name: "Backlog", Kind: KindBacklog},
	{Name: "Doing", Kind: KindActive, WIPLimit: intPtr(3)},
	{Name: "Review", Kind: KindActive},
	{Name: "Done", Kind: KindDone},
}

func intPtr(i int) *int { return &i }

// ClampClaimTTL keeps a requested lease inside the supported window.
func ClampClaimTTL(d time.Duration) time.Duration {
	switch {
	case d < ClaimTTLMin:
		return ClaimTTLMin
	case d > ClaimTTLMax:
		return ClaimTTLMax
	default:
		return d
	}
}
