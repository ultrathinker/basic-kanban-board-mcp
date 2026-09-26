package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ---------------------------------------------------------------------------
// KANB-67: task_create's top-level project is the batch default. Items
// without their own project take it; an item's own project wins.
// ---------------------------------------------------------------------------

func TestRoundTrip_TaskCreate_TopLevelProjectIsTheDefault(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskCreate = &service.TaskCreateResult{Tasks: []domain.TaskView{fixedTask("I6-1"), fixedTask("OPS-1"), fixedTask("I6-2")}}

	res, sc := callTool(t, cs, "task_create", map[string]any{
		"project": "i6",
		"tasks": []map[string]any{
			{"title": "inherits"},
			{"project": "ops", "title": "keeps its own"},
			{"project": "  ", "title": "blank own project inherits too"},
		},
	})
	if res.IsError {
		t.Fatalf("task_create with a top-level project failed: %v", sc)
	}
	got := svc.LastTaskCreate.Tasks
	want := []string{"I6", "OPS", "I6"}
	if len(got) != len(want) {
		t.Fatalf("service got %d tasks, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].ProjectKey != w {
			t.Errorf("tasks[%d] (%q) went to project %q, want %q", i, got[i].Title, got[i].ProjectKey, w)
		}
	}
}

func TestRoundTrip_TaskCreate_BlankTopLevelProjectRefused(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	res, sc := callTool(t, cs, "task_create", map[string]any{
		"project": "   ",
		"tasks":   []map[string]any{{"project": "OPS", "title": "has its own"}},
	})
	if !res.IsError {
		t.Fatalf("a blank top-level project was accepted: %v", sc)
	}
	if len(svc.LastTaskCreate.Tasks) != 0 {
		t.Errorf("service was invoked despite a blank top-level project: %+v", svc.LastTaskCreate.Tasks)
	}
	errObj, _ := sc["error"].(map[string]any)
	if errObj["code"] != "validation" || errObj["field"] != "project" {
		t.Errorf("error = %v, want validation on field project", errObj)
	}
	if rem, _ := errObj["remediation"].(string); rem == "" {
		t.Errorf("no remediation on %v", errObj)
	}
}

// The published schema is what the SDK validates arguments against, so it
// must declare the top-level project as the string it now is, and must not
// still tell callers to keep it out.
func TestTaskCreateSchema_TopLevelProjectIsAPlainString(t *testing.T) {
	t.Parallel()
	s, ok := taskCreateTool().InputSchema.(*jsonschema.Schema)
	if !ok {
		t.Fatalf("task_create InputSchema is %T, want *jsonschema.Schema", taskCreateTool().InputSchema)
	}
	p := prop(s, "project")
	if p.Type != "string" || len(p.Types) != 0 {
		t.Errorf("top-level project schema type = %q %v, want plain string", p.Type, p.Types)
	}
	if strings.Contains(p.Description, "do NOT") || !strings.Contains(p.Description, "default") {
		t.Errorf("top-level project description %q should present it as the batch default", p.Description)
	}
	for _, r := range s.Required {
		if r == "project" {
			t.Error("the top-level project must stay optional")
		}
	}
}

// End to end through the real schema, the real service and a real store: a
// batch naming its project once lands every item without its own project
// there, and the item that names another project lands in that one.
func TestTaskCreate_TopLevelProjectEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{Path: filepath.Join(t.TempDir(), "kanban.db")})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := service.New(st, nil)
	admin := service.Actor{TokenID: "tok-test", Name: "tester", Scopes: domain.Scopes{domain.ScopeAdmin, domain.ScopeWrite, domain.ScopeRead}}
	for _, key := range []string{"KANB", "OPS"} {
		if _, err := svc.ProjectUpsert(ctx, admin, service.ProjectUpsertInput{Mode: service.UpsertCreate, Key: key, Name: key}); err != nil {
			t.Fatalf("create %s: %v", key, err)
		}
	}

	srv := NewServer(svc, "test")
	ct, stt := gomcp.NewInMemoryTransports()
	ss, err := srv.Connect(authContextFor(ctx), stt, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := gomcp.NewClient(&gomcp.Implementation{Name: "test-client", Version: "v0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	res, sc := callTool(t, cs, "task_create", map[string]any{
		"project": "kanb",
		"tasks": []map[string]any{
			{"title": "first default"},
			{"project": "ops", "title": "own project"},
			{"title": "second default"},
		},
	})
	if res.IsError {
		t.Fatalf("task_create failed: %v", sc)
	}
	data, _ := sc["data"].(map[string]any)
	tasks, _ := data["tasks"].([]any)
	var keys []string
	for _, raw := range tasks {
		task, _ := raw.(map[string]any)
		k, _ := task["key"].(string)
		keys = append(keys, k)
	}
	if strings.Join(keys, ",") != "KANB-1,OPS-1,KANB-2" {
		t.Errorf("created %v, want [KANB-1 OPS-1 KANB-2]", keys)
	}

	// Neither level set: the per-item error, naming both places to fix it.
	res, sc = callTool(t, cs, "task_create", map[string]any{
		"tasks": []map[string]any{{"title": "nowhere"}},
	})
	if !res.IsError {
		t.Fatalf("a batch with no project anywhere was accepted: %v", sc)
	}
	errObj, _ := sc["error"].(map[string]any)
	if msg, _ := errObj["message"].(string); msg != "project is required on each task item" {
		t.Errorf("message = %q", msg)
	}
}
