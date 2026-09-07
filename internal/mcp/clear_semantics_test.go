package mcp

import (
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// Regression for the Codex round-2 finding: tags and acceptance replace the
// whole collection, so an explicit empty array must CLEAR it — distinct from an
// absent key, which leaves it alone. The distinction has to survive JSON
// decoding, so these assert the decoded service.TaskPatch, not just the docs.

func TestRoundTrip_TaskUpdate_TagsAndAcceptanceEmptyClears(t *testing.T) {
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
			"tags":       []any{},
			"acceptance": []any{},
		}},
	})
	p := svc.LastTaskUpdate.Patches[0]
	// Non-nil, length zero: "replace with nothing" (clear), which the service
	// distinguishes from nil ("leave alone").
	if p.Tags == nil {
		t.Errorf("Tags is nil after tags:[]; want a non-nil empty slice (clear signal)")
	} else if len(p.Tags) != 0 {
		t.Errorf("Tags = %v, want empty", p.Tags)
	}
	if p.Acceptance == nil {
		t.Errorf("Acceptance is nil after acceptance:[]; want a non-nil empty slice (clear signal)")
	} else if len(p.Acceptance) != 0 {
		t.Errorf("Acceptance = %v, want empty", p.Acceptance)
	}
}

func TestRoundTrip_TaskUpdate_TagsAcceptanceAbsentLeftAlone(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskUpdate = &service.TaskUpdateResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: func() *domain.TaskView {
			tv := fixedTask("BMB-1")
			return &tv
		}()}},
	}

	// A patch that touches neither tags nor acceptance must leave both nil, so
	// the service does not clear them.
	_, _ = callTool(t, cs, "task_update", map[string]any{
		"patches": []map[string]any{{
			"key":        "BMB-1",
			"if_version": 1,
			"title":      "just a rename",
		}},
	})
	p := svc.LastTaskUpdate.Patches[0]
	if p.Tags != nil {
		t.Errorf("Tags = %v after an absent key, want nil (leave alone)", p.Tags)
	}
	if p.Acceptance != nil {
		t.Errorf("Acceptance = %v after an absent key, want nil (leave alone)", p.Acceptance)
	}
}
