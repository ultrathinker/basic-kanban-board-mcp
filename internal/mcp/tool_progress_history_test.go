package mcp

import (
	"context"
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
		Total: 2,
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

// TestProgressHistory_LimitIsAppliedByTheReadNotByTheHandler pins what this
// tool is actually responsible for once the bound moved into SQL: passing the
// caller's limit DOWN to the service, and reporting the scope total the
// service returns verbatim.
//
// It used to assert the truncation itself — with a fake service that returned
// all five marks and a handler that sliced them. That proved the handler's
// arithmetic and nothing about the product: the marks table is append-only,
// so read-everything-then-slice does all the work the limit exists to avoid.
// The truncation is now a property of the store (see
// TestProgressHistory_HistoryTail_KeepsTheNewest in internal/store), and what
// is left to check here is the wiring.
func TestProgressHistory_LimitIsAppliedByTheReadNotByTheHandler(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	// What a real service returns for limit:2 over a five-mark scope — the
	// two newest, oldest-first among themselves, and the UNCAPPED total.
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	svc.DefaultProgressHistory = &service.ProgressHistoryResult{
		ProjectKey: "KANB", TaskKey: "KANB-3",
		Marks: []domain.ProgressMark{
			{ID: "pm-3", Assessor: "alpha", Percent: 30, CreatedAt: base.Add(3 * time.Hour)},
			{ID: "pm-4", Assessor: "alpha", Percent: 40, CreatedAt: base.Add(4 * time.Hour)},
		},
		Total: 5,
	}

	_, sc := callTool(t, cs, "progress_history", map[string]any{"task": "KANB-3", "limit": 2})
	expectOK(t, sc, "progress_history")

	// The load-bearing assertion: the limit reached the read.
	if svc.LastProgressHistory.Limit != 2 {
		t.Errorf("service was called with Limit = %d, want 2 — the bound must go into the read, not be applied to its result",
			svc.LastProgressHistory.Limit)
	}

	data := sc["data"].(map[string]any)
	if data["total"] != float64(5) {
		t.Errorf("data.total = %v, want 5 (the scope total the service reported, not the page size)", data["total"])
	}
	got := data["marks"].([]any)
	if len(got) != 2 {
		t.Fatalf("marks len = %d, want 2", len(got))
	}
	if got[0].(map[string]any)["percent"] != float64(30) || got[1].(map[string]any)["percent"] != float64(40) {
		t.Errorf("marks were reordered or altered: %v", got)
	}
	meta := sc["meta"].(map[string]any)
	if meta["count"] != float64(2) {
		t.Errorf("meta.count = %v, want 2", meta["count"])
	}
}

// TestProgressHistory_OversizedLimitIsRefusedBySchemaNotSilentlyWidened pins
// the other half of the wiring: a caller cannot widen the read beyond
// domain.MaxProgressHistoryLimit.
//
// Note WHERE that is enforced. The handler clamps too — belt and braces for a
// direct Go caller — but over MCP the request never reaches it: the published
// inputSchema carries the maximum, so an oversized limit is a validation
// error, not a silently shrunk read. That is the better answer (the agent
// learns the bound from tools/list and from the refusal), and this test pins
// it as the observable behaviour rather than pinning the unreachable clamp.
func TestProgressHistory_OversizedLimitIsRefusedBySchemaNotSilentlyWidened(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultProgressHistory = &service.ProgressHistoryResult{ProjectKey: "KANB", TaskKey: "KANB-3"}

	_, sc := callTool(t, cs, "progress_history", map[string]any{
		"task": "KANB-3", "limit": domain.MaxProgressHistoryLimit * 10,
	})
	if sc["ok"] != false {
		t.Fatalf("an oversized limit was accepted: %v", sc)
	}
	if svc.LastProgressHistoryActor.TokenID != "" {
		t.Errorf("the service was read despite the refusal: %+v", svc.LastProgressHistory)
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
		Total: 3,
	}

	_, sc := callTool(t, cs, "progress_history", map[string]any{"task": "KANB-3"})
	expectOK(t, sc, "progress_history")
	data := sc["data"].(map[string]any)
	if got := len(data["marks"].([]any)); got != 3 {
		t.Fatalf("marks len = %d, want 3 (no limit given, well under the cap)", got)
	}
	// An omitted limit must still bound the READ — at the cap, not at
	// "everything". This is the clause the old version of this test could not
	// make, because the handler decided the size after the fact.
	if svc.LastProgressHistory.Limit != domain.MaxProgressHistoryLimit {
		t.Errorf("omitted limit reached the service as %d, want the cap %d",
			svc.LastProgressHistory.Limit, domain.MaxProgressHistoryLimit)
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
