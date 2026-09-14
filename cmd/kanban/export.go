package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// exportDocument is the JSON wire format produced by `kanban export` and
// consumed by `kanban import`. It carries the same Projects payload as
// service.Board so callers that already read that field keep working, and
// adds the two append-only streams that the Board type never carried —
// progress marks and chat messages — with project / task keys (not
// internal UUIDs) so they survive the re-keying that happens on import.
//
// The reason this lives next to runExport rather than on service.Board:
// those two streams grow forever (KANB-29's whole point), the Board
// structure is what board_get publishes per-request, and silently widening
// every read would have turned every compact board read into a chart dump.
// The export/import path is the only consumer that needs the full thing.
type exportDocument struct {
	Projects      []service.BoardProject `json:"projects"`
	ProgressMarks []exportProgressMark   `json:"progress_marks"`
	ChatMessages  []exportChatMessage    `json:"chat_messages"`
}

// exportProgressMark carries the human-readable project and task keys so
// the import side can remap them to the freshly-created rows. ID is kept
// so the resulting mark has the same primary key as the source — that
// matters for any external system that referenced the mark by it, and it
// makes a diff between source and destination trivial.
type exportProgressMark struct {
	ID        string     `json:"id"`
	Project   string     `json:"project"`
	Task      string     `json:"task,omitempty"`
	Assessor  string     `json:"assessor"`
	Percent   int        `json:"percent"`
	ETA       *time.Time `json:"eta,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type exportChatMessage struct {
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// exportBoard builds the full export document: the same board the existing
// runExport already publishes, plus the progress marks and chat messages
// that the Board type never carried. The two extra streams are read through
// the store directly because that is the layer that owns the append-only
// tables and is the only place a full, unbounded read exists for them.
func exportBoard(ctx context.Context, svc service.Service, st store.Store, actor service.Actor, projectKey string) (*exportDocument, error) {
	board, err := svc.BoardGet(ctx, actor, service.BoardGetInput{
		ProjectKey: projectKey,
		View:       service.ViewTasks,
		DoneLimit:  domain.MaxDoneLimit,
		Include: service.Includes{
			service.IncludeBody,
			service.IncludeAcceptance,
			service.IncludeLinks,
			service.IncludeMetadata,
		},
	})
	if err != nil {
		return nil, err
	}
	if projectKey != "" && len(board.Projects) == 0 {
		return nil, domain.NotFound("project", projectKey)
	}

	out := &exportDocument{Projects: board.Projects}

	// The store is what the append-only tables live behind: pulling the
	// whole history through service.ProgressHistory would still hit the
	// right row set, but a paginated ChatList would cap each page at 100
	// and force the export to handle cursors. The store's List returns
	// the newest page first and accepts a cursor for the previous one,
	// which is the same loop ChatList runs — just here, in one place.
	err = st.Read(ctx, func(tx store.Tx) error {
		for _, bp := range board.Projects {
			p, err := st.Projects().GetByKey(tx, bp.Key)
			if err != nil {
				return fmt.Errorf("export project %s: %w", bp.Key, err)
			}
			if err := collectProgress(tx, st, bp, p.ID, out); err != nil {
				return err
			}
			if err := collectChat(tx, st, p.ID, bp.Key, out); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// collectProgress reads every mark of one project — both the project-level
// scope (task_id IS NULL) and every task scope — into the export document.
// Tasks are looked up via the Board payload's keys so an export of a
// project the actor can see never has to escape service-layer access checks.
func collectProgress(tx store.Tx, st store.Store, bp service.BoardProject, projectID string, out *exportDocument) error {
	projectMarks, err := st.Progress().History(tx, projectID, nil)
	if err != nil {
		return fmt.Errorf("export progress (project %s): %w", bp.Key, err)
	}
	for _, m := range projectMarks {
		out.ProgressMarks = append(out.ProgressMarks, exportProgressMark{
			ID:        m.ID,
			Project:   bp.Key,
			Assessor:  m.Assessor,
			Percent:   m.Percent,
			ETA:       m.ETA,
			CreatedAt: m.CreatedAt,
		})
	}
	for _, col := range bp.Columns {
		for _, tv := range col.Tasks {
			t, err := st.Tasks().GetByKey(tx, tv.Key)
			if err != nil {
				return fmt.Errorf("export task %s: %w", tv.Key, err)
			}
			marks, err := st.Progress().History(tx, projectID, &t.ID)
			if err != nil {
				return fmt.Errorf("export progress (%s): %w", tv.Key, err)
			}
			for _, m := range marks {
				out.ProgressMarks = append(out.ProgressMarks, exportProgressMark{
					ID:        m.ID,
					Project:   bp.Key,
					Task:      tv.Key,
					Assessor:  m.Assessor,
					Percent:   m.Percent,
					ETA:       m.ETA,
					CreatedAt: m.CreatedAt,
				})
			}
		}
	}
	return nil
}

// collectChat walks the project's chat history page by page (newest first)
// until a short page signals end-of-stream. ChatList's own 100-per-call cap
// is fine for the board UI; export wants everything, and the store's
// ChatRepo.List is the only place that returns a page bounded solely by the
// caller's Limit argument.
func collectChat(tx store.Tx, st store.Store, projectID, projectKey string, out *exportDocument) error {
	const pageSize = 100
	var cursor *domain.ChatCursor
	for {
		msgs, err := st.Chat().List(tx, store.ChatFilter{
			ProjectID: &projectID,
			Limit:     pageSize,
			Before:    cursor,
		})
		if err != nil {
			return fmt.Errorf("export chat (%s): %w", projectKey, err)
		}
		for _, m := range msgs {
			out.ChatMessages = append(out.ChatMessages, exportChatMessage{
				ID:        m.ID,
				Project:   projectKey,
				Author:    m.Author,
				Body:      m.Body,
				CreatedAt: m.CreatedAt,
			})
		}
		if len(msgs) < pageSize {
			return nil
		}
		last := msgs[len(msgs)-1]
		cursor = &domain.ChatCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
}

// importBoard reads an export document and re-creates it against the live
// store. The first pass uses service.* calls so the existing version /
// parent / acceptance / link rules are honoured on the new board; the
// second pass writes progress marks and chat messages with their original
// CreatedAt straight through the store, the one path that accepts a
// caller-supplied timestamp. service.ProgressSet / service.ChatAdd both
// stamp now() — a deliberate choice that keeps the MCP surface from
// letting an agent forge a backdated mark, and the reason the import path
// has to reach past them.
func importBoard(ctx context.Context, svc service.Service, st store.Store, actor service.Actor, raw []byte) error {
	var doc exportDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("import parse: %w", err)
	}
	if len(doc.Projects) == 0 {
		return fmt.Errorf("import: no projects found in input")
	}

	for _, bp := range doc.Projects {
		if err := importProject(ctx, svc, actor, bp, doc); err != nil {
			return err
		}
	}
	// progress + chat come after every project/tasks/link has been
	// re-created, so the references in the marks / messages resolve
	// against the destination board that the prior pass built.
	return importDated(ctx, st, actor, doc)
}

// importProject mirrors the body of the existing runImport's project
// loop. Kept verbatim so a project that round-trips through export-then-
// import looks exactly like one created by `kanban demo` followed by hand
// edits, including the second pass that sets acceptance on done tasks and
// the parent / links pass that turns bare task rows into a hierarchy.
func importProject(ctx context.Context, svc service.Service, actor service.Actor, bp service.BoardProject, doc exportDocument) error {
	cols := make([]service.ColumnSpec, len(bp.Columns))
	for i, c := range bp.Columns {
		cols[i] = service.ColumnSpec{
			Name:     c.Name,
			Kind:     c.Kind,
			WIPLimit: c.WIPLimit,
		}
	}
	desc := bp.Description
	if _, err := svc.ProjectUpsert(ctx, actor, service.ProjectUpsertInput{
		Mode:        service.UpsertCreate,
		Key:         bp.Key,
		Name:        bp.Name,
		Description: &desc,
		Columns:     cols,
		Settings: &service.ProjectSettings{
			EstimateUnit:        &bp.EstimateUnit,
			EnforceDependencies: &bp.EnforceDependencies,
			StrictDone:          &bp.StrictDone,
			ClaimTTLSeconds:     &bp.ClaimTTLSeconds,
		},
	}); err != nil {
		return fmt.Errorf("import project %s: %w", bp.Key, err)
	}

	// Original task IDs -> new task keys. The export side carries the
	// original IDs so a round-trip can reconstruct ParentID and the
	// link list, neither of which a freshly-minted key can be derived
	// from.
	idToKey := make(map[string]string, sumTaskCount(bp))
	for _, col := range bp.Columns {
		for _, tv := range col.Tasks {
			idToKey[tv.ID] = tv.Key
		}
	}

	keyMap := make(map[string]string, sumTaskCount(bp))
	versions := make(map[string]int, sumTaskCount(bp))
	type linkReq struct{ srcKey, dstKey string }
	var linksToCreate []linkReq
	type reparentReq struct{ taskKey, parentKey string }
	var reparents []reparentReq

	for _, col := range bp.Columns {
		for _, tv := range col.Tasks {
			acceptanceStrings := make([]string, 0, len(tv.Acceptance))
			for _, it := range tv.Acceptance {
				acceptanceStrings = append(acceptanceStrings, it.Text)
			}
			res, err := svc.TaskCreate(ctx, actor, service.TaskCreateInput{
				Tasks: []service.NewTask{{
					ProjectKey: bp.Key,
					Column:     col.Name,
					Title:      tv.Title,
					Body:       tv.Body,
					Type:       tv.Type,
					Priority:   tv.Priority,
					Tags:       tv.Tags,
					Estimate:   tv.Estimate,
					Actual:     tv.Actual,
					Assignee:   tv.Assignee,
					Reviewer:   tv.Reviewer,
					Acceptance: acceptanceStrings,
					DueAt:      tv.DueAt,
					Metadata:   tv.Metadata,
				}},
			})
			if err != nil {
				return fmt.Errorf("import task %s: %w", tv.Key, err)
			}
			if len(res.Tasks) == 0 {
				return fmt.Errorf("import task %s: no task returned", tv.Key)
			}
			createdTask := res.Tasks[0]
			keyMap[tv.Key] = createdTask.Key
			versions[createdTask.Key] = createdTask.Version

			hasDone := false
			for _, it := range tv.Acceptance {
				if it.Done {
					hasDone = true
					break
				}
			}
			if hasDone {
				updated, err := applyPatch(ctx, svc, actor, service.TaskPatch{
					Key:        createdTask.Key,
					IfVersion:  &createdTask.Version,
					Acceptance: tv.Acceptance,
				})
				if err != nil {
					return fmt.Errorf("import acceptance of %s: %w", createdTask.Key, err)
				}
				versions[createdTask.Key] = updated.Version
			}

			if tv.ParentID != nil {
				if pKey, ok := idToKey[*tv.ParentID]; ok {
					reparents = append(reparents, reparentReq{
						taskKey:   createdTask.Key,
						parentKey: pKey,
					})
				}
			}

			for _, blockerKey := range tv.BlockedBy {
				linksToCreate = append(linksToCreate, linkReq{srcKey: blockerKey, dstKey: tv.Key})
			}
		}
	}

	for _, r := range reparents {
		newParentKey, ok := keyMap[r.parentKey]
		if !ok {
			continue
		}
		version, ok := versions[r.taskKey]
		if !ok {
			return fmt.Errorf("import parent of %s: no version recorded for the created task", r.taskKey)
		}
		if _, err := applyPatch(ctx, svc, actor, service.TaskPatch{
			Key:       r.taskKey,
			IfVersion: &version,
			Parent:    service.FieldString{Set: true, Value: newParentKey},
		}); err != nil {
			return fmt.Errorf("import parent of %s: %w", r.taskKey, err)
		}
	}

	var addPairs []service.LinkPair
	for _, link := range linksToCreate {
		newSrc, okSrc := keyMap[link.srcKey]
		newDst, okDst := keyMap[link.dstKey]
		if okSrc && okDst {
			addPairs = append(addPairs, service.LinkPair{
				Blocker: newSrc,
				Blocked: newDst,
			})
		}
	}
	if len(addPairs) > 0 {
		if _, err := svc.TaskLink(ctx, actor, service.TaskLinkInput{Add: addPairs}); err != nil {
			return fmt.Errorf("import links for %s: %w", bp.Key, err)
		}
	}
	return nil
}

// sumTaskCount counts tasks across a board payload so the maps built
// during import have the right size hint. The export document's task list
// is the union of every column's tasks; one pass is enough.
func sumTaskCount(bp service.BoardProject) int {
	n := 0
	for _, c := range bp.Columns {
		n += len(c.Tasks)
	}
	return n
}

// importDated writes the progress marks and chat messages with their
// original timestamps. Both rows append under CreatedAt when the caller
// supplies one — exactly what store.Chat().Add and store.Progress().Add
// check at the top of their bodies. This is the only path in the system
// where that stamp can be forged: the MCP surface exposes only
// ProgressSet / ChatAdd, both of which stamp now(), and the service
// surface routes everything through those — by design, so an agent
// cannot smuggle a backdated claim into the chart.
func importDated(ctx context.Context, st store.Store, actor service.Actor, doc exportDocument) error {
	if len(doc.ProgressMarks) == 0 && len(doc.ChatMessages) == 0 {
		return nil
	}
	return st.Write(ctx, func(tx store.Tx) error {
		// Build the project-key -> project-id map once, so each mark /
		// message doesn't repeat the lookup. The service has already
		// upserted these projects in importProject.
		projID := make(map[string]string, len(doc.Projects))
		taskID := make(map[string]string)
		for _, bp := range doc.Projects {
			p, err := st.Projects().GetByKey(tx, bp.Key)
			if err != nil {
				return fmt.Errorf("import resolve project %s: %w", bp.Key, err)
			}
			projID[bp.Key] = p.ID
			for _, col := range bp.Columns {
				for _, tv := range col.Tasks {
					t, err := st.Tasks().GetByKey(tx, tv.Key)
					if err != nil {
						return fmt.Errorf("import resolve task %s: %w", tv.Key, err)
					}
					taskID[bp.Key+"/"+tv.Key] = t.ID
				}
			}
		}

		for _, m := range doc.ProgressMarks {
			pid, ok := projID[m.Project]
			if !ok {
				return fmt.Errorf("import progress mark %s: project %q not in document", m.ID, m.Project)
			}
			var taskIDPtr *string
			if m.Task != "" {
				tid, ok := taskID[m.Project+"/"+m.Task]
				if !ok {
					return fmt.Errorf("import progress mark %s: task %q not in document", m.ID, m.Task)
				}
				taskIDPtr = &tid
			}
			mark := &domain.ProgressMark{
				ID:        m.ID,
				ProjectID: pid,
				TaskID:    taskIDPtr,
				Assessor:  m.Assessor,
				Percent:   m.Percent,
				ETA:       m.ETA,
				CreatedAt: m.CreatedAt,
			}
			if err := st.Progress().Add(tx, mark); err != nil {
				return fmt.Errorf("import progress mark %s: %w", m.ID, err)
			}
		}

		for _, m := range doc.ChatMessages {
			pid, ok := projID[m.Project]
			if !ok {
				return fmt.Errorf("import chat %s: project %q not in document", m.ID, m.Project)
			}
			msg := &domain.ChatMessage{
				ID:        m.ID,
				ProjectID: pid,
				Author:    m.Author,
				Body:      m.Body,
				CreatedAt: m.CreatedAt,
			}
			if err := st.Chat().Add(tx, msg); err != nil {
				return fmt.Errorf("import chat %s: %w", m.ID, err)
			}
		}
		// Imported rows deliberately do not produce a domain event: the
		// source board already did, and a "restored progress mark" event
		// would lie about when it actually happened.
		_ = actor
		return nil
	})
}
