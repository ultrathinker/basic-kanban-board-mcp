package domain

import (
	"errors"
	"fmt"
)

// Code is the machine-readable error code carried in every tool envelope and
// every REST/UI error. The set is closed: adding one is a deliberate contract
// change, because agents branch on these.
type Code string

const (
	CodeNotFound            Code = "not_found"
	CodeValidation          Code = "validation"
	CodeConflict            Code = "conflict"
	CodeBlocked             Code = "blocked"
	CodeWIPExceeded         Code = "wip_exceeded"
	CodeClaimed             Code = "claimed"
	CodeForbidden           Code = "forbidden"
	CodeCycle               Code = "cycle"
	CodeRateLimited         Code = "rate_limited"
	CodePayloadTooLarge     Code = "payload_too_large"
	CodeIdempotencyMismatch Code = "idempotency_mismatch"
)

// Error is the single error type crossing layer boundaries. Remediation is not
// decoration: it is the sentence an agent reads to recover without a human, so
// it must name the concrete next action (see PLAN §6).
type Error struct {
	Code        Code
	Message     string
	Remediation string
	// Current carries the server's current state on a conflict so the caller can
	// merge without a second round trip. Only set for CodeConflict.
	Current any
	// Field names the offending input for CodeValidation, when applicable.
	Field string
	wrap  error
}

func (e *Error) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Field)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.wrap }

// Is lets errors.Is compare by code: errors.Is(err, domain.ErrNotFound).
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// Sentinels for errors.Is comparisons.
var (
	ErrNotFound            = &Error{Code: CodeNotFound}
	ErrValidation          = &Error{Code: CodeValidation}
	ErrConflict            = &Error{Code: CodeConflict}
	ErrBlocked             = &Error{Code: CodeBlocked}
	ErrWIPExceeded         = &Error{Code: CodeWIPExceeded}
	ErrClaimed             = &Error{Code: CodeClaimed}
	ErrForbidden           = &Error{Code: CodeForbidden}
	ErrCycle               = &Error{Code: CodeCycle}
	ErrRateLimited         = &Error{Code: CodeRateLimited}
	ErrPayloadTooLarge     = &Error{Code: CodePayloadTooLarge}
	ErrIdempotencyMismatch = &Error{Code: CodeIdempotencyMismatch}
)

func NotFound(what, key string) *Error {
	return &Error{
		Code:        CodeNotFound,
		Message:     fmt.Sprintf("%s %q not found", what, key),
		Remediation: "Check the key (case-insensitive, form PROJ-N) or call board_get to list what exists.",
	}
}

func Invalid(field, msg, remediation string) *Error {
	return &Error{Code: CodeValidation, Message: msg, Field: field, Remediation: remediation}
}

func Conflict(current any, expected, actual int) *Error {
	return &Error{
		Code:    CodeConflict,
		Message: fmt.Sprintf("version mismatch: you sent if_version=%d, current is %d", expected, actual),
		Remediation: fmt.Sprintf(
			"The current server state is attached as `current` — merge your change into it and retry with if_version=%d. No re-read needed.", actual),
		Current: current,
	}
}

func Blocked(key string, blockers []string) *Error {
	return &Error{
		Code:    CodeBlocked,
		Message: fmt.Sprintf("%s is blocked by %v", key, blockers),
		Remediation: fmt.Sprintf(
			"Finish %v first, remove the link with task_link, or move with force:true and a reason (admin scope).", blockers),
	}
}

func WIPExceeded(column string, limit int) *Error {
	return &Error{
		Code:    CodeWIPExceeded,
		Message: fmt.Sprintf("column %q is at its WIP limit of %d", column, limit),
		Remediation: "Finish or move something out of that column first, or move with force:true and a reason (admin scope).",
	}
}

func Claimed(key, by string) *Error {
	return &Error{
		Code:        CodeClaimed,
		Message:     fmt.Sprintf("%s is claimed by %s and the lease is still live", key, by),
		Remediation: "Pick another task with task_next, wait for the lease to expire, or steal it with force:true (admin scope).",
	}
}

func Forbidden(msg, remediation string) *Error {
	return &Error{Code: CodeForbidden, Message: msg, Remediation: remediation}
}

func Cycle(path []string) *Error {
	return &Error{
		Code:        CodeCycle,
		Message:     fmt.Sprintf("this link would create a cycle: %v", path),
		Remediation: "Dependencies must form a DAG, and a task may not block anything in its own parent chain. Remove one of the existing links first.",
	}
}

// AsError extracts a *Error from any error, or nil if it is not one.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}
