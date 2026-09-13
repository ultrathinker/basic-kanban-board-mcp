package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// TestProgressHistory_TaskTarget verifies that calling with `task` derives
// the project key from the task key itself (ProgressHistoryInput always
// needs one, unlike progress_set) and forwards both to the service.
func TestProgressHistory_TaskTarget(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	svc.DefaultProgressHistory = &service.ProgressHistoryResult{
		ProjectKey: "KANB",
		TaskKey:    "KANB-3",
		Marks: []domain.ProgressMark{
			{ID: "pm-1", Assessor: "Codex", Percent: 70, CreatedAt: now},
			{ID: "pm-2", Assessor: "Codex", Percent: 45, CreatedAt: now.Add(time.Hour)},
		},
	}

	_, sc := callTool(t, cs, "progress_history", map[string]any{"task": "kanb-3"})
	expectOK(t, sc, "progress_history")

	if svc.LastProgressHistory.ProjectKey != "KANB" {
		t.Errorf("service ProjectKey = %q, want KANB (derived from the task key)", svc.LastProgressHistory.ProjectKey)
	}
	if svc.LastProgressHistory.TaskKey != "KANB-3" {
		t.Errorf("service TaskKey = %q, want KANB-3", svc.LastProgressHistory.TaskKey)
	}

	data := sc["data"].(map[string]any)
	if data["project"] != "KANB" {
		t.Errorf("data.project = %v, want KANB", data["project"])
	}
	if data["task"] != "KANB-3" {
		t.Errorf("data.task = %v, want KANB-3", data["task"])
	}
	marks := data["marks"].([]any)
	if len(marks) != 2 {
		t.Fatalf("marks len = %d, want 2", len(marks))
	}
	// Chronological order preserved: 70 first, then the rollback to 45.
	if marks[0].(map[string]any)["percent"] != float64(70) {
		t.Errorf("marks[0].percent = %v, want 70", marks[0].(map[string]any)["percent"])
	}
	if marks[1].(map[string]any)["percent"] != float64(45) {
		t.Errorf("marks[1].percent = %v, want 45 (the rollback, still visible)", marks[1].(map[string]any)["percent"])
	}
	if data["total"] != float64(2) {
		t.Errorf("data.total = %v, want 2", data["total"])
	}
}

// TestProgressHistory_ProjectTarget verifies the project-only scope: no task
// key at all, TaskKey left empty end to end.
func TestProgressHistory_ProjectTarget(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	svc.DefaultProgressHistory = &service.ProgressHistoryResult{
		ProjectKey: "KANB",
		Marks: []domain.ProgressMark{
			{ID: "pm-1", Assessor: "Claude", Percent: 30, CreatedAt: time.Now().UTC()},
		},
	}

	_, sc := callTool(t, cs, "progress_history", map[string]any{"project": "kanb"})
	expectOK(t, sc, "progress_history")

	if svc.LastProgressHistory.ProjectKey != "KANB" {
		t.Errorf("service ProjectKey = %q, want KANB", svc.LastProgressHistory.ProjectKey)
	}
	if svc.LastProgressHistory.TaskKey != "" {
		t.Errorf("service TaskKey = %q, want empty for a project-level history", svc.LastProgressHistory.TaskKey)
	}
	data := sc["data"].(map[string]any)
	if _, present := data["task"]; present {
		t.Errorf("data.task should be omitted for a project-level history: %v", data["task"])
	}
}

// TestProgressHistory_MutualExclusionValidation mirrors progress_set's own
// coverage of the same rule: exactly one of task/project.
func TestProgressHistory_MutualExclusionValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args map[string]any
	}{
		{"both", map[string]any{"task": "KANB-3", "project": "KANB"}},
		{"neither", map[string]any{}},
		{"both_blank", map[string]any{"task": "   ", "project": "   "}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cs, svc := roundtripServer(t, NewServer)

			res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
				Name: "progress_history", Arguments: tc.args,
			})
			if err != nil {
				t.Fatalf("CallTool unexpected protocol error: %v", err)
			}
			env := errorEnvelopeOf(t, res)
			if env["code"] != string(domain.CodeValidation) {
				t.Errorf("code = %v, want %v", env["code"], domain.CodeValidation)
			}
			if svc.LastProgressHistoryActor.TokenID != "" {
				t.Errorf("service was invoked despite the validation failure: %+v", svc.LastProgressHistory)
			}
		})
	}
}

// TestProgressHistory_LimitCapsToMostRecent verifies the truncation
// direction: when the scope's history is longer than `limit`, the OLDEST
// marks are dropped and the NEWEST survive, still in chronological order —
// and `total` still reports the full count.
func TestProgressHistory_LimitCapsToMostRecent(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	var marks []domain.ProgressMark
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		marks = append(marks, domain.ProgressMark{
			ID: fmt.Sprintf("pm-%d", i), Assessor: "alpha",
			Percent: i * 10, CreatedAt: base.Add(time.Duration(i) * time.Hour),
		})
	}
	svc.DefaultProgressHistory = &service.ProgressHistoryResult{
		ProjectKey: "KANB", TaskKey: "KANB-3", Marks: marks,
	}

	_, sc := callTool(t, cs, "progress_history", map[string]any{"task": "KANB-3", "limit": 2})
	expectOK(t, sc, "progress_history")

	data := sc["data"].(map[string]any)
	if data["total"] != float64(5) {
		t.Errorf("data.total = %v, want 5 (the uncapped count)", data["total"])
	}
	got := data["marks"].([]any)
	if len(got) != 2 {
		t.Fatalf("marks len = %d, want 2 (the limit)", len(got))
	}
	// The two most recent: percent 30 then 40, oldest-first among themselves.
	if got[0].(map[string]any)["percent"] != float64(30) {
		t.Errorf("marks[0].percent = %v, want 30 (kept, not the oldest 0)", got[0].(map[string]any)["percent"])
	}
	if got[1].(map[string]any)["percent"] != float64(40) {
		t.Errorf("marks[1].percent = %v, want 40 (the newest)", got[1].(map[string]any)["percent"])
	}
	meta := sc["meta"].(map[string]any)
	if meta["count"] != float64(2) {
		t.Errorf("meta.count = %v, want 2", meta["count"])
	}
}

// TestProgressHistory_NoLimitReturnsEverythingUpToTheCap verifies the
// default: an omitted `limit` returns the whole history rather than an
// empty result, as long as it fits under domain.MaxProgressHistoryLimit.
func TestProgressHistory_NoLimitReturnsEverythingUpToTheCap(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	svc.DefaultProgressHistory = &service.ProgressHistoryResult{
		ProjectKey: "KANB", TaskKey: "KANB-3",
		Marks: []domain.ProgressMark{
			{ID: "pm-1", Assessor: "alpha", Percent: 10, CreatedAt: time.Now().UTC()},
			{ID: "pm-2", Assessor: "alpha", Percent: 20, CreatedAt: time.Now().UTC()},
			{ID: "pm-3", Assessor: "alpha", Percent: 30, CreatedAt: time.Now().UTC()},
		},
	}

	_, sc := callTool(t, cs, "progress_history", map[string]any{"task": "KANB-3"})
	expectOK(t, sc, "progress_history")
	data := sc["data"].(map[string]any)
	if got := len(data["marks"].([]any)); got != 3 {
		t.Fatalf("marks len = %d, want 3 (no limit given, well under the cap)", got)
	}
}

// TestProgressHistory_AvailableOnReadOnlyServer is progress_set's inverse:
// this is a read, so unlike progress_set it belongs on /mcp/readonly.
func TestProgressHistory_AvailableOnReadOnlyServer(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewReadOnlyServer)

	roTools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	found := false
	for _, tl := range roTools.Tools {
		if tl.Name == "progress_history" {
			found = true
		}
	}
	if !found {
		t.Fatalf("read-only server must list progress_history, but did not: %v", names(roTools.Tools))
	}

	svc.DefaultProgressHistory = &service.ProgressHistoryResult{ProjectKey: "KANB"}
	res, sc := callTool(t, cs, "progress_history", map[string]any{"project": "KANB"})
	if res.IsError {
		t.Fatalf("progress_history failed on the read-only server: %v", sc)
	}
	expectOK(t, sc, "progress_history")
}

// TestProgressHistory_DescriptionContent verifies the published description
// covers the operational context PLAN §6 requires: what it reads, that
// rollbacks stay visible, and how to pick a target.
func TestProgressHistory_DescriptionContent(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)
	tool := toolByName(t, cs, "progress_history")
	desc := tool.Description

	for _, phrase := range []string{"history", "rolled back", "task", "project", "chronological"} {
		if !strings.Contains(desc, phrase) {
			t.Errorf("progress_history description missing phrase %q:\n%s", phrase, desc)
		}
	}
}
