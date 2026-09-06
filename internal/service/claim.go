package service

import (
	"context"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// TaskClaim is the singular CAS from PLAN §6.7. claim and renew share the
// same compare-and-set at the store layer (an actor may always "renew" a
// free or self-held lease); only the emitted event type distinguishes them
// for the activity feed.
func (s *svc) TaskClaim(ctx context.Context, a Actor, in TaskClaimInput) (*TaskClaimResult, error) {
	if err := requireWrite(a); err != nil {
		return nil, err
	}
	if in.Key == "" {
		return nil, domain.Invalid("key", "key is required", "Pass the task key.")
	}
	switch in.Action {
	case ClaimTake, ClaimRenew, ClaimRelease:
	default:
		return nil, domain.Invalid("action", "action must be claim, renew or release",
			"Use one of: claim, renew, release.")
	}
	if err := requireAdminForce(a, in.Force); err != nil {
		return nil, err
	}
	ctx = store.WithActor(ctx, a.Name)

	var result TaskClaimResult
	var pending []domain.Event
	err := s.store.Write(ctx, func(tx store.Tx) error {
		t, err := s.resolveTask(tx, a, in.Key)
		if err != nil {
			return err
		}
		proj, err := s.store.Projects().GetByID(tx, t.ProjectID)
		if err != nil {
			return err
		}
		now, err := tx.Now()
		if err != nil {
			return err
		}
		cc := newColumnCache(s, tx)
		pc := newProjectCache(s, tx)
		pc.prime(proj)

		switch in.Action {
		case ClaimRelease:
			ok, err := s.store.Tasks().Release(tx, t.ID, a.Name, in.Force)
			if err != nil {
				return err
			}
			if !ok {
				fresh, ferr := s.store.Tasks().GetByID(tx, t.ID)
				if ferr == nil && fresh.ClaimedBy != nil {
					return domain.Claimed(fresh.Key, *fresh.ClaimedBy)
				}
				return domain.Claimed(t.Key, "another actor")
			}
			if err := s.emit(tx, &pending, a.Name, domain.EventTaskReleased, t.ProjectID, &t.ID,
				map[string]any{"key": t.Key, "force": in.Force}); err != nil {
				return err
			}
			fresh, err := s.store.Tasks().GetByID(tx, t.ID)
			if err != nil {
				return err
			}
			tv, err := s.hydrateView(tx, cc, pc, fresh, now, hydrateOpts{})
			if err != nil {
				return err
			}
			result = TaskClaimResult{Task: tv}

		case ClaimTake, ClaimRenew:
			ttl := ttlFor(proj, in.TTLSeconds)
			if in.Force {
				// Force steals unconditionally: release whoever holds it
				// (idempotent no-op if already free), then claim, which now
				// always succeeds because nothing can hold a live lease.
				if _, err := s.store.Tasks().Release(tx, t.ID, a.Name, true); err != nil {
					return err
				}
			}
			ok, fresh, err := s.store.Tasks().Claim(tx, t.ID, a.Name, ttl)
			if err != nil {
				return err
			}
			if !ok {
				holder := "another actor"
				if fresh.ClaimedBy != nil {
					holder = *fresh.ClaimedBy
				}
				return domain.Claimed(fresh.Key, holder)
			}
			evType := domain.EventTaskClaimed
			if in.Action == ClaimRenew {
				evType = domain.EventTaskRenewed
			}
			if err := s.emit(tx, &pending, a.Name, evType, t.ProjectID, &fresh.ID,
				map[string]any{"key": fresh.Key, "force": in.Force}); err != nil {
				return err
			}
			tv, err := s.hydrateView(tx, cc, pc, fresh, now, hydrateOpts{})
			if err != nil {
				return err
			}
			remain := 0
			if d := domain.LeaseRemaining(fresh, now); d != nil {
				remain = int(d.Seconds())
			}
			result = TaskClaimResult{
				Task:           tv,
				ClaimedBy:      a.Name,
				ClaimExpiresAt: fresh.ClaimExpiresAt,
				RemainSeconds:  remain,
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishAll(pending)
	return &result, nil
}

// ttlFor clamps a caller-requested TTL, defaulting to the project's own
// setting when the caller passes zero.
func ttlFor(p *domain.Project, requestedSeconds int) time.Duration {
	if requestedSeconds <= 0 {
		return domain.ClampClaimTTL(time.Duration(p.ClaimTTLSeconds) * time.Second)
	}
	return domain.ClampClaimTTL(time.Duration(requestedSeconds) * time.Second)
}
