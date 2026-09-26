package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// ---------------------------------------------------------------------------
// Response-size guards (KANB-58).
//
// The IAMT retrospective counted ~840 000 characters of board responses in one
// week-long session. Three shapes paid for most of it, and each is pinned here
// by a byte budget rather than by "the field is absent": a budget also catches
// the next field someone adds to a response nobody reads.
//
//  1. every board read carried the project description — the project's
//     rulebook, thousands of characters — even when it matched no task;
//  2. every write echoed each task whole: closing 17 cards came back as
//     32 000 characters;
//  3. both of the above arrived twice over, as structuredContent next to the
//     text, and the client showed the model the structured copy (the budgets
//     are measured on the text because that is now the ONLY representation;
//     callTool/callToolRaw fail on any structuredContent).
// ---------------------------------------------------------------------------

// rulebook is a project description of realistic size: the KANB rulebook was
// 7-9 thousand characters when this test was written.
var rulebook = strings.Repeat("Project rule: work is accepted only after the orchestrator ran it. ", 120)

func emptyBoardWithRulebook() *service.Board {
	return &service.Board{Projects: []service.BoardProject{{
		Key:         "BMB",
		Name:        "BeeMemoryBank",
		Description: rulebook,
		Version:     3,
		Columns: []service.BoardColumn{
			{Name: "Backlog", Kind: domain.KindBacklog},
			{Name: "Doing", Kind: domain.KindActive},
			{Name: "Done", Kind: domain.KindDone},
		},
	}}}
}

// A board read that matches nothing must cost next to nothing, in both
// formats, and the rulebook must still be one include away.
func TestResponseSize_EmptyBoardReadCarriesNoRulebook(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultBoardGet = emptyBoardWithRulebook()

	const budget = 600
	for _, format := range []string{"compact", "json"} {
		res := callToolRaw(t, cs, "board_get", map[string]any{"project": "BMB", "format": format})
		text := resultText(res)
		if len(text) > budget {
			t.Errorf("format=%s: an empty board read is %d characters, budget %d — is the project description riding along again?\n%.300s", format, len(text), budget, text)
		}
		if strings.Contains(text, "Project rule:") {
			t.Errorf("format=%s: the project description came back without include:[\"description\"]", format)
		}

		res = callToolRaw(t, cs, "board_get", map[string]any{"project": "BMB", "format": format, "include": []any{"description"}})
		if !strings.Contains(resultText(res), "Project rule:") {
			t.Errorf("format=%s: include:[\"description\"] did not return the description — the rulebook must stay reachable", format)
		}
	}

	// "description" widens the project, not the tasks: it must not reach the
	// service as a per-task include it does not know.
	for _, inc := range svc.LastBoardGet.Include {
		if string(inc) == includeDescription {
			t.Errorf("the service received include %q; it is a board_get-only project include", inc)
		}
	}
}

// heavyTask is a card the way a finished one really looks: a long brief, a
// checklist and a written conclusion.
func heavyTask(i int) domain.TaskView {
	tv := fixedTask(fmt.Sprintf("BMB-%d", 100+i))
	tv.Task.Body = strings.Repeat("Brief: what to build, why, and what must not change. ", 30)
	tv.Task.Conclusion = strings.Repeat("Verified by running the full suite and a canary. ", 12)
	for j := 0; j < 8; j++ {
		tv.Task.Acceptance = append(tv.Task.Acceptance, domain.AcceptanceItem{Text: fmt.Sprintf("criterion %d holds under a live run", j), Done: true})
	}
	tv.Task.Version = 5
	tv.ColumnName = "Done"
	tv.ColumnKind = domain.KindDone
	return tv
}

// Closing seventeen cards — the IAMT batch that came back as 32 000
// characters — must answer with an acknowledgement per card, and the whole
// task only when asked for.
func TestResponseSize_ClosingSeventeenCardsAnswersShort(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	const n = 17
	items := make([]service.ItemResult, n)
	patches := make([]any, n)
	for i := 0; i < n; i++ {
		tv := heavyTask(i)
		items[i] = service.ItemResult{Key: tv.Task.Key, OK: true, Task: &tv}
		patches[i] = map[string]any{"key": tv.Task.Key, "column": "Done", "if_version": 4}
	}
	svc.DefaultTaskUpdate = &service.TaskUpdateResult{Items: items}

	res := callToolRaw(t, cs, "task_update", map[string]any{"patches": patches})
	ack := resultText(res)
	const budget = 1500
	if len(ack) > budget {
		t.Errorf("closing %d cards answered with %d characters, budget %d — is the whole task being echoed again?\n%.400s", n, len(ack), budget, ack)
	}
	for _, leak := range []string{"Brief:", "Verified by running", "criterion 0"} {
		if strings.Contains(ack, leak) {
			t.Errorf("the default acknowledgement leaks %q from the task body", leak)
		}
	}
	// The acknowledgement must still carry what the next write needs.
	if !strings.Contains(ack, `"version":5`) || !strings.Contains(ack, `"column":"Done"`) {
		t.Errorf("the acknowledgement lost the AFTER-version or the column:\n%.400s", ack)
	}

	full := resultText(callToolRaw(t, cs, "task_update", map[string]any{"patches": patches, "echo": "full"}))
	if !strings.Contains(full, "Brief:") {
		t.Errorf(`echo:"full" did not return the whole task`)
	}
	if len(full) < 10*len(ack) {
		t.Logf("full echo %d vs ack %d characters", len(full), len(ack))
	}
}

// Creating a batch is the other write that echoed every card whole.
func TestResponseSize_CreatingABatchAnswersShort(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	const n = 6
	created := make([]domain.TaskView, n)
	tasks := make([]any, n)
	for i := 0; i < n; i++ {
		created[i] = heavyTask(i)
		tasks[i] = map[string]any{"project": "BMB", "title": created[i].Task.Title}
	}
	svc.DefaultTaskCreate = &service.TaskCreateResult{Tasks: created}

	ack := resultText(callToolRaw(t, cs, "task_create", map[string]any{"tasks": tasks}))
	const budget = 700
	if len(ack) > budget {
		t.Errorf("creating %d cards answered with %d characters, budget %d\n%.400s", n, len(ack), budget, ack)
	}
	if strings.Contains(ack, "Brief:") {
		t.Errorf("the default acknowledgement of task_create leaks the task body")
	}
	if !strings.Contains(ack, `"key":"BMB-100"`) {
		t.Errorf("the acknowledgement lost the assigned keys:\n%.400s", ack)
	}
	full := resultText(callToolRaw(t, cs, "task_create", map[string]any{"tasks": tasks, "echo": "full"}))
	if !strings.Contains(full, "Brief:") {
		t.Errorf(`echo:"full" did not return the whole task on task_create`)
	}
}

// project_upsert used to echo the whole project — description included, the
// rulebook, thousands of characters — to confirm a settings change. The
// default answer now carries the settings and columns and only the
// description's length; echo:"full" brings it back.
func TestResponseSize_ProjectUpsertAnswersWithoutTheRulebook(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultProjectUpsert = &service.ProjectUpsertResult{
		Project: domain.Project{Key: "BMB", Name: "BeeMemoryBank", Version: 4, Description: rulebook, EstimateUnit: "h", ClaimTTLSeconds: 3600},
		Columns: []domain.Column{{Name: "Backlog", Kind: domain.KindBacklog}, {Name: "Doing", Kind: domain.KindActive}, {Name: "Done", Kind: domain.KindDone}},
	}
	args := map[string]any{"mode": "update", "key": "BMB", "if_version": 3, "settings": map[string]any{"claim_ttl_seconds": 7200}}

	ack := resultText(callToolRaw(t, cs, "project_upsert", args))
	const budget = 600
	if len(ack) > budget {
		t.Errorf("project_upsert answered with %d characters, budget %d\n%.300s", len(ack), budget, ack)
	}
	if strings.Contains(ack, "Project rule:") {
		t.Errorf("the default answer still echoes the description")
	}
	for _, want := range []string{`"version":4`, `"description_length":`, `"columns":`} {
		if !strings.Contains(ack, want) {
			t.Errorf("the short answer lost %s:\n%.300s", want, ack)
		}
	}

	args["echo"] = "full"
	if full := resultText(callToolRaw(t, cs, "project_upsert", args)); !strings.Contains(full, "Project rule:") {
		t.Errorf(`echo:"full" did not return the description`)
	}
}
