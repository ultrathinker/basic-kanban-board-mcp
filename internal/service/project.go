package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ProjectUpsert creates or updates a project and its columns in one
// transaction (PLAN §6.9). mode is required so a key typo can never silently
// fork the board into a second project.
func (s *svc) ProjectUpsert(ctx context.Context, a Actor, in ProjectUpsertInput) (*ProjectUpsertResult, error) {
	if err := requireWrite(a); err != nil {
		return nil, err
	}
	switch in.Mode {
	case UpsertCreate, UpsertUpdate:
	default:
		return nil, domain.Invalid("mode", "mode must be create or update", "Pass create or update.")
	}
	key, err := domain.ValidateProjectKey(in.Key)
	if err != nil {
		return nil, err
	}
	if err := requireProjectAccess(a, key); err != nil {
		return nil, err
	}
	if in.Mode == UpsertUpdate && in.IfVersion == nil {
		return nil, domain.Invalid("if_version", "if_version is required for update",
			"Read the project's current version first, then retry with it.")
	}
	if in.Mode == UpsertCreate && in.Name == "" {
		return nil, domain.Invalid("name", "name is required to create a project", "Pass a display name.")
	}

	ctx = store.WithActor(ctx, a.Name)
	var result ProjectUpsertResult
	var pending []domain.Event
	err = s.store.Write(ctx, func(tx store.Tx) error {
		var p *domain.Project
		var cols []domain.Column
		var err error
		if in.Mode == UpsertCreate {
			p, cols, err = s.projectCreate(tx, &pending, a, key, in)
		} else {
			p, cols, err = s.projectUpdate(tx, &pending, a, key, in)
		}
		if err != nil {
			return err
		}
		result = ProjectUpsertResult{Project: *p, Columns: cols}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishAll(pending)
	return &result, nil
}

func (s *svc) projectCreate(tx store.Tx, pending *[]domain.Event, a Actor, key string, in ProjectUpsertInput) (*domain.Project, []domain.Column, error) {
	p := &domain.Project{
		ID:                  newID(),
		Key:                 key,
		Name:                in.Name,
		EstimateUnit:        "h",
		EnforceDependencies: true,
		StrictDone:          false,
		ClaimTTLSeconds:     int(domain.ClaimTTLDefault.Seconds()),
	}
	if in.Description != nil {
		p.Description = *in.Description
	}
	applyProjectSettings(p, in.Settings)
	if err := s.store.Projects().Create(tx, p); err != nil {
		return nil, nil, err
	}

	specs := in.Columns
	if len(specs) == 0 {
		for _, def := range domain.DefaultColumns {
			specs = append(specs, ColumnSpec{Name: def.Name, Kind: def.Kind, WIPLimit: def.WIPLimit})
		}
	}
	if err := validateColumnSpecs(specs); err != nil {
		return nil, nil, err
	}
	cols := make([]domain.Column, 0, len(specs))
	for i, cs := range specs {
		name, err := domain.ValidateColumnName(cs.Name)
		if err != nil {
			return nil, nil, err
		}
		col := &domain.Column{ID: newID(), ProjectID: p.ID, Name: name, Position: i, Kind: cs.Kind, WIPLimit: cs.WIPLimit}
		if err := s.store.Columns().Create(tx, col); err != nil {
			return nil, nil, err
		}
		cols = append(cols, *col)
	}
	if err := s.emit(tx, pending, a.Name, domain.EventProjectCreated, p.ID, nil, map[string]any{"key": p.Key}); err != nil {
		return nil, nil, err
	}
	return p, cols, nil
}

func (s *svc) projectUpdate(tx store.Tx, pending *[]domain.Event, a Actor, key string, in ProjectUpsertInput) (*domain.Project, []domain.Column, error) {
	p, err := s.store.Projects().GetByKey(tx, key)
	if err != nil {
		return nil, nil, err
	}
	if in.Name != "" {
		p.Name = in.Name
	}
	if in.Description != nil {
		p.Description = *in.Description
	}
	applyProjectSettings(p, in.Settings)
	wasArchiving := false
	if in.Archived != nil {
		if *in.Archived && p.ArchivedAt == nil {
			now, err := tx.Now()
			if err != nil {
				return nil, nil, err
			}
			p.ArchivedAt = &now
			wasArchiving = true
		} else if !*in.Archived {
			p.ArchivedAt = nil
		}
	}
	if err := s.store.Projects().Update(tx, p, in.IfVersion); err != nil {
		return nil, nil, err
	}

	var cols []domain.Column
	if in.Columns != nil || len(in.RemoveColumns) > 0 {
		cols, err = s.applyColumnChanges(tx, p, in.Columns, in.RemoveColumns, a.Name)
		if err != nil {
			return nil, nil, err
		}
	} else {
		existing, err := s.store.Columns().ListByProject(tx, p.ID)
		if err != nil {
			return nil, nil, err
		}
		for _, c := range existing {
			cols = append(cols, *c)
		}
	}

	if err := s.emit(tx, pending, a.Name, domain.EventProjectUpdated, p.ID, nil, map[string]any{"key": p.Key}); err != nil {
		return nil, nil, err
	}
	if wasArchiving {
		if err := s.emit(tx, pending, a.Name, domain.EventProjectArchived, p.ID, nil, map[string]any{"key": p.Key}); err != nil {
			return nil, nil, err
		}
	}
	return p, cols, nil
}

func applyProjectSettings(p *domain.Project, in *ProjectSettings) {
	if in == nil {
		return
	}
	if in.EstimateUnit != nil {
		p.EstimateUnit = *in.EstimateUnit
	}
	if in.EnforceDependencies != nil {
		p.EnforceDependencies = *in.EnforceDependencies
	}
	if in.StrictDone != nil {
		p.StrictDone = *in.StrictDone
	}
	if in.ClaimTTLSeconds != nil {
		p.ClaimTTLSeconds = int(domain.ClampClaimTTL(time.Duration(*in.ClaimTTLSeconds) * time.Second).Seconds())
	}
}

func validateColumnSpecs(specs []ColumnSpec) error {
	if len(specs) == 0 {
		return domain.Invalid("columns", "at least one column is required", "Pass at least one column.")
	}
	if len(specs) > domain.MaxColumnsPerPrj {
		return domain.Invalid("columns",
			fmt.Sprintf("%d columns, the limit is %d", len(specs), domain.MaxColumnsPerPrj),
			"Reduce the number of columns.")
	}
	seen := map[string]bool{}
	for _, cs := range specs {
		name, err := domain.ValidateColumnName(cs.Name)
		if err != nil {
			return err
		}
		lname := strings.ToLower(name)
		if seen[lname] {
			return domain.Invalid("columns", fmt.Sprintf("column name %q appears more than once", name),
				"Column names must be unique within a project.")
		}
		seen[lname] = true
		if !cs.Kind.Valid() {
			return domain.Invalid("kind", fmt.Sprintf("column kind %q is invalid", cs.Kind),
				"Use one of: backlog, active, done.")
		}
		if cs.WIPLimit != nil {
			// A WIP limit only means anything on an active column (it is the
			// count task_next gates against), and a non-positive limit makes
			// that column permanently "full" — count >= 0 is always true — so
			// task_next would refuse all work with no way to see why.
			if cs.Kind != domain.KindActive {
				return domain.Invalid("wip_limit",
					fmt.Sprintf("column %q is %s; only active columns may carry a WIP limit", name, cs.Kind),
					"Drop the WIP limit, or make the column active.")
			}
			if *cs.WIPLimit <= 0 {
				return domain.Invalid("wip_limit",
					fmt.Sprintf("column %q has WIP limit %d; it must be positive", name, *cs.WIPLimit),
					"Use a positive limit, or omit it for no limit.")
			}
		}
	}
	return nil
}

// applyColumnChanges reconciles the project's current columns with the
// caller's full desired list. Every existing column missing from the
// desired list must have a matching remove_columns entry naming where its
// tasks go — PLAN §6.9 requires this explicitly so a shorter columns array
// can never silently lose data.
func (s *svc) applyColumnChanges(tx store.Tx, p *domain.Project, specs []ColumnSpec, removals []RemoveColumn, actor string) ([]domain.Column, error) {
	if err := validateColumnSpecs(specs); err != nil {
		return nil, err
	}
	existing, err := s.store.Columns().ListByProject(tx, p.ID)
	if err != nil {
		return nil, err
	}
	existingByName := make(map[string]*domain.Column, len(existing))
	for _, c := range existing {
		existingByName[strings.ToLower(c.Name)] = c
	}
	wantByName := make(map[string]bool, len(specs))
	for _, cs := range specs {
		name, _ := domain.ValidateColumnName(cs.Name)
		wantByName[strings.ToLower(name)] = true
	}
	removalByName := make(map[string]RemoveColumn, len(removals))
	for _, r := range removals {
		removalByName[strings.ToLower(strings.TrimSpace(r.Name))] = r
	}

	for lname, c := range existingByName {
		if wantByName[lname] {
			continue
		}
		rem, ok := removalByName[lname]
		if !ok {
			return nil, domain.Invalid("remove_columns",
				fmt.Sprintf("column %q is missing from columns and not listed in remove_columns", c.Name),
				"Add it back to columns, or list it in remove_columns with move_tasks_to.")
		}
		destName, err := domain.ValidateColumnName(rem.MoveTasksTo)
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(destName, c.Name) {
			return nil, domain.Invalid("move_tasks_to", "move_tasks_to must not be the column being removed",
				"Pick a different destination column.")
		}
		if !wantByName[strings.ToLower(destName)] && existingByName[strings.ToLower(destName)] == nil {
			return nil, domain.NotFound("column", rem.MoveTasksTo)
		}
	}

	out := make([]domain.Column, 0, len(specs))
	for i, cs := range specs {
		name, _ := domain.ValidateColumnName(cs.Name)
		if existingCol, ok := existingByName[strings.ToLower(name)]; ok {
			existingCol.Name = name
			existingCol.Position = i
			existingCol.Kind = cs.Kind
			existingCol.WIPLimit = cs.WIPLimit
			if err := s.store.Columns().Update(tx, existingCol); err != nil {
				return nil, err
			}
			out = append(out, *existingCol)
		} else {
			col := &domain.Column{ID: newID(), ProjectID: p.ID, Name: name, Position: i, Kind: cs.Kind, WIPLimit: cs.WIPLimit}
			if err := s.store.Columns().Create(tx, col); err != nil {
				return nil, err
			}
			out = append(out, *col)
		}
	}

	outByName := make(map[string]*domain.Column, len(out))
	for i := range out {
		outByName[strings.ToLower(out[i].Name)] = &out[i]
	}
	for lname, c := range existingByName {
		if wantByName[lname] {
			continue
		}
		rem := removalByName[lname]
		destName, _ := domain.ValidateColumnName(rem.MoveTasksTo)
		dest, ok := outByName[strings.ToLower(destName)]
		if !ok {
			return nil, domain.NotFound("column", rem.MoveTasksTo)
		}
		tasks, err := s.store.Tasks().List(tx, store.TaskFilter{
			ColumnIDs: []string{c.ID}, IncludeDone: true, IncludeArchived: true,
		})
		if err != nil {
			return nil, err
		}
		for _, t := range tasks {
			before, after, err := s.store.Tasks().NeighbourRanks(tx, dest.ID, store.RankBottom)
			if err != nil {
				return nil, err
			}
			if err := s.store.Tasks().Move(tx, t.ID, dest.ID, midRank(before, after), actor); err != nil {
				return nil, err
			}
			// Herding tasks out of a removed column can cross the done
			// boundary (move_tasks_to names Done, or a done column is being
			// removed into an active one). The same rule as a normal move
			// applies: no live lease survives the boundary. Archived tasks
			// are herded too, and Release is an idempotent no-op on an
			// unclaimed row, so no filtering is needed.
			if crossesDoneBoundary(c.Kind, dest.Kind) {
				if _, err := s.store.Tasks().Release(tx, t.ID, actor, true); err != nil {
					return nil, err
				}
			}
		}
		if err := s.store.Columns().Delete(tx, c.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}
