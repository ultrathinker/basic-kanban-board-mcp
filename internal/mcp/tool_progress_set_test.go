package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// TestProgressSet_TaskAndProjectTargets verifies that calling with task targets
// a task, while calling with project targets the overall project.
func TestProgressSet_TaskAndProjectTargets(t *testing.T) {
	t.Parallel()

	t.Run("task_target", func(t *testing.T) {
		t.Parallel()
		cs, svc := roundtripServer(t, NewServer)

		now := time.Now().UTC().Truncate(time.Second)
		eta := now.Add(48 * time.Hour)
		taskKey := "KANB-3"
		summary := 40

		svc.DefaultProgressSet = &service.ProgressSetResult{
			Mark: domain.ProgressMark{
				ID:        "pm-101",
				ProjectID: "proj-1",
				TaskID:    &taskKey,
				Assessor:  "Codex",
				Percent:   40,
				ETA:       &eta,
				CreatedAt: now,
			},
			TaskKey:        taskKey,
			ProjectKey:     "KANB",
			SummaryPercent: &summary,
			Assessors:      1,
		}

		res, sc := callTool(t, cs, "progress_set", map[string]any{
			"task":     "KANB-3",
			"assessor": "Codex",
			"percent":  40,
			"eta":      eta.Format(time.RFC3339),
		})
		if res.IsError {
			t.Fatalf("callTool failed: %v", sc)
		}
		expectOK(t, sc, "progress_set")

		if svc.LastProgressSet.TaskKey != "KANB-3" {
			t.Errorf("service TaskKey = %q, want KANB-3", svc.LastProgressSet.TaskKey)
		}
		if svc.LastProgressSet.ProjectKey != "" {
			t.Errorf("service ProjectKey = %q, want empty for task assessment", svc.LastProgressSet.ProjectKey)
		}
		if svc.LastProgressSet.Assessor != "Codex" {
			t.Errorf("service Assessor = %q, want Codex", svc.LastProgressSet.Assessor)
		}
		if svc.LastProgressSet.Percent != 40 {
			t.Errorf("service Percent = %d, want 40", svc.LastProgressSet.Percent)
		}
		if svc.LastProgressSet.ETA == nil || !svc.LastProgressSet.ETA.Equal(eta) {
			t.Errorf("service ETA = %v, want %v", svc.LastProgressSet.ETA, eta)
		}

		data, ok := sc["data"].(map[string]any)
		if !ok {
			t.Fatalf("sc[data] is %T, want map[string]any: %v", sc["data"], sc)
		}
		mark, ok := data["mark"].(map[string]any)
		if !ok {
			t.Fatalf("data[mark] is %T, want map[string]any: %v", data["mark"], data)
		}
		if mark["id"] != "pm-101" {
			t.Errorf("mark.id = %v, want pm-101", mark["id"])
		}
		if mark["project"] != "KANB" {
			t.Errorf("mark.project = %v, want KANB", mark["project"])
		}
		if mark["task"] != "KANB-3" {
			t.Errorf("mark.task = %v, want KANB-3", mark["task"])
		}
		if mark["percent"] != float64(40) {
			t.Errorf("mark.percent = %v, want 40", mark["percent"])
		}

		sumMap, ok := data["summary"].(map[string]any)
		if !ok {
			t.Fatalf("data[summary] is %T, want map[string]any: %v", data["summary"], data)
		}
		if sumMap["percent"] != float64(40) {
			t.Errorf("summary.percent = %v, want 40", sumMap["percent"])
		}
		if sumMap["assessors"] != float64(1) {
			t.Errorf("summary.assessors = %v, want 1", sumMap["assessors"])
		}
	})

	t.Run("project_target", func(t *testing.T) {
		t.Parallel()
		cs, svc := roundtripServer(t, NewServer)

		now := time.Now().UTC().Truncate(time.Second)
		summary := 65

		svc.DefaultProgressSet = &service.ProgressSetResult{
			Mark: domain.ProgressMark{
				ID:        "pm-102",
				ProjectID: "proj-1",
				TaskID:    nil,
				Assessor:  "Claude",
				Percent:   65,
				CreatedAt: now,
			},
			ProjectKey:     "KANB",
			SummaryPercent: &summary,
			Assessors:      1,
		}

		res, sc := callTool(t, cs, "progress_set", map[string]any{
			"project":  "KANB",
			"assessor": "Claude",
			"percent":  65,
		})
		if res.IsError {
			t.Fatalf("callTool failed: %v", sc)
		}
		expectOK(t, sc, "progress_set")

		if svc.LastProgressSet.ProjectKey != "KANB" {
			t.Errorf("service ProjectKey = %q, want KANB", svc.LastProgressSet.ProjectKey)
		}
		if svc.LastProgressSet.TaskKey != "" {
			t.Errorf("service TaskKey = %q, want empty for project assessment", svc.LastProgressSet.TaskKey)
		}

		data, ok := sc["data"].(map[string]any)
		if !ok {
			t.Fatalf("sc[data] is %T, want map[string]any", sc["data"])
		}
		mark, ok := data["mark"].(map[string]any)
		if !ok {
			t.Fatalf("data[mark] is %T, want map[string]any", data["mark"])
		}
		if mark["project"] != "KANB" {
			t.Errorf("mark.project = %v, want KANB", mark["project"])
		}
		if _, exists := mark["task"]; exists {
			t.Errorf("mark.task should be omitted for project assessment: %v", mark["task"])
		}
	})
}

// TestProgressSet_MutuallyExclusiveTargetValidation verifies that providing
// both task and project, or providing neither, returns clear validation errors.
func TestProgressSet_MutuallyExclusiveTargetValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args map[string]any
	}{
		{
			name: "both_task_and_project",
			args: map[string]any{
				"task":     "KANB-3",
				"project":  "KANB",
				"assessor": "alpha",
				"percent":  50,
			},
		},
		{
			name: "neither_task_nor_project",
			args: map[string]any{
				"assessor": "alpha",
				"percent":  50,
			},
		},
		{
			name: "empty_task_and_empty_project",
			args: map[string]any{
				"task":     "   ",
				"project":  "   ",
				"assessor": "alpha",
				"percent":  50,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cs, svc := roundtripServer(t, NewServer)

			res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
				Name:      "progress_set",
				Arguments: tc.args,
			})
			if err != nil {
				t.Fatalf("CallTool unexpected protocol error: %v", err)
			}
			env := errorEnvelopeOf(t, res)
			if env["code"] != string(domain.CodeValidation) {
				t.Errorf("code = %v, want %v", env["code"], domain.CodeValidation)
			}
			msg, _ := env["message"].(string)
			if !strings.Contains(msg, "task") || !strings.Contains(msg, "project") {
				t.Errorf("error message should mention both task and project: %q", msg)
			}
			rem, _ := env["remediation"].(string)
			if rem == "" {
				t.Errorf("remediation should not be empty")
			}
			// Verify service was never called
			if svc.LastProgressSet.Assessor != "" {
				t.Errorf("service was invoked despite mutual exclusivity failure: %+v", svc.LastProgressSet)
			}
		})
	}
}

// TestProgressSet_PercentAndEtaValidation verifies that percent outside 0..100
// and malformed eta timestamps are rejected on input before calling the service.
func TestProgressSet_PercentAndEtaValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		args          map[string]any
		expectedField string
	}{
		{
			name: "percent_negative",
			args: map[string]any{
				"task":     "KANB-3",
				"assessor": "alpha",
				"percent":  -5,
			},
			expectedField: "percent",
		},
		{
			name: "percent_over_100",
			args: map[string]any{
				"task":     "KANB-3",
				"assessor": "alpha",
				"percent":  105,
			},
			expectedField: "percent",
		},
		{
			name: "eta_malformed_string",
			args: map[string]any{
				"task":     "KANB-3",
				"assessor": "alpha",
				"percent":  50,
				"eta":      "tomorrow at 5pm",
			},
			expectedField: "eta",
		},
		{
			name: "eta_empty_string",
			args: map[string]any{
				"task":     "KANB-3",
				"assessor": "alpha",
				"percent":  50,
				"eta":      "   ",
			},
			expectedField: "eta",
		},
		{
			name: "eta_date_only_without_time",
			args: map[string]any{
				"task":     "KANB-3",
				"assessor": "alpha",
				"percent":  50,
				"eta":      "2026-09-12",
			},
			expectedField: "eta",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cs, svc := roundtripServer(t, NewServer)

			res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
				Name:      "progress_set",
				Arguments: tc.args,
			})
			if err != nil {
				t.Fatalf("unexpected protocol error: %v", err)
			}
			env := errorEnvelopeOf(t, res)
			if env["code"] != string(domain.CodeValidation) {
				t.Errorf("code = %v, want %v", env["code"], domain.CodeValidation)
			}
			msg, _ := env["message"].(string)
			if !strings.Contains(strings.ToLower(msg), tc.expectedField) {
				t.Errorf("error message %q should mention %q", msg, tc.expectedField)
			}
			// Service must NOT have been called
			if svc.LastProgressSet.Assessor != "" {
				t.Errorf("service was called despite input validation failure: %+v", svc.LastProgressSet)
			}
		})
	}
}

// TestProgressSet_TwoCallsSameAssessor_SummaryTakesSecond verifies that two
// consecutive calls by the same assessor append two history marks, and the
// aggregated summary reflects only the latest mark.
func TestProgressSet_TwoCallsSameAssessor_SummaryTakesSecond(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	var marksHistory []domain.ProgressMark
	latestMarks := make(map[string]domain.ProgressMark)

	svc.NextProgressSet = func(_ context.Context, _ service.Actor, in service.ProgressSetInput) (*service.ProgressSetResult, error) {
		m := domain.ProgressMark{
			ID:        fmt.Sprintf("pm-%d", len(marksHistory)+1),
			ProjectID: "proj-1",
			TaskID:    &in.TaskKey,
			Assessor:  in.Assessor,
			Percent:   in.Percent,
			ETA:       in.ETA,
			CreatedAt: time.Now().UTC(),
		}
		marksHistory = append(marksHistory, m)
		latestMarks[in.Assessor] = m

		// Compute mean of latest marks
		sum := 0
		for _, lm := range latestMarks {
			sum += lm.Percent
		}
		mean := service.RoundMeanHalfUp(sum, len(latestMarks))
		assessors := len(latestMarks)

		return &service.ProgressSetResult{
			Mark:           m,
			TaskKey:        in.TaskKey,
			ProjectKey:     "KANB",
			SummaryPercent: &mean,
			Assessors:      assessors,
		}, nil
	}

	// Call 1: alpha reports 30%
	res1, sc1 := callTool(t, cs, "progress_set", map[string]any{
		"task":     "KANB-3",
		"assessor": "alpha",
		"percent":  30,
	})
	if res1.IsError {
		t.Fatalf("call 1 failed: %v", sc1)
	}
	expectOK(t, sc1, "progress_set")

	d1 := sc1["data"].(map[string]any)
	sum1 := d1["summary"].(map[string]any)
	if sum1["percent"] != float64(30) || sum1["assessors"] != float64(1) {
		t.Errorf("call 1 summary = %v, want 30 and 1 assessor", sum1)
	}

	// Queue handler for call 2
	svc.NextProgressSet = func(_ context.Context, _ service.Actor, in service.ProgressSetInput) (*service.ProgressSetResult, error) {
		m := domain.ProgressMark{
			ID:        fmt.Sprintf("pm-%d", len(marksHistory)+1),
			ProjectID: "proj-1",
			TaskID:    &in.TaskKey,
			Assessor:  in.Assessor,
			Percent:   in.Percent,
			ETA:       in.ETA,
			CreatedAt: time.Now().UTC(),
		}
		marksHistory = append(marksHistory, m)
		latestMarks[in.Assessor] = m

		sum := 0
		for _, lm := range latestMarks {
			sum += lm.Percent
		}
		mean := service.RoundMeanHalfUp(sum, len(latestMarks))
		assessors := len(latestMarks)

		return &service.ProgressSetResult{
			Mark:           m,
			TaskKey:        in.TaskKey,
			ProjectKey:     "KANB",
			SummaryPercent: &mean,
			Assessors:      assessors,
		}, nil
	}

	// Call 2: same assessor alpha updates to 80%
	res2, sc2 := callTool(t, cs, "progress_set", map[string]any{
		"task":     "KANB-3",
		"assessor": "alpha",
		"percent":  80,
	})
	if res2.IsError {
		t.Fatalf("call 2 failed: %v", sc2)
	}
	expectOK(t, sc2, "progress_set")

	// Verify history has TWO entries, not overwritten
	if len(marksHistory) != 2 {
		t.Fatalf("history marks count = %d, want 2", len(marksHistory))
	}
	if marksHistory[0].Percent != 30 || marksHistory[1].Percent != 80 {
		t.Fatalf("history percents = [%d, %d], want [30, 80]", marksHistory[0].Percent, marksHistory[1].Percent)
	}

	// Verify summary takes only the latest mark (80), not the average (55)
	d2 := sc2["data"].(map[string]any)
	sum2 := d2["summary"].(map[string]any)
	if sum2["percent"] != float64(80) {
		t.Errorf("call 2 summary percent = %v, want 80 (latest mark)", sum2["percent"])
	}
	if sum2["assessors"] != float64(1) {
		t.Errorf("call 2 assessors = %v, want 1", sum2["assessors"])
	}
}

// TestProgressSet_RollbackDown_SavedAndReadFromHistory verifies that a downward
// revision (rollback from 70% to 45%) is preserved in history and visible.
func TestProgressSet_RollbackDown_SavedAndReadFromHistory(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	var history []domain.ProgressMark

	recordHandler := func(_ context.Context, _ service.Actor, in service.ProgressSetInput) (*service.ProgressSetResult, error) {
		m := domain.ProgressMark{
			ID:        fmt.Sprintf("pm-rev-%d", len(history)+1),
			ProjectID: "proj-1",
			TaskID:    &in.TaskKey,
			Assessor:  in.Assessor,
			Percent:   in.Percent,
			ETA:       in.ETA,
			CreatedAt: time.Now().UTC(),
		}
		history = append(history, m)
		pct := in.Percent
		return &service.ProgressSetResult{
			Mark:           m,
			TaskKey:        in.TaskKey,
			ProjectKey:     "KANB",
			SummaryPercent: &pct,
			Assessors:      1,
		}, nil
	}

	// 1. Initial high estimate: 70%
	svc.NextProgressSet = recordHandler
	res1, sc1 := callTool(t, cs, "progress_set", map[string]any{
		"task":     "KANB-3",
		"assessor": "alpha",
		"percent":  70,
	})
	if res1.IsError {
		t.Fatalf("call 1 failed: %v", sc1)
	}
	expectOK(t, sc1, "progress_set")

	// 2. Rollback down to 45%
	svc.NextProgressSet = recordHandler
	res2, sc2 := callTool(t, cs, "progress_set", map[string]any{
		"task":     "KANB-3",
		"assessor": "alpha",
		"percent":  45,
	})
	if res2.IsError {
		t.Fatalf("call 2 failed: %v", sc2)
	}
	expectOK(t, sc2, "progress_set")

	// 3. Verify history preserves both points in sequence
	if len(history) != 2 {
		t.Fatalf("history length = %d, want 2", len(history))
	}
	if history[0].Percent != 70 {
		t.Errorf("history[0] percent = %d, want 70", history[0].Percent)
	}
	if history[1].Percent != 45 {
		t.Errorf("history[1] percent = %d, want 45", history[1].Percent)
	}

	d2 := sc2["data"].(map[string]any)
	sum2 := d2["summary"].(map[string]any)
	if sum2["percent"] != float64(45) {
		t.Errorf("downgraded summary percent = %v, want 45", sum2["percent"])
	}
}

// TestProgressSet_UnknownFieldRejected verifies that unexpected properties in
// arguments are rejected by the schema validator.
func TestProgressSet_UnknownFieldRejected(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "progress_set",
		Arguments: map[string]any{
			"task":        "KANB-3",
			"assessor":    "alpha",
			"percent":     50,
			"extra_field": "unexpected",
		},
	})
	if err != nil {
		t.Fatalf("schema rejection should be a CallToolResult, not a Go error: %v", err)
	}
	env := errorEnvelopeOf(t, res)
	if env["code"] != string(domain.CodeValidation) {
		t.Errorf("code = %v, want %v", env["code"], domain.CodeValidation)
	}
	msg, _ := env["message"].(string)
	if !strings.Contains(msg, "extra_field") {
		t.Errorf("error message should name the unknown field: %q", msg)
	}
	if svc.LastProgressSet.Assessor != "" {
		t.Errorf("handler ran despite schema error: %+v", svc.LastProgressSet)
	}
}

// TestProgressSet_ReadOnlyServerLacksTool_AndFullServerHasIt verifies that
// progress_set is excluded from read-only MCP server and present in full server.
func TestProgressSet_ReadOnlyServerLacksTool_AndFullServerHasIt(t *testing.T) {
	t.Parallel()

	// 1. Read-only server lacks progress_set
	roCS, _ := roundtripServer(t, NewReadOnlyServer)
	roTools, err := roCS.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("roCS.ListTools: %v", err)
	}
	for _, tl := range roTools.Tools {
		if tl.Name == "progress_set" {
			t.Fatalf("read-only server MUST NOT list progress_set, but found it")
		}
	}

	// Calling progress_set on read-only server must fail
	roRes, roErr := roCS.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "progress_set",
		Arguments: map[string]any{
			"task":     "KANB-3",
			"assessor": "alpha",
			"percent":  50,
		},
	})
	if roErr == nil && (roRes == nil || !roRes.IsError) {
		t.Fatalf("calling progress_set on read-only server should fail; got res=%v, err=%v", roRes, roErr)
	}

	// 2. Full server has progress_set
	fullCS, fullSvc := roundtripServer(t, NewServer)
	pct := 50
	fullSvc.DefaultProgressSet = &service.ProgressSetResult{
		Mark: domain.ProgressMark{
			ID:        "pm-full-1",
			ProjectID: "proj-1",
			Assessor:  "alpha",
			Percent:   50,
			CreatedAt: time.Now().UTC(),
		},
		ProjectKey:     "KANB",
		SummaryPercent: &pct,
		Assessors:      1,
	}

	fullTools, err := fullCS.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("fullCS.ListTools: %v", err)
	}
	found := false
	for _, tl := range fullTools.Tools {
		if tl.Name == "progress_set" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("full server must list progress_set, but did not")
	}

	// Calling progress_set on full server succeeds
	fullRes, fullSC := callTool(t, fullCS, "progress_set", map[string]any{
		"task":     "KANB-3",
		"assessor": "alpha",
		"percent":  50,
	})
	if fullRes.IsError {
		t.Fatalf("callTool on full server failed: %v", fullSC)
	}
	expectOK(t, fullSC, "progress_set")
}

// TestProgressSet_DescriptionContent verifies that the published description
// clearly states the operational context: periodic assessment, under/overestimating
// is expected, rollback is normal, and eta is date/time completion forecast.
func TestProgressSet_DescriptionContent(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)
	tool := toolByName(t, cs, "progress_set")
	desc := tool.Description

	requiredPhrases := []string{
		"periodic",
		"Overestimating",
		"rolling",
		"backward",
		"defeat",
		"RFC3339",
		"forecast",
		"not a remaining duration",
	}

	for _, phrase := range requiredPhrases {
		if !strings.Contains(desc, phrase) {
			t.Errorf("progress_set description missing phrase %q:\n%s", phrase, desc)
		}
	}
}

// TestProgressSet_AssessorTooLongRejected verifies that the assessor field is
// bounded by the same limit (domain.MaxAssigneeLen) a chat author already is
// — an independent review found this missing: an unbounded assessor name is
// stored forever (progress_marks is append-only, thinning is forbidden by
// the owner) and re-rendered on every board load and every chart open, so an
// oversized name is a permanent, repeating cost rather than a one-off one.
// The schema itself refuses anything past the limit before the handler even
// runs, so the service is never reached and never called.
func TestProgressSet_AssessorTooLongRejected(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "progress_set",
		Arguments: map[string]any{
			"task":     "KANB-3",
			"assessor": strings.Repeat("x", domain.MaxAssigneeLen+1),
			"percent":  50,
		},
	})
	if err != nil {
		t.Fatalf("CallTool unexpected protocol error: %v", err)
	}
	env := errorEnvelopeOf(t, res)
	if env["code"] != string(domain.CodeValidation) {
		t.Errorf("code = %v, want %v", env["code"], domain.CodeValidation)
	}
	if svc.LastProgressSet.Assessor != "" {
		t.Errorf("service was called despite an over-length assessor: %+v", svc.LastProgressSet)
	}
}

// TestProgressSet_EmptyAssessorRejected verifies that an empty assessor is rejected.
func TestProgressSet_EmptyAssessorRejected(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "progress_set",
		Arguments: map[string]any{
			"task":     "KANB-3",
			"assessor": "   ",
			"percent":  50,
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	env := errorEnvelopeOf(t, res)
	if env["code"] != string(domain.CodeValidation) {
		t.Errorf("code = %v, want %v", env["code"], domain.CodeValidation)
	}
	if !strings.Contains(env["message"].(string), "assessor") {
		t.Errorf("message %q should mention assessor", env["message"])
	}
	if svc.LastProgressSet.Assessor != "" {
		t.Errorf("service called despite empty assessor: %+v", svc.LastProgressSet)
	}
}
