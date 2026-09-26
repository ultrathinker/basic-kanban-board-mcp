package service

import (
	"sort"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// AttentionSampleSize bounds how many idle cards board_get names per
// project. The line is read on every session start, so its cost must not
// grow with how neglected a board is; Attention.Count still says how many
// there are in total.
const AttentionSampleSize = 5

// now is the service clock: the transaction's database clock, unless a test
// pinned it. Idle time is a comparison against "now", and a test that has to
// wait an hour of wall clock to see a card go stale is not a test.
func (s *svc) now(tx store.Tx) (time.Time, error) {
	if s.clock != nil {
		return s.clock(tx)
	}
	return tx.Now()
}

// lastMovement is the newest instant that says "someone is working on this
// card" (KANB-61). Each input is something the board already stores and can
// read without a query per card:
//
//   - ColumnEnteredAt: the card entered its column;
//   - UpdatedAt: any versioned edit (a move writes it too);
//   - ClaimedAt: a claim or a lease renewal — the store rewrites it on both,
//     and neither bumps the version or UpdatedAt, so it must be read apart;
//   - noteAt / markAt: the newest note and progress mark, which live in
//     their own tables and also leave the task row untouched.
//
// Zero values are simply older than everything else.
func lastMovement(t *domain.Task, noteAt, markAt time.Time) time.Time {
	last := t.ColumnEnteredAt
	for _, at := range []time.Time{t.UpdatedAt, noteAt, markAt} {
		if at.After(last) {
			last = at
		}
	}
	if t.ClaimedAt != nil && t.ClaimedAt.After(last) {
		last = *t.ClaimedAt
	}
	return last
}

// idleSince is how long a card has gone without movement. A clock that
// reads earlier than the movement (a pinned test clock, or skew between two
// processes writing the same file) is no idle time, never a negative one.
func idleSince(last, now time.Time) time.Duration {
	if d := now.Sub(last); d > 0 {
		return d
	}
	return 0
}

// idleThreshold is the project's claim TTL as the store would grant it: a
// card idle for longer than one lease has outlived any honest claim on it.
func idleThreshold(p *domain.Project) time.Duration {
	return domain.ClampClaimTTL(time.Duration(p.ClaimTTLSeconds) * time.Second)
}

// buildAttention keeps the cards idle for strictly longer than threshold,
// most idle first (ties by key, so the line is stable between two reads of
// an unchanged board), and samples the head. nil when none qualify.
func buildAttention(idle []IdleTask, threshold time.Duration) *Attention {
	var stale []IdleTask
	for _, it := range idle {
		if it.Idle > threshold {
			stale = append(stale, it)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	sort.Slice(stale, func(i, j int) bool {
		if stale[i].Idle != stale[j].Idle {
			return stale[i].Idle > stale[j].Idle
		}
		return stale[i].Key < stale[j].Key
	})
	a := &Attention{Count: len(stale)}
	if len(stale) > AttentionSampleSize {
		stale = stale[:AttentionSampleSize]
	}
	a.Sample = stale
	return a
}

// newestMark is the latest creation time among one card's latest marks.
func newestMark(marks []domain.ProgressMark) time.Time {
	var at time.Time
	for _, m := range marks {
		if m.CreatedAt.After(at) {
			at = m.CreatedAt
		}
	}
	return at
}

// acceptanceCount is a card's "X of N".
func acceptanceCount(items []domain.AcceptanceItem) (done, total int) {
	for _, it := range items {
		if it.Done {
			done++
		}
	}
	return done, len(items)
}

// boardActivity computes a project's Attention and, when asked for, the
// per-card Activity of its active-column cards, inside the board read's own
// transaction.
//
// Without include:["progress"] it reads the notes and marks tables only when
// some card's row alone already looks idle: a note or a mark can only make a
// card fresher, never staler, so a card that is fresh on its row is settled
// without them — and a busy board pays no extra query at all.
func (s *svc) boardActivity(tx store.Tx, p *domain.Project, active []*domain.Task, wantProgress bool, now time.Time) (*Attention, map[string]TaskActivity, error) {
	threshold := idleThreshold(p)
	candidates := active
	if !wantProgress {
		candidates = nil
		for _, t := range active {
			if idleSince(lastMovement(t, time.Time{}, time.Time{}), now) > threshold {
				candidates = append(candidates, t)
			}
		}
	}
	if len(candidates) == 0 {
		return nil, nil, nil
	}

	ids := make([]string, len(candidates))
	for i, t := range candidates {
		ids[i] = t.ID
	}
	notes, err := s.store.Notes().LatestByTasks(tx, ids)
	if err != nil {
		return nil, nil, err
	}
	marks, err := s.store.Progress().LatestByTask(tx, p.ID)
	if err != nil {
		return nil, nil, err
	}

	idle := make([]IdleTask, 0, len(candidates))
	var activity map[string]TaskActivity
	if wantProgress {
		activity = make(map[string]TaskActivity, len(candidates))
	}
	for _, t := range candidates {
		last := lastMovement(t, notes[t.ID], newestMark(marks[t.ID]))
		d := idleSince(last, now)
		idle = append(idle, IdleTask{Key: t.Key, Idle: d})
		if wantProgress {
			done, total := acceptanceCount(t.Acceptance)
			pct, _ := taskPercent(marks[t.ID])
			activity[t.Key] = TaskActivity{
				AcceptanceDone:  done,
				AcceptanceTotal: total,
				Percent:         pct,
				LastActivityAt:  last,
				Idle:            d,
			}
		}
	}
	return buildAttention(idle, threshold), activity, nil
}

// subtaskKinds breaks a parent's live subtasks down by column kind.
func (s *svc) subtaskKinds(tx store.Tx, cc *columnCache, parentID string) (SubtaskKinds, error) {
	var k SubtaskKinds
	children, err := s.store.Tasks().Children(tx, parentID)
	if err != nil {
		return k, err
	}
	for _, ch := range children {
		if ch.ArchivedAt != nil {
			continue
		}
		col, err := cc.get(ch.ColumnID)
		if err != nil {
			return k, err
		}
		switch col.Kind {
		case domain.KindBacklog:
			k.Backlog++
		case domain.KindActive:
			k.Active++
		case domain.KindWaiting:
			k.Waiting++
		case domain.KindDone:
			k.Done++
		}
	}
	return k, nil
}
