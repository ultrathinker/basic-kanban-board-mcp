package mcp

import "github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"

// errorEnvelope is the wire shape of a domain error inside the
// {ok:false, op, error:{...}} envelope (PLAN §6). Every field maps straight
// through from *domain.Error unchanged: code, message, remediation and the
// conflicting `current` state are the caller's only way to recover without a
// human, so nothing here may be summarised or dropped (AGENTS.md "Fail
// loud").
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
	return &errorEnvelope{
		Code:        string(e.Code),
		Message:     e.Message,
		Remediation: e.Remediation,
		Current:     e.Current,
		Field:       e.Field,
	}
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
// type keeps the envelope shape consistent across all nine tools instead of
// nine near-identical structs.
type toolMeta struct {
	Count      int                `json:"count,omitempty"`
	Warnings   []string           `json:"warnings,omitempty"`
	Replayed   bool               `json:"replayed,omitempty"`
	NotFound   []string           `json:"not_found,omitempty"`
	WIPFull    bool               `json:"wip_full,omitempty"`
	Reasons    *nextReasonsOut    `json:"reasons,omitempty"`
	BlockedTop []blockedSampleOut `json:"blocked_top,omitempty"`
	ClaimedKey string             `json:"claimed_key,omitempty"`
	StartedKey string             `json:"started_key,omitempty"`
}
