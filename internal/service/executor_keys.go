package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ExecutorKeyIssue mints an executor key (KANB-60). One call gives an agent
// its identity (the key's name), its place in the listed projects'
// participant lists, and write access to exactly the cards assigned to that
// name. The list of executors IS the list of issued keys: there is no second
// catalogue to keep in sync.
//
// Authority is checked per project inside the write transaction: an admin
// may issue for any project, anyone else must be the appointed coordinator
// of every project on the key — an executor key never reaches a project its
// issuer does not run. Executor keys themselves can never issue keys.
func (s *svc) ExecutorKeyIssue(ctx context.Context, a Actor, in ExecutorKeyIssueInput) (*ExecutorKeyIssueResult, error) {
	if err := requireWrite(a, "issue executor keys"); err != nil {
		return nil, err
	}
	if err := domain.ValidateTokenName("name", in.Name); err != nil {
		return nil, err
	}
	ttl, err := domain.ExecutorKeyTTL(in.TTLSeconds)
	if err != nil {
		return nil, err
	}
	keys, err := executorKeyProjects(in.ProjectKeys)
	if err != nil {
		return nil, err
	}
	// Generated before the transaction so the write lock is never held
	// across a read of the system's random source.
	secret, err := auth.NewSecret()
	if err != nil {
		return nil, err
	}
	ctx = store.WithActor(ctx, a.Name)

	var result ExecutorKeyIssueResult
	var pending []domain.Event
	err = s.store.Write(ctx, func(tx store.Tx) error {
		now, err := tx.Now()
		if err != nil {
			return err
		}
		projects := make([]*domain.Project, 0, len(keys))
		for _, k := range keys {
			p, err := s.resolveProject(tx, a, k)
			if err != nil {
				return err
			}
			if err := mayIssueFor(a, p); err != nil {
				return err
			}
			projects = append(projects, p)
		}

		tok := &domain.Token{
			Name:        in.Name,
			Hash:        auth.HashToken(secret),
			Scopes:      domain.Scopes{domain.ScopeExecutor},
			ProjectKeys: keys,
		}
		expires := now.Add(ttl)
		tok.ExpiresAt = &expires

		existing, err := s.store.Tokens().GetByName(tx, in.Name)
		switch {
		case isNotFound(err):
			tok.ID = newID()
			tok.CreatedAt = now
			if err := s.store.Tokens().Create(tx, tok); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if err := s.mayReissue(tx, a, existing, in.Name, now); err != nil {
				return err
			}
			tok.ID = existing.ID
			if err := s.store.Tokens().Reissue(tx, tok); err != nil {
				return err
			}
			result.Reissued = true
		}

		for _, p := range projects {
			if err := s.emit(tx, &pending, a.Name, domain.EventExecutorKeyIssued, p.ID, nil, map[string]any{
				"name":       tok.Name,
				"token_id":   tok.ID,
				"expires_at": expires.UTC().Format(time.RFC3339),
				"reissued":   result.Reissued,
			}); err != nil {
				return err
			}
		}
		result.TokenID = tok.ID
		result.Name = tok.Name
		result.ProjectKeys = keys
		result.ExpiresAt = expires
		return nil
	})
	if err != nil {
		return nil, err
	}
	result.Secret = secret
	s.publishAll(pending)
	return &result, nil
}

// executorKeyProjects validates the key's project list. An executor key is
// always bounded to named projects — an empty list would mean "every
// project", which is a standing participant's reach, not an agent's. A
// repeated key is refused rather than folded, so a caller that meant two
// different projects learns about its typo.
func executorKeyProjects(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, domain.Invalid("projects", "at least one project is required",
			"Name the project(s) the executor works in; an executor key never covers every project.")
	}
	if len(raw) > domain.MaxExecutorKeyProjects {
		return nil, domain.Invalid("projects",
			fmt.Sprintf("%d projects, the limit is %d", len(raw), domain.MaxExecutorKeyProjects),
			"Issue a key per group of projects.")
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, r := range raw {
		k, err := domain.ValidateProjectKey(r)
		if err != nil {
			return nil, err
		}
		if seen[k] {
			return nil, domain.Invalid("projects", fmt.Sprintf("project %s is listed twice", k),
				"List each project once.")
		}
		seen[k] = true
		out = append(out, k)
	}
	return out, nil
}

// mayIssueFor is the authority rule: admin, or the project's appointed
// coordinator. The coordinator is compared by tokens.id, the identity that
// survives rotation, never by name.
func mayIssueFor(a Actor, p *domain.Project) error {
	if a.IsAdmin() {
		return nil
	}
	if a.TokenID != "" && p.CoordinatorTokenID == a.TokenID {
		return nil
	}
	return domain.Forbidden(
		fmt.Sprintf("%s is not the coordinator of project %s, and only an admin or the project's coordinator may issue executor keys for it", a.Name, p.Key),
		fmt.Sprintf("Ask the coordinator of %s (board_get shows it) or an admin to issue the key, or leave %s off the list.", p.Key, p.Key))
}

// mayReissue decides whether an existing token row may take a new executor
// key under its name. Only a dead executor key may: reusing its row keeps
// the name — and so every card already assigned to it — attached to one
// identity. Everything else is refused, and the refusal says why:
//   - a standing (read/write/admin) token's name is never taken over;
//   - a revoked key stays dead, because someone killed it on purpose;
//   - a live key is not replaced under the running executor's feet;
//   - a key once issued for projects the caller does not run is not the
//     caller's to recycle.
func (s *svc) mayReissue(tx store.Tx, a Actor, existing *domain.Token, name string, now time.Time) error {
	if existing.Name != name {
		return domain.Invalid("name",
			fmt.Sprintf("a token named %s already exists; names are unique regardless of case", existing.Name),
			fmt.Sprintf("Pass exactly %q to re-issue that executor key, or choose a different name.", existing.Name))
	}
	if !existing.Scopes.OwnCardsOnly() {
		return domain.Invalid("name",
			fmt.Sprintf("the name %s belongs to a standing %s token, not to an executor key", name, joinScopes(existing.Scopes)),
			"Choose a different name for the executor.")
	}
	if !existing.Active() {
		return domain.Invalid("name",
			fmt.Sprintf("executor key %s was revoked", name),
			"A revoked key stays dead; choose a different name for the executor.")
	}
	if existing.ActiveAt(now) {
		return domain.Invalid("name",
			fmt.Sprintf("executor key %s is live until %s", name, existing.ExpiresAt.UTC().Format(time.RFC3339)),
			"Choose a different name, or re-issue this one after it expires.")
	}
	if a.IsAdmin() {
		return nil
	}
	for _, k := range existing.ProjectKeys {
		p, err := s.store.Projects().GetByKey(tx, k)
		if isNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if p.CoordinatorTokenID != a.TokenID {
			return domain.Forbidden(
				fmt.Sprintf("executor key %s was issued for project %s, which %s does not coordinate", name, p.Key, a.Name),
				"Choose a different name, or ask an admin to re-issue this one.")
		}
	}
	return nil
}

func joinScopes(ss domain.Scopes) string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, string(s))
	}
	return strings.Join(out, "+")
}
