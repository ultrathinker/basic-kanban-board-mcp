package service

import (
	"context"
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
// resolveBlockerID) can take a slice of plans as a parameter.
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
	inserted           *domain.Task // populated after Create
}

func (s *svc) TaskCreate(ctx context.Context, a Actor, in TaskCreateInput) (*TaskCreateResult, error) {
	if err := requireWrite(a); err != nil {
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
	ctx = store.WithActor(ctx, a.Name)

	var result TaskCreateResult
	var pending []domain.Event
	err := s.store.Write(ctx, func(tx store.Tx) error {
		now := tx.Now()
		cc := newColumnCache(s, tx)
		pc := newProjectCache(s, tx)

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
		refToKey := make(map[string]string, len(plans))
		refToID := make(map[string]string, len(plans))
		for _, p := range plans {
			if p.isReplay {
				if p.new.Ref != "" {
					refToKey[p.new.Ref] = p.replayed.Key
				}
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
				Tags:       append([]string(nil), p.new.Tags...),
				Assignee:   p.new.Assignee,
				Acceptance: buildAcceptance(p.new.Acceptance),
				DueAt:      p.new.DueAt,
				Metadata:   p.new.Metadata,
				CreatedBy:  a.Name,
				UpdatedBy:  a.Name,
			}
			p.inserted = t
			if p.new.Ref != "" {
				refToKey[p.new.Ref] = t.Key
				refToID[p.new.Ref] = t.ID
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
				pid, err := s.resolveNewParent(tx, p.new.Parent, refToKey, refToID, plans)
				if err != nil {
					return err
				}
				p.inserted.ParentID = &pid
			}
			if len(p.new.BlockedBy) > 0 {
				ids := make([]string, 0, len(p.new.BlockedBy))
				for _, raw := range p.new.BlockedBy {
					id, err := s.resolveBlockerID(tx, raw, refToKey, refToID)
					if err != nil {
						return err
					}
					ids = append(ids, id)
				}
				p.resolvedBlockerIDs = ids
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
		for _, p := range plans {
			if p.inserted == nil {
				continue
			}
			t := p.inserted
			for _, blockerID := range p.resolvedBlockerIDs {
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

		result = TaskCreateResult{Tasks: outTasks, Replayed: replayed}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishAll(pending)
	return &result, nil
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
	if err := domain.ValidateAcceptance(buildAcceptance(n.Acceptance)); err != nil {
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
func (s *svc) resolveNewParent(tx store.Tx, raw string, refToKey, refToID map[string]string, plans []*createItemPlan) (string, error) {
	if strings.HasPrefix(raw, "@") {
		ref := strings.TrimPrefix(raw, "@")
		id, ok := refToID[ref]
		if !ok {
			return "", domain.Invalid("parent",
				fmt.Sprintf("parent ref %q does not match any item in this batch", raw),
				"Use @name pointing at a ref in the same batch, or a literal task key.")
		}
		// Depth check: the referenced in-batch item must itself be a
		// top-level task (its own Parent is empty). Look it up in the
		// plan rather than the database — the row hasn't been inserted
		// yet at this point.
		for _, p := range plans {
			if p.isReplay || p.inserted == nil {
				continue
			}
			if p.new.Ref == ref && p.new.Parent != "" {
				return "", domain.Invalid("parent",
					"parent ref is itself a subtask (max depth 2); subtasks may not have children",
					"Pick a top-level task as the parent; subtasks are leaves.")
			}
		}
		return id, nil
	}
	t, err := s.store.Tasks().GetByKey(tx, raw)
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

// resolveBlockerID resolves a single BlockedBy entry to a task ID.
func (s *svc) resolveBlockerID(tx store.Tx, raw string, refToKey, refToID map[string]string) (string, error) {
	if strings.HasPrefix(raw, "@") {
		ref := strings.TrimPrefix(raw, "@")
		id, ok := refToID[ref]
		if !ok {
			return "", domain.Invalid("blocked_by",
				fmt.Sprintf("blocker ref %q does not match any item in this batch", raw),
				"Use @name pointing at a ref in the same batch, or a literal task key.")
		}
		return id, nil
	}
	t, err := s.store.Tasks().GetByKey(tx, raw)
	if err != nil {
		return "", err
	}
	return t.ID, nil
}

// ---------------------------------------------------------------------------
// task_update — per-item results by default, atomic on request (PLAN §6.5)
//
// Atomic semantics (PLAN §6.5):
//   - Atomic == true  → the first item error returned from the closure
//     rolls back the entire transaction. Every other item gets a
//     synthetic ItemResult{Err: <closure error>}. We never apply a
//     partial batch.
//   - Atomic == false → per-item results. A mutating repo call that
//     fails for one item is recorded in that item's ItemResult.Err and
//     the loop continues. The `prepared` snapshot guarantees that
//     every decision that could fail has already been validated
//     against the loaded task state, so a true mid-loop failure
//     should be rare.
//
// store.Tx gives us no savepoints, so even under Atomic=false we cannot
// "roll back just this item". The only safe pattern is validate first,
// then write — exactly what the two-phase shape below does.
// ---------------------------------------------------------------------------

func (s *svc) TaskUpdate(ctx context.Context, a Actor, in TaskUpdateInput) (*TaskUpdateResult, error) {
	if err := requireWrite(a); err != nil {
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
		now := tx.Now()
		cc := newColumnCache(s, tx)
		pc := newProjectCache(s, tx)
		prepareds := make([]*prepared, len(in.Patches))

		// ----- Phase 1: validate each patch in isolation -----
		for i, patch := range in.Patches {
			results[i].Key = patch.Key
			if patch.Key == "" {
				results[i].Err = domain.Invalid("key", "key is required", "Pass the task key.")
				if in.Atomic {
					return results[i].Err
				}
				continue
			}
			prep, err := s.prepareUpdate(tx, a, patch)
			if err != nil {
				results[i].Err = domain.AsError(err)
				if in.Atomic {
					return err
				}
				continue
			}
			prepareds[i] = prep
		}

		// ----- Phase 2: apply each prepared patch -----
		for i, patch := range in.Patches {
			if results[i].Err != nil {
				continue
			}
			prep := prepareds[i]
			results[i].Key = prep.task.Key
			tv, err := s.applyUpdate(tx, prep, patch, a, &pending, cc, pc, now)
			if err != nil {
				results[i].Err = domain.AsError(err)
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
		p.Assignee.Set || p.Assignee.Clear ||
		p.DueAt.Set || p.DueAt.Clear ||
		len(p.Tags) > 0 ||
		p.Column != "" ||
		p.Parent.Set || p.Parent.Clear ||
		len(p.Acceptance) > 0 || len(p.AcceptanceCheck) > 0 || len(p.AcceptanceAdd) > 0 ||
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
	if err := requireForce(a, patch.Force, patch.Reason); err != nil {
		return nil, err
	}
	if patch.IfVersion == nil && isReplacementPatch(patch) {
		return nil, domain.Invalid("if_version",
			"if_version is required when a replacement-style field is set",
			"Read the task's current version with task_get and retry with if_version=<n>.")
	}
	if patch.IfVersion != nil && task.Version != *patch.IfVersion {
		tv, err := s.hydrateView(tx, newColumnCache(s, tx), newProjectCache(s, tx), task, tx.Now(), hydrateOpts{})
		if err != nil {
			return nil, err
		}
		return nil, domain.Conflict(&tv, *patch.IfVersion, task.Version)
	}

	prep := &prepared{task: task, project: proj}

	if patch.Column != "" {
		dst, err := s.resolveColumn(tx, proj, patch.Column)
		if err != nil {
			return nil, err
		}
		prep.column = dst
		cnt, err := s.store.Columns().CountTasks(tx, dst.ID, task.ID)
		if err != nil {
			return nil, err
		}
		openBlocks, err := s.store.Links().OpenBlockers(tx, task.ID)
		if err != nil {
			return nil, err
		}
		var acceptanceRemaining int
		if proj.StrictDone {
			for _, it := range task.Acceptance {
				if !it.Done {
					acceptanceRemaining++
				}
			}
		}
		if err := domain.CheckMove(domain.MoveCheck{
			TaskKey:             task.Key,
			From:                domain.Column{ID: task.ColumnID, ProjectID: task.ProjectID},
			To:                  *dst,
			OpenBlocks:          openBlocks,
			ToCount:             cnt,
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

// validatePatchShape enforces PLAN §6.5's mutual-exclusion rules.
func (s *svc) validatePatchShape(p TaskPatch) error {
	if len(p.Tags) > 0 && (len(p.TagsAdd) > 0 || len(p.TagsRemove) > 0) {
		return domain.Invalid("tags",
			"tags is mutually exclusive with tags_add and tags_remove",
			"Pick one mode: replace (tags), add (tags_add), or remove (tags_remove).")
	}
	if len(p.Acceptance) > 0 && (len(p.AcceptanceCheck) > 0 || len(p.AcceptanceAdd) > 0) {
		return domain.Invalid("acceptance",
			"acceptance is mutually exclusive with acceptance_check and acceptance_add",
			"Pick one mode: replace (acceptance), check (acceptance_check), or add (acceptance_add).")
	}
	return nil
}

// applyUpdate applies one prepared patch. It is the only path that
// writes — every decision that could fail has already been made in
// prepareUpdate, so a repo call returning an error here means the
// snapshot went stale (a concurrent writer beat us) and is surfaced to
// the caller via ItemResult.Err.
func (s *svc) applyUpdate(
	tx store.Tx, prep *prepared, patch TaskPatch, a Actor,
	pending *[]domain.Event, cc *columnCache, pc *projectCache, now time.Time,
) (*domain.TaskView, error) {
	t := copyTask(prep.task)

	contentChanged := false
	moved := false

	if patch.Title != nil {
		t.Title = *patch.Title
		contentChanged = true
	}
	if patch.Body != nil {
		t.Body = *patch.Body
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
	if patch.Assignee.Set {
		v := patch.Assignee.Value
		t.Assignee = &v
		contentChanged = true
	} else if patch.Assignee.Clear {
		t.Assignee = nil
		contentChanged = true
	}
	if len(patch.Tags) > 0 {
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
			pt, err := s.store.Tasks().GetByKey(tx, patch.Parent.Value)
			if err != nil {
				return nil, err
			}
			pid := pt.ID
			t.ParentID = &pid
		}
		contentChanged = true
	}
	switch {
	case len(patch.Acceptance) > 0:
		items := make([]domain.AcceptanceItem, len(patch.Acceptance))
		for i, ai := range patch.Acceptance {
			items[i] = ai
		}
		if err := domain.ValidateAcceptance(items); err != nil {
			return nil, err
		}
		t.Acceptance = items
		contentChanged = true
	case len(patch.AcceptanceCheck) > 0:
		for _, idx := range patch.AcceptanceCheck {
			if idx < 0 || idx >= len(t.Acceptance) {
				return nil, domain.Invalid("acceptance_check",
					fmt.Sprintf("index %d is out of range (have %d items)", idx, len(t.Acceptance)),
					"Pass indices in [0, len(acceptance)).")
			}
			t.Acceptance[idx].Done = true
		}
		contentChanged = true
	case len(patch.AcceptanceAdd) > 0:
		for _, raw := range patch.AcceptanceAdd {
			t.Acceptance = append(t.Acceptance, domain.AcceptanceItem{Text: raw, Done: false})
		}
		if err := domain.ValidateAcceptance(t.Acceptance); err != nil {
			return nil, err
		}
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
	if t.ParentID != nil {
		s := *t.ParentID
		clone.ParentID = &s
	}
	if t.Estimate != nil {
		f := *t.Estimate
		clone.Estimate = &f
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
	if err := requireWrite(a); err != nil {
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
		now := tx.Now()
		cc := newColumnCache(s, tx)
		pc := newProjectCache(s, tx)

		for i, item := range in.Items {
			results[i].Key = item.Key
			if item.Key == "" {
				results[i].Err = domain.Invalid("key", "key is required", "Pass the task key.")
				continue
			}
			task, err := s.resolveTask(tx, a, item.Key)
			if err != nil {
				results[i].Err = domain.AsError(err)
				continue
			}
			if item.IfVersion != nil && task.Version != *item.IfVersion {
				tv, err := s.hydrateView(tx, cc, pc, task, now, hydrateOpts{})
				if err != nil {
					results[i].Err = domain.AsError(err)
					continue
				}
				results[i].Err = domain.Conflict(&tv, *item.IfVersion, task.Version)
				continue
			}
			if !in.Restore && in.CascadeSubtasks {
				if err := s.archiveChildren(tx, task, a, &pending); err != nil {
					results[i].Err = domain.AsError(err)
					continue
				}
			}
			// Force-release any claim.
			if _, err := s.store.Tasks().Release(tx, task.ID, a.Name, true); err != nil {
				results[i].Err = domain.AsError(err)
				continue
			}
			// Clear focus if this task held it.
			proj, err := s.store.Projects().GetByID(tx, task.ProjectID)
			if err != nil {
				results[i].Err = domain.AsError(err)
				continue
			}
			if proj.FocusTaskID != nil && *proj.FocusTaskID == task.ID {
				if err := s.store.Projects().SetFocus(tx, proj.ID, nil); err != nil {
					results[i].Err = domain.AsError(err)
					continue
				}
				if err := s.emit(tx, &pending, a.Name, domain.EventFocusChanged, proj.ID, &task.ID,
					map[string]any{"key": task.Key, "focus": false}); err != nil {
					results[i].Err = domain.AsError(err)
					continue
				}
			}
			if err := s.store.Tasks().Archive(tx, task.ID, !in.Restore, a.Name); err != nil {
				results[i].Err = domain.AsError(err)
				continue
			}
			evType := domain.EventTaskArchived
			if in.Restore {
				evType = domain.EventTaskRestored
			}
			if err := s.emit(tx, &pending, a.Name, evType, task.ProjectID, &task.ID,
				map[string]any{"key": task.Key}); err != nil {
				results[i].Err = domain.AsError(err)
				continue
			}
			fresh, err := s.store.Tasks().GetByID(tx, task.ID)
			if err != nil {
				results[i].Err = domain.AsError(err)
				continue
			}
			tv, err := s.hydrateView(tx, cc, pc, fresh, now, hydrateOpts{})
			if err != nil {
				results[i].Err = domain.AsError(err)
				continue
			}
			results[i].OK = true
			results[i].Task = &tv
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishAll(pending)
	return &TaskRemoveResult{Items: results}, nil
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
