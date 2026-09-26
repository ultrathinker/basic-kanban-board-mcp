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
	switch {
	case in.Renew && in.ParticipantOnly:
		return nil, domain.Invalid("renew", "renew and participant_only cannot be combined",
			"A participant without a key never expires, so there is nothing to renew; send one of the two.")
	case in.Renew:
		return s.executorKeyRenew(ctx, a, in)
	case in.ParticipantOnly:
		return s.participantWithoutKey(ctx, a, in)
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
	if err := isExecutorKey(existing, name); err != nil {
		return err
	}
	if !existing.Active() {
		return domain.Invalid("name",
			fmt.Sprintf("executor key %s was revoked", name),
			"A revoked key stays dead; choose a different name for the executor.")
	}
	if existing.ActiveAt(now) {
		return domain.Invalid("name",
			fmt.Sprintf("executor key %s is live until %s", name, existing.ExpiresAt.UTC().Format(time.RFC3339)),
			"Choose a different name, re-issue this one after it expires, or extend it with renew:true (the secret stays).")
	}
	return s.coordinatesKeyProjects(tx, a, existing, "re-issue")
}

// coordinatesKeyProjects is the authority over an existing executor key:
// an admin, or the coordinator of every project the key was issued for. A
// project deleted since is skipped — nobody coordinates it any more, and it
// must not pin the key to admins forever.
func (s *svc) coordinatesKeyProjects(tx store.Tx, a Actor, existing *domain.Token, verb string) error {
	if a.IsAdmin() {
		return nil
	}
	name := existing.Name
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
				fmt.Sprintf("Choose a different name, or ask an admin to %s this one.", verb))
		}
	}
	return nil
}

// isExecutorKey refuses to treat any other kind of token row as an executor
// key, and says which kind it is: re-issuing or renewing a standing token or
// a participant without a key on an executor's terms would silently change
// what that name is.
func isExecutorKey(existing *domain.Token, name string) error {
	if existing.HasNoSecret() {
		return domain.Invalid("name",
			fmt.Sprintf("the name %s belongs to a participant without a key, not to an executor key", name),
			"Choose a different name for the executor's key; a participant without a key has no secret to issue or renew.")
	}
	if !existing.Scopes.OwnCardsOnly() {
		return domain.Invalid("name",
			fmt.Sprintf("the name %s belongs to a standing %s token, not to an executor key", name, joinScopes(existing.Scopes)),
			"Choose a different name for the executor.")
	}
	return nil
}

// executorKeyRenew extends an executor key without touching its secret
// (KANB-68). Agents pause on a usage limit and resume hours later with the
// secret still in their launcher's config; a key that expired in between
// would lock them out, and a re-issue would mean a new secret to deliver.
// A renewal moves only the expiry, of a live or an already expired key, so
// the secret the agent holds keeps working and nothing new is shown to
// anyone. A revoked key stays dead, as it does for a re-issue.
func (s *svc) executorKeyRenew(ctx context.Context, a Actor, in ExecutorKeyIssueInput) (*ExecutorKeyIssueResult, error) {
	if len(in.ProjectKeys) > 0 {
		return nil, domain.Invalid("projects", "projects cannot be sent with renew: a renewal keeps the key's projects",
			"Omit projects to renew the key as it is, or issue a new key under another name for a different project list.")
	}
	ttl, err := domain.ExecutorKeyTTL(in.TTLSeconds)
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
		existing, err := s.store.Tokens().GetByName(tx, in.Name)
		if isNotFound(err) {
			return &domain.Error{Code: domain.CodeNotFound,
				Message:     fmt.Sprintf("no executor key named %q", in.Name),
				Remediation: "Check the name, or issue a new key without renew."}
		}
		if err != nil {
			return err
		}
		if existing.Name != in.Name {
			return domain.Invalid("name",
				fmt.Sprintf("the executor key is named %s, not %s", existing.Name, in.Name),
				fmt.Sprintf("Pass exactly %q to renew it.", existing.Name))
		}
		if err := isExecutorKey(existing, in.Name); err != nil {
			return err
		}
		if !existing.Active() {
			return domain.Invalid("name",
				fmt.Sprintf("executor key %s was revoked", in.Name),
				"A revoked key stays dead and cannot be renewed; issue a key under a different name.")
		}
		if err := s.coordinatesKeyProjects(tx, a, existing, "renew"); err != nil {
			return err
		}
		expires := now.Add(ttl)
		if err := s.store.Tokens().SetExpiry(tx, existing.ID, expires); err != nil {
			return err
		}
		// An expired key rejoins its projects' participant lists here, so
		// open boards hear about it the same way they hear about an issue.
		if err := s.emitKeyEvent(tx, &pending, a, existing.ProjectKeys, map[string]any{
			"name":       existing.Name,
			"token_id":   existing.ID,
			"expires_at": expires.UTC().Format(time.RFC3339),
			"renewed":    true,
		}); err != nil {
			return err
		}
		result = ExecutorKeyIssueResult{
			TokenID: existing.ID, Name: existing.Name, ProjectKeys: existing.ProjectKeys,
			ExpiresAt: expires, Renewed: true,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishAll(pending)
	return &result, nil
}

// participantWithoutKey makes a name a participant of the listed projects
// with no secret at all (KANB-68): someone cards are assigned to and
// reviewed by — the owner, a person, an agent run from outside — who never
// calls the board under that name. Before this the only way was a read
// token minted by the CLI with its secret thrown away, which leaves a
// working credential nobody knows about. Here no secret is ever generated,
// the stored hash is a marker no secret hashes to, and the row never
// expires.
//
// Calling it again for the same participant adds the new projects to it;
// projects it already has are left alone, so a repeat is harmless. The
// authority is the same as for a key: admin, or the coordinator of every
// listed project.
func (s *svc) participantWithoutKey(ctx context.Context, a Actor, in ExecutorKeyIssueInput) (*ExecutorKeyIssueResult, error) {
	if in.TTLSeconds != 0 {
		return nil, domain.Invalid("ttl_seconds", "ttl_seconds cannot be sent with participant_only: a participant without a key never expires",
			"Omit ttl_seconds.")
	}
	keys, err := executorKeyProjects(in.ProjectKeys)
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
		for _, k := range keys {
			p, err := s.resolveProject(tx, a, k)
			if err != nil {
				return err
			}
			if err := mayIssueFor(a, p); err != nil {
				return err
			}
		}

		var tok *domain.Token
		var added []string
		existing, err := s.store.Tokens().GetByName(tx, in.Name)
		switch {
		case isNotFound(err):
			id := newID()
			tok = &domain.Token{
				ID:   id,
				Name: in.Name,
				Hash: domain.NoSecretHash(id),
				// read is the least a token row can carry; it is never
				// exercised, because nothing can authenticate as this row.
				Scopes:      domain.Scopes{domain.ScopeRead},
				ProjectKeys: keys,
				CreatedAt:   now,
			}
			if err := s.store.Tokens().Create(tx, tok); err != nil {
				return err
			}
			added = keys
		case err != nil:
			return err
		default:
			if err := mayExtendParticipant(existing, in.Name); err != nil {
				return err
			}
			tok = existing
			// An empty list already means every project; there is nothing
			// to add to it.
			if len(existing.ProjectKeys) > 0 {
				var merged []string
				merged, added = domain.MergeProjectKeys(existing.ProjectKeys, keys)
				if len(added) > 0 {
					tok.ProjectKeys = merged
					if err := s.store.Tokens().Reissue(tx, tok); err != nil {
						return err
					}
				}
			}
		}

		if err := s.emitKeyEvent(tx, &pending, a, added, map[string]any{
			"name":             tok.Name,
			"token_id":         tok.ID,
			"participant_only": true,
		}); err != nil {
			return err
		}
		result = ExecutorKeyIssueResult{
			TokenID: tok.ID, Name: tok.Name, ProjectKeys: tok.ProjectKeys,
			ParticipantOnly: true, AddedProjects: added,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishAll(pending)
	return &result, nil
}

// mayExtendParticipant decides whether the row already under the name is a
// participant without a key that may take more projects. Any other kind of
// token is refused by name: turning a key or a standing token into a
// secretless participant would lock out whoever holds its secret.
func mayExtendParticipant(existing *domain.Token, name string) error {
	if existing.Name != name {
		return domain.Invalid("name",
			fmt.Sprintf("a token named %s already exists; names are unique regardless of case", existing.Name),
			fmt.Sprintf("Pass exactly %q to add projects to that participant, or choose a different name.", existing.Name))
	}
	if !existing.HasNoSecret() {
		kind := "a standing " + joinScopes(existing.Scopes) + " token"
		if existing.Scopes.OwnCardsOnly() {
			kind = "an executor key"
		}
		return domain.Invalid("name",
			fmt.Sprintf("the name %s belongs to %s, which has a secret; participant_only adds projects only to a participant without a key", name, kind),
			"Choose a different name for the participant.")
	}
	if !existing.Active() {
		return domain.Invalid("name",
			fmt.Sprintf("participant %s was revoked", name),
			"A revoked participant stays removed; choose a different name.")
	}
	return nil
}

// emitKeyEvent announces a participant-list change in each named project
// that still exists. The payload never carries a secret: an event is
// replayed to every subscriber.
func (s *svc) emitKeyEvent(tx store.Tx, pending *[]domain.Event, a Actor, projectKeys []string, payload map[string]any) error {
	for _, k := range projectKeys {
		p, err := s.store.Projects().GetByKey(tx, k)
		if isNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err := s.emit(tx, pending, a.Name, domain.EventExecutorKeyIssued, p.ID, nil, payload); err != nil {
			return err
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
