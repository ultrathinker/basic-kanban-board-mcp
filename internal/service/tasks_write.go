package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ---------------------------------------------------------------------------
// task_create — all-or-nothing (PLAN §6.4)
//
// One store.Write per call. Every check (refs, depth, parent chain, links,
// idempotency) happens inside the same transaction as the inserts, so a
// bad item rolls back the whole batch (store.Write sees the returned
// error) — no per-item compensation is needed for create.
//
// Per-item rollback is the problem for update/remove; for create the
// transaction itself is the rollback mechanism, and it is exact.
// ---------------------------------------------------------------------------

// createItemPlan is the per-item plan constructed during TaskCreate. It
// is shared between the validation, allocation and insertion phases; the
// type lives at package scope so the helper functions (resolveNewParent,
// resolveBatchTaskRef) can take a slice of plans as a parameter.
type createItemPlan struct {
	new        NewTask
	project    *domain.Project
	column     *domain.Column
	hash       string
	replayed   *domain.TaskView
	isReplay   bool
	validation *domain.Error
	// resolvedParentID / resolvedBlockerIDs are populated during the
	// resolution phase and consumed during insertion.
	resolvedParentID   string
	resolvedBlockerIDs []string
	// resolvedBlocks are the tasks this item blocks: id always, the loaded
	// task only when it already existed (its version moves, as task_link
	// moves it).
	resolvedBlocks []blockTarget
	inserted       *domain.Task // populated after Create
}

func (s *svc) TaskCreate(ctx context.Context, a Actor, in TaskCreateInput) (*TaskCreateResult, error) {
	if err := requireWrite(a, "create tasks"); err != nil {
		return nil, err
	}
	if len(in.Tasks) == 0 {
		return nil, domain.Invalid("tasks", "at least one task is required",
			"Pass 1..100 NewTask items.")
	}
	if len(in.Tasks) > domain.MaxBatchTasks {
		return nil, domain.Invalid("tasks",
			fmt.Sprintf("%d tasks, the limit is %d", len(in.Tasks), domain.MaxBatchTasks),
			"Split the request into multiple calls.")
	}
	if in.Restore {
		if err := requireAdmin(a, "restore tasks from an export"); err != nil {
			return nil, err
		}
	}
	ctx = store.WithActor(ctx, a.Name)

	var result TaskCreateResult
	var pending []domain.Event
	err := s.store.Write(ctx, func(tx store.Tx) error {
		now, err := tx.Now()
		if err != nil {
			return err
		}
		cc := newColumnCache(s, tx)
		pc := newProjectCache(s, tx)

		// ----- Acceptance guard (KANB-47) -----
		// A source_message turns the whole call into the atomic acceptance of
		// one command: the checks, the task inserts and the acceptance record
		// all share THIS transaction, so a failure anywhere rolls back
		// everything — a half-accepted command cannot exist.
		var guard *acceptanceGuard
		if in.SourceMessage != "" {
			guard, err = s.prepareAcceptance(tx, a, in)
			if err != nil {
				return err
			}
			if guard.already != nil {
				views, verr := s.acceptanceReplayViews(tx, cc, pc, guard.already, now)
				if verr != nil {
					return verr
				}
				result = TaskCreateResult{Tasks: views, Replayed: true, AlreadyAccepted: true}
				return nil
			}
		}

		// ----- Plan phase: per-item classification -----
		// Each item is either "new" (to be inserted), "replay" (idempotent
		// replay of a prior request, response is already on disk), or
		// "invalid" (a field validation that should fail the whole batch
		// — never returned from prepareCreate; we surface those later in
		// the "first error short-circuits" pattern).
		plans := make([]*createItemPlan, len(in.Tasks))
		seenRefs := make(map[string]int, len(in.Tasks))

		for i, nt := range in.Tasks {
			plan := &createItemPlan{new: nt}
			plans[i] = plan

			if nt.Ref != "" {
				if _, dup := seenRefs[nt.Ref]; dup {
					return domain.Invalid("ref",
						fmt.Sprintf("ref %q appears more than once in the batch", nt.Ref),
						"Ref names must be unique within a batch.")
				}
				seenRefs[nt.Ref] = i
			}

			if err := validateNewTask(nt); err != nil {
				plan.validation = domain.AsError(err)
				continue
			}

			if nt.IdempotencyKey != "" {
				rec, err := s.store.Idempotency().Get(tx, a.TokenID, nt.IdempotencyKey)
				if err != nil && !isNotFound(err) {
					return err
				}
				if rec != nil {
					hash, herr := hashNewTask(nt)
					if herr != nil {
						return herr
					}
					if rec.RequestHash != hash {
						return &domain.Error{
							Code:        domain.CodeIdempotencyMismatch,
							Message:     fmt.Sprintf("idempotency key %q was used with a different request body", nt.IdempotencyKey),
							Remediation: "Use a new idempotency key, or replay the original request to receive the original response.",
						}
					}
					tv, derr := decodeTaskView(rec.Response)
					if derr != nil {
						return derr
					}
					plan.replayed = &tv
					plan.isReplay = true
					continue
				}
				h, err := hashNewTask(nt)
				if err != nil {
					return err
				}
				plan.hash = h
			}
		}

		// Per-item: resolve project + column under access check, for "new" items.
		// The assignee is checked against the project's participants here, in
		// the same transaction as the insert, so a key issued or expiring
		// concurrently cannot slip between the check and the write.
		var pix *participantIndex
		for _, p := range plans {
			if p.isReplay || p.validation != nil {
				continue
			}
			proj, err := s.resolveProject(tx, a, p.new.ProjectKey)
			if err != nil {
				p.validation = domain.AsError(err)
				continue
			}
			p.project = proj
			col, err := s.resolveColumn(tx, proj, p.new.Column)
			if err != nil {
				p.validation = domain.AsError(err)
				continue
			}
			p.column = col
			if !in.Restore && p.new.Assignee != nil && *p.new.Assignee != "" {
				if pix == nil {
					if pix, err = s.buildParticipantIndex(tx, now); err != nil {
						return err
					}
				}
				if err := pix.checkAssignee(proj, "", *p.new.Assignee); err != nil {
					p.validation = domain.AsError(err)
					continue
				}
			}
		}

		// Any validation error short-circuits the whole batch — store.Write
		// will see the returned error and roll back atomically.
		for _, p := range plans {
			if p.validation != nil {
				return p.validation
			}
		}

		// ----- Key allocation phase: assign ID + Key + Rank to each new item.
		// Done in input order so refs can point forward to items that come
		// later in the same batch.
		//
		// The map holds the whole plan, not just the allocated id: resolving a
		// ref now also has to compare the two items' projects, and the depth
		// rule needs the referenced item's own Parent.
		refToPlan := make(map[string]*createItemPlan, len(plans))
		for _, p := range plans {
			if p.isReplay {
				continue
			}
			seq, err := s.store.Projects().NextTaskSeq(tx, p.project.ID)
			if err != nil {
				return err
			}
			key := domain.TaskKey(p.project.Key, seq)
			before, after, err := s.store.Tasks().NeighbourRanks(tx, p.column.ID, store.RankBottom)
			if err != nil {
				return err
			}
			t := &domain.Task{
				ID:         newID(),
				Key:        key,
				ProjectID:  p.project.ID,
				ColumnID:   p.column.ID,
				Rank:       midRank(before, after),
				Title:      p.new.Title,
				Body:       p.new.Body,
				Type:       p.new.Type,
				Priority:   p.new.Priority,
				Estimate:   p.new.Estimate,
				Actual:     p.new.Actual,
				Tags:       append([]string(nil), p.new.Tags...),
				Assignee:   p.new.Assignee,
				Reviewer:   p.new.Reviewer,
				Outcome:    outcomeOrOpen(p.new.Outcome),
				Conclusion: p.new.Conclusion,
				Acceptance: buildAcceptance(p.new.Acceptance),
				DueAt:      p.new.DueAt,
				Metadata:   p.new.Metadata,
				CreatedBy:  a.Name,
				UpdatedBy:  a.Name,
			}
			p.inserted = t
			if p.new.Ref != "" {
				refToPlan[p.new.Ref] = p
			}
		}

		// ----- Parent / BlockedBy resolution (against the in-memory plan;
		// tasks are inserted after this phase so in-batch refs use the
		// already-allocated IDs).
		for _, p := range plans {
			if p.isReplay || p.inserted == nil {
				continue
			}
			if p.new.Parent != "" {
				pid, err := s.resolveNewParent(tx, a, p, p.new.Parent, refToPlan)
				if err != nil {
					return err
				}
				p.inserted.ParentID = &pid
			}
			if len(p.new.BlockedBy) > 0 {
				ids := make([]string, 0, len(p.new.BlockedBy))
				for _, raw := range p.new.BlockedBy {
					id, _, err := s.resolveBatchTaskRef(tx, a, p, "blocked_by", raw, refToPlan)
					if err != nil {
						return err
					}
					ids = append(ids, id)
				}
				p.resolvedBlockerIDs = ids
			}
			for _, raw := range p.new.Blocks {
				id, existing, err := s.resolveBatchTaskRef(tx, a, p, "blocks", raw, refToPlan)
				if err != nil {
					return err
				}
				p.resolvedBlocks = append(p.resolvedBlocks, blockTarget{id: id, existing: existing})
			}
		}

		// ----- Insert tasks -----
		for _, p := range plans {
			if p.inserted == nil {
				continue
			}
			if err := s.store.Tasks().Create(tx, p.inserted); err != nil {
				return err
			}
		}
		for _, p := range plans {
			if p.inserted == nil {
				continue
			}
			t := p.inserted
			if err := s.emit(tx, &pending, a.Name, domain.EventTaskCreated, t.ProjectID, &t.ID,
				map[string]any{"key": t.Key}); err != nil {
				return err
			}
		}

		// ----- Insert links (after tasks so cycle detection has them all) -----
		// blocked_by and blocks are one edge seen from two ends, so a batch
		// that states the same edge both ways must add it once.
		type edge struct{ blocker, blocked string }
		added := map[edge]bool{}
		for _, p := range plans {
			if p.inserted == nil {
				continue
			}
			t := p.inserted
			for _, bt := range p.resolvedBlocks {
				e := edge{t.ID, bt.id}
				if added[e] {
					continue
				}
				added[e] = true
				if err := s.store.Links().Add(tx, &domain.Link{
					BlockerID: t.ID,
					BlockedID: bt.id,
					Type:      domain.LinkBlocks,
					CreatedBy: a.Name,
				}); err != nil {
					return err
				}
				if bt.existing != nil {
					if err := s.bumpVersion(tx, bt.existing, a.Name); err != nil {
						return err
					}
				}
				if err := s.emit(tx, &pending, a.Name, domain.EventLinkAdded, t.ProjectID, &bt.id,
					map[string]any{"blocker_id": t.ID, "blocked_id": bt.id}); err != nil {
					return err
				}
			}
		}
		for _, p := range plans {
			if p.inserted == nil {
				continue
			}
			t := p.inserted
			for _, blockerID := range p.resolvedBlockerIDs {
				e := edge{blockerID, t.ID}
				if added[e] {
					continue
				}
				added[e] = true
				if err := s.store.Links().Add(tx, &domain.Link{
					BlockerID: blockerID,
					BlockedID: t.ID,
					Type:      domain.LinkBlocks,
					CreatedBy: a.Name,
				}); err != nil {
					return err
				}
				if err := s.emit(tx, &pending, a.Name, domain.EventLinkAdded, t.ProjectID, &t.ID,
					map[string]any{"blocker_id": blockerID, "blocked_id": t.ID}); err != nil {
					return err
				}
			}
		}

		// ----- Hydrate views + write idempotency records -----
		outTasks := make([]domain.TaskView, 0, len(plans))
		replayed := false
		for _, p := range plans {
			if p.isReplay {
				replayed = true
				outTasks = append(outTasks, *p.replayed)
				continue
			}
			t := p.inserted
			tv, err := s.hydrateView(tx, cc, pc, t, now, hydrateOpts{})
			if err != nil {
				return err
			}
			if p.new.IdempotencyKey != "" {
				encoded, err := encodeTaskView(tv)
				if err != nil {
					return err
				}
				if err := s.store.Idempotency().Put(tx, &domain.IdempotencyRecord{
					TokenID:     a.TokenID,
					Key:         p.new.IdempotencyKey,
					RequestHash: p.hash,
					Response:    encoded,
				}); err != nil {
					return err
				}
			}
			outTasks = append(outTasks, tv)
		}

		// The acceptance record joins the batch in the same transaction: by
		// the time this runs, every task row exists, so the link and the
		// tasks commit or roll back as one unit.
		if guard != nil {
			if err := s.recordAcceptance(tx, a, guard, plans, now); err != nil {
				return err
			}
		}

		result = TaskCreateResult{Tasks: outTasks, Replayed: replayed}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishAll(pending)
	return &result, nil
}

// ---------------------------------------------------------------------------
// source_message acceptance (KANB-47)
//
// Accepting a command is the one write in the product where "did it happen?"
// must survive a crash between two writes. The link "command -> tasks ->
// acceptor" is a row in command_acceptances, written in the same transaction
// as the tasks it names; the UNIQUE(message_id) constraint is the rule "a
// command is accepted once per project", enforced by the database rather than
// by check-then-insert discipline — two racing acceptances cannot both commit.
// ---------------------------------------------------------------------------

// acceptanceGuard carries one source_message acceptance through TaskCreate's
// transaction. already is non-nil exactly when the command was accepted
// before: the call then replays the stored outcome instead of creating.
type acceptanceGuard struct {
	message *domain.ChatMessage
	project *domain.Project
	// hash fingerprints the batch content this acceptance covers, so a
	// retried acceptance with different content is a loud conflict.
	hash    string
	already *domain.CommandAcceptance
}

// prepareAcceptance validates a source_message against every acceptance rule
// and decides replay-vs-create. All of it runs inside the caller's write
// transaction, so nothing here can race the checks that follow it.
//
// The rules (KANB-47 §13.4):
//  1. the message exists and is a kind:command — a question needs a reply
//     (reply_to), not a task, and a fictitious card for a pure question is
//     what this refusal prevents;
//  2. the accepting actor is the command's resolved_executor — compared by
//     tokens.id, never by the author signature, which the caller controls;
//  3. the batch belongs to the command's project;
//  4. a command already accepted replays its stored task_keys to the SAME
//     acceptor, refuses any other token, and refuses incompatible content.
func (s *svc) prepareAcceptance(tx store.Tx, a Actor, in TaskCreateInput) (*acceptanceGuard, error) {
	msg, err := s.store.Chat().Get(tx, strings.TrimSpace(in.SourceMessage))
	if isNotFound(err) {
		return nil, domain.NotFound("source_message", in.SourceMessage)
	}
	if err != nil {
		return nil, err
	}
	if msg.Kind != domain.MessageCommand {
		return nil, domain.Invalid("source_message",
			fmt.Sprintf("message %s is a %s, not a command", msg.ID, msg.Kind),
			"Only a kind:command message can be accepted into tasks; answer a question with project_post(reply_to: ...) instead.")
	}
	project, err := s.store.Projects().GetByID(tx, msg.ProjectID)
	if err != nil {
		return nil, err
	}
	// The actor must be able to see the project the command lives in at all.
	if _, err := s.resolveProject(tx, a, project.Key); err != nil {
		return nil, err
	}
	for i, nt := range in.Tasks {
		canonical, err := domain.ValidateProjectKey(nt.ProjectKey)
		if err != nil {
			return nil, err
		}
		if canonical != project.Key {
			return nil, domain.Invalid("source_message",
				fmt.Sprintf("the command lives in project %s but task %d targets %s", project.Key, i+1, canonical),
				"An accepted batch is created in the command's own project.")
		}
	}

	if acc, err := s.store.Acceptances().GetByMessage(tx, msg.ID); err == nil && acc != nil {
		if acc.AcceptedByTokenID != a.TokenID {
			by, _ := s.store.Tokens().GetByID(tx, acc.AcceptedByTokenID)
			who := acc.AcceptedByTokenID
			if by != nil {
				who = by.Name
			}
			return nil, domain.Forbidden(
				fmt.Sprintf("command %s was already accepted by %s", msg.ID, who),
				"Only the accepting agent may replay an acceptance; create ordinary tasks for your own work instead.")
		}
		hash, err := hashAcceptedBatch(msg.ID, in.Tasks)
		if err != nil {
			return nil, err
		}
		if acc.RequestHash != hash {
			return nil, &domain.Error{
				Code: domain.CodeIdempotencyMismatch,
				Message: fmt.Sprintf(
					"command %s was already accepted with different content; its original tasks (%s) are unchanged",
					msg.ID, strings.Join(acc.TaskKeys, ", ")),
				Remediation: "Replay the exact original batch to receive the original tasks, or post a new command for the changed work.",
			}
		}
		return &acceptanceGuard{message: msg, project: project, hash: hash, already: acc}, nil
	} else if err != nil && !isNotFound(err) {
		return nil, err
	}

	if msg.ResolvedExecutor == "" {
		return nil, domain.Invalid("source_message",
			fmt.Sprintf("command %s has no resolved executor to accept it", msg.ID),
			"The command must be addressed to exactly one executor; ask the sender to repost with a recipient.")
	}
	if a.TokenID != msg.ResolvedExecutor {
		executor, _ := s.store.Tokens().GetByID(tx, msg.ResolvedExecutor)
		who := msg.ResolvedExecutor
		if executor != nil {
			who = executor.Name
		}
		return nil, domain.Forbidden(
			fmt.Sprintf("only %s may accept command %s", who, msg.ID),
			"This command is addressed to its resolved executor; ask them to accept it, or ask for a command addressed to you.")
	}
	hash, err := hashAcceptedBatch(msg.ID, in.Tasks)
	if err != nil {
		return nil, err
	}
	return &acceptanceGuard{message: msg, project: project, hash: hash}, nil
}

// recordAcceptance writes the durable link after the batch's rows exist, in
// the same transaction. plans (not outTasks) is the source for ids and keys
// so per-item idempotency replays are named too: the acceptance must cover
// every task the command produced, however each one came to exist.
func (s *svc) recordAcceptance(tx store.Tx, a Actor, guard *acceptanceGuard, plans []*createItemPlan, now time.Time) error {
	ids := make([]string, 0, len(plans))
	keys := make([]string, 0, len(plans))
	for _, p := range plans {
		switch {
		case p.inserted != nil:
			ids = append(ids, p.inserted.ID)
			keys = append(keys, p.inserted.Key)
		case p.isReplay && p.replayed != nil:
			ids = append(ids, p.replayed.ID)
			keys = append(keys, p.replayed.Key)
		default:
			return fmt.Errorf("acceptance: plan item produced no task")
		}
	}
	return s.store.Acceptances().Put(tx, &domain.CommandAcceptance{
		ID:                newID(),
		MessageID:         guard.message.ID,
		ProjectID:         guard.project.ID,
		AcceptedByTokenID: a.TokenID,
		TaskIDs:           ids,
		TaskKeys:          keys,
		RequestHash:       guard.hash,
		CreatedAt:         now,
	})
}

// acceptanceReplayViews materialises the CURRENT views of an acceptance's
// stored task keys. The keys are the durable answer to "which tasks came of
// this command"; their state is read live, so a replay shows where the work
// stands now, not where it stood at acceptance time.
func (s *svc) acceptanceReplayViews(tx store.Tx, cc *columnCache, pc *projectCache, acc *domain.CommandAcceptance, now time.Time) ([]domain.TaskView, error) {
	byKey, err := s.store.Tasks().GetManyByKeys(tx, acc.TaskKeys)
	if err != nil {
		return nil, err
	}
	out := make([]domain.TaskView, 0, len(acc.TaskKeys))
	for _, key := range acc.TaskKeys {
		t, ok := byKey[strings.ToUpper(key)]
		if !ok {
			return nil, domain.NotFound("task", key)
		}
		tv, err := s.hydrateView(tx, cc, pc, t, now, hydrateOpts{})
		if err != nil {
			return nil, err
		}
		out = append(out, tv)
	}
	return out, nil
}

// acceptanceItemContent is the acceptance replay's projection of one
// NewTask item: EVERY field the caller sent. It deliberately differs from
// idemContent — the 24h task_create replay projection — which ignores Ref
// and IdempotencyKey because there the key is the identity and a differing
// ref must still count as the same retry. For an acceptance the COMMAND
// message is the identity and the batch is the content, so a replay that
// differs in reviewer, recorded actuals, client ref handles, per-item
// idempotency keys, or any other field is a different request and must
// conflict rather than silently reuse the original batch with fields
// dropped on the floor.
//
// Canonicalization mirrors hashNewTask where the semantics agree: Metadata
// keys sorted, set-like slices (Tags, BlockedBy) sorted, DueAt normalised
// to UTC. Acceptance criteria keep their order — acceptance_check rows are
// index-based, so [a, b] and [b, a] create different checks and are
// different content.
type acceptanceItemContent struct {
	ProjectKey     string          `json:"project"`
	Title          string          `json:"title"`
	Body           string          `json:"body"`
	Type           domain.Type     `json:"type"`
	Priority       domain.Priority `json:"priority"`
	Estimate       *float64        `json:"estimate,omitempty"`
	Actual         *float64        `json:"actual,omitempty"`
	Tags           []string        `json:"tags"`
	Assignee       *string         `json:"assignee,omitempty"`
	Reviewer       *string         `json:"reviewer,omitempty"`
	Column         string          `json:"column"`
	Parent         string          `json:"parent"`
	BlockedBy      []string        `json:"blocked_by"`
	Acceptance     []string        `json:"acceptance"`
	DueAt          *time.Time      `json:"due_at,omitempty"`
	Metadata       map[string]any  `json:"metadata"`
	Ref            string          `json:"ref"`
	IdempotencyKey string          `json:"idempotency_key"`
}

// hashAcceptanceItem projects one item into acceptanceItemContent and hashes
// the canonical JSON form.
func hashAcceptanceItem(n NewTask) (string, error) {
	item := acceptanceItemContent{
		ProjectKey:     domain.NormalizeProjectKey(n.ProjectKey),
		Title:          n.Title,
		Body:           n.Body,
		Type:           n.Type,
		Priority:       n.Priority,
		Estimate:       n.Estimate,
		Actual:         n.Actual,
		Assignee:       n.Assignee,
		Reviewer:       n.Reviewer,
		Column:         n.Column,
		Parent:         n.Parent,
		Metadata:       sortedMetadata(n.Metadata),
		Ref:            n.Ref,
		IdempotencyKey: n.IdempotencyKey,
	}
	if n.Tags != nil {
		item.Tags = append([]string(nil), n.Tags...)
		sort.Strings(item.Tags)
	}
	if n.BlockedBy != nil {
		item.BlockedBy = append([]string(nil), n.BlockedBy...)
		sort.Strings(item.BlockedBy)
	}
	if n.Acceptance != nil {
		item.Acceptance = append([]string(nil), n.Acceptance...)
	}
	if n.DueAt != nil {
		t := n.DueAt.UTC()
		item.DueAt = &t
	}
	enc, err := json.Marshal(item)
	if err != nil {
		return "", fmt.Errorf("acceptance: hash batch item: %w", err)
	}
	sum := sha256.Sum256(enc)
	return hex.EncodeToString(sum[:]), nil
}

// hashAcceptedBatch fingerprints the content of an accepted batch: the
// command message id plus every item's FULL sent content, in order. Two
// acceptances of one command with equal fingerprints are the same
// acceptance; anything else — a changed reviewer, changed actuals, changed
// refs or per-item idempotency keys, reordered acceptance checks, a
// different batch — is different content and must conflict, not silently
// extend and not silently reuse.
func hashAcceptedBatch(messageID string, tasks []NewTask) (string, error) {
	h := sha256.New()
	h.Write([]byte(messageID))
	for _, nt := range tasks {
		item, err := hashAcceptanceItem(nt)
		if err != nil {
			return "", err
		}
		h.Write([]byte{0})
		h.Write([]byte(item))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// validateNewTask runs every pure-field check that does not need the
// database. Called per item before any DB work.
func validateNewTask(n NewTask) error {
	if _, err := domain.ValidateProjectKey(n.ProjectKey); err != nil {
		return err
	}
	if _, err := domain.ValidateTitle(n.Title); err != nil {
		return err
	}
	if err := domain.ValidateBody(n.Body); err != nil {
		return err
	}
	if n.Type == "" {
		n.Type = domain.TypeTask
	}
	if !n.Type.Valid() {
		return domain.Invalid("type", fmt.Sprintf("type %q is invalid", n.Type),
			"Use one of task, bug, feat, chore, doc, perf, research.")
	}
	if !n.Priority.Valid() {
		return domain.Invalid("priority", fmt.Sprintf("priority %d is invalid", n.Priority),
			"Use 0..4 (none..critical).")
	}
	if _, err := domain.NormalizeTags(n.Tags); err != nil {
		return err
	}
	if n.Assignee != nil {
		if err := domain.ValidateActorName("assignee", *n.Assignee); err != nil {
			return err
		}
	}
	if n.Reviewer != nil {
		if err := domain.ValidateActorName("reviewer", *n.Reviewer); err != nil {
			return err
		}
	}
	if err := domain.ValidateAcceptance(buildAcceptance(n.Acceptance)); err != nil {
		return err
	}
	if n.Outcome != nil && !n.Outcome.Valid() {
		return domain.Invalid("outcome", fmt.Sprintf("outcome %q is invalid", *n.Outcome),
			"Use one of: open, holds, refuted, superseded, moot.")
	}
	if err := domain.ValidateConclusion(n.Conclusion); err != nil {
		return err
	}
	return nil
}

// buildAcceptance lifts a []string into []AcceptanceItem.
func buildAcceptance(in []string) []domain.AcceptanceItem {
	if len(in) == 0 {
		return nil
	}
	out := make([]domain.AcceptanceItem, len(in))
	for i, s := range in {
		out[i] = domain.AcceptanceItem{Text: s, Done: false}
	}
	return out
}

// resolveNewParent resolves Parent to a real task ID. The depth check
// (MaxSubtaskDepth=2) is enforced HERE because TaskRepo.Create does not
// validate parent depth — only Update does, and only on reparent.
//
// A literal key goes through resolveRelatedTask, so a parent the actor
// cannot read is refused and a parent in another project is refused; the
// in-batch @ref branch compares the two plans' projects for the same reason.
func (s *svc) resolveNewParent(tx store.Tx, a Actor, owner *createItemPlan, raw string, refToPlan map[string]*createItemPlan) (string, error) {
	if strings.HasPrefix(raw, "@") {
		ref := strings.TrimPrefix(raw, "@")
		target, ok := refToPlan[ref]
		if !ok {
			return "", domain.Invalid("parent",
				fmt.Sprintf("parent ref %q does not match any item in this batch", raw),
				"Use @name pointing at a ref in the same batch, or a literal task key.")
		}
		if target.project.ID != owner.project.ID {
			return "", crossProjectEdge("parent", owner.inserted.Key, target.inserted.Key)
		}
		// Depth check: the referenced in-batch item must itself be a
		// top-level task (its own Parent is empty). Read it off the plan
		// rather than the database — the row hasn't been inserted yet.
		if target.new.Parent != "" {
			return "", domain.Invalid("parent",
				"parent ref is itself a subtask (max depth 2); subtasks may not have children",
				"Pick a top-level task as the parent; subtasks are leaves.")
		}
		return target.inserted.ID, nil
	}
	t, err := s.resolveRelatedTask(tx, a, "parent", raw, owner.inserted.Key, owner.project.ID)
	if err != nil {
		return "", err
	}
	if t.ParentID != nil {
		return "", domain.Invalid("parent",
			"parent is already a subtask (max depth 2); subtasks may not have children",
			"Pick a top-level task as the parent; subtasks are leaves.")
	}
	return t.ID, nil
}

// resolveBatchTaskRef resolves one blocked_by or blocks entry of a
// task_create item to a task ID, under the same access and same-project
// rules as resolveNewParent. "@ref" points at another item of the batch;
// anything else is an existing key, and then the loaded task is returned
// too so its version can move. field names the input in errors.
func (s *svc) resolveBatchTaskRef(tx store.Tx, a Actor, owner *createItemPlan, field, raw string, refToPlan map[string]*createItemPlan) (string, *domain.Task, error) {
	if strings.HasPrefix(raw, "@") {
		ref := strings.TrimPrefix(raw, "@")
		target, ok := refToPlan[ref]
		if !ok {
			return "", nil, domain.Invalid(field,
				fmt.Sprintf("%s ref %q does not match any item in this batch", field, raw),
				"Use @name pointing at a ref in the same batch, or a literal task key.")
		}
		if target == owner {
			return "", nil, domain.Invalid(field,
				fmt.Sprintf("%s ref %q points at the item itself", field, raw),
				"A task cannot block itself.")
		}
		if target.project.ID != owner.project.ID {
			return "", nil, crossProjectEdge(field, owner.inserted.Key, target.inserted.Key)
		}
		return target.inserted.ID, nil, nil
	}
	t, err := s.resolveRelatedTask(tx, a, field, raw, owner.inserted.Key, owner.project.ID)
	if err != nil {
		return "", nil, err
	}
	return t.ID, t, nil
}

// blockTarget is one resolved `blocks` entry of a task_create item.
type blockTarget struct {
	id       string
	existing *domain.Task // nil for an item of the same batch
}

func outcomeOrOpen(o *domain.Outcome) domain.Outcome {
	if o == nil {
		return domain.OutcomeOpen
	}
	return *o
}

// ---------------------------------------------------------------------------
// task_update — per-item results by default, atomic on request (PLAN §6.5)
//
// Atomic semantics (PLAN §6.5):
//   - Atomic == true  → the first item error returned from the closure
//     rolls back the entire transaction. We never apply a partial batch.
//   - Atomic == false → per-item results. An item that fails validation
//     or a mutating repo call is recorded in that item's ItemResult.Err
//     and the loop continues with the next patch.
//
// Every item runs inside its own nested unit of work (tx.Nested, a SQLite
// SAVEPOINT), so "the failed item changed nothing" is enforced by the
// database instead of by the discipline of validating everything before
// writing anything. That discipline is still worth keeping — a check that
// runs before the first write produces a better error — but it is no longer
// load-bearing, and an item that writes and then fails halfway (a move
// followed by an out-of-range acceptance index, say) can no longer commit
// half of itself under an ok:false result.
//
// Each patch is validated and applied before the next one is looked at.
// Validating the whole batch first and only then writing reads more
// tidily, but it turns every check into a time-of-check/time-of-use race
// against the batch itself: two patches moving into the same column both
// read the pre-batch occupancy, both pass a WIP limit with room for one,
// and both land. PLAN §11 invariant 6 ("WIP / dependency / strict_done
// enforced inside the transaction, no TOCTOU") forbids exactly that, so
// patch N is checked against the state patches 1..N-1 have already
// written into this same transaction.
// ---------------------------------------------------------------------------

// runItem applies one item of a per-item batch inside its own nested unit of
// work and returns (itemFailure, fatal): itemFailure is that item's
// ItemResult.Err, fatal aborts the whole call.
//
// The database half of the rollback is tx.Nested's job. The half it cannot do
// is the events: s.emit stages them in the pending slice, in this process,
// and they are published only after the outer transaction commits — so a
// rolled-back item whose events are left staged would announce a move that
// never happened to every SSE subscriber. Recording the length before the
// item and truncating on failure is the only place that can be fixed, which
// is why store.Tx.Nested's contract says so explicitly.
//
// Two failures are deliberately NOT per-item outcomes:
//   - the savepoint machinery itself failing, after which the transaction's
//     state is no longer something we can reason about item by item;
//   - an error that is not a *domain.Error, i.e. an I/O or driver failure
//     rather than a refusal. Reporting one as ok:false with no code and no
//     remediation is exactly the silent downgrade AGENTS.md forbids.
func runItem(tx store.Tx, pending *[]domain.Event, fn func(store.Tx) error) (itemFailure *domain.Error, fatal error) {
	staged := len(*pending)
	var item *nestedItemError
	nestErr := tx.Nested(func(itx store.Tx) error {
		if err := fn(itx); err != nil {
			// Wrapped so the error identity survives: Nested returns the
			// item's own error unchanged when the rollback worked, and joins
			// its own failure to it when it did not.
			item = &nestedItemError{err: err}
			return item
		}
		return nil
	})
	if nestErr == nil {
		return nil, nil
	}
	*pending = (*pending)[:staged]
	if item == nil || nestErr != error(item) {
		return nil, nestErr
	}
	de := domain.AsError(item.err)
	if de == nil {
		return nil, item.err
	}
	return de, nil
}

// nestedItemError tags the error a batch item returned, so runItem can tell
// "this item refused" from "the unit of work itself broke".
type nestedItemError struct{ err error }

func (e *nestedItemError) Error() string { return e.err.Error() }
func (e *nestedItemError) Unwrap() error { return e.err }

func (s *svc) TaskUpdate(ctx context.Context, a Actor, in TaskUpdateInput) (*TaskUpdateResult, error) {
	if err := requireCardWrite(a); err != nil {
		return nil, err
	}
	if len(in.Patches) == 0 {
		return nil, domain.Invalid("patches", "at least one patch is required",
			"Pass 1..100 TaskPatch items.")
	}
	if len(in.Patches) > domain.MaxBatchTasks {
		return nil, domain.Invalid("patches",
			fmt.Sprintf("%d patches, the limit is %d", len(in.Patches), domain.MaxBatchTasks),
			"Split the request into multiple calls.")
	}
	ctx = store.WithActor(ctx, a.Name)

	results := make([]ItemResult, len(in.Patches))
	var pending []domain.Event

	err := s.store.Write(ctx, func(tx store.Tx) error {
		now, err := tx.Now()
		if err != nil {
			return err
		}
		cc := newColumnCache(s, tx)
		pc := newProjectCache(s, tx)

		for i, patch := range in.Patches {
			results[i].Key = patch.Key
			if patch.Key == "" {
				results[i].Err = domain.Invalid("key", "key is required", "Pass the task key.")
				if in.Atomic {
					return results[i].Err
				}
				continue
			}
			var tv *domain.TaskView
			itemErr, fatal := runItem(tx, &pending, func(itx store.Tx) error {
				// prepareUpdate re-reads the task, its project and the target
				// column's occupancy every time, so the checks below see the
				// writes made by the earlier patches of this same batch.
				prep, err := s.prepareUpdate(itx, a, patch)
				if err != nil {
					return err
				}
				results[i].Key = prep.task.Key
				tv, err = s.applyUpdate(itx, prep, patch, a, &pending, cc.bind(itx), pc.bind(itx), now)
				return err
			})
			if fatal != nil {
				return fatal
			}
			if itemErr != nil {
				results[i].Err = itemErr
				if in.Atomic {
					return itemErr
				}
				continue
			}
			results[i].OK = true
			results[i].Task = tv
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishAll(pending)
	return &TaskUpdateResult{Items: results}, nil
}

// prepared is the per-patch snapshot built during prepareUpdate.
type prepared struct {
	task    *domain.Task
	project *domain.Project
	column  *domain.Column
	updated *domain.TaskView
}

// isReplacementPatch reports whether a TaskPatch contains any field that
// requires if_version (PLAN §6.5: "if_version is required for any
// replacement-style field"). Note-only and TagsAdd/TagsRemove are exempt.
func isReplacementPatch(p TaskPatch) bool {
	return p.Title != nil || p.Body != nil || p.Type != nil || p.Priority != nil ||
		p.Estimate.Set || p.Estimate.Clear ||
		p.Actual.Set || p.Actual.Clear ||
		p.Assignee.Set || p.Assignee.Clear ||
		p.Reviewer.Set || p.Reviewer.Clear ||
		p.Outcome != nil || p.Conclusion != nil ||
		p.BodyAppend != nil ||
		p.DueAt.Set || p.DueAt.Clear ||
		p.Tags != nil ||
		p.Column != "" ||
		p.Parent.Set || p.Parent.Clear ||
		p.Acceptance != nil || len(p.AcceptanceCheck) > 0 || len(p.AcceptanceAdd) > 0 ||
		len(p.MetadataMerge) > 0
}

// prepareUpdate validates one patch and returns the snapshot used by
// applyUpdate. All check-time errors are returned here; applyUpdate only
// runs the actual repo writes.
func (s *svc) prepareUpdate(tx store.Tx, a Actor, patch TaskPatch) (*prepared, error) {
	task, err := s.resolveTask(tx, a, patch.Key)
	if err != nil {
		return nil, err
	}
	proj, err := s.store.Projects().GetByID(tx, task.ProjectID)
	if err != nil {
		return nil, err
	}
	if err := s.validatePatchShape(patch); err != nil {
		return nil, err
	}
	if a.OwnCardsOnly() {
		if err := domain.ExecutorOwns(a.Name, task); err != nil {
			return nil, err
		}
		if err := domain.ExecutorFieldsRefused(a.Name, task.Key, executorRefusedFields(patch)); err != nil {
			return nil, err
		}
	}
	if err := requireForce(a, patch.Force, patch.Reason); err != nil {
		return nil, err
	}
	if patch.IfVersion == nil && isReplacementPatch(patch) {
		return nil, domain.Invalid("if_version",
			"if_version is required when a replacement-style field is set",
			"Read the task's current version with task_get and retry with if_version=<n>.")
	}
	if patch.IfVersion != nil && task.Version != *patch.IfVersion {
		now, err := tx.Now()
		if err != nil {
			return nil, err
		}
		tv, err := s.hydrateView(tx, newColumnCache(s, tx), newProjectCache(s, tx), task, now, hydrateOpts{})
		if err != nil {
			return nil, err
		}
		return nil, domain.Conflict(&tv, *patch.IfVersion, task.Version)
	}

	prep := &prepared{task: task, project: proj}

	// Only a NEW value is checked: a card whose stored assignee predates
	// this rule, or names a key that has since expired, keeps working for
	// every other edit, and re-sending the same value is not a change.
	if patch.Assignee.Set && patch.Assignee.Value != "" &&
		(task.Assignee == nil || *task.Assignee != patch.Assignee.Value) {
		now, err := tx.Now()
		if err != nil {
			return nil, err
		}
		pix, err := s.buildParticipantIndex(tx, now)
		if err != nil {
			return nil, err
		}
		if err := pix.checkAssignee(proj, task.Key, patch.Assignee.Value); err != nil {
			return nil, err
		}
	}

	if patch.Column != "" {
		dst, err := s.resolveColumn(tx, proj, patch.Column)
		if err != nil {
			return nil, err
		}
		prep.column = dst
		fromCol, err := s.store.Columns().GetByID(tx, task.ColumnID)
		if err != nil {
			return nil, err
		}
		if a.OwnCardsOnly() {
			if err := domain.ExecutorMayMove(a.Name, task.Key, *fromCol, *dst); err != nil {
				return nil, err
			}
		}
		openBlocks, err := s.store.Links().OpenBlockers(tx, task.ID)
		if err != nil {
			return nil, err
		}
		var acceptanceRemaining int
		if proj.StrictDone {
			// Judge the checklist THIS patch produces, not the stored one: an
			// atomic "acceptance_check the last item + column:Done" must pass.
			projected, err := projectedAcceptance(task.Acceptance, patch)
			if err != nil {
				return nil, err
			}
			for _, it := range projected {
				if !it.Done {
					acceptanceRemaining++
				}
			}
		}
		if err := domain.CheckMove(domain.MoveCheck{
			TaskKey:             task.Key,
			From:                *fromCol,
			To:                  *dst,
			OpenBlocks:          openBlocks,
			AcceptanceRemaining: acceptanceRemaining,
			EnforceDependencies: proj.EnforceDependencies,
			StrictDone:          proj.StrictDone,
			Force:               patch.Force,
			ForceReason:         patch.Reason,
			ActorIsAdmin:        a.IsAdmin(),
		}); err != nil {
			return nil, err
		}
	}
	return prep, nil
}

// executorRefusedFields lists the fields of a patch an executor key may not
// set (KANB-60). The allowance is a whitelist — note, column and rank —
// because a blacklist would silently admit every field added to TaskPatch
// after it was written. Assignee and reviewer are who does and who checks
// the work; outcome, acceptance and focus are the judgement the reviewer
// makes; force bypasses the very rules the key exists under.
func executorRefusedFields(p TaskPatch) []string {
	var out []string
	add := func(on bool, name string) {
		if on {
			out = append(out, name)
		}
	}
	add(p.Title != nil, "title")
	add(p.Body != nil, "body")
	add(p.BodyAppend != nil, "body_append")
	add(p.Type != nil, "type")
	add(p.Priority != nil, "priority")
	add(p.Estimate.Set || p.Estimate.Clear, "estimate")
	add(p.Actual.Set || p.Actual.Clear, "actual")
	add(p.Assignee.Set || p.Assignee.Clear, "assignee")
	add(p.Reviewer.Set || p.Reviewer.Clear, "reviewer")
	add(p.Outcome != nil, "outcome")
	add(p.Conclusion != nil, "conclusion")
	add(p.DueAt.Set || p.DueAt.Clear, "due_at")
	add(p.Tags != nil, "tags")
	add(len(p.TagsAdd) > 0, "tags_add")
	add(len(p.TagsRemove) > 0, "tags_remove")
	add(p.Parent.Set || p.Parent.Clear, "parent")
	add(p.Acceptance != nil, "acceptance")
	add(len(p.AcceptanceCheck) > 0, "acceptance_check")
	add(len(p.AcceptanceAdd) > 0, "acceptance_add")
	add(p.Focus != nil, "focus")
	add(len(p.MetadataMerge) > 0, "metadata_merge")
	add(p.Force, "force")
	return out
}

// validatePatchShape enforces PLAN §6.5's mutual-exclusion rules.
func (s *svc) validatePatchShape(p TaskPatch) error {
	if p.Tags != nil && (len(p.TagsAdd) > 0 || len(p.TagsRemove) > 0) {
		return domain.Invalid("tags",
			"tags is mutually exclusive with tags_add and tags_remove",
			"Pick one mode: replace (tags), add (tags_add), or remove (tags_remove).")
	}
	if p.Acceptance != nil && (len(p.AcceptanceCheck) > 0 || len(p.AcceptanceAdd) > 0) {
		return domain.Invalid("acceptance",
			"acceptance is mutually exclusive with acceptance_check and acceptance_add",
			"Pick one mode: replace (acceptance), check (acceptance_check), or add (acceptance_add).")
	}
	if p.Body != nil && p.BodyAppend != nil {
		return domain.Invalid("body_append",
			"body and body_append are mutually exclusive",
			"Send either body (replace) or body_append (append), not both.")
	}
	return nil
}

// projectedAcceptance returns what a task's acceptance list becomes after a
// patch, WITHOUT mutating the input. It is the single source of truth used by
// both the strict-done gate (which must judge the checklist the same patch
// produces, not the stored one — otherwise "tick the last item AND move to
// Done" in one atomic patch is wrongly refused) and the actual write, so the
// two can never disagree. The precedence mirrors task_update's mutual
// exclusion: a full replace wins, else check-by-index, else add.
func projectedAcceptance(current []domain.AcceptanceItem, patch TaskPatch) ([]domain.AcceptanceItem, error) {
	switch {
	case patch.Acceptance != nil:
		items := append([]domain.AcceptanceItem(nil), patch.Acceptance...)
		if err := domain.ValidateAcceptance(items); err != nil {
			return nil, err
		}
		return items, nil
	case len(patch.AcceptanceCheck) > 0:
		items := append([]domain.AcceptanceItem(nil), current...)
		for _, idx := range patch.AcceptanceCheck {
			if idx < 0 || idx >= len(items) {
				return nil, domain.Invalid("acceptance_check",
					fmt.Sprintf("index %d is out of range (have %d items)", idx, len(items)),
					"Pass indices in [0, len(acceptance)).")
			}
			items[idx].Done = true
		}
		return items, nil
	case len(patch.AcceptanceAdd) > 0:
		items := append([]domain.AcceptanceItem(nil), current...)
		for _, raw := range patch.AcceptanceAdd {
			items = append(items, domain.AcceptanceItem{Text: raw, Done: false})
		}
		if err := domain.ValidateAcceptance(items); err != nil {
			return nil, err
		}
		return items, nil
	}
	return current, nil
}

// applyUpdate applies one prepared patch and is the only path that writes.
//
// Most decisions that can fail are made in prepareUpdate, and keeping it
// that way produces better errors. Not all of them are, though — the
// acceptance-index range, the tag normalisation and the parent-chain rules
// the store enforces are all decided here, after earlier fields of the same
// patch have already been written. This used to be a claim that everything
// fallible happened first, which was untrue and one forgotten check away
// from committing half an item. It is now merely a preference: the caller
// runs this inside a nested unit of work, so an error at any point rolls
// this item's writes back and is surfaced via ItemResult.Err.
func (s *svc) applyUpdate(
	tx store.Tx, prep *prepared, patch TaskPatch, a Actor,
	pending *[]domain.Event, cc *columnCache, pc *projectCache, now time.Time,
) (*domain.TaskView, error) {
	t := copyTask(prep.task)

	contentChanged := false
	moved := false

	if patch.Title != nil {
		if _, err := domain.ValidateTitle(*patch.Title); err != nil {
			return nil, err
		}
		t.Title = *patch.Title
		contentChanged = true
	}
	if patch.Body != nil {
		if err := domain.ValidateBody(*patch.Body); err != nil {
			return nil, err
		}
		t.Body = *patch.Body
		contentChanged = true
	}
	if patch.BodyAppend != nil {
		// BodyAppend grows the body in place: a blank line separates the
		// existing content from the appended block when the body is already
		// populated, and an empty body just becomes the appended text. The
		// caller already passed the body+body_append mutual-exclusion check
		// in validatePatchShape, so Body is unchanged here. The result is
		// bounded like any other body write: repeated appends must not grow a
		// row past MaxBodyBytes just because each piece was individually small.
		var next string
		if t.Body == "" {
			next = *patch.BodyAppend
		} else {
			next = t.Body + "\n\n" + *patch.BodyAppend
		}
		if err := domain.ValidateBody(next); err != nil {
			return nil, err
		}
		t.Body = next
		contentChanged = true
	}
	if patch.Type != nil {
		t.Type = *patch.Type
		contentChanged = true
	}
	if patch.Priority != nil {
		t.Priority = *patch.Priority
		contentChanged = true
	}
	if patch.Estimate.Set {
		v := patch.Estimate.Value
		t.Estimate = &v
		contentChanged = true
	} else if patch.Estimate.Clear {
		t.Estimate = nil
		contentChanged = true
	}
	if patch.Actual.Set {
		v := patch.Actual.Value
		t.Actual = &v
		contentChanged = true
	} else if patch.Actual.Clear {
		t.Actual = nil
		contentChanged = true
	}
	if patch.Assignee.Set {
		if err := domain.ValidateActorName("assignee", patch.Assignee.Value); err != nil {
			return nil, err
		}
		v := patch.Assignee.Value
		t.Assignee = &v
		contentChanged = true
	} else if patch.Assignee.Clear {
		t.Assignee = nil
		contentChanged = true
	}
	if patch.Reviewer.Set {
		if err := domain.ValidateActorName("reviewer", patch.Reviewer.Value); err != nil {
			return nil, err
		}
		v := patch.Reviewer.Value
		t.Reviewer = &v
		contentChanged = true
	} else if patch.Reviewer.Clear {
		t.Reviewer = nil
		contentChanged = true
	}
	if patch.Outcome != nil {
		if !patch.Outcome.Valid() {
			return nil, domain.Invalid("outcome", fmt.Sprintf("outcome %q is invalid", *patch.Outcome),
				"Use one of: open, holds, refuted, superseded, moot.")
		}
		t.Outcome = *patch.Outcome
		contentChanged = true
	}
	if patch.Conclusion != nil {
		if err := domain.ValidateConclusion(*patch.Conclusion); err != nil {
			return nil, err
		}
		t.Conclusion = *patch.Conclusion
		contentChanged = true
	}
	if patch.Tags != nil {
		norm, err := domain.NormalizeTags(patch.Tags)
		if err != nil {
			return nil, err
		}
		t.Tags = norm
		contentChanged = true
	}
	if len(patch.TagsAdd) > 0 {
		norm, err := domain.NormalizeTags(patch.TagsAdd)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, tag := range t.Tags {
			seen[tag] = true
		}
		merged := append([]string(nil), t.Tags...)
		for _, tag := range norm {
			if !seen[tag] {
				merged = append(merged, tag)
				seen[tag] = true
			}
		}
		sort.Strings(merged)
		t.Tags = merged
		contentChanged = true
	}
	if len(patch.TagsRemove) > 0 {
		remove, err := domain.NormalizeTags(patch.TagsRemove)
		if err != nil {
			return nil, err
		}
		rmSet := map[string]bool{}
		for _, tag := range remove {
			rmSet[tag] = true
		}
		out := t.Tags[:0]
		for _, tag := range t.Tags {
			if !rmSet[tag] {
				out = append(out, tag)
			}
		}
		t.Tags = out
		contentChanged = true
	}
	if prep.column != nil {
		fromCol, err := cc.get(prep.task.ColumnID)
		if err != nil {
			return nil, err
		}
		pos := store.RankBottom
		if strings.EqualFold(patch.Rank, "top") {
			pos = store.RankTop
		}
		before, after, err := s.store.Tasks().NeighbourRanks(tx, prep.column.ID, pos)
		if err != nil {
			return nil, err
		}
		if err := s.store.Tasks().Move(tx, t.ID, prep.column.ID, midRank(before, after), a.Name); err != nil {
			return nil, err
		}
		t.ColumnID = prep.column.ID
		moved = true
		// A lease says "I am working on this right now". Crossing the done
		// boundary ends that sentence in both directions: into Done the work
		// is finished (TaskRemove's archive path force-releases for the same
		// reason), and out of Done the task must arrive unclaimed or another
		// agent's task_next skips it as claimed_by_other until the stale TTL
		// happens to expire. Release does not bump version, so the caller's
		// already-checked if_version stays valid.
		if crossesDoneBoundary(fromCol.Kind, prep.column.Kind) {
			if _, err := s.store.Tasks().Release(tx, t.ID, a.Name, true); err != nil {
				return nil, err
			}
		}
	}
	if patch.Parent.Set {
		if patch.Parent.Value == "" {
			t.ParentID = nil
		} else {
			// Actor-aware and project-local: a reparent used to read the row
			// straight out of the repository, which let a patch adopt a
			// parent in a project the token cannot see.
			pt, err := s.resolveRelatedTask(tx, a, "parent", patch.Parent.Value, t.Key, t.ProjectID)
			if err != nil {
				return nil, err
			}
			pid := pt.ID
			t.ParentID = &pid
		}
		contentChanged = true
	}
	if patch.Acceptance != nil || len(patch.AcceptanceCheck) > 0 || len(patch.AcceptanceAdd) > 0 {
		next, err := projectedAcceptance(t.Acceptance, patch)
		if err != nil {
			return nil, err
		}
		t.Acceptance = next
		contentChanged = true
	}
	if patch.DueAt.Set {
		v := patch.DueAt.Value
		t.DueAt = &v
		contentChanged = true
	} else if patch.DueAt.Clear {
		t.DueAt = nil
		contentChanged = true
	}
	if len(patch.MetadataMerge) > 0 {
		if t.Metadata == nil {
			t.Metadata = map[string]any{}
		}
		for k, v := range patch.MetadataMerge {
			if v == nil {
				delete(t.Metadata, k)
			} else {
				t.Metadata[k] = v
			}
		}
		contentChanged = true
	}

	if patch.Note != "" {
		if err := s.store.Notes().Add(tx, &domain.Note{
			ID:     newID(),
			TaskID: t.ID,
			Author: a.Name,
			Body:   patch.Note,
		}); err != nil {
			return nil, err
		}
		if err := s.emit(tx, pending, a.Name, domain.EventNoteAdded, t.ProjectID, &t.ID,
			map[string]any{"key": t.Key}); err != nil {
			return nil, err
		}
	}

	if patch.Focus != nil {
		holdsFocus := prep.project.FocusTaskID != nil && *prep.project.FocusTaskID == t.ID
		// PLAN §6.5: "focus:false clears focus only if this task holds it".
		// Clearing unconditionally would let a routine edit of task B drop the
		// focus banner task A had set — and bump the project version for a
		// change nobody asked for. Focus is project state, so the only task
		// allowed to release it is the one holding it.
		if *patch.Focus || holdsFocus {
			var newFocus *string
			if *patch.Focus {
				id := t.ID
				newFocus = &id
			}
			if err := s.store.Projects().SetFocus(tx, prep.project.ID, newFocus); err != nil {
				return nil, err
			}
			if err := s.emit(tx, pending, a.Name, domain.EventFocusChanged, prep.project.ID, &t.ID,
				map[string]any{"key": t.Key, "focus": *patch.Focus}); err != nil {
				return nil, err
			}
		}
	}

	if contentChanged {
		t.UpdatedBy = a.Name
		// When this patch also moved the task, the row's version has already
		// been bumped by Move itself. The caller's if_version was checked in
		// prepareUpdate inside this same serialized transaction, so passing it
		// again would compare against a bump we just made ourselves and turn
		// every move+edit patch into a spurious conflict.
		ifVersion := patch.IfVersion
		if moved {
			ifVersion = nil
		}
		if err := s.store.Tasks().Update(tx, t, ifVersion); err != nil {
			return nil, err
		}
		if err := s.emit(tx, pending, a.Name, domain.EventTaskUpdated, t.ProjectID, &t.ID,
			map[string]any{"key": t.Key}); err != nil {
			return nil, err
		}
	}
	if moved {
		// Emitted for every move, not only moves that also changed content:
		// the live board refreshes off events, so a silent move would stay
		// invisible until the next poll.
		if err := s.emit(tx, pending, a.Name, domain.EventTaskMoved, t.ProjectID, &t.ID,
			map[string]any{"key": t.Key, "column": prep.column.Name}); err != nil {
			return nil, err
		}
	}

	// Re-read the row before hydrating. Move and Release write columns the
	// in-memory copy cannot know (version, done_at, started_at, the claim
	// fields), and a stale version in the returned view becomes the caller's
	// next spurious conflict. startClaimedTask and TaskRemove follow the same
	// re-read pattern after a move.
	fresh, err := s.store.Tasks().GetByID(tx, t.ID)
	if err != nil {
		return nil, err
	}
	tv, err := s.hydrateView(tx, cc, pc, fresh, now, hydrateOpts{})
	if err != nil {
		return nil, err
	}
	return &tv, nil
}

// crossesDoneBoundary reports whether a column move between these two kinds
// passes the done boundary in either direction. Both directions matter:
// entering Done the work is finished, and leaving Done the task must arrive
// unclaimed — a reopened task pre-claimed by whoever closed it is invisible
// to task_next until the stale lease TTL expires. Done-to-done moves also
// count, keeping the invariant "no live lease on a done-kind column" total.
func crossesDoneBoundary(from, to domain.Kind) bool {
	return from == domain.KindDone || to == domain.KindDone
}

// copyTask returns a value-copy of t.
func copyTask(t *domain.Task) *domain.Task {
	if t == nil {
		return nil
	}
	clone := *t
	if t.Tags != nil {
		clone.Tags = append([]string(nil), t.Tags...)
	}
	if t.Acceptance != nil {
		clone.Acceptance = append([]domain.AcceptanceItem(nil), t.Acceptance...)
	}
	if t.Metadata != nil {
		md := make(map[string]any, len(t.Metadata))
		for k, v := range t.Metadata {
			md[k] = v
		}
		clone.Metadata = md
	}
	if t.Assignee != nil {
		s := *t.Assignee
		clone.Assignee = &s
	}
	if t.Reviewer != nil {
		s := *t.Reviewer
		clone.Reviewer = &s
	}
	if t.ParentID != nil {
		s := *t.ParentID
		clone.ParentID = &s
	}
	if t.Estimate != nil {
		f := *t.Estimate
		clone.Estimate = &f
	}
	if t.Actual != nil {
		f := *t.Actual
		clone.Actual = &f
	}
	if t.DueAt != nil {
		d := *t.DueAt
		clone.DueAt = &d
	}
	if t.ClaimedBy != nil {
		s := *t.ClaimedBy
		clone.ClaimedBy = &s
	}
	if t.ClaimedAt != nil {
		d := *t.ClaimedAt
		clone.ClaimedAt = &d
	}
	if t.ClaimExpiresAt != nil {
		d := *t.ClaimExpiresAt
		clone.ClaimExpiresAt = &d
	}
	if t.StartedAt != nil {
		d := *t.StartedAt
		clone.StartedAt = &d
	}
	if t.DoneAt != nil {
		d := *t.DoneAt
		clone.DoneAt = &d
	}
	if t.ArchivedAt != nil {
		d := *t.ArchivedAt
		clone.ArchivedAt = &d
	}
	return &clone
}

// ---------------------------------------------------------------------------
// task_remove — archive (or restore) with optional cascade (PLAN §6.8)
//
// task_remove always returns per-item results (the spec does not expose
// an `atomic` knob here). A failing item never sinks the rest.
// ---------------------------------------------------------------------------

func (s *svc) TaskRemove(ctx context.Context, a Actor, in TaskRemoveInput) (*TaskRemoveResult, error) {
	if err := requireWrite(a, "archive or restore tasks"); err != nil {
		return nil, err
	}
	if len(in.Items) == 0 {
		return nil, domain.Invalid("items", "at least one item is required",
			"Pass 1..100 RemoveItem entries.")
	}
	if len(in.Items) > domain.MaxBatchTasks {
		return nil, domain.Invalid("items",
			fmt.Sprintf("%d items, the limit is %d", len(in.Items), domain.MaxBatchTasks),
			"Split the request into multiple calls.")
	}
	ctx = store.WithActor(ctx, a.Name)

	results := make([]ItemResult, len(in.Items))
	var pending []domain.Event

	err := s.store.Write(ctx, func(tx store.Tx) error {
		now, err := tx.Now()
		if err != nil {
			return err
		}
		cc := newColumnCache(s, tx)
		pc := newProjectCache(s, tx)

		for i, item := range in.Items {
			results[i].Key = item.Key
			if item.Key == "" {
				results[i].Err = domain.Invalid("key", "key is required", "Pass the task key.")
				continue
			}
			var tv *domain.TaskView
			itemErr, fatal := runItem(tx, &pending, func(itx store.Tx) error {
				var err error
				tv, err = s.removeOne(itx, a, item, in, &pending, cc.bind(itx), pc.bind(itx), now)
				return err
			})
			if fatal != nil {
				return fatal
			}
			if itemErr != nil {
				results[i].Err = itemErr
				continue
			}
			results[i].OK = true
			results[i].Task = tv
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishAll(pending)
	return &TaskRemoveResult{Items: results}, nil
}

// removeOne archives (or restores) one task_remove item. It runs inside the
// item's own nested unit of work, so it may write first and fail afterwards:
// the cascade below archives every child before the parent's own archive is
// attempted, and half of that landing under an ok:false result is exactly
// what the savepoint exists to prevent.
func (s *svc) removeOne(
	tx store.Tx, a Actor, item RemoveItem, in TaskRemoveInput,
	pending *[]domain.Event, cc *columnCache, pc *projectCache, now time.Time,
) (*domain.TaskView, error) {
	task, err := s.resolveTask(tx, a, item.Key)
	if err != nil {
		return nil, err
	}
	if item.IfVersion != nil && task.Version != *item.IfVersion {
		tv, err := s.hydrateView(tx, cc, pc, task, now, hydrateOpts{})
		if err != nil {
			return nil, err
		}
		return nil, domain.Conflict(&tv, *item.IfVersion, task.Version)
	}
	if !in.Restore && in.CascadeSubtasks {
		if err := s.archiveChildren(tx, task, a, pending); err != nil {
			return nil, err
		}
	}
	// Force-release any claim.
	if _, err := s.store.Tasks().Release(tx, task.ID, a.Name, true); err != nil {
		return nil, err
	}
	// Clear focus if this task held it.
	proj, err := s.store.Projects().GetByID(tx, task.ProjectID)
	if err != nil {
		return nil, err
	}
	if proj.FocusTaskID != nil && *proj.FocusTaskID == task.ID {
		if err := s.store.Projects().SetFocus(tx, proj.ID, nil); err != nil {
			return nil, err
		}
		if err := s.emit(tx, pending, a.Name, domain.EventFocusChanged, proj.ID, &task.ID,
			map[string]any{"key": task.Key, "focus": false}); err != nil {
			return nil, err
		}
	}
	if err := s.store.Tasks().Archive(tx, task.ID, !in.Restore, a.Name); err != nil {
		return nil, err
	}
	evType := domain.EventTaskArchived
	if in.Restore {
		evType = domain.EventTaskRestored
	}
	if err := s.emit(tx, pending, a.Name, evType, task.ProjectID, &task.ID,
		map[string]any{"key": task.Key}); err != nil {
		return nil, err
	}
	fresh, err := s.store.Tasks().GetByID(tx, task.ID)
	if err != nil {
		return nil, err
	}
	tv, err := s.hydrateView(tx, cc, pc, fresh, now, hydrateOpts{})
	if err != nil {
		return nil, err
	}
	return &tv, nil
}

// archiveChildren recursively archives every unarchived descendant of t.
// Used by TaskRemove when CascadeSubtasks is true. Depth is bounded by
// MaxSubtaskDepth=2, so this is at most one level deep.
func (s *svc) archiveChildren(tx store.Tx, t *domain.Task, a Actor, pending *[]domain.Event) error {
	children, err := s.store.Tasks().Children(tx, t.ID)
	if err != nil {
		return err
	}
	for _, ch := range children {
		if ch.ArchivedAt != nil {
			continue
		}
		if _, err := s.store.Tasks().Release(tx, ch.ID, a.Name, true); err != nil {
			return err
		}
		proj, err := s.store.Projects().GetByID(tx, ch.ProjectID)
		if err != nil {
			return err
		}
		if proj.FocusTaskID != nil && *proj.FocusTaskID == ch.ID {
			if err := s.store.Projects().SetFocus(tx, proj.ID, nil); err != nil {
				return err
			}
			if err := s.emit(tx, pending, a.Name, domain.EventFocusChanged, proj.ID, &ch.ID,
				map[string]any{"key": ch.Key, "focus": false}); err != nil {
				return err
			}
		}
		if err := s.store.Tasks().Archive(tx, ch.ID, true, a.Name); err != nil {
			return err
		}
		if err := s.emit(tx, pending, a.Name, domain.EventTaskArchived, ch.ProjectID, &ch.ID,
			map[string]any{"key": ch.Key, "via": t.Key}); err != nil {
			return err
		}
	}
	return nil
}

// isNotFound reports whether err is a *domain.Error with CodeNotFound.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if e := domain.AsError(err); e != nil {
		return e.Code == domain.CodeNotFound
	}
	return false
}
