package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// roundtripServer wires a fake-service-backed mcp.Server and a fresh
// ClientSession over in-memory transports, with the actor pre-injected on
// the server-side Connect ctx (per the handover's trap #2: the in-memory
// transport has no per-HTTP-request context to hook into, so the auth
// context must be on the server's Connect call itself).
//
// It returns the connected ClientSession and the fake service so tests can
// arrange scripted service responses and inspect the last forwarded call.
func roundtripServer(t *testing.T, newServer func(svc service.Service, version string) *gomcp.Server) (*gomcp.ClientSession, *fakeService) {
	t.Helper()
	svc := &fakeService{}
	srv := newServer(svc, "test")
	ct, st := gomcp.NewInMemoryTransports()
	serverCtx := authContextFor(context.Background())
	ss, err := srv.Connect(serverCtx, st, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	client := gomcp.NewClient(&gomcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	cs, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, svc
}

// callTool is a small wrapper that calls cs.CallTool and returns the
// result; the second return is the JSON-decoded structuredContent (typed
// to map for assertions).
func callTool(t *testing.T, cs *gomcp.ClientSession, name string, args map[string]any) (*gomcp.CallToolResult, map[string]any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	if res == nil {
		t.Fatalf("CallTool(%s): nil result", name)
	}
	if len(res.Content) == 0 {
		t.Fatalf("CallTool(%s): empty Content", name)
	}
	if res.StructuredContent == nil {
		t.Fatalf("CallTool(%s): nil StructuredContent", name)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structuredContent: %v", err)
	}
	var sc map[string]any
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	return res, sc
}

// expectOK asserts the envelope's happy-path markers. The MCP wire form
// has ok:true at the root of structuredContent; per-item tools still carry
// ok:true at the envelope level even when individual items errored (so the
// envelope's meta.warnings is the signal for those — checked in
// dedicated tests).
func expectOK(t *testing.T, sc map[string]any, op string) {
	t.Helper()
	ok, _ := sc["ok"].(bool)
	if !ok {
		t.Fatalf("op=%s: structuredContent ok=false: %v", op, sc)
	}
	if got, _ := sc["op"].(string); got != op {
		t.Fatalf("op=%s: structuredContent op=%q", op, got)
	}
}

// fixedTask is the deterministic TaskView every round-trip test that
// needs an echoed task feeds back from its fake service. The fields used by
// JSON output are populated; the rest are zero and omitted.
func fixedTask(key string) domain.TaskView {
	return domain.TaskView{
		Task: domain.Task{
			Key:       key,
			Title:     "Sample " + key,
			Type:      domain.TypeTask,
			Priority:  domain.PriorityNone,
			Version:   1,
			CreatedAt: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
			CreatedBy: "tester",
			UpdatedBy: "tester",
		},
		ProjectKey: "BMB",
		ColumnName: "Backlog",
		ColumnKind: domain.KindBacklog,
	}
}

// ---------------------------------------------------------------------------
// board_get
// ---------------------------------------------------------------------------

func TestRoundTrip_BoardGet_Success(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	col := &service.BoardColumn{Name: "Backlog", Kind: domain.KindBacklog, Count: 1}
	col.Tasks = []domain.TaskView{fixedTask("BMB-1")}
	svc.DefaultBoardGet = &service.Board{
		Projects: []service.BoardProject{{
			Key: "BMB", Name: "BeeMemoryBank",
			Columns: []service.BoardColumn{*col},
		}},
	}

	res, sc := callTool(t, cs, "board_get", map[string]any{"project": "BMB"})
	expectOK(t, sc, "board_get")

	// Compact format is the default; the text content must call into Render
	// and start with the version header.
	if len(res.Content) == 0 {
		t.Fatalf("empty Content")
	}
	tc, ok := res.Content[0].(*gomcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *TextContent", res.Content[0])
	}
	if !strings.HasPrefix(tc.Text, "compact_version=") {
		t.Errorf("compact text does not start with version header: %q", tc.Text)
	}

	// StructuredContent: projects[0].columns[0].tasks[0].key must round-trip.
	projects, _ := sc["data"].(map[string]any)["projects"].([]any)
	if len(projects) != 1 {
		t.Fatalf("projects len = %d, want 1", len(projects))
	}
	p0 := projects[0].(map[string]any)
	if p0["key"] != "BMB" {
		t.Errorf("project key = %v", p0["key"])
	}
	cols, _ := p0["columns"].([]any)
	if len(cols) != 1 {
		t.Fatalf("columns len = %d, want 1", len(cols))
	}
	c0 := cols[0].(map[string]any)
	tasks, _ := c0["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("tasks len = %d, want 1", len(tasks))
	}
	if tasks[0].(map[string]any)["key"] != "BMB-1" {
		t.Errorf("task key = %v", tasks[0].(map[string]any)["key"])
	}

	// Service was invoked with our input (not the empty zero value).
	if svc.LastBoardGet.ProjectKey != "BMB" {
		t.Errorf("LastBoardGet.ProjectKey = %q, want %q", svc.LastBoardGet.ProjectKey, "BMB")
	}
	if !svc.LastBoardGetActor.CanWrite() {
		t.Errorf("actor scopes lost in transport: %v", svc.LastBoardGetActor.Scopes)
	}
}

func TestRoundTrip_BoardGet_JSONFormat(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultBoardGet = &service.Board{
		Projects: []service.BoardProject{{
			Key: "BMB", Name: "BeeMemoryBank",
			Columns: []service.BoardColumn{{Name: "Backlog", Kind: domain.KindBacklog, Count: 0}},
		}},
	}

	res, _ := callTool(t, cs, "board_get", map[string]any{"project": "BMB", "format": "json"})
	tc, ok := res.Content[0].(*gomcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *TextContent", res.Content[0])
	}
	// JSON format: text content is pretty-JSON, not the compact grammar.
	if strings.HasPrefix(tc.Text, "compact_version=") {
		t.Errorf("format=json should not call Render: %q", tc.Text)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &parsed); err != nil {
		t.Fatalf("format=json text is not JSON: %v\n%s", err, tc.Text)
	}
	if _, ok := parsed["data"]; !ok {
		t.Errorf("JSON text missing data envelope: %v", parsed)
	}
}

// ---------------------------------------------------------------------------
// task_get
// ---------------------------------------------------------------------------

func TestRoundTrip_TaskGet_Success(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskGet = &service.TaskGetResult{
		Tasks: []domain.TaskView{fixedTask("BMB-1")},
	}

	_, sc := callTool(t, cs, "task_get", map[string]any{"keys": []string{"BMB-1"}})
	expectOK(t, sc, "task_get")

	items, _ := sc["data"].(map[string]any)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items len = %d", len(items))
	}
	item := items[0].(map[string]any)
	if item["key"] != "BMB-1" {
		t.Errorf("item key = %v", item["key"])
	}
	if item["ok"] != true {
		t.Errorf("item ok = %v, want true", item["ok"])
	}
}

func TestRoundTrip_TaskGet_PerItemNotFound(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	// Service echoes only BMB-1; BMB-9 must be reported per-item as not_found.
	svc.DefaultTaskGet = &service.TaskGetResult{
		Tasks:    []domain.TaskView{fixedTask("BMB-1")},
		NotFound: []string{"BMB-9"},
	}

	_, sc := callTool(t, cs, "task_get", map[string]any{"keys": []string{"BMB-1", "BMB-9"}})
	items, _ := sc["data"].(map[string]any)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items len = %d, want 2", len(items))
	}
	// Order preserved.
	if items[0].(map[string]any)["ok"] != true {
		t.Errorf("BMB-1 should be ok")
	}
	if items[1].(map[string]any)["ok"] != false {
		t.Errorf("BMB-9 should be ok:false")
	}
	if c := items[1].(map[string]any)["error"].(map[string]any)["code"]; c != "not_found" {
		t.Errorf("BMB-9 error code = %v, want not_found", c)
	}
	meta := sc["meta"].(map[string]any)
	if nf, _ := meta["not_found"].([]any); len(nf) != 1 || nf[0] != "BMB-9" {
		t.Errorf("meta.not_found = %v, want [BMB-9]", meta["not_found"])
	}
}

// ---------------------------------------------------------------------------
// task_create
// ---------------------------------------------------------------------------

func TestRoundTrip_TaskCreate_Success(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskCreate = &service.TaskCreateResult{
		Tasks: []domain.TaskView{fixedTask("BMB-1")},
	}

	_, sc := callTool(t, cs, "task_create", map[string]any{
		"tasks": []map[string]any{{
			"project": "BMB",
			"title":   "first task",
			"ref":     "first",
		}},
	})
	expectOK(t, sc, "task_create")
	tasks, _ := sc["data"].(map[string]any)["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("tasks len = %d", len(tasks))
	}

	// Service received the new task with a normalized project key and the
	// ref the caller supplied (refs are resolved inside the batch by the
	// service; we only forward the request here).
	if got := svc.LastTaskCreate.Tasks[0].ProjectKey; got != "BMB" {
		t.Errorf("ProjectKey = %q, want BMB", got)
	}
	if got := svc.LastTaskCreate.Tasks[0].Ref; got != "first" {
		t.Errorf("Ref = %q, want first", got)
	}
}

func TestRoundTrip_TaskCreate_AllOrNothing(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskCreate = &service.TaskCreateResult{Tasks: []domain.TaskView{fixedTask("BMB-1")}}

	// A request-shape failure anywhere in the batch aborts the whole call
	// before the service is ever invoked. We trigger it with a parent that
	// the schema accepts (it's a free-form string — refs allowed in
	// task_create) but our translation rejects: anything that is neither a
	// well-formed "PROJ-N" task key nor a "@name" symbolic reference.
	res, sc := callTool(t, cs, "task_create", map[string]any{
		"tasks": []map[string]any{
			{"project": "BMB", "title": "ok task"},
			{"project": "BMB", "title": "bad parent", "parent": "not-a-key-or-ref"},
		},
	})
	if res.IsError != true {
		t.Errorf("IsError = %v, want true", res.IsError)
	}
	if len(svc.LastTaskCreate.Tasks) != 0 {
		t.Errorf("service was invoked despite an invalid input: %+v", svc.LastTaskCreate.Tasks)
	}
	if errObj, ok := sc["error"].(map[string]any); ok {
		if errObj["code"] != "validation" {
			t.Errorf("error.code = %v, want validation", errObj["code"])
		}
	} else {
		t.Errorf("no error envelope on validation failure: %v", sc)
	}
}

// TestRoundTrip_TaskCreate_MissingProjectPerItem covers the classic
// mistake: a tasks[] item without "project". This is the SAME failure the
// user hits by putting "project" at the top level of the call — the SDK
// silently ignores an unknown top-level key rather than rejecting it, so the
// items still lack "project". Before the fix, taskCreateTool left "project"
// in the item's schema `required` list, so the SDK failed first with a terse
// "missing properties: [project]". taskCreateTool now drops that requirement
// (dropRequired), letting the empty item reach newTaskToService, which is
// where the friendly "put project INSIDE each item" remediation lives.
func TestRoundTrip_TaskCreate_MissingProjectPerItem(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	res, sc := callTool(t, cs, "task_create", map[string]any{
		"tasks": []map[string]any{{"title": "orphan item"}},
	})
	if res.IsError != true {
		t.Errorf("IsError = %v, want true", res.IsError)
	}
	if len(svc.LastTaskCreate.Tasks) != 0 {
		t.Errorf("service was invoked despite missing project: %+v", svc.LastTaskCreate.Tasks)
	}
	errObj, ok := sc["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error envelope: %v", sc)
	}
	if errObj["code"] != "validation" {
		t.Errorf("code = %v, want validation", errObj["code"])
	}
	if field, _ := errObj["field"].(string); field != "project" {
		t.Errorf("field = %v, want project", field)
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "project") {
		t.Errorf("message %q should mention project", msg)
	}
	rem, _ := errObj["remediation"].(string)
	low := strings.ToLower(rem)
	if !strings.Contains(low, "item") && !strings.Contains(low, "inside") {
		t.Errorf("remediation %q should say item or inside", rem)
	}
}

// TestRoundTrip_TaskCreate_MissingProjectExplicitEmpty covers the
// close cousin of the mistake: the agent sent the key with an
// empty value. TrimSpace must catch that, not normalize "" into the
// service.
func TestRoundTrip_TaskCreate_MissingProjectExplicitEmpty(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	res, sc := callTool(t, cs, "task_create", map[string]any{
		"tasks": []map[string]any{{"project": "   ", "title": "whitespace only"}},
	})
	if res.IsError != true {
		t.Errorf("IsError = %v, want true", res.IsError)
	}
	if len(svc.LastTaskCreate.Tasks) != 0 {
		t.Errorf("service was invoked despite empty project: %+v", svc.LastTaskCreate.Tasks)
	}
	if errObj := sc["error"].(map[string]any); errObj["code"] != "validation" {
		t.Errorf("code = %v, want validation", errObj["code"])
	}
}

// TestRoundTrip_TaskCreate_TopLevelProject is the user's literal complaint:
// "project" placed at the top level of the call instead of inside each item.
// A bare taskCreateInput would let the SDK reject the extra key with a generic
// message that never names where "project" belongs; the declared-but-forbidden
// field turns it into the same friendly, actionable remediation.
func TestRoundTrip_TaskCreate_TopLevelProject(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	res, sc := callTool(t, cs, "task_create", map[string]any{
		"project": "I6",
		"tasks":   []map[string]any{{"title": "orphan top-level project"}},
	})
	if res.IsError != true {
		t.Errorf("IsError = %v, want true", res.IsError)
	}
	if len(svc.LastTaskCreate.Tasks) != 0 {
		t.Errorf("service was invoked despite a top-level project: %+v", svc.LastTaskCreate.Tasks)
	}
	errObj, ok := sc["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error envelope: %v", sc)
	}
	if errObj["code"] != "validation" {
		t.Errorf("code = %v, want validation", errObj["code"])
	}
	rem, _ := errObj["remediation"].(string)
	low := strings.ToLower(rem)
	if !strings.Contains(low, "inside") && !strings.Contains(low, "item") {
		t.Errorf("remediation %q should tell the caller to move project inside each item", rem)
	}
}

// ---------------------------------------------------------------------------
// task_update — happy path plus the bug we fixed: parent is plain key, not @ref
// ---------------------------------------------------------------------------

func TestRoundTrip_TaskUpdate_Success(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskUpdate = &service.TaskUpdateResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: func() *domain.TaskView {
			tv := fixedTask("BMB-1")
			return &tv
		}()}},
	}

	_, sc := callTool(t, cs, "task_update", map[string]any{
		"patches": []map[string]any{{
			"key":        "BMB-1",
			"if_version": 1,
			"title":      "renamed",
		}},
	})
	expectOK(t, sc, "task_update")
	if got := svc.LastTaskUpdate.Patches[0].Title; got == nil || *got != "renamed" {
		t.Errorf("Title = %v, want \"renamed\"", got)
	}
}

func TestRoundTrip_TaskUpdate_ParentIsKeyNotRef(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskUpdate = &service.TaskUpdateResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: func() *domain.TaskView {
			tv := fixedTask("BMB-1")
			return &tv
		}()}},
	}

	// The bug from the handover: a "@ref" parent in task_update is no longer
	// accepted — there is no batch to resolve against, so the value must be a
	// task key. The replacement path runs as a per-item validation failure
	// inside the batch (the envelope itself is ok:true with ok:false items).
	_, sc := callTool(t, cs, "task_update", map[string]any{
		"patches": []map[string]any{{
			"key":        "BMB-1",
			"if_version": 1,
			"parent":     "@ref",
		}},
	})
	if sc["ok"] != true {
		t.Fatalf("envelope ok = %v, want true (per-item failure is reported inside ok:true envelope)", sc["ok"])
	}
	items, _ := sc["data"].(map[string]any)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items len = %d, want 1", len(items))
	}
	item := items[0].(map[string]any)
	if item["ok"] != false {
		t.Errorf("item ok = %v, want false", item["ok"])
	}
	perr, ok := item["error"].(map[string]any)
	if !ok {
		t.Fatalf("per-item error missing: %v", item)
	}
	if perr["code"] != "validation" {
		t.Errorf("per-item error.code = %v, want validation", perr["code"])
	}

	// Service must not have been invoked: the per-item translation failure
	// short-circuits before svc.TaskUpdate is reached.
	if len(svc.LastTaskUpdate.Patches) != 0 {
		t.Errorf("service was invoked with %d patches despite the @ref error", len(svc.LastTaskUpdate.Patches))
	}
}

func TestRoundTrip_TaskUpdate_ParentClearedWithNull(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskUpdate = &service.TaskUpdateResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: func() *domain.TaskView {
			tv := fixedTask("BMB-1")
			return &tv
		}()}},
	}

	// Explicit JSON null must clear the parent (tri-state decoding).
	_, _ = callTool(t, cs, "task_update", map[string]any{
		"patches": []map[string]any{{
			"key":        "BMB-1",
			"if_version": 1,
			"parent":     nil,
		}},
	})
	if got := svc.LastTaskUpdate.Patches[0].Parent.Clear; !got {
		t.Errorf("Parent.Clear = %v, want true", got)
	}
}

func TestRoundTrip_TaskUpdate_EstimateClearWithNull(t *testing.T) {
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
			"estimate":   nil,
		}},
	})
	if got := svc.LastTaskUpdate.Patches[0].Estimate.Clear; !got {
		t.Errorf("Estimate.Clear = %v, want true", got)
	}
}

// TestRoundTrip_TaskUpdate_AcceptanceStrings verifies the wire asymmetry
// fix: a bare JSON string ("a") is equivalent to {"text":"a","done":false}.
// task_create already accepted strings; task_update used to require the
// object form, so agents got silent drops when they tried to mirror the
// shape they had just learned.
func TestRoundTrip_TaskUpdate_AcceptanceStrings(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskUpdate = &service.TaskUpdateResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: func() *domain.TaskView {
			tv := fixedTask("BMB-1")
			return &tv
		}()}},
	}

	_, sc := callTool(t, cs, "task_update", map[string]any{
		"patches": []map[string]any{{
			"key":        "BMB-1",
			"if_version": 1,
			"acceptance": []string{"a", "b"},
		}},
	})
	expectOK(t, sc, "task_update")

	items := svc.LastTaskUpdate.Patches[0].Acceptance
	if len(items) != 2 {
		t.Fatalf("acceptance len = %d, want 2", len(items))
	}
	if items[0].Text != "a" || items[0].Done {
		t.Errorf("acceptance[0] = %+v, want {a, false}", items[0])
	}
	if items[1].Text != "b" || items[1].Done {
		t.Errorf("acceptance[1] = %+v, want {b, false}", items[1])
	}
}

// TestRoundTrip_TaskUpdate_AcceptanceObjects pins the existing object
// shape: the UnmarshalJSON change must not break callers that send
// {"text":"c","done":true} the way task_update has always accepted it.
func TestRoundTrip_TaskUpdate_AcceptanceObjects(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskUpdate = &service.TaskUpdateResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: func() *domain.TaskView {
			tv := fixedTask("BMB-1")
			return &tv
		}()}},
	}

	_, sc := callTool(t, cs, "task_update", map[string]any{
		"patches": []map[string]any{{
			"key":        "BMB-1",
			"if_version": 1,
			"acceptance": []map[string]any{{"text": "c", "done": true}},
		}},
	})
	expectOK(t, sc, "task_update")

	items := svc.LastTaskUpdate.Patches[0].Acceptance
	if len(items) != 1 {
		t.Fatalf("acceptance len = %d, want 1", len(items))
	}
	if items[0].Text != "c" || !items[0].Done {
		t.Errorf("acceptance[0] = %+v, want {c, true}", items[0])
	}
}

// TestRoundTrip_TaskUpdate_AcceptanceMixed guards against an over-eager
// unmarshaller: a mixed list (string + object) must succeed and produce
// both shapes, in order.
func TestRoundTrip_TaskUpdate_AcceptanceMixed(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskUpdate = &service.TaskUpdateResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: func() *domain.TaskView {
			tv := fixedTask("BMB-1")
			return &tv
		}()}},
	}

	_, sc := callTool(t, cs, "task_update", map[string]any{
		"patches": []map[string]any{{
			"key":        "BMB-1",
			"if_version": 1,
			"acceptance": []any{"a", map[string]any{"text": "b", "done": true}},
		}},
	})
	expectOK(t, sc, "task_update")

	items := svc.LastTaskUpdate.Patches[0].Acceptance
	if len(items) != 2 {
		t.Fatalf("acceptance len = %d, want 2", len(items))
	}
	if items[0].Text != "a" || items[0].Done {
		t.Errorf("acceptance[0] = %+v, want {a, false}", items[0])
	}
	if items[1].Text != "b" || !items[1].Done {
		t.Errorf("acceptance[1] = %+v, want {b, true}", items[1])
	}
}

// ---------------------------------------------------------------------------
// task_link
// ---------------------------------------------------------------------------

func TestRoundTrip_TaskLink_Success(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	tv1 := fixedTask("BMB-1")
	tv2 := fixedTask("BMB-2")
	svc.DefaultTaskLink = &service.TaskLinkResult{Tasks: []domain.TaskView{tv1, tv2}}

	_, sc := callTool(t, cs, "task_link", map[string]any{
		"add": []map[string]any{{"blocker": "bmb-1", "blocked": "BMB-2"}},
	})
	expectOK(t, sc, "task_link")
	if got := svc.LastTaskLink.Add[0].Blocker; got != "BMB-1" {
		t.Errorf("normalized blocker = %q, want BMB-1", got)
	}
	if got := svc.LastTaskLink.Add[0].Blocked; got != "BMB-2" {
		t.Errorf("normalized blocked = %q, want BMB-2", got)
	}
}

func TestRoundTrip_TaskLink_EmptyRejected(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	res, _ := callTool(t, cs, "task_link", map[string]any{})
	if res.IsError != true {
		t.Errorf("IsError = %v, want true (empty add+remove must be rejected)", res.IsError)
	}
	if len(svc.LastTaskLink.Add) != 0 || len(svc.LastTaskLink.Remove) != 0 {
		t.Errorf("service was invoked despite empty input: %+v", svc.LastTaskLink)
	}
}

// ---------------------------------------------------------------------------
// task_claim
// ---------------------------------------------------------------------------

func TestRoundTrip_TaskClaim_Success(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	exp := time.Date(2026, 9, 6, 13, 0, 0, 0, time.UTC)
	svc.DefaultTaskClaim = &service.TaskClaimResult{
		Task:           fixedTask("BMB-1"),
		ClaimedBy:      "tester",
		ClaimExpiresAt: &exp,
		RemainSeconds:  3600,
	}

	_, sc := callTool(t, cs, "task_claim", map[string]any{
		"key":    "BMB-1",
		"action": "claim",
	})
	expectOK(t, sc, "task_claim")
	data, _ := sc["data"].(map[string]any)
	if data["claimed_by"] != "tester" {
		t.Errorf("claimed_by = %v", data["claimed_by"])
	}
	if data["lease_remaining_seconds"].(float64) != 3600 {
		t.Errorf("lease_remaining_seconds = %v", data["lease_remaining_seconds"])
	}
}

// ---------------------------------------------------------------------------
// task_next
// ---------------------------------------------------------------------------

func TestRoundTrip_TaskNext_Success(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskNext = &service.NextResult{
		Tasks:      []domain.TaskView{fixedTask("BMB-1")},
		ClaimedKey: "BMB-1",
		StartedKey: "BMB-1",
		WIPFull:    true,
		Reasons: service.NextReasons{
			BlockedDependency: 2, WIPFull: 1, ClaimedByOther: 0,
			ParentIncomplete: 0, NotLeaf: 0,
		},
		BlockedTop: []service.BlockedSample{{Key: "BMB-9", BlockedBy: []string{"BMB-1"}}},
	}

	_, sc := callTool(t, cs, "task_next", map[string]any{
		"project": "BMB", "action": "peek", "limit": 1,
	})
	expectOK(t, sc, "task_next")
	meta := sc["meta"].(map[string]any)
	if meta["wip_full"] != true {
		t.Errorf("wip_full = %v, want true", meta["wip_full"])
	}
	if meta["claimed_key"] != "BMB-1" {
		t.Errorf("claimed_key = %v", meta["claimed_key"])
	}
	if reasons := meta["reasons"].(map[string]any); reasons["blocked_dependency"].(float64) != 2 {
		t.Errorf("reasons.blocked_dependency = %v, want 2", reasons["blocked_dependency"])
	}
	blockedTop := meta["blocked_top"].([]any)
	if len(blockedTop) != 1 || blockedTop[0].(map[string]any)["key"] != "BMB-9" {
		t.Errorf("blocked_top = %v", meta["blocked_top"])
	}
}

// ---------------------------------------------------------------------------
// task_remove
// ---------------------------------------------------------------------------

func TestRoundTrip_TaskRemove_Success(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	tv := fixedTask("BMB-1")
	svc.DefaultTaskRemove = &service.TaskRemoveResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: &tv}},
	}

	_, sc := callTool(t, cs, "task_remove", map[string]any{
		"items": []map[string]any{{"key": "bmb-1", "if_version": 1}},
	})
	expectOK(t, sc, "task_remove")
	if got := svc.LastTaskRemove.Items[0].Key; got != "BMB-1" {
		t.Errorf("normalized key = %q, want BMB-1", got)
	}
	if !svc.LastTaskRemove.CascadeSubtasks {
		t.Errorf("CascadeSubtasks = false, want true when omitted")
	}
}

// TestRoundTrip_TaskRemove_CascadeSubtasksDefaultsToTrue verifies finding #11:
// PLAN §6.8 specifies cascade_subtasks defaults to true so removing a parent
// archives its subtasks rather than silently orphaning them.
func TestRoundTrip_TaskRemove_CascadeSubtasksDefaultsToTrue(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	parent := fixedTask("BMB-1")
	svc.DefaultTaskRemove = &service.TaskRemoveResult{
		Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: &parent}},
	}

	_, sc := callTool(t, cs, "task_remove", map[string]any{
		"items": []map[string]any{{"key": "BMB-1"}},
	})
	expectOK(t, sc, "task_remove")
	if !svc.LastTaskRemove.CascadeSubtasks {
		t.Fatalf("CascadeSubtasks was false; expected default true when omitted")
	}
}

func TestRoundTrip_TaskRemove_CascadeSubtasksExplicit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		val  bool
	}{
		{"explicit_false", false},
		{"explicit_true", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cs, svc := roundtripServer(t, NewServer)
			parent := fixedTask("BMB-1")
			svc.DefaultTaskRemove = &service.TaskRemoveResult{
				Items: []service.ItemResult{{Key: "BMB-1", OK: true, Task: &parent}},
			}

			_, sc := callTool(t, cs, "task_remove", map[string]any{
				"items":            []map[string]any{{"key": "BMB-1"}},
				"cascade_subtasks": tc.val,
			})
			expectOK(t, sc, "task_remove")
			if got := svc.LastTaskRemove.CascadeSubtasks; got != tc.val {
				t.Fatalf("CascadeSubtasks = %v, want %v", got, tc.val)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// project_upsert
// ---------------------------------------------------------------------------

func TestRoundTrip_ProjectUpsert_Success(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultProjectUpsert = &service.ProjectUpsertResult{
		Project: domain.Project{
			Key: "BMB", Name: "BeeMemoryBank", Version: 1,
			EstimateUnit: "h",
		},
		Columns: []domain.Column{
			{Name: "Backlog", Kind: domain.KindBacklog, Position: 0},
		},
	}

	_, sc := callTool(t, cs, "project_upsert", map[string]any{
		"mode": "create", "key": "bmb", "name": "BeeMemoryBank",
	})
	expectOK(t, sc, "project_upsert")
	data := sc["data"].(map[string]any)
	if data["key"] != "BMB" {
		t.Errorf("normalized project key = %v, want BMB", data["key"])
	}
}

// ---------------------------------------------------------------------------
// Service-layer errors round-trip through the envelope (no Go error from handler)
// ---------------------------------------------------------------------------

func TestRoundTrip_ServiceDomainError_EnvelopesWithoutProtocolError(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskGet = nil
	svc.NextTaskGet = func(_ context.Context, _ service.Actor, _ service.TaskGetInput) (*service.TaskGetResult, error) {
		return nil, domain.Blocked("BMB-9", []string{"BMB-1", "BMB-2"})
	}

	res, sc := callTool(t, cs, "task_get", map[string]any{"keys": []string{"BMB-9"}})
	if res.IsError != true {
		t.Errorf("IsError = %v, want true", res.IsError)
	}
	errObj, ok := sc["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error envelope: %v", sc)
	}
	if errObj["code"] != "blocked" {
		t.Errorf("code = %v, want blocked", errObj["code"])
	}
	// StructuredContent MUST be set even on failure — the handover §5 trap #1.
	if res.StructuredContent == nil {
		t.Errorf("StructuredContent nil; envelope contract broken")
	}
}

func TestRoundTrip_ServiceNonDomainError_WrappedAsValidation(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskGet = nil
	svc.NextTaskGet = func(_ context.Context, _ service.Actor, _ service.TaskGetInput) (*service.TaskGetResult, error) {
		return nil, errService
	}

	res, sc := callTool(t, cs, "task_get", map[string]any{"keys": []string{"BMB-1"}})
	if res.IsError != true {
		t.Errorf("IsError = %v, want true", res.IsError)
	}
	errObj := sc["error"].(map[string]any)
	if errObj["code"] != "validation" {
		t.Errorf("code = %v, want validation (non-domain errors wrap to validation)", errObj["code"])
	}
	if !strings.Contains(errObj["message"].(string), "kaboom") {
		t.Errorf("message %q does not preserve the underlying cause", errObj["message"])
	}
}

// ---------------------------------------------------------------------------
// Auth context missing on the server's Connect ctx → every tool refuses.
// (This is the wiring bug the handover calls out: the actor must be on the
// Connect ctx for in-memory transports, not per-call.)
// ---------------------------------------------------------------------------

func TestRoundTrip_NoActorOnConnectCtx_AllToolsForbidden(t *testing.T) {
	t.Parallel()
	svc := &fakeService{}
	srv := NewServer(svc, "test")
	ct, st := gomcp.NewInMemoryTransports()
	// Note: NO auth.WithAuth here.
	ss, err := srv.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	client := gomcp.NewClient(&gomcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	cs, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	// Every read tool must surface a forbidden error envelope, not a
	// JSON-RPC protocol fault.
	for _, name := range []string{"board_get", "task_get", "task_next"} {
		t.Run(name, func(t *testing.T) {
			args := map[string]any{}
			switch name {
			case "board_get":
				// nothing
			case "task_get":
				args["keys"] = []string{"BMB-1"}
			case "task_next":
				// nothing
			}
			res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: name, Arguments: args})
			if err != nil {
				t.Fatalf("CallTool returned a Go error: %v (must be envelope)", err)
			}
			if res == nil {
				t.Fatalf("nil result")
			}
			if res.StructuredContent == nil {
				t.Fatalf("StructuredContent nil; envelope contract broken")
			}
			raw, _ := json.Marshal(res.StructuredContent)
			var sc map[string]any
			_ = json.Unmarshal(raw, &sc)
			if sc["ok"] != false {
				t.Fatalf("ok != false: %v", sc)
			}
			errObj, ok := sc["error"].(map[string]any)
			if !ok {
				t.Fatalf("no error envelope: %v", sc)
			}
			if errObj["code"] != "forbidden" {
				t.Errorf("code = %v, want forbidden", errObj["code"])
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ListTools on a freshly constructed server returns exactly nine tools in
// the full server and three in the read-only server.
// ---------------------------------------------------------------------------

func TestListTools_FullServer_HasNine(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 9 {
		t.Errorf("tools count = %d, want 9 (got: %v)", len(res.Tools), names(res.Tools))
	}
	want := map[string]bool{
		"board_get": false, "task_next": false, "task_get": false,
		"task_create": false, "task_update": false, "task_link": false,
		"task_claim": false, "task_remove": false, "project_upsert": false,
	}
	for _, t := range res.Tools {
		want[t.Name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("missing tool %q in full server list", name)
		}
	}
}

func names(tools []*gomcp.Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}

// keep auth used so this file links even when only some tests run
var _ = auth.Manager{}
