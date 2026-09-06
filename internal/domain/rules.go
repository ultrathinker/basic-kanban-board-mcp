package domain

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// equalFold is ASCII-only case-insensitive comparison. Keys, tags and column
// names are ASCII by construction, so this avoids Unicode folding surprises.
func equalFold(a, b string) bool { return strings.EqualFold(a, b) }

// ---------------------------------------------------------------------------
// Keys
//
// Every external surface accepts keys case-insensitively and returns them
// canonical (uppercase project, uppercase key). Agents routinely emit "bmb-14";
// rejecting that costs a turn for nothing.
// ---------------------------------------------------------------------------

// NormalizeProjectKey upper-cases and trims a project key.
func NormalizeProjectKey(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// ValidateProjectKey enforces [A-Z][A-Z0-9]{1,7} on the normalized form.
func ValidateProjectKey(s string) (string, error) {
	k := NormalizeProjectKey(s)
	if len(k) < MinProjectKeyLen || len(k) > MaxProjectKeyLen {
		return "", Invalid("key",
			fmt.Sprintf("project key must be %d-%d characters, got %d", MinProjectKeyLen, MaxProjectKeyLen, len(k)),
			"Use a short uppercase code such as BMB or KANBAN.")
	}
	for i, r := range k {
		ok := (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return "", Invalid("key",
				fmt.Sprintf("project key %q must start with a letter and contain only A-Z and 0-9", k),
				"Use a short uppercase code such as BMB or KANBAN.")
		}
	}
	return k, nil
}

// TaskKey builds the canonical PROJ-N handle.
func TaskKey(projectKey string, seq int) string {
	return fmt.Sprintf("%s-%d", NormalizeProjectKey(projectKey), seq)
}

// ParseTaskKey splits a task key case-insensitively into its project key and
// sequence number.
func ParseTaskKey(s string) (projectKey string, seq int, err error) {
	raw := strings.TrimSpace(s)
	i := strings.LastIndex(raw, "-")
	if i <= 0 || i == len(raw)-1 {
		return "", 0, Invalid("key",
			fmt.Sprintf("%q is not a task key", s),
			"Task keys look like BMB-14. Call board_get to see the keys that exist.")
	}
	pk, err := ValidateProjectKey(raw[:i])
	if err != nil {
		return "", 0, err
	}
	n, convErr := strconv.Atoi(raw[i+1:])
	if convErr != nil || n <= 0 {
		return "", 0, Invalid("key",
			fmt.Sprintf("%q is not a task key: %q is not a positive number", s, raw[i+1:]),
			"Task keys look like BMB-14.")
	}
	return pk, n, nil
}

// NormalizeTaskKey returns the canonical uppercase form of a task key.
func NormalizeTaskKey(s string) (string, error) {
	pk, seq, err := ParseTaskKey(s)
	if err != nil {
		return "", err
	}
	return TaskKey(pk, seq), nil
}

// ---------------------------------------------------------------------------
// Field validation
// ---------------------------------------------------------------------------

// NormalizeTitle collapses internal whitespace and strips newlines. The compact
// grammar is line-oriented and space-separated, so a title containing a newline
// or a run of spaces would break positional parsing for every reader.
func NormalizeTitle(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func ValidateTitle(s string) (string, error) {
	t := NormalizeTitle(s)
	if t == "" {
		return "", Invalid("title", "title must not be empty", "Give the task a short imperative title.")
	}
	if len([]rune(t)) > MaxTitleLen {
		return "", Invalid("title",
			fmt.Sprintf("title is %d characters, the limit is %d", len([]rune(t)), MaxTitleLen),
			"Shorten the title and put the detail in body.")
	}
	return t, nil
}

// NormalizeTag lower-cases and rejects whitespace: the compact grammar prefixes
// tags with '#' and separates them with spaces, so a tag with a space in it
// would be unparseable.
func NormalizeTag(s string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" {
		return "", Invalid("tags", "a tag must not be empty", "Drop the empty entry.")
	}
	if len(t) > MaxTagLen {
		return "", Invalid("tags",
			fmt.Sprintf("tag %q is longer than %d characters", t, MaxTagLen), "Use a shorter tag.")
	}
	for _, r := range t {
		if unicode.IsSpace(r) || r == '#' {
			return "", Invalid("tags",
				fmt.Sprintf("tag %q must not contain spaces or '#'", t),
				"Use a single lowercase word, e.g. sync or ui.")
		}
	}
	return t, nil
}

// NormalizeTags normalizes, de-duplicates and sorts. Sorting is part of the
// compact contract: the same task must render identically on every read.
func NormalizeTags(in []string) ([]string, error) {
	if len(in) > MaxTags {
		return nil, Invalid("tags",
			fmt.Sprintf("%d tags, the limit is %d", len(in), MaxTags), "Remove some tags.")
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		t, err := NormalizeTag(raw)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	sortStrings(out)
	return out, nil
}

func ValidateBody(s string) error {
	if len(s) > MaxBodyBytes {
		return Invalid("body",
			fmt.Sprintf("body is %d bytes, the limit is %d", len(s), MaxBodyBytes),
			"Split the detail into subtasks or a linked document.")
	}
	return nil
}

func ValidateAcceptance(items []AcceptanceItem) error {
	if len(items) > MaxAcceptance {
		return Invalid("acceptance",
			fmt.Sprintf("%d acceptance items, the limit is %d", len(items), MaxAcceptance),
			"Group the criteria or split the task.")
	}
	for i, it := range items {
		if strings.TrimSpace(it.Text) == "" {
			return Invalid("acceptance", fmt.Sprintf("acceptance item %d is empty", i), "Remove it or give it text.")
		}
		if len([]rune(it.Text)) > MaxAcceptanceText {
			return Invalid("acceptance",
				fmt.Sprintf("acceptance item %d is longer than %d characters", i, MaxAcceptanceText),
				"Shorten it.")
		}
	}
	return nil
}

func ValidateColumnName(s string) (string, error) {
	n := NormalizeTitle(s)
	if n == "" {
		return "", Invalid("column", "column name must not be empty", "Name the column, e.g. Doing.")
	}
	if len([]rune(n)) > MaxColumnNameLen {
		return "", Invalid("column",
			fmt.Sprintf("column name is longer than %d characters", MaxColumnNameLen), "Use a shorter name.")
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Version bumping
//
// This is the single source of truth for PLAN §7. It exists as code, not prose,
// because the whole optimistic-concurrency story collapses if a background
// lease renewal by one agent invalidates another agent's if_version edit.
// ---------------------------------------------------------------------------

// MutationKind classifies a write so callers cannot get the version rule wrong.
type MutationKind int

const (
	// MutationContent covers title, body, type, priority, estimate, tags,
	// assignee, acceptance, due_at, metadata, parent, rank, column, archive,
	// restore — everything a concurrent editor must not silently clobber.
	MutationContent MutationKind = iota
	// MutationStructure is a link add/remove: it changes readiness, so both
	// endpoint tasks bump.
	MutationStructure
	// MutationLease is claim/renew/release. Deliberately does NOT bump.
	MutationLease
	// MutationNote is an appended note. Deliberately does NOT bump.
	MutationNote
	// MutationFocus changes project state, not task state: the PROJECT version
	// bumps, the task's does not.
	MutationFocus
)

// BumpsTaskVersion reports whether a mutation increments Task.Version.
func BumpsTaskVersion(k MutationKind) bool {
	switch k {
	case MutationContent, MutationStructure:
		return true
	default:
		return false
	}
}

// BumpsProjectVersion reports whether a mutation increments Project.Version.
func BumpsProjectVersion(k MutationKind) bool { return k == MutationFocus }

// ---------------------------------------------------------------------------
// Transitions
// ---------------------------------------------------------------------------

// MoveCheck is everything a move needs to be decided. It is a plain struct so
// the rule can be table-tested without a database.
type MoveCheck struct {
	TaskKey    string
	From       Column
	To         Column
	OpenBlocks []string // keys of blockers that are not done
	// ToCount is how many unarchived tasks the destination already holds,
	// excluding this task when it is already there.
	ToCount             int
	AcceptanceRemaining int
	EnforceDependencies bool
	StrictDone          bool
	Force               bool
	ForceReason         string
	ActorIsAdmin        bool
}

// CheckMove decides whether a task may enter a column. It returns a *Error with
// the precise code the tool layer surfaces (blocked, wip_exceeded, validation,
// forbidden) so every caller reports the same thing.
//
// Force is deliberately admin-only with a mandatory reason: a bypass that any
// write token can use is not a guard rail, it is a suggestion.
func CheckMove(c MoveCheck) error {
	if c.Force {
		if !c.ActorIsAdmin {
			return Forbidden("force requires admin scope",
				"Ask an admin, or satisfy the rule instead: finish the blockers, free WIP, or tick the acceptance items.")
		}
		if strings.TrimSpace(c.ForceReason) == "" {
			return Invalid("reason", "force requires a reason", "Say why the rule is being bypassed; it is written to the event log.")
		}
		return nil
	}
	if c.EnforceDependencies && c.To.Kind != KindBacklog && len(c.OpenBlocks) > 0 {
		return Blocked(c.TaskKey, c.OpenBlocks)
	}
	if c.To.Kind == KindActive && c.To.WIPLimit != nil && c.ToCount >= *c.To.WIPLimit {
		return WIPExceeded(c.To.Name, *c.To.WIPLimit)
	}
	if c.StrictDone && c.To.Kind == KindDone && c.AcceptanceRemaining > 0 {
		return Invalid("acceptance",
			fmt.Sprintf("%d acceptance items are still unchecked", c.AcceptanceRemaining),
			"Tick them with task_update.acceptance_check, or turn strict_done off for the project.")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Leases
// ---------------------------------------------------------------------------

// LeaseLive reports whether a claim is still in force at now. All comparisons
// use the server clock; never a client-supplied timestamp.
func LeaseLive(t *Task, now time.Time) bool {
	return t != nil && t.ClaimedBy != nil && t.ClaimExpiresAt != nil && t.ClaimExpiresAt.After(now)
}

// LeaseRemaining returns the time left on a live lease, or nil.
func LeaseRemaining(t *Task, now time.Time) *time.Duration {
	if !LeaseLive(t, now) {
		return nil
	}
	d := t.ClaimExpiresAt.Sub(now)
	return &d
}

// ClaimableBy reports whether actor may take the task now: free, expired, or
// already theirs (an idempotent re-claim must succeed).
func ClaimableBy(t *Task, actor string, now time.Time) bool {
	if t == nil {
		return false
	}
	if !LeaseLive(t, now) {
		return true
	}
	return t.ClaimedBy != nil && *t.ClaimedBy == actor
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
