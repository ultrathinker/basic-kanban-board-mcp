package mcp

import (
	"context"
	"sort"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// TestBoardGuide_AvailableOnBothServers is criterion #1 of KANB-42:
// board_guide is a read-only tool and must therefore exist on the
// read-only server too, otherwise an agent that only has a read token
// cannot ask the most basic "how does this board work?" question. It
// also must exist on the full server — read-write agents should reach
// for it before they start writing.
func TestBoardGuide_AvailableOnBothServers(t *testing.T) {
	t.Parallel()
	for name, newSrv := range map[string]func(service.Service, string) *gomcp.Server{
		"full":      NewServer,
		"read-only": NewReadOnlyServer,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cs, _ := roundtripServer(t, newSrv)
			res, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatalf("ListTools: %v", err)
			}
			found := false
			for _, tl := range res.Tools {
				if tl.Name == "board_guide" {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s server must list board_guide; got %v", name, names(res.Tools))
			}
			// And the tool must actually work — a tool that is registered
			// but always errors is the failure mode this would not catch.
			toolRes, sc := callTool(t, cs, "board_guide", map[string]any{})
			if toolRes.IsError {
				t.Fatalf("board_guide failed: %v", sc)
			}
			expectOK(t, sc, "board_guide")
		})
	}
}

// TestBoardGuide_NoMutation is the negative half of "doesn't change
// anything": no BoardGet/ChatAdd/ProgressSet call is permitted to leave
// the test's fake service with a non-zero Last* field. The fake records
// the last input to every method; if board_guide had side effects, one
// of them would be non-zero after the call.
func TestBoardGuide_NoMutation(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	if _, sc := callTool(t, cs, "board_guide", map[string]any{}); sc["ok"] != true {
		t.Fatalf("board_guide failed: %v", sc)
	}
	for _, c := range []struct {
		name string
		seen bool
	}{
		{"BoardGet", svc.LastBoardGet.ProjectKey != ""},
		{"TaskNext", svc.LastTaskNext.ProjectKey != ""},
		{"TaskGet", len(svc.LastTaskGet.Keys) > 0},
		{"TaskCreate", len(svc.LastTaskCreate.Tasks) > 0},
		{"TaskUpdate", len(svc.LastTaskUpdate.Patches) > 0},
		{"TaskLink", len(svc.LastTaskLink.Add) > 0 || len(svc.LastTaskLink.Remove) > 0},
		{"TaskClaim", svc.LastTaskClaim.Key != ""},
		{"TaskRemove", len(svc.LastTaskRemove.Items) > 0},
		{"ProjectUpsert", svc.LastProjectUpsert.Key != ""},
		{"ChatAdd", svc.LastChatAdd.ProjectKey != ""},
		{"ChatList", svc.LastChatList.ProjectKey != ""},
		{"TaskProgress", svc.LastTaskProgress.ProjectKey != ""},
		{"ProjectProgress", svc.LastProjectProgress.ProjectKey != ""},
		{"ProgressTrackDelete", svc.LastProgressTrackDelete.ProjectKey != ""},
		{"ProgressSet", svc.LastProgressSet.Assessor != ""},
		{"ProgressHistory", svc.LastProgressHistory.ProjectKey != ""},
	} {
		if c.seen {
			t.Errorf("board_guide touched %s (read-only must not call any mutating method)", c.name)
		}
	}
}

// TestBoardGuide_ToolCountAndListDeriveFromRegistry is the canary the
// card explicitly names: the tool count and the per-tool list must come
// from the registry, not a hand-kept constant. The full server exposes
// 14 tools; the read-only exposes 5. If a future change adds a tool but
// forgets to update NewServer's specsFromTools slice, the divergence is
// visible here: published count != data.tool_count.
func TestBoardGuide_ToolCountAndListDeriveFromRegistry(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		newSrv      func(service.Service, string) *gomcp.Server
		wantCount   int
		wantMembers []string
	}{
		{"full", NewServer, 14, []string{
			"board_get", "board_guide", "executor_key_issue", "progress_history", "progress_set",
			"project_post", "project_upsert", "task_claim", "task_create",
			"task_get", "task_link", "task_next", "task_remove", "task_update",
		}},
		{"read-only", NewReadOnlyServer, 5, []string{
			"board_get", "board_guide", "progress_history", "task_get", "task_next",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cs, _ := roundtripServer(t, tc.newSrv)
			listed, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatalf("ListTools: %v", err)
			}
			if len(listed.Tools) != tc.wantCount {
				t.Fatalf("server advertised %d tools, want %d", len(listed.Tools), tc.wantCount)
			}

			res, sc := callTool(t, cs, "board_guide", map[string]any{})
			if res.IsError {
				t.Fatalf("board_guide failed: %v", sc)
			}
			data := sc["data"].(map[string]any)
			if got := int(data["tool_count"].(float64)); got != tc.wantCount {
				t.Errorf("board_guide.data.tool_count = %d, want %d", got, tc.wantCount)
			}
			tools := data["tools"].([]any)
			if len(tools) != tc.wantCount {
				t.Fatalf("board_guide.data.tools len = %d, want %d", len(tools), tc.wantCount)
			}
			gotNames := make([]string, 0, len(tools))
			for _, t := range tools {
				gotNames = append(gotNames, t.(map[string]any)["name"].(string))
			}
			sort.Strings(gotNames)
			wantSorted := append([]string(nil), tc.wantMembers...)
			sort.Strings(wantSorted)
			if !equalStringSlices(gotNames, wantSorted) {
				t.Errorf("board_guide.data.tools names = %v, want %v", gotNames, wantSorted)
			}
		})
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestBoardGuide_ToolParametersAreExtracted asserts that each tool in
// the response has a non-empty Parameters list when its schema declares
// properties. board_get and board_guide are the two examples that
// carry meaningful parameters (project key, optional inclusion flags)
// and the test pins that the wiring reaches the schema, not just the
// tool name.
func TestBoardGuide_ToolParametersAreExtracted(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)
	_, sc := callTool(t, cs, "board_guide", map[string]any{})

	tools := sc["data"].(map[string]any)["tools"].([]any)
	for _, raw := range tools {
		m := raw.(map[string]any)
		name := m["name"].(string)
		switch name {
		case "board_get", "progress_set", "task_update":
			// Tools that DO have parameters
			params, ok := m["parameters"].([]any)
			if !ok || len(params) == 0 {
				t.Errorf("%s: parameters missing or empty; %v", name, m)
			}
		case "board_guide":
			// board_guide itself accepts an optional project; its
			// parameter list should mention project.
			params, _ := m["parameters"].([]any)
			found := false
			for _, p := range params {
				if p.(map[string]any)["name"] == "project" {
					found = true
				}
			}
			if !found {
				t.Errorf("board_guide parameters missing 'project': %v", m)
			}
		}
	}
}

// TestBoardGuide_ProjectSettingsWhenGiven: with `project`, the response
// carries the project's settings (estimate unit, claim TTL, strict_done,
// enforce_dependencies, archived). Without project, no project block.
// The /mcp/readonly path can also ask for project — it's a read.
func TestBoardGuide_ProjectSettingsWhenGiven(t *testing.T) {
	t.Parallel()
	for name, newSrv := range map[string]func(service.Service, string) *gomcp.Server{
		"full":      NewServer,
		"read-only": NewReadOnlyServer,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cs, svc := roundtripServer(t, newSrv)
			svc.DefaultBoardGet = configuredBoard()

			// No project: no project field on data.
			_, sc := callTool(t, cs, "board_guide", map[string]any{})
			data := sc["data"].(map[string]any)
			if _, present := data["project"]; present {
				t.Errorf("data.project should be omitted when no project given: %v", data["project"])
			}

			// With project: settings appear.
			_, sc2 := callTool(t, cs, "board_guide", map[string]any{"project": "BMB"})
			data2 := sc2["data"].(map[string]any)
			proj, ok := data2["project"].(map[string]any)
			if !ok {
				t.Fatalf("data.project missing: %v", data2)
			}
			if proj["key"] != "BMB" {
				t.Errorf("data.project.key = %v, want BMB", proj["key"])
			}
			if proj["estimate_unit"] != "d" {
				t.Errorf("data.project.estimate_unit = %v, want d", proj["estimate_unit"])
			}
			if proj["claim_ttl_seconds"] != float64(900) {
				t.Errorf("data.project.claim_ttl_seconds = %v, want 900", proj["claim_ttl_seconds"])
			}
		})
	}
}

// TestBoardGuide_UnknownProject refuses with a not_found error rather
// than silently returning an empty project block.
func TestBoardGuide_UnknownProject(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)

	res, sc := callTool(t, cs, "board_guide", map[string]any{"project": "ZZZ"})
	if !res.IsError {
		t.Fatalf("expected an error for unknown project; got %v", sc)
	}
	errObj := sc["error"].(map[string]any)
	if errObj["code"] != string(domain.CodeNotFound) {
		t.Errorf("code = %v, want %v", errObj["code"], domain.CodeNotFound)
	}
}

// TestBoardGuide_GuideTextUsesDomainConstants is the regression test for
// "explanations come from the same source". The guide text must include
// the lease TTLs and compact version pulled from internal/domain, not
// from a hand-copied literal — when those constants change, this test
// catches it.
func TestBoardGuide_GuideTextUsesDomainConstants(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)
	_, sc := callTool(t, cs, "board_guide", map[string]any{})
	guide := sc["data"].(map[string]any)["guide"].(string)

	for _, want := range []string{
		"compact_version=",
		"Identity:",
		"task_claim",
		"version",
		"note",
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("guide text missing %q: %s", want, guide)
		}
	}
}
