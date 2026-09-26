package mcp

import (
	"fmt"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// errorEnvelope is the wire shape of a domain error inside the
// {ok:false, op, error:{...}} envelope (PLAN §6). Every field maps straight
// through from *domain.Error unchanged: code, message, remediation and the
// conflicting `current` state are the caller's only way to recover without a
// human, so none of them may be dropped (AGENTS.md "Fail loud"). `current`
// is the one field that is SHAPED: see conflictCurrent.
type errorEnvelope struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
	// Current carries the server's state on a conflict so the caller can
	// merge without a second round trip. Only set for code "conflict".
	Current any    `json:"current,omitempty"`
	Field   string `json:"field,omitempty"`
}

// newErrorEnvelope maps a *domain.Error onto the wire envelope. Returns nil
// for a nil input so callers can assign it straight into an Error* field
// with omitempty semantics.
func newErrorEnvelope(e *domain.Error) *errorEnvelope {
	if e == nil {
		return nil
	}
	env := &errorEnvelope{
		Code:        string(e.Code),
		Message:     e.Message,
		Remediation: e.Remediation,
		Current:     e.Current,
		Field:       e.Field,
	}
	if cur, version, ok := conflictCurrent(e.Current); ok {
		env.Current = cur
		env.Remediation = fmt.Sprintf(
			"`current` carries the version and the short fields as the server has them now; merge your change and retry with if_version=%d. Body, conclusion and checklist are left out — read them with task_get only if your change touches them.", version)
	}
	return env
}

// conflictTask is `current` for a task conflict. The envelope used to carry
// the raw Go struct: PascalCase keys (ProjectID, LeaseRemain in nanoseconds)
// unlike every other response, and the whole body, conclusion and checklist
// — thousands of characters to tell an agent "the version moved". It now
// carries what a merge decision needs: the version to retry with, who moved
// it and when, and the short fields a patch usually touches (found live,
// 26.09.2026, follow-up to KANB-58).
type conflictTask struct {
	Key        string           `json:"key"`
	Project    string           `json:"project,omitempty"`
	Version    int              `json:"version"`
	Column     string           `json:"column,omitempty"`
	UpdatedAt  time.Time        `json:"updated_at"`
	UpdatedBy  string           `json:"updated_by,omitempty"`
	Title      string           `json:"title"`
	Type       domain.Type      `json:"type"`
	Priority   string           `json:"priority"`
	Assignee   *string          `json:"assignee,omitempty"`
	Reviewer   *string          `json:"reviewer,omitempty"`
	Tags       []string         `json:"tags,omitempty"`
	Estimate   *float64         `json:"estimate,omitempty"`
	Outcome    domain.Outcome   `json:"outcome,omitempty"`
	ClaimedBy  *string          `json:"claimed_by,omitempty"`
	Acceptance *acceptanceTally `json:"acceptance,omitempty"`
}

type acceptanceTally struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

// conflictProject is `current` for a project conflict (project_upsert).
type conflictProject struct {
	Key       string    `json:"key"`
	Version   int       `json:"version"`
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updated_at"`
}

// conflictCurrent shapes a conflict's `current`; ok is false for anything it
// does not know, which then passes through unchanged.
func conflictCurrent(cur any) (any, int, bool) {
	switch c := cur.(type) {
	case *domain.TaskView:
		if c == nil {
			return nil, 0, false
		}
		out := conflictTaskOf(&c.Task)
		out.Project = c.ProjectKey
		out.Column = c.ColumnName
		return out, c.Version, true
	case *domain.Task:
		if c == nil {
			return nil, 0, false
		}
		return conflictTaskOf(c), c.Version, true
	case *domain.Project:
		if c == nil {
			return nil, 0, false
		}
		return conflictProject{Key: c.Key, Version: c.Version, Name: c.Name, UpdatedAt: c.UpdatedAt}, c.Version, true
	}
	return nil, 0, false
}

func conflictTaskOf(t *domain.Task) conflictTask {
	out := conflictTask{
		Key: t.Key, Version: t.Version, UpdatedAt: t.UpdatedAt, UpdatedBy: t.UpdatedBy,
		Title: t.Title, Type: t.Type, Priority: t.Priority.String(),
		Assignee: t.Assignee, Reviewer: t.Reviewer, Tags: t.Tags, Estimate: t.Estimate,
		Outcome: t.Outcome, ClaimedBy: t.ClaimedBy,
	}
	if n := len(t.Acceptance); n > 0 {
		done := 0
		for _, a := range t.Acceptance {
			if a.Done {
				done++
			}
		}
		out.Acceptance = &acceptanceTally{Done: done, Total: n}
	}
	return out
}

// asDomainError extracts a *domain.Error from err. The service contract is
// to always return *domain.Error for a caller-facing failure; anything else
// reaching this layer is a bug elsewhere, so it is wrapped as a validation
// error rather than left to blow up as a bare JSON-RPC fault, per AGENTS.md
// "Fail loud" (every refusal names what happened and what to do next, even
// this one).
func asDomainError(err error) *domain.Error {
	if err == nil {
		return nil
	}
	if de := domain.AsError(err); de != nil {
		return de
	}
	return &domain.Error{
		Code:        domain.CodeValidation,
		Message:     err.Error(),
		Remediation: "This is an unexpected internal error; please retry, and report it if it persists.",
	}
}

// toolMeta is the shared shape of every tool's `meta` field. Every tool uses
// a subset; the rest stay zero and are omitted from the wire form, so one
// type keeps the envelope shape consistent across every tool instead of
// nine near-identical structs.
type toolMeta struct {
	Count      int                `json:"count,omitempty"`
	Warnings   []string           `json:"warnings,omitempty"`
	Replayed   bool               `json:"replayed,omitempty"`
	NotFound   []string           `json:"not_found,omitempty"`
	Projection *projectionOut     `json:"projection,omitempty"`
	Reasons    *nextReasonsOut    `json:"reasons,omitempty"`
	BlockedTop []blockedSampleOut `json:"blocked_top,omitempty"`
	ClaimedKey string             `json:"claimed_key,omitempty"`
	StartedKey string             `json:"started_key,omitempty"`
	// AlreadyAccepted marks a source_message acceptance that replayed the
	// original one: data.tasks carries the original task keys' current views
	// and nothing new was created (KANB-47).
	AlreadyAccepted bool `json:"already_accepted,omitempty" jsonschema:"true when source_message replayed an existing acceptance: same task keys, no new batch"`
}
