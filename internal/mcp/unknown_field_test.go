package mcp

import (
	"context"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// TestUnknownField_RejectedAtEveryTool proves that additionalProperties:false
// is real and enforced by the SDK's schema validator. Silent tolerance is
// the failure mode PLAN §6 rule 1 exists to prevent: a typo'd field name
// would otherwise never surface, leaving the agent convinced it sent what
// it meant.
//
// We exercise at least one tool from each of the three input shapes
// (object-root, array-of-objects root, nested object inside array) to
// catch any one of them silently dropping the additionalProperties:false
// constraint. We also assert the handler never runs: the schema gates
// this before our code sees it.
func TestUnknownField_RejectedAtEveryTool(t *testing.T) {
	t.Parallel()

	t.Run("object_root_tool_task_get", func(t *testing.T) {
		t.Parallel()
		cs, svc := roundtripServer(t, NewServer)
		svc.DefaultTaskGet = &service.TaskGetResult{}

		res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
			Name: "task_get",
			Arguments: map[string]any{
				"keys":        []string{"BMB-1"},
				"surprise":    "I was not declared in the schema",
				"extra_field": 42,
			},
		})
		if err != nil {
			t.Fatalf("schema rejection should be a CallToolResult, not a Go error: %v", err)
		}
		if !res.IsError {
			t.Errorf("IsError = false, want true; extra top-level fields must be rejected")
		}
		tc, _ := res.Content[0].(*gomcp.TextContent)
		if tc == nil || !strings.Contains(tc.Text, "surprise") {
			t.Errorf("error text should name the unknown field; got %q", tc.Text)
		}
		if len(svc.LastTaskGet.Keys) > 0 {
			t.Errorf("handler was invoked despite invalid input: %+v", svc.LastTaskGet)
		}
	})

	t.Run("array_root_tool_task_create", func(t *testing.T) {
		t.Parallel()
		cs, svc := roundtripServer(t, NewServer)
		svc.DefaultTaskCreate = &service.TaskCreateResult{}

		res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
			Name: "task_create",
			Arguments: map[string]any{
				"tasks": []map[string]any{{
					"project": "BMB",
					"title":   "ok task",
				}},
				"surprise": "I was not declared in the schema",
			},
		})
		if err != nil {
			t.Fatalf("schema rejection should be a CallToolResult, not a Go error: %v", err)
		}
		if !res.IsError {
			t.Errorf("IsError = false, want true; extra top-level fields must be rejected")
		}
		if len(svc.LastTaskCreate.Tasks) > 0 {
			t.Errorf("handler was invoked despite invalid input: %+v", svc.LastTaskCreate)
		}
	})

	t.Run("nested_object_inside_array_task_update", func(t *testing.T) {
		t.Parallel()
		cs, svc := roundtripServer(t, NewServer)
		svc.DefaultTaskUpdate = &service.TaskUpdateResult{}

		res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
			Name: "task_update",
			Arguments: map[string]any{
				"patches": []map[string]any{{
					"key":        "BMB-1",
					"if_version": 1,
					"title":      "rename",
					"surprise":   "I was not declared in the schema",
				}},
			},
		})
		if err != nil {
			t.Fatalf("schema rejection should be a CallToolResult, not a Go error: %v", err)
		}
		if !res.IsError {
			t.Errorf("IsError = false, want true; nested extra field must be rejected")
		}
		tc, _ := res.Content[0].(*gomcp.TextContent)
		if tc == nil || !strings.Contains(tc.Text, "surprise") {
			t.Errorf("error text should name the unknown field; got %q", tc.Text)
		}
		if len(svc.LastTaskUpdate.Patches) > 0 {
			t.Errorf("handler was invoked despite invalid input: %+v", svc.LastTaskUpdate)
		}
	})
}

// TestUnknownField_SilentTypoCatches is the example case from PLAN §6
// rule 1: a typo'd "assigne" instead of "assignee" must not silently
// succeed. Without additionalProperties:false the patch would look like
// "no change to assignee" and the agent would never know.
func TestUnknownField_SilentTypoCatches(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskUpdate = &service.TaskUpdateResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true}},
	}

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "task_update",
		Arguments: map[string]any{
			"patches": []map[string]any{{
				"key":        "BMB-1",
				"if_version": 1,
				"assigne":    "tester", // typo
			},
			},
		},
	})
	if err != nil {
		t.Fatalf("schema rejection should be a CallToolResult, not a Go error: %v", err)
	}
	if !res.IsError {
		t.Errorf("typo'd field name accepted: silent tolerance is the failure mode")
	}
	if len(svc.LastTaskUpdate.Patches) > 0 {
		t.Errorf("handler ran with typo'd input: %+v", svc.LastTaskUpdate)
	}
}
