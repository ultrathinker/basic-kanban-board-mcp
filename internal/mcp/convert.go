package mcp

import (
	"fmt"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// acceptanceOut is the wire form of one domain.AcceptanceItem.
type acceptanceOut struct {
	Text string `json:"text"`
	Done bool   `json:"done"`
}

func acceptanceOutList(items []domain.AcceptanceItem) []acceptanceOut {
	if len(items) == 0 {
		return nil
	}
	out := make([]acceptanceOut, len(items))
	for i, it := range items {
		out[i] = acceptanceOut{Text: it.Text, Done: it.Done}
	}
	return out
}

// noteOut is the wire form of one domain.Note. TaskID is omitted: the note
// is always read in the context of its task, so echoing the ID back would
// only invite a caller to key off a UUID instead of the task's key.
type noteOut struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

func noteOutList(notes []domain.Note) []noteOut {
	if len(notes) == 0 {
		return nil
	}
	out := make([]noteOut, len(notes))
	for i, n := range notes {
		out[i] = noteOut{Author: n.Author, Body: n.Body, CreatedAt: formatTime(n.CreatedAt)}
	}
	return out
}

// taskVersionDoc is the published description of taskOut.Version, repeated
// verbatim in that field's struct tag (tags must be literals, so the compiler
// cannot do it for us — TestVersionSemantics_PinnedInSchema asserts the two
// never diverge).
//
// It exists because the rule it states was, for a while, written down nowhere
// a model looks. A dogfooding agent sent if_version:2, got version:2 back,
// could not tell "the write landed and legitimately did not bump" from "this
// echo is stale", and defensively re-read every task with task_get before
// every versioned write — doubling its write cost over one missing sentence.
const taskVersionDoc = "the task version AFTER this call, and authoritative: chain your next if_version from it, " +
	"and never re-read a task just to learn its version. note, lease operations (task_claim) and focus deliberately " +
	"do not move the version, so a response echoing the same number you sent as if_version means the write landed " +
	"and the version legitimately did not change."

// versionEchoRule states the same contract in prose, for the surfaces that
// carry sentences rather than schema properties: the initialize instructions
// and the task_update tool description.
const versionEchoRule = "The `version` on every returned task is the value AFTER the call and is authoritative — " +
	"chain your next `if_version` from it and never re-read a task just to learn its version. " +
	"`note`, lease operations and `focus` do not move a task's version by design, so a result echoing the same " +
	"version you sent means the write landed and the version legitimately did not change."

// taskOut is the canonical JSON rendering of a domain.TaskView, shared by
// every tool that returns tasks (task_get, task_next, task_create,
// task_update, task_link, task_claim, board_get). Fields gated by `include`
// are nil/empty when not requested, matching the compact rendering's own
// omit-when-absent rule so the two forms never disagree about what was
// asked for.
type taskOut struct {
	Key                   string          `json:"key"`
	Project               string          `json:"project"`
	Column                string          `json:"column"`
	ColumnKind            domain.Kind     `json:"column_kind"`
	Type                  domain.Type     `json:"type"`
	Priority              string          `json:"priority"`
	Title                 string          `json:"title"`
	Body                  *string         `json:"body,omitempty"`
	Estimate              *float64        `json:"estimate,omitempty"`
	Actual                *float64        `json:"actual,omitempty"`
	EstimateUnit          string          `json:"estimate_unit,omitempty"`
	EstimateError         *float64        `json:"estimate_error,omitempty"`
	Tags                  []string        `json:"tags,omitempty"`
	Assignee              *string         `json:"assignee,omitempty"`
	Reviewer              *string         `json:"reviewer,omitempty"`
	Outcome               string          `json:"outcome,omitempty"`
	Conclusion            string          `json:"conclusion,omitempty"`
	ClaimedBy             *string         `json:"claimed_by,omitempty"`
	ClaimExpiresAt        *string         `json:"claim_expires_at,omitempty"`
	LeaseRemainingSeconds *int            `json:"lease_remaining_seconds,omitempty"`
	Acceptance            []acceptanceOut `json:"acceptance,omitempty"`
	AcceptanceTotal       *int            `json:"acceptance_total,omitempty" jsonschema:"present only when acceptance was clipped by a bounded projection: the number of items the task actually has. Call task_get for the whole list."`
	DueAt                 *string         `json:"due_at,omitempty"`
	SubDone               int             `json:"sub_done,omitempty"`
	SubTotal              int             `json:"sub_total,omitempty"`
	BlockedBy             []string        `json:"blocked_by,omitempty"`
	Blocks                []string        `json:"blocks,omitempty"`
	Ready                 bool            `json:"ready"`
	Version               int             `json:"version" jsonschema:"the task version AFTER this call, and authoritative: chain your next if_version from it, and never re-read a task just to learn its version. note, lease operations (task_claim) and focus deliberately do not move the version, so a response echoing the same number you sent as if_version means the write landed and the version legitimately did not change."`
	Metadata              map[string]any  `json:"metadata,omitempty"`
	Notes                 []noteOut       `json:"notes,omitempty"`
	NotesNextBefore       *string         `json:"notes_next_before,omitempty" jsonschema:"pass back as task_get's notes_before to read the next, older page of this task's notes. Absent means there are none."`
	CreatedAt             string          `json:"created_at"`
	UpdatedAt             string          `json:"updated_at"`
	CreatedBy             string          `json:"created_by"`
	UpdatedBy             string          `json:"updated_by"`
	ArchivedAt            *string         `json:"archived_at,omitempty"`
}

// taskViewOut renders a domain.TaskView under a service.Projection — the
// service's own statement of what it put in the value, not this layer's guess
// at what the request implied. BlockedBy is never gated: the compact renderer
// always shows it because readiness is not optional information, and the JSON
// form must not disagree.
func taskViewOut(tv *domain.TaskView, proj service.Projection) taskOut {
	out := taskOut{
		Key:          tv.Key,
		Project:      tv.ProjectKey,
		Column:       tv.ColumnName,
		ColumnKind:   tv.ColumnKind,
		Type:         tv.Type,
		Priority:     tv.Priority.String(),
		Title:        tv.Title,
		Estimate:     tv.Estimate,
		Actual:       tv.Actual,
		EstimateUnit: tv.EstimateUnit,
		Tags:         tv.Tags,
		Assignee:     tv.Assignee,
		Reviewer:     tv.Reviewer,
		ClaimedBy:    tv.ClaimedBy,
		SubDone:      tv.SubDone,
		SubTotal:     tv.SubTotal,
		BlockedBy:    tv.BlockedBy,
		Ready:        tv.Ready,
		Version:      tv.Version,
		CreatedAt:    formatTime(tv.CreatedAt),
		UpdatedAt:    formatTime(tv.UpdatedAt),
		CreatedBy:    tv.CreatedBy,
		UpdatedBy:    tv.UpdatedBy,
		ArchivedAt:   formatTimePtr(tv.ArchivedAt),
		DueAt:        formatTimePtr(tv.DueAt),
	}
	// estimate_error = actual - estimate: surfaced only when both fields are
	// set, so a half-filled row does not advertise a meaningless number.
	// Positive = under-estimated (the work took longer than expected).
	if tv.Estimate != nil && tv.Actual != nil {
		err := *tv.Actual - *tv.Estimate
		out.EstimateError = &err
	}
	if tv.ClaimedBy != nil {
		out.ClaimExpiresAt = formatTimePtr(tv.ClaimExpiresAt)
		if tv.LeaseRemain != nil {
			secs := int(tv.LeaseRemain.Seconds())
			out.LeaseRemainingSeconds = &secs
		}
	}
	if proj.Has(service.IncludeBody) {
		out.Body = &tv.Body
	}
	if proj.Has(service.IncludeAcceptance) {
		out.Acceptance = acceptanceOutList(tv.Acceptance)
		// A clipped list that does not say it was clipped reads as the whole
		// list. The projection knows the real count; publish it.
		if total, ok := proj.AcceptanceTotals[tv.Key]; ok {
			t := total
			out.AcceptanceTotal = &t
		}
	}
	if proj.Has(service.IncludeNotes) {
		out.Notes = noteOutList(tv.Notes)
	}
	if proj.Has(service.IncludeMetadata) {
		out.Metadata = tv.Metadata
	}
	if proj.Has(service.IncludeLinks) {
		out.Blocks = tv.Blocks
	}
	// Outcome is the task's epistemic verdict; the default ("open") is hidden
	// so a routine read on a fresh task does not have to mention it.
	if tv.Outcome != "" && tv.Outcome != domain.OutcomeOpen {
		out.Outcome = string(tv.Outcome)
	}
	out.Conclusion = tv.Conclusion
	return out
}

func taskViewOutList(tvs []domain.TaskView, proj service.Projection) []taskOut {
	if len(tvs) == 0 {
		return nil
	}
	out := make([]taskOut, len(tvs))
	for i := range tvs {
		out[i] = taskViewOut(&tvs[i], proj)
	}
	return out
}

// columnOut is the wire form of one domain.Column.
type columnOut struct {
	Name     string      `json:"name"`
	Kind     domain.Kind `json:"kind"`
	WIPLimit *int        `json:"wip_limit,omitempty"`
}

func columnOutList(cols []domain.Column) []columnOut {
	if len(cols) == 0 {
		return nil
	}
	out := make([]columnOut, len(cols))
	for i, c := range cols {
		out[i] = columnOut{Name: c.Name, Kind: c.Kind, WIPLimit: c.WIPLimit}
	}
	return out
}

// projectOut is the wire form of a domain.Project plus its columns. Focus is
// deliberately omitted: domain.Project only carries the focused task's
// internal ID, and every external surface speaks keys, not UUIDs — with no
// service method to resolve one to the other from this call, surfacing the
// ID would break the "keys not UUIDs" contract (PLAN §6 principles).
type projectOut struct {
	Key                 string      `json:"key"`
	Name                string      `json:"name"`
	Description         string      `json:"description,omitempty"`
	Version             int         `json:"version"`
	EstimateUnit        string      `json:"estimate_unit"`
	EnforceDependencies bool        `json:"enforce_dependencies"`
	StrictDone          bool        `json:"strict_done"`
	ClaimTTLSeconds     int         `json:"claim_ttl_seconds"`
	Archived            bool        `json:"archived"`
	Columns             []columnOut `json:"columns,omitempty"`
}

func projectOutFrom(p domain.Project, cols []domain.Column) projectOut {
	return projectOut{
		Key:                 p.Key,
		Name:                p.Name,
		Description:         p.Description,
		Version:             p.Version,
		EstimateUnit:        p.EstimateUnit,
		EnforceDependencies: p.EnforceDependencies,
		StrictDone:          p.StrictDone,
		ClaimTTLSeconds:     p.ClaimTTLSeconds,
		Archived:            p.ArchivedAt != nil,
		Columns:             columnOutList(cols),
	}
}

// ---------------------------------------------------------------------------
// Scalar helpers shared by every tool's input/output conversion.
// ---------------------------------------------------------------------------

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := formatTime(*t)
	return &s
}

// parseRFC3339 parses a caller-supplied timestamp, naming the offending
// field in the error so a validation failure is actionable without a
// second round trip (AGENTS.md "Fail loud").
func parseRFC3339(field, s string) (time.Time, *domain.Error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, domain.Invalid(field,
			fmt.Sprintf("%q is not a valid RFC3339 timestamp: %v", s, err),
			"Use RFC3339, e.g. 2026-09-06T15:00:00Z.")
	}
	return t, nil
}

// parsePriorityName converts the canonical priority name used on every
// external surface into the internal int representation.
func parsePriorityName(field, s string) (domain.Priority, *domain.Error) {
	p, ok := domain.ParsePriority(s)
	if !ok {
		return domain.PriorityNone, domain.Invalid(field,
			fmt.Sprintf("%q is not a valid priority", s),
			"Use one of: none, low, medium, high, critical.")
	}
	return p, nil
}

// parseOutcomeName converts a wire-form outcome name into the domain value,
// naming the offending field so a validation error is actionable.
func parseOutcomeName(field, s string) (domain.Outcome, *domain.Error) {
	o, ok := domain.ParseOutcome(s)
	if !ok {
		return domain.OutcomeOpen, domain.Invalid(field,
			fmt.Sprintf("outcome %q is not valid", s),
			"Use one of: open, holds, refuted, superseded, moot.")
	}
	return o, nil
}

// outcomeNames is the enum published in every schema that accepts an outcome
// name, built from domain.AllOutcomes so the wire enum can never drift from
// the internal representation.
func outcomeNames() []string {
	names := make([]string, len(domain.AllOutcomes))
	for i, o := range domain.AllOutcomes {
		names[i] = string(o)
	}
	return names
}

// priorityNames is the enum published in every schema that accepts or
// returns a priority name, built from domain.Priority so the wire enum can
// never drift from the internal representation.
func priorityNames() []string {
	names := make([]string, 0, int(domain.PriorityCritical)+1)
	for p := domain.PriorityNone; p <= domain.PriorityCritical; p++ {
		names = append(names, p.String())
	}
	return names
}

// typeNames is the enum for domain.Type, built from domain.AllTypes.
func typeNames() []string {
	names := make([]string, len(domain.AllTypes))
	for i, t := range domain.AllTypes {
		names[i] = string(t)
	}
	return names
}

func toIncludes(vals []string) service.Includes {
	if len(vals) == 0 {
		return nil
	}
	out := make(service.Includes, len(vals))
	for i, v := range vals {
		out[i] = service.Include(v)
	}
	return out
}
