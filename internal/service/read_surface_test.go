package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ---------------------------------------------------------------------------
// Architecture review finding #10: the read surface did not close two write
// loops. board_get published no project version, settings or estimate unit, so
// project_upsert(mode:"update") had no legitimate source for its if_version;
// and task_get had no notes cursor, so a task's older working history was
// unreachable through the agent API.
// ---------------------------------------------------------------------------

// TestBoardGet_PublishesProjectConfiguration is the loop-closing test: what
// board_get returns has to be enough to drive project_upsert's update without
// a tenth tool and without guessing at settings the caller never saw.
func TestBoardGet_PublishesProjectConfiguration(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	unit := "d"
	ttl := 900
	strict := true
	if _, err := env.svc.ProjectUpsert(ctx, env.actor, ProjectUpsertInput{
		Mode:      UpsertUpdate,
		Key:       env.proj.Key,
		IfVersion: &env.proj.Version,
		Settings: &ProjectSettings{
			EstimateUnit:    &unit,
			ClaimTTLSeconds: &ttl,
			StrictDone:      &strict,
		},
	}); err != nil {
		t.Fatalf("project_upsert: %v", err)
	}

	board, err := env.svc.BoardGet(ctx, env.actor, BoardGetInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("board_get: %v", err)
	}
	bp := board.Projects[0]

	if bp.Version <= env.proj.Version {
		t.Errorf("version = %d, want above the pre-update %d", bp.Version, env.proj.Version)
	}
	if bp.EstimateUnit != "d" {
		t.Errorf("estimate_unit = %q, want d", bp.EstimateUnit)
	}
	if bp.ClaimTTLSeconds != ttl {
		t.Errorf("claim_ttl_seconds = %d, want %d", bp.ClaimTTLSeconds, ttl)
	}
	if !bp.StrictDone {
		t.Errorf("strict_done = false, want true")
	}
	if !bp.EnforceDependencies {
		t.Errorf("enforce_dependencies = false, want true (seeded)")
	}
	if bp.Archived {
		t.Errorf("archived = true, want false")
	}
	if len(bp.Columns) != len(domain.DefaultColumns) {
		t.Errorf("columns = %d, want the full configuration (%d)", len(bp.Columns), len(domain.DefaultColumns))
	}

	// The point of publishing the version: the value board_get reported must
	// be accepted as if_version by the very next update, with no other read.
	if _, err := env.svc.ProjectUpsert(ctx, env.actor, ProjectUpsertInput{
		Mode:      UpsertUpdate,
		Key:       bp.Key,
		IfVersion: &bp.Version,
		Name:      "Renamed from a board read alone",
	}); err != nil {
		t.Fatalf("update using the version board_get published: %v", err)
	}
}

// addNote writes one note with an explicit timestamp. The service's own note
// path stamps the database clock, which is only millisecond-resolution — fine
// in production, too coarse to build a deterministic cursor fixture on.
func addNote(t *testing.T, env *testEnv, taskID, body string, at time.Time) {
	t.Helper()
	if err := env.Write(context.Background(), func(tx store.Tx) error {
		return env.Notes().Add(tx, &domain.Note{
			ID:        uuid.NewString(),
			TaskID:    taskID,
			Author:    env.actor.Name,
			Body:      body,
			CreatedAt: at,
		})
	}); err != nil {
		t.Fatalf("add note: %v", err)
	}
}

// TestTaskGet_NotesBeforePagesOlderHistory walks the whole history of a task
// that has more notes than one page holds. Before the cursor existed,
// everything past the newest MaxNotesPerRead was unreachable to an agent.
func TestTaskGet_NotesBeforePagesOlderHistory(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()
	task := makeBacklogTask(t, env, "long-running")

	const total = domain.MaxNotesPerRead*2 + 5
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	for i := 0; i < total; i++ {
		addNote(t, env, task.ID, fmt.Sprintf("note-%02d", i), base.Add(time.Duration(i)*time.Minute))
	}

	var seen []string
	var cursor *time.Time
	for page := 0; ; page++ {
		if page > 5 {
			t.Fatalf("cursor did not terminate after %d pages", page)
		}
		res, err := env.svc.TaskGet(ctx, env.actor, TaskGetInput{
			Keys:        []string{task.Key},
			Include:     Includes{IncludeNotes},
			NotesBefore: cursor,
		})
		if err != nil {
			t.Fatalf("task_get page %d: %v", page, err)
		}
		notes := res.Tasks[0].Notes
		if len(notes) > domain.MaxNotesPerRead {
			t.Fatalf("page %d returned %d notes, over the %d cap", page, len(notes), domain.MaxNotesPerRead)
		}
		for _, n := range notes {
			seen = append(seen, n.Body)
		}
		next, more := res.NotesNext[task.Key]
		if !more {
			break
		}
		cursor = &next
	}

	if len(seen) != total {
		t.Fatalf("walked %d notes, want all %d", len(seen), total)
	}
	// Newest first, and no note repeated or skipped across the page boundary.
	for i, body := range seen {
		want := fmt.Sprintf("note-%02d", total-1-i)
		if body != want {
			t.Fatalf("note %d = %q, want %q (order or cursor boundary is wrong)", i, body, want)
		}
	}
}

// TestTaskGet_NotesBeforeWithoutNotes_Refuses: a cursor into a page the caller
// did not ask for cannot be honoured, and silently dropping it would leave
// them paging through nothing and believing the history had ended.
func TestTaskGet_NotesBeforeWithoutNotes_Refuses(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "no notes requested")
	at := time.Now().UTC()

	_, err := env.svc.TaskGet(context.Background(), env.actor, TaskGetInput{
		Keys:        []string{task.Key},
		Include:     Includes{IncludeBody},
		NotesBefore: &at,
	})
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeValidation {
		t.Fatalf("err = %v, want a validation error", err)
	}
	if de.Remediation == "" {
		t.Errorf("refusal has no remediation: %+v", de)
	}
}

// ---------------------------------------------------------------------------
// Architecture review finding #21: "included but bounded" had no
// representation, so task_next's default answer cost more than reading the
// entire board.
// ---------------------------------------------------------------------------

// seedRichCandidate creates one backlog task with a body and an acceptance
// list long enough for both detail tiers to have something to clip.
func seedRichCandidate(t *testing.T, env *testEnv, acceptanceItems int) string {
	t.Helper()
	body := strings.Repeat("Implementation notes for the candidate card. ", 100)
	acceptance := make([]string, acceptanceItems)
	for i := range acceptance {
		acceptance[i] = fmt.Sprintf("AC-%02d: the criterion text", i+1)
	}
	res, err := env.svc.TaskCreate(context.Background(), env.actor, TaskCreateInput{
		Tasks: []NewTask{{
			ProjectKey: env.proj.Key,
			Title:      "Rich candidate",
			Body:       body,
			Type:       domain.TypeTask,
			Priority:   domain.PriorityHigh,
			Acceptance: acceptance,
		}},
	})
	if err != nil {
		t.Fatalf("create candidate: %v", err)
	}
	return res.Tasks[0].Key
}

// TestTaskNext_ProjectionTiers is the table test for the two detail levels.
// The result must both APPLY the bounds and REPORT them: a clipped answer that
// does not say it was clipped is indistinguishable from a short task.
func TestTaskNext_ProjectionTiers(t *testing.T) {
	env := openTestEnv(t)
	const items = 25
	key := seedRichCandidate(t, env, items)

	cases := []struct {
		name           string
		detail         NextDetail
		include        Includes
		wantBodyLimit  int
		wantAcceptance int
	}{
		{
			name:           "absent detail is summary",
			include:        Includes{IncludeBody, IncludeAcceptance},
			wantBodyLimit:  NextSummaryBodyBytes,
			wantAcceptance: NextSummaryAcceptanceItems,
		},
		{
			name:           "explicit summary",
			detail:         NextDetailSummary,
			include:        Includes{IncludeBody, IncludeAcceptance},
			wantBodyLimit:  NextSummaryBodyBytes,
			wantAcceptance: NextSummaryAcceptanceItems,
		},
		{
			name:           "full is wider but still bounded",
			detail:         NextDetailFull,
			include:        Includes{IncludeBody, IncludeAcceptance},
			wantBodyLimit:  domain.NextBodyTruncate,
			wantAcceptance: domain.NextAcceptanceItems,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := env.svc.TaskNext(context.Background(), env.actor, TaskNextInput{
				ProjectKey: env.proj.Key,
				Action:     NextPeek,
				Include:    tc.include,
				Detail:     tc.detail,
			})
			if err != nil {
				t.Fatalf("task_next: %v", err)
			}
			tv := res.Tasks[0]

			if len(tv.Body) > tc.wantBodyLimit+32 {
				t.Errorf("body = %d bytes, want clipped to ~%d", len(tv.Body), tc.wantBodyLimit)
			}
			if !strings.Contains(tv.Body, "chars") {
				t.Errorf("clipped body does not report what is missing: %q", tv.Body)
			}
			if len(tv.Acceptance) != tc.wantAcceptance {
				t.Errorf("acceptance = %d items, want %d", len(tv.Acceptance), tc.wantAcceptance)
			}

			p := res.Projection
			if p.Body != DetailBounded || p.BodyLimitBytes != tc.wantBodyLimit {
				t.Errorf("projection body = %q/%d, want bounded/%d", p.Body, p.BodyLimitBytes, tc.wantBodyLimit)
			}
			if p.Acceptance != DetailBounded || p.AcceptanceLimit != tc.wantAcceptance {
				t.Errorf("projection acceptance = %q/%d, want bounded/%d", p.Acceptance, p.AcceptanceLimit, tc.wantAcceptance)
			}
			if got := p.AcceptanceTotals[key]; got != items {
				t.Errorf("projection acceptance total for %s = %d, want %d", key, got, items)
			}
			if !p.Has(IncludeBody) || !p.Has(IncludeAcceptance) {
				t.Errorf("projection fields = %v, want body and acceptance", p.Fields)
			}
		})
	}
}

// TestTaskNext_SelectionIsSeparateFromDetail keeps the two axes apart: a field
// left out of `include` must be absent whatever the detail level says, and the
// projection must not claim to have returned it.
func TestTaskNext_SelectionIsSeparateFromDetail(t *testing.T) {
	env := openTestEnv(t)
	seedRichCandidate(t, env, 5)

	res, err := env.svc.TaskNext(context.Background(), env.actor, TaskNextInput{
		ProjectKey: env.proj.Key,
		Action:     NextPeek,
		Include:    Includes{IncludeAcceptance},
		Detail:     NextDetailFull,
	})
	if err != nil {
		t.Fatalf("task_next: %v", err)
	}
	tv := res.Tasks[0]
	if tv.Body != "" {
		t.Errorf("body returned although it was not selected: %q", tv.Body)
	}
	if len(tv.Acceptance) == 0 {
		t.Errorf("acceptance missing although it was selected")
	}
	if res.Projection.Has(IncludeBody) {
		t.Errorf("projection claims body was returned; it was not selected")
	}
	// Metadata is outside task_next's vocabulary entirely (PLAN §6.2).
	if res.Projection.Has(IncludeMetadata) {
		t.Errorf("projection claims metadata; task_next never returns it")
	}
}

// TestTaskNext_RejectsUnknownDetail: an unrecognised detail level is refused,
// not quietly rounded to one that exists.
func TestTaskNext_RejectsUnknownDetail(t *testing.T) {
	env := openTestEnv(t)
	_, err := env.svc.TaskNext(context.Background(), env.actor, TaskNextInput{
		ProjectKey: env.proj.Key,
		Action:     NextPeek,
		Detail:     NextDetail("verbose"),
	})
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeValidation {
		t.Fatalf("err = %v, want a validation error", err)
	}
	if de.Field != "detail" {
		t.Errorf("error field = %q, want detail", de.Field)
	}
}
