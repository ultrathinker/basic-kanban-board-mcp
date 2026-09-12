package service

import (
	"context"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// TaskProgressItem.Tracks / ProjectProgressResult.ManualTracks: the
// per-assessor breakdown behind the aggregate mean, added so the web layer's
// delete-track control can name an assessor and a point count without
// reaching into the store itself (layering: web calls service, never store).
// ---------------------------------------------------------------------------

func TestTaskProgress_TracksCarryPerAssessorBreakdown(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	task := makeBacklogTask(t, env, "assessed by two")
	addProgressMark(t, env, "m1", &task.ID, "alpha", 10, progressTestBase)
	addProgressMark(t, env, "m2", &task.ID, "alpha", 80, progressTestBase.Add(time.Second))
	addProgressMark(t, env, "m3", &task.ID, "beta", 60, progressTestBase)

	got, err := env.svc.TaskProgress(ctx, env.actor, TaskProgressInput{
		ProjectKey: env.proj.Key, Keys: []string{task.Key},
	})
	if err != nil {
		t.Fatalf("TaskProgress: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(got.Items))
	}
	tracks := got.Items[0].Tracks
	if len(tracks) != 2 {
		t.Fatalf("tracks = %+v, want 2 entries (alpha, beta)", tracks)
	}
	byAssessor := map[string]AssessorTrack{}
	for _, tr := range tracks {
		byAssessor[tr.Assessor] = tr
	}
	if a := byAssessor["alpha"]; a.Percent != 80 || a.Count != 2 {
		t.Fatalf("alpha's track = %+v, want percent=80 (the latest) count=2 (the whole track)", a)
	}
	if b := byAssessor["beta"]; b.Percent != 60 || b.Count != 1 {
		t.Fatalf("beta's track = %+v, want percent=60 count=1", b)
	}

	// A task nobody assessed carries no tracks at all — not an empty slice
	// masquerading as "checked, found nothing".
	untouched := makeBacklogTask(t, env, "nobody assessed this")
	got2, err := env.svc.TaskProgress(ctx, env.actor, TaskProgressInput{
		ProjectKey: env.proj.Key, Keys: []string{untouched.Key},
	})
	if err != nil {
		t.Fatalf("TaskProgress (untouched): %v", err)
	}
	if len(got2.Items[0].Tracks) != 0 {
		t.Fatalf("untouched task tracks = %+v, want none", got2.Items[0].Tracks)
	}
}

func TestProjectProgress_ManualTracksIgnoreTaskMarks(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	// Project-level: alpha revises 20 -> 90, beta 40.
	addProgressMark(t, env, "p1", nil, "alpha", 20, progressTestBase)
	addProgressMark(t, env, "p2", nil, "alpha", 90, progressTestBase.Add(time.Second))
	addProgressMark(t, env, "p3", nil, "beta", 40, progressTestBase)
	// A task-level mark for the same assessor must not bleed into the
	// project-level breakdown — same isolation ProjectProgress.Manual
	// already guarantees for the mean itself.
	task := makeBacklogTask(t, env, "task-level noise")
	addProgressMark(t, env, "t1", &task.ID, "alpha", 100, progressTestBase)

	got, err := env.svc.ProjectProgress(ctx, env.actor, ProjectProgressInput{ProjectKey: env.proj.Key})
	if err != nil {
		t.Fatalf("ProjectProgress: %v", err)
	}
	if len(got.ManualTracks) != 2 {
		t.Fatalf("manual tracks = %+v, want 2 entries (alpha, beta)", got.ManualTracks)
	}
	byAssessor := map[string]AssessorTrack{}
	for _, tr := range got.ManualTracks {
		byAssessor[tr.Assessor] = tr
	}
	if a := byAssessor["alpha"]; a.Percent != 90 || a.Count != 2 {
		t.Fatalf("alpha's manual track = %+v, want percent=90 count=2 (project-level marks only)", a)
	}
	if b := byAssessor["beta"]; b.Percent != 40 || b.Count != 1 {
		t.Fatalf("beta's manual track = %+v, want percent=40 count=1", b)
	}
}

// TestProgressTrackDelete_RecomputesAggregateAndTracks proves the delete is
// visible through the normal read path immediately afterward: the mean
// recomputes over the survivors and the deleted assessor's row is gone from
// Tracks, not just from the store's own history table.
func TestProgressTrackDelete_RecomputesAggregateAndTracks(t *testing.T) {
	env := openTestEnv(t)
	ctx := context.Background()

	task := makeBacklogTask(t, env, "two assessors")
	addProgressMark(t, env, "a1", &task.ID, "alpha", 20, progressTestBase)
	addProgressMark(t, env, "a2", &task.ID, "alpha", 80, progressTestBase.Add(time.Second))
	addProgressMark(t, env, "b1", &task.ID, "beta", 60, progressTestBase)

	before, err := env.svc.TaskProgress(ctx, env.actor, TaskProgressInput{
		ProjectKey: env.proj.Key, Keys: []string{task.Key},
	})
	if err != nil {
		t.Fatalf("TaskProgress (before): %v", err)
	}
	if before.Items[0].Percent == nil || *before.Items[0].Percent != 70 {
		t.Fatalf("percent before delete = %v, want 70 (mean of 80, 60)", before.Items[0].Percent)
	}

	if _, err := env.svc.ProgressTrackDelete(ctx, env.actor, ProgressTrackDeleteInput{
		ProjectKey: env.proj.Key, TaskKey: task.Key, Assessor: "alpha",
	}); err != nil {
		t.Fatalf("ProgressTrackDelete: %v", err)
	}

	after, err := env.svc.TaskProgress(ctx, env.actor, TaskProgressInput{
		ProjectKey: env.proj.Key, Keys: []string{task.Key},
	})
	if err != nil {
		t.Fatalf("TaskProgress (after): %v", err)
	}
	if after.Items[0].Percent == nil || *after.Items[0].Percent != 60 {
		t.Fatalf("percent after deleting alpha = %v, want 60 (beta alone)", after.Items[0].Percent)
	}
	if len(after.Items[0].Tracks) != 1 || after.Items[0].Tracks[0].Assessor != "beta" {
		t.Fatalf("tracks after delete = %+v, want [beta] only", after.Items[0].Tracks)
	}
}
