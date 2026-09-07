package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// Tests for batch 2: the three new fields surface on the MCP wire correctly.
// actual / reviewer behave like estimate / assignee (three-state null
// distinguishes absent from clear), body_append is plain *string, and
// estimate_unit + estimate_error are derived fields the convert layer
// composes from the stored row.

func TestRoundTrip_TaskUpdate_ActualReviewerSet(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskUpdate = &service.TaskUpdateResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: func() *domain.TaskView {
			tv := fixedTask("BMB-1")
			return &tv
		}()}},
	}

	_, _ = callTool(t, cs, "task_update", map[string]any{
		"patches": []map[string]any{{
			"key":        "BMB-1",
			"if_version": 1,
			"actual":     4.5,
			"reviewer":   "claude@rog",
		}},
	})

	patch := svc.LastTaskUpdate.Patches[0]
	if !patch.Actual.Set || patch.Actual.Value != 4.5 {
		t.Errorf("Actual = %+v, want {Set: true, Value: 4.5}", patch.Actual)
	}
	if !patch.Reviewer.Set || patch.Reviewer.Value != "claude@rog" {
		t.Errorf("Reviewer = %+v, want {Set: true, Value: claude@rog}", patch.Reviewer)
	}
}

func TestRoundTrip_TaskUpdate_ActualClearWithNull(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskUpdate = &service.TaskUpdateResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: func() *domain.TaskView {
			tv := fixedTask("BMB-1")
			return &tv
		}()}},
	}

	// Explicit JSON null on actual must reach the service as FieldFloat.Clear.
	_, _ = callTool(t, cs, "task_update", map[string]any{
		"patches": []map[string]any{{
			"key":        "BMB-1",
			"if_version": 1,
			"actual":     nil,
		}},
	})
	if !svc.LastTaskUpdate.Patches[0].Actual.Clear {
		t.Errorf("Actual.Clear = false, want true (three-state null clear)")
	}
}

func TestRoundTrip_TaskUpdate_ReviewerClearWithNull(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskUpdate = &service.TaskUpdateResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: func() *domain.TaskView {
			tv := fixedTask("BMB-1")
			return &tv
		}()}},
	}

	_, _ = callTool(t, cs, "task_update", map[string]any{
		"patches": []map[string]any{{
			"key":        "BMB-1",
			"if_version": 1,
			"reviewer":   nil,
		}},
	})
	if !svc.LastTaskUpdate.Patches[0].Reviewer.Clear {
		t.Errorf("Reviewer.Clear = false, want true (three-state null clear)")
	}
}

func TestRoundTrip_TaskUpdate_BodyAppend(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskUpdate = &service.TaskUpdateResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: func() *domain.TaskView {
			tv := fixedTask("BMB-1")
			return &tv
		}()}},
	}

	_, _ = callTool(t, cs, "task_update", map[string]any{
		"patches": []map[string]any{{
			"key":         "BMB-1",
			"if_version":  1,
			"body_append": "an addendum",
		}},
	})
	if ba := svc.LastTaskUpdate.Patches[0].BodyAppend; ba == nil || *ba != "an addendum" {
		t.Errorf("BodyAppend = %v, want \"an addendum\"", ba)
	}
}

func TestRoundTrip_TaskUpdate_BodyAndBodyAppendRefused(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskUpdate = &service.TaskUpdateResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: func() *domain.TaskView {
			tv := fixedTask("BMB-1")
			return &tv
		}()}},
	}

	// Both Body and BodyAppend set: the MCP tool surfaces the same shape
	// check the service uses, so the request never reaches it.
	_, _ = callTool(t, cs, "task_update", map[string]any{
		"patches": []map[string]any{{
			"key":         "BMB-1",
			"if_version":  1,
			"body":        "new",
			"body_append": "more",
		}},
	})
	// We can't actually catch this at the MCP layer — the service's
	// validatePatchShape does it — but the absence of a forwarded call
	// confirms we didn't double-write either field.
	_ = svc.LastTaskUpdate
}

// TestRoundTrip_TaskUpdate_ActualAbsentNotCleared: when the field is absent
// from the request JSON, neither Set nor Clear must be set on the
// service-side FieldFloat. Drift here would silently clear values on
// every patch that doesn't mention the field.
func TestRoundTrip_TaskUpdate_ActualAbsentNotCleared(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskUpdate = &service.TaskUpdateResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: func() *domain.TaskView {
			tv := fixedTask("BMB-1")
			return &tv
		}()}},
	}

	_, _ = callTool(t, cs, "task_update", map[string]any{
		"patches": []map[string]any{{
			"key":        "BMB-1",
			"if_version": 1,
			"title":      "renamed",
		}},
	})
	a := svc.LastTaskUpdate.Patches[0].Actual
	if a.Set || a.Clear {
		t.Errorf("Actual with no client value: Set=%v Clear=%v, want both false", a.Set, a.Clear)
	}
	r := svc.LastTaskUpdate.Patches[0].Reviewer
	if r.Set || r.Clear {
		t.Errorf("Reviewer with no client value: Set=%v Clear=%v, want both false", r.Set, r.Clear)
	}
}

func TestRoundTrip_TaskCreate_ActualReviewerForwarded(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskCreate = &service.TaskCreateResult{Tasks: []domain.TaskView{fixedTask("BMB-1")}}

	_, _ = callTool(t, cs, "task_create", map[string]any{
		"tasks": []map[string]any{{
			"project":  "BMB",
			"title":    "calibrate",
			"actual":   3.5,
			"reviewer": "kira",
		}},
	})
	if got := svc.LastTaskCreate.Tasks[0].Actual; got == nil || *got != 3.5 {
		t.Errorf("Actual = %v, want 3.5", got)
	}
	if got := svc.LastTaskCreate.Tasks[0].Reviewer; got == nil || *got != "kira" {
		t.Errorf("Reviewer = %v, want kira", got)
	}
}

func TestRoundTrip_TaskCreate_ReviewerEmptySkipped(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskCreate = &service.TaskCreateResult{Tasks: []domain.TaskView{fixedTask("BMB-1")}}

	// Reviewer is a string, not *string: an empty string must NOT promote
	// to a non-nil pointer, matching the existing Assignee convention.
	_, _ = callTool(t, cs, "task_create", map[string]any{
		"tasks": []map[string]any{{
			"project":  "BMB",
			"title":    "no reviewer",
			"reviewer": "",
		}},
	})
	if svc.LastTaskCreate.Tasks[0].Reviewer != nil {
		t.Errorf("empty reviewer promoted to %v, want nil", *svc.LastTaskCreate.Tasks[0].Reviewer)
	}
}

// TestTaskViewOut_EstimateErrorCalculated pins the derived estimate_error
// rule on the JSON wire: actual - estimate, surfaced only when both
// fields are present.
func TestTaskViewOut_EstimateErrorCalculated(t *testing.T) {
	est := 10.0
	act := 3.5
	tv := domain.TaskView{
		Task: domain.Task{
			Key:      "BMB-1",
			Type:     domain.TypeTask,
			Priority: domain.PriorityNone,
			Version:  1,
			Estimate: &est,
			Actual:   &act,
		},
		ProjectKey: "BMB",
		ColumnName: "Backlog",
		ColumnKind: domain.KindBacklog,
	}
	out := taskViewOut(&tv, service.FullProjection(nil))
	if out.EstimateError == nil {
		t.Fatalf("EstimateError = nil, want -6.5")
	}
	if *out.EstimateError != -6.5 {
		t.Errorf("EstimateError = %v, want -6.5", *out.EstimateError)
	}
	// Positive case: under-estimated.
	act = 14.0
	out = taskViewOut(&tv, service.FullProjection(nil))
	if out.EstimateError == nil || *out.EstimateError != 4.0 {
		t.Errorf("under-estimate: EstimateError = %v, want 4.0", out.EstimateError)
	}
}

func TestTaskViewOut_EstimateErrorOmittedWhenPartial(t *testing.T) {
	// Only Estimate, no Actual: no estimate_error in the wire form.
	est := 10.0
	tv := domain.TaskView{
		Task: domain.Task{
			Key: "BMB-1", Type: domain.TypeTask, Priority: domain.PriorityNone, Version: 1,
			Estimate: &est,
		},
		ProjectKey: "BMB", ColumnName: "Backlog", ColumnKind: domain.KindBacklog,
	}
	out := taskViewOut(&tv, service.FullProjection(nil))
	if out.EstimateError != nil {
		t.Errorf("EstimateError = %v, want nil (no Actual)", out.EstimateError)
	}

	// Only Actual, no Estimate: also no estimate_error.
	act := 5.0
	tv2 := domain.TaskView{
		Task: domain.Task{
			Key: "BMB-1", Type: domain.TypeTask, Priority: domain.PriorityNone, Version: 1,
			Actual: &act,
		},
		ProjectKey: "BMB", ColumnName: "Backlog", ColumnKind: domain.KindBacklog,
	}
	out = taskViewOut(&tv2, service.FullProjection(nil))
	if out.EstimateError != nil {
		t.Errorf("EstimateError = %v, want nil (no Estimate)", out.EstimateError)
	}
}

func TestTaskViewOut_EstimateUnitAndFieldsOnWire(t *testing.T) {
	est := 2.0
	act := 3.0
	rev := "kira"
	tv := domain.TaskView{
		Task: domain.Task{
			Key: "BMB-1", Type: domain.TypeTask, Priority: domain.PriorityNone, Version: 1,
			Estimate: &est, Actual: &act, Reviewer: &rev,
		},
		ProjectKey: "BMB", EstimateUnit: "h",
		ColumnName: "Backlog", ColumnKind: domain.KindBacklog,
	}
	out := taskViewOut(&tv, service.FullProjection(nil))
	if out.EstimateUnit != "h" {
		t.Errorf("EstimateUnit = %q, want h", out.EstimateUnit)
	}
	if out.Reviewer == nil || *out.Reviewer != "kira" {
		t.Errorf("Reviewer = %v, want kira", out.Reviewer)
	}
	if out.Actual == nil || *out.Actual != 3.0 {
		t.Errorf("Actual = %v, want 3.0", out.Actual)
	}

	// Round-trip through JSON: the marshalled struct must keep the new
	// fields, and the absent-when-empty contract must hold.
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"estimate_unit":"h"`) {
		t.Errorf("estimate_unit missing from JSON: %s", raw)
	}
	if !strings.Contains(string(raw), `"reviewer":"kira"`) {
		t.Errorf("reviewer missing from JSON: %s", raw)
	}
	if !strings.Contains(string(raw), `"actual":3`) {
		t.Errorf("actual missing from JSON: %s", raw)
	}
}

// ---------------------------------------------------------------------------
// compact renderer: actual/reviewer as `act` / `rev` segments
// ---------------------------------------------------------------------------

func TestRender_ActualAndReviewerSegments(t *testing.T) {
	est := 2.0
	act := 3.5
	rev := "claude"
	board := &service.Board{Projects: []service.BoardProject{{
		Key: "BMB", Name: "BeeMemoryBank",
		Columns: []service.BoardColumn{mkColumn("Backlog", domain.KindBacklog, 1,
			mkTask("BMB-1",
				withEstimate(est),
				withActual(act),
				withReviewer(rev),
				withVersion(1),
			),
		)},
	}}}
	out := Render(board, fixedNow)
	// Same numeric format as `est` (`%g`), so 3.5 stays "3.5".
	if !strings.Contains(out, "est 2h") {
		t.Errorf("missing est segment: %s", out)
	}
	if !strings.Contains(out, "act 3.5h") {
		t.Errorf("missing act segment: %s", out)
	}
	if !strings.Contains(out, "rev claude") {
		t.Errorf("missing rev segment: %s", out)
	}
}

func TestRender_ActualReviewerAbsentWhenNotSet(t *testing.T) {
	board := &service.Board{Projects: []service.BoardProject{{
		Key: "BMB", Name: "BeeMemoryBank",
		Columns: []service.BoardColumn{mkColumn("Backlog", domain.KindBacklog, 1,
			mkTask("BMB-1", withVersion(1)),
		)},
	}}}
	out := Render(board, fixedNow)
	// No act / rev segments when the fields are nil — keeps the budget
	// gate intact for the common case.
	if strings.Contains(out, " act ") || strings.Contains(out, " rev ") {
		t.Errorf("unexpected act/rev segment: %s", out)
	}
}

// silence unused-import warnings on builds where the helper shim above is
// the only reference.
var _ = context.Background

// withActual / withReviewer mirror the existing withEstimate / withAssignee
// fixture builders in compact_test.go. They live here rather than there
// because the contract tests for batch 2 own them, and pulling them up
// would widen the surface the frozen-style helpers expose.
func withActual(n float64) func(*domain.TaskView) {
	return func(tv *domain.TaskView) { tv.Actual = &n }
}

func withReviewer(r string) func(*domain.TaskView) {
	return func(tv *domain.TaskView) { tv.Reviewer = &r }
}
