package mcp

import (
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// ---------------------------------------------------------------------------
// KANB-47 — task_create(source_message) on the MCP surface.
// ---------------------------------------------------------------------------

// TestTaskCreate_SourceMessageForwarded: the parameter reaches the service,
// the response flags a replayed acceptance in meta, and the tool description
// states the external-actions caveat the acceptance guarantee does NOT cover.
func TestTaskCreate_SourceMessageForwarded(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskCreate = &service.TaskCreateResult{
		Tasks:           []domain.TaskView{fixedTask("KANB-9")},
		AlreadyAccepted: true,
	}

	res, sc := callTool(t, cs, "task_create", map[string]any{
		"tasks": []any{map[string]any{
			"project": "kanb", "title": "The commanded work",
		}},
		"source_message": "msg-7",
	})
	if res.IsError {
		t.Fatalf("callTool failed: %v", sc)
	}
	expectOK(t, sc, "task_create")

	if svc.LastTaskCreate.SourceMessage != "msg-7" {
		t.Errorf("service SourceMessage = %q, want msg-7", svc.LastTaskCreate.SourceMessage)
	}

	meta, _ := sc["meta"].(map[string]any)
	if meta["already_accepted"] != true {
		t.Errorf("meta.already_accepted = %v, want true on a replayed acceptance", meta["already_accepted"])
	}

	tool := toolByName(t, cs, "task_create")
	if !strings.Contains(tool.Description, "does not prevent an external command") {
		t.Error("task_create description does not state that acceptance does not cover external side effects")
	}
}
