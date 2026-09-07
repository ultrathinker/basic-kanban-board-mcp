package service

import (
	"context"
	"sync"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// This file pins the headline safety property a shared markdown file cannot
// offer (PLAN §6.2): two DIFFERENT actors both ask task_next(start) for the
// same single ready task, and exactly one of them gets it. The loser must be
// handed an empty result — not an error, not the winner's task — and must not
// disturb the winner's lease.
//
// The nearest existing neighbours cover one side each:
// TestTaskNext_StartMovesAndClaimsAtomically has a single actor (no loser),
// and TestTaskNext_ConcurrentStartExactlyOneWins races one shared actor
// against a WIP-full column, where losers get a wip_exceeded error. Neither
// proves "a second, different actor gets nothing".

// TestAtomicClaim_SecondActorGetsNothing is the deterministic core: A starts
// the only ready task, then B asks to start and must receive nothing.
func TestAtomicClaim_SecondActorGetsNothing(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "the only ready task")

	// A takes it. The returned card must already show the move and the lease,
	// because agents chain their next if_version from data.tasks[].
	resA, err := env.svc.TaskNext(context.Background(), env.actor, TaskNextInput{
		ProjectKey: env.proj.Key, Action: NextStart, Limit: 1,
	})
	if err != nil {
		t.Fatalf("actor A start: %v", err)
	}
	if resA.StartedKey != task.Key {
		t.Fatalf("actor A StartedKey = %q, want %q", resA.StartedKey, task.Key)
	}
	var aView *domain.TaskView
	for i := range resA.Tasks {
		if resA.Tasks[i].Key == task.Key {
			aView = &resA.Tasks[i]
			break
		}
	}
	if aView == nil {
		t.Fatalf("started task %q missing from actor A's data.tasks[]", task.Key)
	}
	if aView.ColumnName != "Doing" {
		t.Fatalf("actor A's returned column = %q, want Doing", aView.ColumnName)
	}
	if aView.ClaimedBy == nil || *aView.ClaimedBy != env.actor.Name {
		t.Fatalf("actor A's returned ClaimedBy = %v, want %q", aView.ClaimedBy, env.actor.Name)
	}

	// B asks next. The task left the backlog when A started it, so B's
	// candidate list is empty: a losing start is an empty result, not an error.
	b := env.actorWith(t, nonAdmin)
	b.Name = "agent-b"
	resB, err := env.svc.TaskNext(context.Background(), b, TaskNextInput{
		ProjectKey: env.proj.Key, Action: NextStart, Limit: 1,
	})
	if err != nil {
		t.Fatalf("actor B start: %v (want an empty result, not an error)", err)
	}
	if resB.StartedKey != "" {
		t.Fatalf("actor B StartedKey = %q, want empty (the task is already taken)", resB.StartedKey)
	}
	if len(resB.Tasks) != 0 {
		t.Fatalf("actor B was offered %d tasks, want 0", len(resB.Tasks))
	}

	// B's losing call must not have stolen or double-claimed the task.
	fresh := freshView(t, env, task.Key)
	if fresh.ColumnName != "Doing" {
		t.Fatalf("after B's call ColumnName = %q, want Doing", fresh.ColumnName)
	}
	if fresh.ClaimedBy == nil || *fresh.ClaimedBy != env.actor.Name {
		t.Fatalf("after B's call ClaimedBy = %v, want %q still holding the lease",
			fresh.ClaimedBy, env.actor.Name)
	}
}

// TestAtomicClaim_ParallelTwoActorsExactlyOneStarts asserts the same outcome
// without relying on call order: both actors race task_next(start) for the
// one task, exactly one gets a non-empty StartedKey, and the stored lease
// names that winner — never both, never neither.
func TestAtomicClaim_ParallelTwoActorsExactlyOneStarts(t *testing.T) {
	env := openTestEnv(t)
	task := makeBacklogTask(t, env, "race me")

	b := env.actorWith(t, nonAdmin)
	b.Name = "agent-b"
	actors := []Actor{env.actor, b}

	type outcome struct {
		started string
		err     error
	}
	// Distinct indices, one writer each, so no further sync is needed until
	// wg.Wait hands the slice back to this goroutine.
	outcomes := make([]outcome, len(actors))

	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := range actors {
		wg.Add(1)
		go func(i int, a Actor) {
			defer wg.Done()
			<-gate
			res, err := env.svc.TaskNext(context.Background(), a, TaskNextInput{
				ProjectKey: env.proj.Key, Action: NextStart, Limit: 1,
			})
			if err != nil {
				outcomes[i] = outcome{err: err}
				return
			}
			outcomes[i] = outcome{started: res.StartedKey}
		}(i, actors[i])
	}
	close(gate)
	wg.Wait()

	wins := 0
	winner := ""
	for i, o := range outcomes {
		if o.err != nil {
			t.Fatalf("actor %s failed: %v", actors[i].Name, o.err)
		}
		switch o.started {
		case "":
			// The loser: nothing started, nothing offered.
		case task.Key:
			wins++
			winner = actors[i].Name
		default:
			t.Fatalf("actor %s started %q, want %q or empty", actors[i].Name, o.started, task.Key)
		}
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1", wins)
	}
	fresh := freshView(t, env, task.Key)
	if fresh.ColumnName != "Doing" {
		t.Fatalf("ColumnName = %q after the race, want Doing", fresh.ColumnName)
	}
	if fresh.ClaimedBy == nil || *fresh.ClaimedBy != winner {
		t.Fatalf("ClaimedBy = %v after the race, want the winner %q", fresh.ClaimedBy, winner)
	}
}
