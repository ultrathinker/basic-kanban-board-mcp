package mcp

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// ---------------------------------------------------------------------------
// KANB-67: settings.idle_after_seconds on the wire. The bounds and the
// threshold itself are the service's (internal/service idle_after_test.go);
// these tests pin that the value reaches it, is echoed back, and is readable
// on board_get.
// ---------------------------------------------------------------------------

func TestProjectUpsert_IdleAfterForwardedAndEchoed(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultProjectUpsert = &service.ProjectUpsertResult{
		Project: domain.Project{Key: "BMB", Name: "BeeMemoryBank", Version: 2, IdleAfterSeconds: 172800},
	}

	_, sc := callTool(t, cs, "project_upsert", map[string]any{
		"mode": "update", "key": "bmb", "if_version": 1,
		"settings": map[string]any{"idle_after_seconds": 172800},
	})
	expectOK(t, sc, "project_upsert")
	if s := svc.LastProjectUpsert.Settings; s == nil || s.IdleAfterSeconds == nil || *s.IdleAfterSeconds != 172800 {
		t.Fatalf("service settings = %+v, want idle_after_seconds 172800", svc.LastProjectUpsert.Settings)
	}
	data, _ := sc["data"].(map[string]any)
	if got, _ := data["idle_after_seconds"].(float64); got != 172800 {
		t.Errorf("echo idle_after_seconds = %v, want 172800", data["idle_after_seconds"])
	}

	// Absent stays absent: nil, never a 0 that would clear the setting.
	_, sc = callTool(t, cs, "project_upsert", map[string]any{
		"mode": "update", "key": "bmb", "if_version": 2,
		"settings": map[string]any{"claim_ttl_seconds": 600},
	})
	expectOK(t, sc, "project_upsert")
	if s := svc.LastProjectUpsert.Settings; s == nil || s.IdleAfterSeconds != nil {
		t.Errorf("idle_after_seconds left out, yet the service got %+v", s)
	}
}

// The ceiling is published on the schema, so a value past it never reaches
// the service; the description states the floor that a schema minimum of 0
// (the "clear" value) cannot.
func TestProjectUpsert_IdleAfterBoundsPublished(t *testing.T) {
	t.Parallel()
	s, ok := projectUpsertTool().InputSchema.(*jsonschema.Schema)
	if !ok {
		t.Fatalf("project_upsert InputSchema is %T", projectUpsertTool().InputSchema)
	}
	idle := prop(prop(s, "settings"), "idle_after_seconds")
	if idle.Minimum == nil || *idle.Minimum != 0 || idle.Maximum == nil || *idle.Maximum != domain.IdleAfterMax.Seconds() {
		t.Errorf("idle_after_seconds bounds = %v..%v, want 0..%v", idle.Minimum, idle.Maximum, domain.IdleAfterMax.Seconds())
	}
	if !strings.Contains(idle.Description, strconv.Itoa(int(domain.IdleAfterMin.Seconds()))) {
		t.Errorf("description %q does not state the floor", idle.Description)
	}

	cs, svc := roundtripServer(t, NewServer)
	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "project_upsert",
		Arguments: map[string]any{
			"mode": "update", "key": "BMB", "if_version": 1,
			"settings": map[string]any{"idle_after_seconds": int(domain.IdleAfterMax.Seconds()) + 1},
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("idle_after_seconds past the published maximum was accepted")
	}
	if svc.LastProjectUpsert.Settings != nil {
		t.Errorf("service was invoked despite the schema rejection: %+v", svc.LastProjectUpsert.Settings)
	}
}

// board_get's JSON carries the setting on the project, and leaves it out
// when the project never set one.
func TestBoardGet_JSONCarriesIdleAfter(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultBoardGet = &service.Board{Projects: []service.BoardProject{
		{Key: "SLOW", Name: "Slow", Version: 1, EstimateUnit: "h", ClaimTTLSeconds: 3600, IdleAfterSeconds: 172800},
		{Key: "PLAIN", Name: "Plain", Version: 1, EstimateUnit: "h", ClaimTTLSeconds: 3600},
	}}
	_, sc := callTool(t, cs, "board_get", map[string]any{"format": "json"})
	expectOK(t, sc, "board_get")
	data, _ := sc["data"].(map[string]any)
	projects, _ := data["projects"].([]any)
	if len(projects) != 2 {
		t.Fatalf("projects = %v", data["projects"])
	}
	slow, _ := projects[0].(map[string]any)
	if got, _ := slow["idle_after_seconds"].(float64); got != 172800 {
		t.Errorf("SLOW idle_after_seconds = %v, want 172800", slow["idle_after_seconds"])
	}
	plain, _ := projects[1].(map[string]any)
	if v, present := plain["idle_after_seconds"]; present {
		t.Errorf("PLAIN never set idle_after_seconds, yet board_get says %v", v)
	}
}
