package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// Tests for the outcome / conclusion wiring on the wire:
//
//   - task_update forwards both fields onto service.TaskPatch.Outcome and
//     service.TaskPatch.Conclusion, validating outcome against
//     domain.ParseOutcome and refusing invalid names with the same
//     Remediation shape as parsePriorityName.
//   - The read form (taskOut) carries outcome only when it is not the
//     default ("open"); conclusion is always emitted (omitempty hides it
//     when empty).
//
// Compact rendering tests for outcome live in actual_reviewer_test.go and
// budget fixture handling in compact_test.go.

func TestRoundTrip_TaskUpdate_OutcomeRefutedForwarded(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskUpdate = &service.TaskUpdateResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: func() *domain.TaskView {
			tv := fixedTask("BMB-1")
			tv.Outcome = domain.OutcomeRefuted
			tv.Conclusion = "the calibration was wrong"
			return &tv
		}()}},
	}

	_, _ = callTool(t, cs, "task_update", map[string]any{
		"patches": []map[string]any{{
			"key":        "BMB-1",
			"if_version": 1,
			"outcome":    "refuted",
			"conclusion": "the calibration was wrong",
		}},
	})

	patch := svc.LastTaskUpdate.Patches[0]
	if patch.Outcome == nil || *patch.Outcome != domain.OutcomeRefuted {
		t.Errorf("Outcome = %v, want OutcomeRefuted", patch.Outcome)
	}
	if patch.Conclusion == nil || *patch.Conclusion != "the calibration was wrong" {
		t.Errorf("Conclusion = %v, want %q", patch.Conclusion, "the calibration was wrong")
	}
}

func TestRoundTrip_TaskUpdate_OutcomeInvalidRefused(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskUpdate = &service.TaskUpdateResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: func() *domain.TaskView {
			tv := fixedTask("BMB-1")
			return &tv
		}()}},
	}

	// An unknown outcome must be rejected at the MCP layer with a domain
	// error and a remediation string — the call must never reach the
	// service. callTool fatals on empty Content, which is the success
	// shape; here we expect an error envelope, so call the SDK directly.
	res, err := cs.CallTool(t.Context(), &gomcp.CallToolParams{
		Name: "task_update",
		Arguments: map[string]any{
			"patches": []map[string]any{{
				"key":        "BMB-1",
				"if_version": 1,
				"outcome":    "nope",
			}},
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res == nil || len(res.Content) == 0 {
		t.Fatalf("expected error content, got nil result")
	}
	tc, ok := res.Content[0].(*gomcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *TextContent", res.Content[0])
	}
	if !strings.Contains(tc.Text, "outcome") {
		t.Errorf("error must name the offending field: %s", tc.Text)
	}
	if !strings.Contains(tc.Text, "open") || !strings.Contains(tc.Text, "moot") {
		t.Errorf("remediation must list valid outcome names: %s", tc.Text)
	}
	// The service must not have been touched — a request-shape error fails
	// the whole call before it reaches the write path.
	if svc.LastTaskUpdate.Patches != nil {
		t.Errorf("invalid outcome reached service: %+v", svc.LastTaskUpdate.Patches)
	}
}

func TestRoundTrip_TaskUpdate_ConclusionEmptyClears(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskUpdate = &service.TaskUpdateResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: func() *domain.TaskView {
			tv := fixedTask("BMB-1")
			return &tv
		}()}},
	}

	// Empty-string conclusion is the explicit-clear signal at the wire
	// layer; service.TaskPatch.Conclusion is a *string so the absence
	// (not sent) and the empty (sent) stay distinguishable in storage.
	_, _ = callTool(t, cs, "task_update", map[string]any{
		"patches": []map[string]any{{
			"key":        "BMB-1",
			"if_version": 1,
			"conclusion": "",
		}},
	})
	if c := svc.LastTaskUpdate.Patches[0].Conclusion; c == nil || *c != "" {
		t.Errorf("Conclusion = %v, want pointer to empty string", c)
	}
}

// TestTaskViewOut_OutcomeAndConclusionOnWire pins the read-side contract:
// outcome is on the wire only when it is not the default; conclusion is on
// the wire whenever it is non-empty (the field stays "omitempty" so an
// empty conclusion does not bloat every read).
func TestTaskViewOut_OutcomeAndConclusionOnWire(t *testing.T) {
	tv := domain.TaskView{
		Task: domain.Task{
			Key:      "BMB-1",
			Type:     domain.TypeTask,
			Priority: domain.PriorityNone,
			Version:  1,
			Outcome:  domain.OutcomeRefuted,
		},
		ProjectKey: "BMB", ColumnName: "Backlog", ColumnKind: domain.KindBacklog,
	}
	out := taskViewOut(&tv, service.FullProjection(nil))
	if out.Outcome != string(domain.OutcomeRefuted) {
		t.Errorf("Outcome = %q, want %q", out.Outcome, domain.OutcomeRefuted)
	}
	if out.Conclusion != "" {
		t.Errorf("Conclusion = %q, want empty (no conclusion set)", out.Conclusion)
	}

	tv.Conclusion = "verdict: the post-mortem held"
	tv.Outcome = domain.OutcomeOpen // open default — must be hidden
	out = taskViewOut(&tv, service.FullProjection(nil))
	if out.Outcome != "" {
		t.Errorf("Outcome = %q, want empty (default open is hidden)", out.Outcome)
	}
	if out.Conclusion != "verdict: the post-mortem held" {
		t.Errorf("Conclusion = %q, want %q", out.Conclusion, "verdict: the post-mortem held")
	}

	// Round-trip through JSON: outcome disappears when open; conclusion
	// disappears when empty.
	tv.Conclusion = ""
	out = taskViewOut(&tv, service.FullProjection(nil))
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"outcome"`) {
		t.Errorf("outcome leaked into JSON when default: %s", raw)
	}
	if strings.Contains(string(raw), `"conclusion"`) {
		t.Errorf("conclusion leaked into JSON when empty: %s", raw)
	}
}
