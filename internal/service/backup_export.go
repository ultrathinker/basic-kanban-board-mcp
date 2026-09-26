package service

import (
	"context"
	"fmt"
	"sort"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ExportInput selects what Export writes.
type ExportInput struct {
	// ProjectKey limits the document to one project; empty exports every
	// project the actor can read.
	ProjectKey string
}

// exportIncludes is every per-task field a backup must carry.
var exportIncludes = Includes{IncludeBody, IncludeAcceptance, IncludeLinks, IncludeMetadata}

// chatExportPage is the page size of the feed walk. It is a page, not a cap:
// the walk runs until the feed ends.
const chatExportPage = 100

// Export builds the full backup document for the projects the actor can
// read. It is the one builder behind `kanban export` and both web Export
// buttons, so the three can never disagree about what a backup contains.
//
// Done cards are exported without board_get's cap: a backup that silently
// drops done cards past the cap is not a backup (KANB-62). The board part is
// read through BoardGet, so the projects in the document are exactly those
// the actor's token may see.
func (s *svc) Export(ctx context.Context, a Actor, in ExportInput) (*ExportDocument, error) {
	if err := requireRead(a); err != nil {
		return nil, err
	}
	board, err := s.BoardGet(ctx, a, BoardGetInput{
		ProjectKey: in.ProjectKey,
		View:       ViewTasks,
		DoneLimit:  DoneLimitUnlimited,
		Include:    exportIncludes,
	})
	if err != nil {
		return nil, err
	}
	if in.ProjectKey != "" && len(board.Projects) == 0 {
		return nil, domain.NotFound("project", in.ProjectKey)
	}

	out := &ExportDocument{Projects: board.Projects, NextTaskSeq: map[string]int{}}
	// Archived keys are gathered inside the read below (a store filter is the
	// only way to see them) and hydrated through TaskGet afterwards, so an
	// archived card is exported in exactly the shape a live one is.
	archivedKeys := make(map[string][]string, len(board.Projects))

	ctx = store.WithActor(ctx, a.Name)
	err = s.store.Read(ctx, func(tx store.Tx) error {
		origin, err := s.store.TaskHistory().Origin(tx)
		if err != nil {
			return fmt.Errorf("export journal origin: %w", err)
		}
		out.HistoryStartsAt = origin

		for _, bp := range board.Projects {
			p, err := s.store.Projects().GetByKey(tx, bp.Key)
			if err != nil {
				return fmt.Errorf("export project %s: %w", bp.Key, err)
			}
			all, err := s.store.Tasks().List(tx, store.TaskFilter{
				ProjectIDs:      []string{p.ID},
				IncludeArchived: true,
				IncludeDone:     true,
			})
			if err != nil {
				return fmt.Errorf("export tasks (project %s): %w", bp.Key, err)
			}
			sortByKeyNumber(all)
			for _, t := range all {
				if t.ArchivedAt != nil {
					archivedKeys[bp.Key] = append(archivedKeys[bp.Key], t.Key)
				}
			}
			out.NextTaskSeq[bp.Key] = p.NextTaskSeq
			if err := s.exportNotes(tx, bp.Key, all, out); err != nil {
				return err
			}
			if err := s.exportProgress(tx, bp.Key, p.ID, all, out); err != nil {
				return err
			}
			if err := s.exportChat(tx, bp.Key, p.ID, out); err != nil {
				return err
			}
			if err := s.exportJournal(tx, bp.Key, p.ID, out); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	for _, bp := range board.Projects {
		keys := archivedKeys[bp.Key]
		// TaskGet takes at most MaxGetKeys per call; a mature board archives
		// far more than that.
		for len(keys) > 0 {
			chunk := keys
			if len(chunk) > domain.MaxGetKeys {
				chunk = chunk[:domain.MaxGetKeys]
			}
			keys = keys[len(chunk):]
			res, err := s.TaskGet(ctx, a, TaskGetInput{Keys: chunk, Include: exportIncludes})
			if err != nil {
				return nil, fmt.Errorf("export archived tasks (project %s): %w", bp.Key, err)
			}
			for _, tv := range res.Tasks {
				out.ArchivedTasks = append(out.ArchivedTasks, ExportArchivedTask{Project: bp.Key, Task: tv})
			}
		}
	}
	return out, nil
}

// exportNotes appends every note of every card of one project, live or
// archived, oldest first so a re-import inserts them in their own order.
func (s *svc) exportNotes(tx store.Tx, projectKey string, tasks []*domain.Task, out *ExportDocument) error {
	for _, t := range tasks {
		notes, err := s.store.Notes().ListByTask(tx, t.ID, 1<<30, nil)
		if err != nil {
			return fmt.Errorf("export notes of %s: %w", t.Key, err)
		}
		for i := len(notes) - 1; i >= 0; i-- {
			n := notes[i]
			out.Notes = append(out.Notes, ExportNote{
				ID: n.ID, Project: projectKey, Task: t.Key,
				Author: n.Author, Body: n.Body, CreatedAt: n.CreatedAt,
			})
		}
	}
	return nil
}

// exportProgress appends every mark of one project: the project-level scope
// first, then every card's scope, archived cards included.
func (s *svc) exportProgress(tx store.Tx, projectKey, projectID string, tasks []*domain.Task, out *ExportDocument) error {
	add := func(taskKey string, marks []domain.ProgressMark) {
		for _, m := range marks {
			out.ProgressMarks = append(out.ProgressMarks, ExportProgressMark{
				ID: m.ID, Project: projectKey, Task: taskKey,
				Assessor: m.Assessor, Percent: m.Percent, ETA: m.ETA, CreatedAt: m.CreatedAt,
			})
		}
	}
	marks, err := s.store.Progress().History(tx, projectID, nil)
	if err != nil {
		return fmt.Errorf("export progress (project %s): %w", projectKey, err)
	}
	add("", marks)
	for _, t := range tasks {
		id := t.ID
		marks, err := s.store.Progress().History(tx, projectID, &id)
		if err != nil {
			return fmt.Errorf("export progress (%s): %w", t.Key, err)
		}
		add(t.Key, marks)
	}
	return nil
}

// exportChat walks the project's feed forward in insertion order — the
// feed's own ordering authority, Seq — so the document lists messages in the
// order import must recreate them, and a reply always follows the message it
// answers. Every protocol field travels, and so do the acceptances of the
// project's commands.
func (s *svc) exportChat(tx store.Tx, projectKey, projectID string, out *ExportDocument) error {
	var commands []string
	var after *domain.FeedCursor
	for {
		msgs, err := s.store.Chat().List(tx, store.ChatFilter{
			ProjectID: &projectID,
			Ascending: true,
			After:     after,
			Limit:     chatExportPage,
		})
		if err != nil {
			return fmt.Errorf("export chat (%s): %w", projectKey, err)
		}
		for _, m := range msgs {
			out.ChatMessages = append(out.ChatMessages, ExportChatMessage{
				ID: m.ID, Project: projectKey, Author: m.Author, Body: m.Body, CreatedAt: m.CreatedAt,
				Kind: m.Kind, AuthorTokenID: m.AuthorTokenID, Recipient: m.Recipient,
				ResolvedExecutor: m.ResolvedExecutor, ReplyTo: m.ReplyToID,
				IdempotencyKey: m.IdempotencyKey, Seq: m.Seq,
			})
			if m.Kind == domain.MessageCommand {
				commands = append(commands, m.ID)
			}
		}
		if len(msgs) < chatExportPage {
			break
		}
		last := msgs[len(msgs)-1]
		after = &domain.FeedCursor{Seq: last.Seq, ID: last.ID}
	}
	if len(commands) == 0 {
		return nil
	}
	accepted, err := s.store.Acceptances().ByMessages(tx, commands)
	if err != nil {
		return fmt.Errorf("export command acceptances (%s): %w", projectKey, err)
	}
	for _, id := range commands {
		acc, ok := accepted[id]
		if !ok {
			continue
		}
		out.CommandAcceptances = append(out.CommandAcceptances, ExportCommandAcceptance{
			ID: acc.ID, Project: projectKey, Message: acc.MessageID, AcceptedBy: acc.AcceptedByTokenID,
			TaskKeys: acc.TaskKeys, RequestHash: acc.RequestHash, CreatedAt: acc.CreatedAt,
		})
	}
	return nil
}

// exportJournal appends one project's whole lifecycle journal, re-keyed:
// column ids become names and task ids become keys. A dangling reference (a
// column or task hard-deleted since — task_history keeps no foreign key on
// those, by design) becomes an empty string rather than failing the export:
// the entry's kind and time are still worth keeping, and import leaves an
// unresolved name off.
func (s *svc) exportJournal(tx store.Tx, projectKey, projectID string, out *ExportDocument) error {
	entries, err := s.store.TaskHistory().ListByProject(tx, projectID)
	if err != nil {
		return fmt.Errorf("export journal (project %s): %w", projectKey, err)
	}
	taskKeys := make(map[string]string)
	lookupTask := func(id string) string {
		if id == "" {
			return ""
		}
		if k, ok := taskKeys[id]; ok {
			return k
		}
		k := ""
		if t, err := s.store.Tasks().GetByID(tx, id); err == nil {
			k = t.Key
		}
		taskKeys[id] = k
		return k
	}
	colNames := make(map[string]string)
	lookupColumn := func(id *string) string {
		if id == nil || *id == "" {
			return ""
		}
		if n, ok := colNames[*id]; ok {
			return n
		}
		n := ""
		if c, err := s.store.Columns().GetByID(tx, *id); err == nil {
			n = c.Name
		}
		colNames[*id] = n
		return n
	}
	for _, e := range entries {
		je := ExportJournalEntry{
			Project:       projectKey,
			Actor:         e.Actor,
			Kind:          string(e.Kind),
			TS:            e.TS,
			Reconstructed: e.Reconstructed,
			Archived:      e.Archived,
			FromColumn:    lookupColumn(e.FromColumn),
			ToColumn:      lookupColumn(e.ToColumn),
			OldEstimate:   e.OldEstimate,
			NewEstimate:   e.NewEstimate,
		}
		if e.TaskID != nil {
			je.Task = lookupTask(*e.TaskID)
		}
		if e.FromKind != nil {
			je.FromKind = string(*e.FromKind)
		}
		if e.ToKind != nil {
			je.ToKind = string(*e.ToKind)
		}
		if e.OldParent != nil {
			je.OldParent = lookupTask(*e.OldParent)
		}
		if e.NewParent != nil {
			je.NewParent = lookupTask(*e.NewParent)
		}
		out.Journal = append(out.Journal, je)
	}
	return nil
}

// sortByKeyNumber orders cards by their number. The store lists them by
// column id, a random UUID, so without this the per-card streams (notes,
// progress) would come out in a different order on every board holding the
// same cards, and two exports of one board could not be compared.
func sortByKeyNumber(tasks []*domain.Task) {
	num := func(t *domain.Task) int {
		_, n, _ := domain.ParseTaskKey(t.Key)
		return n
	}
	sort.SliceStable(tasks, func(i, j int) bool { return num(tasks[i]) < num(tasks[j]) })
}
