package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// TestBoardGet_ArrayEnumFiltersAreAccepted: filter.types and
// filter.column_kinds are arrays of enum values. The enum used to sit on the
// array itself, so the SDK compared ["bug"] to "bug" and refused every call;
// filter.types had never worked through MCP (found live, KANB-59). This goes
// through the real schema validation, not straight to the handler.
func TestBoardGet_ArrayEnumFiltersAreAccepted(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultBoardGet = emptyBoardWithRulebook()

	res := callToolRaw(t, cs, "board_get", map[string]any{
		"project": "BMB",
		"filter":  map[string]any{"types": []any{"bug", "feat"}, "column_kinds": []any{"active", "waiting"}},
	})
	if res.IsError {
		t.Fatalf("a valid array-enum filter was refused: %s", resultText(res))
	}
	f := svc.LastBoardGet.Filter
	if len(f.Types) != 2 || f.Types[0] != domain.TypeBug {
		t.Errorf("types did not reach the service: %v", f.Types)
	}
	if len(f.ColumnKinds) != 2 || f.ColumnKinds[0] != domain.KindActive || f.ColumnKinds[1] != domain.KindWaiting {
		t.Errorf("column_kinds did not reach the service: %v", f.ColumnKinds)
	}

	// A value outside the enum is still refused by the schema.
	res = callToolRaw(t, cs, "board_get", map[string]any{
		"project": "BMB", "filter": map[string]any{"column_kinds": []any{"in-progress"}},
	})
	if !res.IsError {
		t.Errorf("column_kinds:[in-progress] was accepted")
	}
}

// TestNoToolPutsAnEnumOnAnArray walks every published input schema: an enum
// next to items is the defect above, wherever it appears.
func TestNoToolPutsAnEnumOnAnArray(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		raw, _ := json.Marshal(tool.InputSchema)
		var schema any
		_ = json.Unmarshal(raw, &schema)
		walkSchema(schema, tool.Name, func(path string, m map[string]any) {
			_, hasEnum := m["enum"]
			_, hasItems := m["items"]
			if hasEnum && hasItems {
				t.Errorf("%s: enum on an array at %s refuses every value; put it on items", tool.Name, path)
			}
		})
	}
}

func walkSchema(v any, path string, visit func(string, map[string]any)) {
	switch x := v.(type) {
	case map[string]any:
		visit(path, x)
		for k, child := range x {
			walkSchema(child, path+"/"+k, visit)
		}
	case []any:
		for _, child := range x {
			walkSchema(child, path+"/[]", visit)
		}
	}
}
