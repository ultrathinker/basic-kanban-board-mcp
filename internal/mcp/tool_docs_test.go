package mcp

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// The schema and the tool description are the entire briefing a model gets:
// it never reads PLAN.md. Every test in this file therefore asserts against
// the *published* form — what tools/list actually sends — rather than against
// the Go constants behind it.

// schemaNode walks a published JSON Schema by map key. Array element schemas
// are reached through the literal "items" segment, matching JSON Schema itself.
func schemaNode(t *testing.T, root any, path ...string) map[string]any {
	t.Helper()
	raw, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	var node map[string]any
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	for i, seg := range path {
		next, ok := node[seg].(map[string]any)
		if !ok {
			t.Fatalf("schema path %v: no object at %q (step %d)", path, seg, i)
		}
		node = next
	}
	return node
}

// TestVersionSemantics_PinnedInOutputSchema is LIVE-FINDINGS §6. The behaviour
// was always correct — the response carries the post-write version — but the
// rule was written down nowhere a model looks, so a dogfooding agent could not
// tell "sent 2, got 2" (a note, which by design does not bump) from a stale
// echo. It defended itself with a task_get before every versioned write and
// doubled its write cost for a whole session.
//
// The description is the fix, so the description is what gets pinned.
func TestVersionSemantics_PinnedInOutputSchema(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)

	// Every tool that returns a task shares taskOut, so the same sentence has
	// to reach the schema through each of the two result shapes.
	cases := []struct {
		tool string
		path []string
	}{
		{"task_update", []string{"properties", "data", "properties", "items", "items", "properties", "task", "properties", "version"}},
		{"task_get", []string{"properties", "data", "properties", "items", "items", "properties", "task", "properties", "version"}},
		{"task_next", []string{"properties", "data", "properties", "tasks", "items", "properties", "version"}},
		{"task_create", []string{"properties", "data", "properties", "tasks", "items", "properties", "version"}},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			t.Parallel()
			tool := toolByName(t, cs, tc.tool)
			if tool.OutputSchema == nil {
				t.Fatalf("%s publishes no output schema", tc.tool)
			}
			node := schemaNode(t, tool.OutputSchema, tc.path...)
			got, _ := node["description"].(string)
			if got != taskVersionDoc {
				t.Errorf("%s version description drifted from taskVersionDoc:\n got: %q\nwant: %q", tc.tool, got, taskVersionDoc)
			}
		})
	}
}

// TestVersionSemantics_StatedInProse pins the same two facts on the surfaces
// that carry sentences: the initialize instructions and the task_update
// description. Both must survive independently — an agent that skims the
// instructions and one that reads only the tool it is about to call are both
// real.
func TestVersionSemantics_StatedInProse(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)

	surfaces := map[string]string{
		"instructions":            instructionsText,
		"task_update description": toolByName(t, cs, "task_update").Description,
	}
	// (a) the returned version is post-write and authoritative; (b) note,
	// lease and focus do not move it by design.
	facts := []string{"AFTER", "authoritative", "never re-read", "`note`", "`focus`", "did not change"}
	for name, text := range surfaces {
		for _, fact := range facts {
			if !strings.Contains(text, fact) {
				t.Errorf("%s no longer states %q:\n%s", name, fact, text)
			}
		}
	}
}

// TestRefSyntax_ShownLiterally is LIVE-FINDINGS §7: the description read
// "task keys or `@ref`s", and the agent sent "@ref:scaffold". The prefix is
// the single character @; only a literal example says that unambiguously.
func TestRefSyntax_ShownLiterally(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)
	tool := toolByName(t, cs, "task_create")

	item := schemaNode(t, tool.InputSchema, "properties", "tasks", "items", "properties")
	for _, field := range []string{"parent", "blocked_by", "ref"} {
		prop, ok := item[field].(map[string]any)
		if !ok {
			t.Fatalf("task_create has no %q property", field)
		}
		desc, _ := prop["description"].(string)
		if !strings.Contains(desc, "@scaffold") {
			t.Errorf("%s description lacks a literal @<ref> example: %q", field, desc)
		}
		if strings.Contains(desc, "@ref") {
			t.Errorf("%s description still writes %q, which reads as a literal prefix: %q", field, "@ref", desc)
		}
	}
	if !strings.Contains(tool.Description, `["@scaffold"]`) {
		t.Errorf("task_create description lacks the literal reference example:\n%s", tool.Description)
	}
	// Not even as a counter-example: naming the wrong form is how it gets copied.
	if strings.Contains(tool.Description, "@ref") {
		t.Errorf("task_create description still writes %q:\n%s", "@ref", tool.Description)
	}
}

// TestResultShapes_DocumentedInDescriptions is LIVE-FINDINGS §5 and §3. The
// two shapes are deliberate — task_get answers per requested key and must
// report a missing one in place — but a client that wrote code against one is
// surprised by the other, and the description is the only place it can learn
// the difference.
func TestResultShapes_DocumentedInDescriptions(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)

	cases := []struct {
		tool string
		want []string
	}{
		{"task_get", []string{"`data.items[]`", "`data.tasks[]`"}},
		{"task_next", []string{"`data.tasks[]`", "`data.items[]`"}},
		{"task_create", []string{"`data.tasks[]`"}},
		{"task_update", []string{"`data.items[]`"}},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			t.Parallel()
			desc := toolByName(t, cs, tc.tool).Description
			for _, want := range tc.want {
				if !strings.Contains(desc, want) {
					t.Errorf("%s description does not name %s:\n%s", tc.tool, want, desc)
				}
			}
		})
	}
}

// TestEnvelope_TopLevelShapeIsPinned is LIVE-FINDINGS §3: a client reached for
// structuredContent.tasks[].key and found nothing, because the envelope nests
// results under `data` (PLAN §6). The emitted shape was right; nothing pinned
// it. This does.
func TestEnvelope_TopLevelShapeIsPinned(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskCreate = &service.TaskCreateResult{
		Tasks: []domain.TaskView{fixedTask("BMB-1")},
	}

	_, sc := callTool(t, cs, "task_create", map[string]any{
		"tasks": []map[string]any{{"project": "BMB", "title": "first task"}},
	})

	keys := make([]string, 0, len(sc))
	for k := range sc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got, want := strings.Join(keys, ","), "data,meta,ok,op"; got != want {
		t.Errorf("envelope keys = %s, want %s", got, want)
	}
	if _, stray := sc["tasks"]; stray {
		t.Errorf("tasks must live under data, never at the top level: %v", sc)
	}
	tasks, _ := sc["data"].(map[string]any)["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("data.tasks len = %d, want 1", len(tasks))
	}
	first, _ := tasks[0].(map[string]any)
	if first["key"] != "BMB-1" {
		t.Errorf("data.tasks[0].key = %v, want BMB-1", first["key"])
	}
	if _, ok := first["version"]; !ok {
		t.Errorf("data.tasks[0] carries no version; a caller cannot chain if_version: %v", first)
	}
}
