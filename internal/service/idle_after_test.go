package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// The project's own idle threshold (KANB-67)
// ---------------------------------------------------------------------------

// setIdleAfter sends settings.idle_after_seconds through ProjectUpsert, the
// path project_upsert takes, and returns the stored project.
func setIdleAfter(t *testing.T, env *testEnv, seconds int) (domain.Project, error) {
	t.Helper()
	res, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode: UpsertUpdate, Key: env.proj.Key, IfVersion: intPtrLocal(projectVersion(t, env)),
		Settings: &ProjectSettings{IdleAfterSeconds: intPtrLocal(seconds)},
	})
	if err != nil {
		return domain.Project{}, err
	}
	return res.Project, nil
}

// With the setting, the threshold is the setting and no longer the claim
// TTL: a card idle one second past the lease is NOT named, one second short
// of the setting is not either, exactly at it is not (strict), one second
// past it is. The setting is echoed on the board read.
func TestAttention_ThresholdIsIdleAfterWhenSet(t *testing.T) {
	env := openTestEnv(t)
	const idleAfter = 48 * time.Hour
	if _, err := setIdleAfter(t, env, int(idleAfter.Seconds())); err != nil {
		t.Fatalf("set idle_after_seconds: %v", err)
	}
	task := makeTaskIn(t, env, env.cols["Doing"], "in review for days", nil)
	ttl := time.Duration(env.proj.ClaimTTLSeconds) * time.Second
	if ttl+time.Second >= idleAfter {
		t.Fatalf("test needs the claim TTL (%v) well below the setting (%v)", ttl, idleAfter)
	}
	t0 := rowMovement(task)

	cases := []struct {
		name  string
		now   time.Time
		stale bool
	}{
		{"one second past the claim ttl", t0.Add(ttl + time.Second), false},
		{"one second younger than the setting", t0.Add(idleAfter - time.Second), false},
		{"exactly the setting", t0.Add(idleAfter), false},
		{"one second older than the setting", t0.Add(idleAfter + time.Second), true},
	}
	for _, tc := range cases {
		env.pinClock(tc.now)
		bp := env.boardProject(t, BoardGetInput{View: ViewTasks})
		if bp.IdleAfterSeconds != int(idleAfter.Seconds()) {
			t.Fatalf("board IdleAfterSeconds = %d, want %d", bp.IdleAfterSeconds, int(idleAfter.Seconds()))
		}
		if !tc.stale {
			if bp.Attention != nil {
				t.Errorf("%s: attention %+v, want none", tc.name, *bp.Attention)
			}
			continue
		}
		if bp.Attention == nil || bp.Attention.Count != 1 {
			t.Fatalf("%s: attention %+v, want exactly %s", tc.name, bp.Attention, task.Key)
		}
		if got := bp.Attention.Sample[0]; got.Key != task.Key || got.Idle != idleAfter+time.Second {
			t.Errorf("%s: sample %+v, want %s idle %v", tc.name, got, task.Key, idleAfter+time.Second)
		}
	}
}

// 0 clears the setting, and the threshold goes back to the claim TTL — the
// behaviour of a project that never set it.
func TestAttention_ClearingIdleAfterRestoresTheClaimTTL(t *testing.T) {
	env := openTestEnv(t)
	if _, err := setIdleAfter(t, env, int((48 * time.Hour).Seconds())); err != nil {
		t.Fatalf("set: %v", err)
	}
	p, err := setIdleAfter(t, env, 0)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if p.IdleAfterSeconds != 0 {
		t.Fatalf("after clearing, IdleAfterSeconds = %d, want 0", p.IdleAfterSeconds)
	}
	task := makeTaskIn(t, env, env.cols["Doing"], "working", nil)
	env.pinClock(rowMovement(task).Add(time.Duration(env.proj.ClaimTTLSeconds)*time.Second + time.Second))
	bp := env.boardProject(t, BoardGetInput{View: ViewSummary})
	if bp.IdleAfterSeconds != 0 {
		t.Errorf("board IdleAfterSeconds = %d, want 0", bp.IdleAfterSeconds)
	}
	if got := attentionKeys(bp.Attention); len(got) != 1 || got[0] != task.Key {
		t.Errorf("attention = %v, want [%s] at one second past the claim ttl", got, task.Key)
	}
}

// Leaving the setting out of an update keeps whatever the project had: a
// settings object that only touches claim_ttl_seconds must not wipe it.
func TestProjectUpsert_IdleAfterSurvivesAnUnrelatedSettingsUpdate(t *testing.T) {
	env := openTestEnv(t)
	if _, err := setIdleAfter(t, env, 7200); err != nil {
		t.Fatalf("set: %v", err)
	}
	res, err := env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
		Mode: UpsertUpdate, Key: env.proj.Key, IfVersion: intPtrLocal(projectVersion(t, env)),
		Settings: &ProjectSettings{ClaimTTLSeconds: intPtrLocal(600)},
	})
	if err != nil {
		t.Fatalf("update claim ttl: %v", err)
	}
	if res.Project.IdleAfterSeconds != 7200 || res.Project.ClaimTTLSeconds != 600 {
		t.Errorf("idle_after=%d claim_ttl=%d, want 7200 and 600", res.Project.IdleAfterSeconds, res.Project.ClaimTTLSeconds)
	}
}

// The bounds are refused, not clamped, on both create and update, and a
// refused update leaves the stored value alone.
func TestProjectUpsert_IdleAfterBounds(t *testing.T) {
	lo, hi := int(domain.IdleAfterMin.Seconds()), int(domain.IdleAfterMax.Seconds())
	cases := []struct {
		name    string
		seconds int
		ok      bool
	}{
		{"zero clears", 0, true},
		{"the floor", lo, true},
		{"one below the floor", lo - 1, false},
		{"the ceiling", hi, true},
		{"one above the ceiling", hi + 1, false},
		{"negative", -1, false},
		{"large enough to overflow a Duration", 1 << 62, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := openTestEnv(t)
			if _, err := setIdleAfter(t, env, 3600); err != nil {
				t.Fatalf("seed: %v", err)
			}
			p, err := setIdleAfter(t, env, tc.seconds)
			if tc.ok {
				if err != nil {
					t.Fatalf("update: %v", err)
				}
				if p.IdleAfterSeconds != tc.seconds {
					t.Errorf("stored %d, want %d unchanged by any clamp", p.IdleAfterSeconds, tc.seconds)
				}
			} else {
				assertIdleAfterRefused(t, err)
				if bp := env.boardProject(t, BoardGetInput{View: ViewSummary}); bp.IdleAfterSeconds != 3600 {
					t.Errorf("a refused update changed the stored value to %d", bp.IdleAfterSeconds)
				}
			}

			_, err = env.svc.ProjectUpsert(context.Background(), env.actor, ProjectUpsertInput{
				Mode: UpsertCreate, Key: "NEWP", Name: "New",
				Settings: &ProjectSettings{IdleAfterSeconds: intPtrLocal(tc.seconds)},
			})
			if tc.ok && err != nil {
				t.Fatalf("create: %v", err)
			}
			if !tc.ok {
				assertIdleAfterRefused(t, err)
			}
		})
	}
}

func assertIdleAfterRefused(t *testing.T, err error) {
	t.Helper()
	de := domain.AsError(err)
	if de == nil || de.Code != domain.CodeValidation {
		t.Fatalf("err = %v, want a validation error", err)
	}
	if de.Field != "settings.idle_after_seconds" || de.Remediation == "" || !strings.Contains(de.Message, "idle_after_seconds") {
		t.Errorf("error %+v should name settings.idle_after_seconds and say what to do", de)
	}
}
