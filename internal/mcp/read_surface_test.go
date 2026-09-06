package mcp

import (
	"strings"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// ---------------------------------------------------------------------------
// Architecture review finding #10, at the MCP boundary.
// ---------------------------------------------------------------------------

// configuredBoard is a one-project board with every setting set to something
// other than its default, so a field that fails to travel is visible rather
// than accidentally correct.
func configuredBoard() *service.Board {
	return &service.Board{Projects: []service.BoardProject{{
		Key:                 "BMB",
		Name:                "BeeMemoryBank",
		Description:         "the board under test",
		Version:             7,
		FocusKey:            "BMB-1",
		EstimateUnit:        "d",
		EnforceDependencies: true,
		StrictDone:          true,
		ClaimTTLSeconds:     900,
		Columns: []service.BoardColumn{
			{Name: "Backlog", Kind: domain.KindBacklog, Count: 1,
				Tasks: []domain.TaskView{withEstimateOn(fixedTask("BMB-1"), 3)}},
			{Name: "Doing", Kind: domain.KindActive, WIPLimit: intPtr(3), Count: 0},
		},
		DoneTotal: 12,
	}}}
}

func intPtr(v int) *int { return &v }

func withEstimateOn(tv domain.TaskView, n float64) domain.TaskView {
	tv.Estimate = &n
	return tv
}

// TestBoardGet_PublishesProjectVersionAndSettings is the loop-closing test at
// the wire: project_upsert(mode:"update") requires if_version and its own
// description sends callers to board_get for it, so board_get has to actually
// carry it — along with the settings a caller must echo back rather than guess.
func TestBoardGet_PublishesProjectVersionAndSettings(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultBoardGet = configuredBoard()

	_, sc := callTool(t, cs, "board_get", map[string]any{"project": "BMB"})
	expectOK(t, sc, "board_get")

	p := sc["data"].(map[string]any)["projects"].([]any)[0].(map[string]any)

	for field, want := range map[string]any{
		"version":              float64(7),
		"estimate_unit":        "d",
		"enforce_dependencies": true,
		"strict_done":          true,
		"claim_ttl_seconds":    float64(900),
		"description":          "the board under test",
	} {
		if got := p[field]; got != want {
			t.Errorf("projects[0].%s = %v, want %v", field, got, want)
		}
	}

	// Full column configuration, not just the ones holding tasks.
	cols := p["columns"].([]any)
	if len(cols) != 2 {
		t.Fatalf("columns = %d, want 2", len(cols))
	}
	doing := cols[1].(map[string]any)
	if doing["kind"] != string(domain.KindActive) || doing["wip_limit"] != float64(3) {
		t.Errorf("second column = %v, want the active column with its WIP limit", doing)
	}
}

// TestBoardGet_CompactCarriesProjectVersion pins the version onto the DEFAULT
// format. A version published only in structuredContent would still leave the
// compact reader — the format the tool tells agents to prefer — unable to
// follow its read with a write.
func TestBoardGet_CompactCarriesProjectVersion(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultBoardGet = configuredBoard()

	res, _ := callTool(t, cs, "board_get", map[string]any{"project": "BMB"})
	text := res.Content[0].(*gomcp.TextContent).Text
	header := strings.Split(text, "\n")[1]
	if !strings.HasSuffix(header, " · v7") {
		t.Errorf("compact project header %q carries no version", header)
	}
}

// TestBoardGet_CompactUsesConfiguredEstimateUnit is the regression test for
// the second half of finding #10: the unit was configurable per project and
// the renderer was always handed nil, so a project in days printed hours.
func TestBoardGet_CompactUsesConfiguredEstimateUnit(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultBoardGet = configuredBoard()

	res, _ := callTool(t, cs, "board_get", map[string]any{"project": "BMB"})
	text := res.Content[0].(*gomcp.TextContent).Text
	if !strings.Contains(text, "est 3d") {
		t.Errorf("estimate did not use the project's configured unit:\n%s", text)
	}
	if strings.Contains(text, "est 3h") {
		t.Errorf("estimate still rendered as hours on a project configured in days:\n%s", text)
	}
}

// TestTaskGet_NotesCursorRoundTrips checks both directions of the cursor: the
// caller's `notes_before` reaches the service, and the service's per-task
// cursor comes back as `notes_next_before` on the task it belongs to.
func TestTaskGet_NotesCursorRoundTrips(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	cursor := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	tv := fixedTask("BMB-1")
	tv.Notes = []domain.Note{{Author: "tester", Body: "older note", CreatedAt: cursor}}
	svc.DefaultTaskGet = &service.TaskGetResult{
		Tasks:     []domain.TaskView{tv},
		NotesNext: map[string]time.Time{"BMB-1": cursor},
	}

	sent := "2026-09-06T12:00:00Z"
	_, sc := callTool(t, cs, "task_get", map[string]any{
		"keys":         []any{"BMB-1"},
		"include":      []any{"notes"},
		"notes_before": sent,
	})
	expectOK(t, sc, "task_get")

	if svc.LastTaskGet.NotesBefore == nil {
		t.Fatalf("notes_before was not forwarded to the service")
	}
	if got := svc.LastTaskGet.NotesBefore.UTC().Format(time.RFC3339); got != sent {
		t.Errorf("forwarded notes_before = %q, want %q", got, sent)
	}

	task := sc["data"].(map[string]any)["items"].([]any)[0].(map[string]any)["task"].(map[string]any)
	if got := task["notes_next_before"]; got != cursor.Format(time.RFC3339) {
		t.Errorf("notes_next_before = %v, want %v", got, cursor.Format(time.RFC3339))
	}
	if len(task["notes"].([]any)) != 1 {
		t.Errorf("notes missing from the page that carried the cursor")
	}
}

// TestTaskGet_NoCursorWhenHistoryEnds: the cursor must be absent when there is
// nothing older, so its presence is a reliable "there is more" signal.
func TestTaskGet_NoCursorWhenHistoryEnds(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultTaskGet = &service.TaskGetResult{Tasks: []domain.TaskView{fixedTask("BMB-1")}}

	_, sc := callTool(t, cs, "task_get", map[string]any{
		"keys":    []any{"BMB-1"},
		"include": []any{"notes"},
	})
	expectOK(t, sc, "task_get")

	task := sc["data"].(map[string]any)["items"].([]any)[0].(map[string]any)["task"].(map[string]any)
	if _, present := task["notes_next_before"]; present {
		t.Errorf("notes_next_before present although the service reported no older notes")
	}
}

// TestTaskGet_MalformedNotesBefore_Refuses names the offending field instead of
// failing somewhere deeper with a cursor the caller cannot correct.
func TestTaskGet_MalformedNotesBefore_Refuses(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)

	res, sc := callTool(t, cs, "task_get", map[string]any{
		"keys":         []any{"BMB-1"},
		"include":      []any{"notes"},
		"notes_before": "yesterday",
	})
	if !res.IsError {
		t.Fatalf("malformed cursor accepted")
	}
	e := sc["error"].(map[string]any)
	if e["code"] != string(domain.CodeValidation) {
		t.Errorf("code = %v, want %v", e["code"], domain.CodeValidation)
	}
	if e["field"] != "notes_before" {
		t.Errorf("field = %v, want notes_before", e["field"])
	}
	if e["remediation"] == "" {
		t.Errorf("refusal carries no remediation: %v", e)
	}
}
