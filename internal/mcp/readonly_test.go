package mcp

import (
	"context"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// TestReadOnly_ExactlyThreeToolsListed pins the read-only server's exposed
// surface (PLAN §6 rule 4): only board_get, task_get, task_next. The other
// six mutating tools must not even be advertised, so a misconfigured client
// cannot reach them without flipping the endpoint.
func TestReadOnly_ExactlyThreeToolsListed(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewReadOnlyServer)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 3 {
		t.Fatalf("tools count = %d, want 3 (got %v)", len(res.Tools), names(res.Tools))
	}
	want := map[string]bool{
		"board_get": false, "task_get": false, "task_next": false,
	}
	for _, tl := range res.Tools {
		want[tl.Name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("missing tool %q on read-only server", name)
		}
	}
	for _, tl := range res.Tools {
		if !want[tl.Name] {
			t.Errorf("read-only server lists a non-allowed tool %q", tl.Name)
		}
	}
}

// TestReadOnly_TaskNext_ClaimRejected and TestReadOnly_TaskNext_StartRejected
// cover the loud-failure rule for /mcp/readonly: claim and start return a
// forbidden envelope — never silently downgraded to peek. The latter would
// leave an agent believing it took work it did not (PLAN §6 rule 4).
func TestReadOnly_TaskNext_ClaimRejected(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewReadOnlyServer)
	res, sc := callTool(t, cs, "task_next", map[string]any{"action": "claim"})
	if res.IsError != true {
		t.Errorf("IsError = %v, want true", res.IsError)
	}
	if sc["ok"] != false {
		t.Errorf("ok = %v, want false", sc["ok"])
	}
	errObj, ok := sc["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error envelope: %v", sc)
	}
	if errObj["code"] != "forbidden" {
		t.Errorf("code = %v, want forbidden", errObj["code"])
	}
}

func TestReadOnly_TaskNext_StartRejected(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewReadOnlyServer)
	res, sc := callTool(t, cs, "task_next", map[string]any{"action": "start"})
	if res.IsError != true {
		t.Errorf("IsError = %v, want true", res.IsError)
	}
	if sc["ok"] != false {
		t.Errorf("ok = %v, want false", sc["ok"])
	}
	errObj, ok := sc["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error envelope: %v", sc)
	}
	if errObj["code"] != "forbidden" {
		t.Errorf("code = %v, want forbidden", errObj["code"])
	}
}

// TestReadOnly_TaskNext_PeekAllowed is the negative case: peek (the read
// action) still works on the read-only server. Without it the test above
// would also pass for a buggy server that disabled every action.
func TestReadOnly_TaskNext_PeekAllowed(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewReadOnlyServer)
	svc.DefaultTaskNext = &service.NextResult{}

	_, sc := callTool(t, cs, "task_next", map[string]any{"action": "peek"})
	if sc["ok"] != true {
		t.Errorf("peek on read-only server: ok = %v, want true; sc = %v", sc["ok"], sc)
	}
}
