package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Executor keys (KANB-60). An orchestrating agent issues one per agent it
// launches; the key's name is the executor's identity and its project list is
// where the executor participates. The key writes to its own cards only and
// cannot cross the done boundary, so the executor can report work but never
// accept it.
const (
	// ExecutorKeyDefaultTTL is how long a key lives when the issuer does not
	// say: one working day, so a key that outlives its agent dies overnight.
	ExecutorKeyDefaultTTL = 24 * time.Hour
	// ExecutorKeyMinTTL keeps a key long enough to be pasted into a launcher.
	ExecutorKeyMinTTL = 5 * time.Minute
	// ExecutorKeyMaxTTL bounds a key to a working week. A longer-lived
	// identity is a standing participant, and those are write tokens minted by
	// an operator with `kanban token create`, not keys an agent hands out.
	ExecutorKeyMaxTTL = 7 * 24 * time.Hour
	// MaxExecutorKeyProjects bounds one key's project list. An executor works
	// on a task, not on a portfolio; the number is generous and only exists so
	// the argument has a published bound.
	MaxExecutorKeyProjects = 20
)

// ExecutorKeyTTL resolves the requested lifetime in seconds: zero means the
// default, anything outside [min, max] is refused rather than clamped — a
// caller that asked for a month and silently got a week would be surprised by
// the expiry at the worst moment.
func ExecutorKeyTTL(seconds int) (time.Duration, error) {
	if seconds == 0 {
		return ExecutorKeyDefaultTTL, nil
	}
	d := time.Duration(seconds) * time.Second
	if seconds < 0 || d < ExecutorKeyMinTTL || d > ExecutorKeyMaxTTL {
		return 0, Invalid("ttl_seconds",
			fmt.Sprintf("ttl_seconds %d is outside %d..%d", seconds,
				int(ExecutorKeyMinTTL.Seconds()), int(ExecutorKeyMaxTTL.Seconds())),
			fmt.Sprintf("Omit ttl_seconds for the default of %d (24h), or pass a value in range; issue a fresh key when this one runs out.",
				int(ExecutorKeyDefaultTTL.Seconds())))
	}
	return d, nil
}

// ValidateTokenName applies the rules every token name follows: it is the
// actor identity written into claims, notes and assignee fields, so it must
// be non-empty, free of whitespace (it is typed into launch commands and
// compared byte for byte) and fit the assignee field it is meant to fill.
func ValidateTokenName(field, s string) error {
	if s == "" {
		return Invalid(field, field+" is required", "Pass the executor's name, e.g. claude-exec-1; it becomes the key's identity.")
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return Invalid(field, fmt.Sprintf("%s %q contains whitespace", field, s),
			"Use a name without spaces, e.g. claude-exec-1.")
	}
	return ValidateActorName(field, s)
}

// ExecutorOwns refuses a write by an executor key to a card that is not
// assigned to it. Ownership is the assignee field compared byte for byte with
// the key's name: the name is the identity, and a case-folded match would let
// "Worker" act on "worker"'s cards.
func ExecutorOwns(actor string, t *Task) error {
	if t.Assignee != nil && *t.Assignee == actor {
		return nil
	}
	who := "nobody"
	if t.Assignee != nil && *t.Assignee != "" {
		who = *t.Assignee
	}
	return Forbidden(
		fmt.Sprintf("task %s is assigned to %s; executor key %s writes only to its own cards", t.Key, who, actor),
		fmt.Sprintf("Work on a card assigned to %s (board_get filter assignee), or ask the coordinator to assign %s to you.", actor, t.Key))
}

// ExecutorMayMove refuses a move by an executor key across the done
// boundary, in either direction. Moving into done is accepting the work, and
// the reviewer does that; moving out of done reopens work the reviewer
// accepted, which is the same decision taken back.
func ExecutorMayMove(actor, taskKey string, from, to Column) error {
	if to.Kind == KindDone && from.ID != to.ID {
		return Forbidden(
			fmt.Sprintf("executor key %s cannot move %s into %s (a done column): executors hand work over, the reviewer accepts it", actor, taskKey, to.Name),
			"Move the card into the review column (an active column) and let the reviewer close it.")
	}
	if from.Kind == KindDone && to.Kind != KindDone {
		return Forbidden(
			fmt.Sprintf("executor key %s cannot move %s out of %s (a done column): reopening accepted work is the reviewer's call", actor, taskKey, from.Name),
			"Post to the project feed (project_post) why the card should be reopened, and let the reviewer move it.")
	}
	return nil
}

// ExecutorFieldsRefused refuses a task_update patch by an executor key that
// touches fields outside its allowance. fields are the patch's field names
// the executor may not set; the refusal names them all so the caller can
// drop them in one retry.
func ExecutorFieldsRefused(actor, taskKey string, fields []string) error {
	if len(fields) == 0 {
		return nil
	}
	sorted := append([]string(nil), fields...)
	sort.Strings(sorted)
	return Forbidden(
		fmt.Sprintf("executor key %s may not set %s on %s", actor, strings.Join(sorted, ", "), taskKey),
		"An executor key may only add a note and move its own card (column, rank) outside done columns; ask the coordinator for anything else.")
}

// ExecutorRefused is the refusal for an operation an executor key cannot
// perform at all. op names the operation in the caller's words.
func ExecutorRefused(actor, op string) error {
	return Forbidden(
		fmt.Sprintf("executor key %s cannot %s", actor, op),
		"An executor key claims, moves and annotates its own cards, records progress on them and posts to the project feed; ask the coordinator (a write or admin token) for anything else.")
}
