package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
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
	// Journal is the task lifecycle journal (KANB-30), re-keyed the same way
	// ProgressMarks and ChatMessages are: project and task KEYS instead of
	// internal UUIDs, and column NAMES instead of column ids, so a row can
	// be replayed against a freshly-created board whose ids will not match
	// the source's. Without this, moving a board to another machine would
	// zero out every history chart (KANB-31's replay) exactly the way an
	// export without progress marks or chat used to (KANB-29) — the same
	// defect, a third time, on data that did not exist yet when KANB-29 was
	// fixed.
	Journal []exportJournalEntry `json:"journal"`
	// HistoryStartsAt carries the instant from which the source's journal is
	// trustworthy (KANB-30's origin, set once by migration 0006). Import
	// must plant this same instant on the destination instead of leaving
	// the destination's own migration stamp standing — otherwise every
	// re-imported board would report that its journal became trustworthy on
	// the day it was moved, not the day it actually started.
	HistoryStartsAt time.Time `json:"history_starts_at"`

	// Truncated names every project whose done tasks did not all fit in
	// this export. board_get (and therefore this export, which reads the
	// board through the same service call) caps how many done tasks a
	// project hands back at domain.MaxDoneLimit — that is a deliberate,
	// service-layer invariant this command does not try to bypass. But
	// KANB-29's whole complaint is data loss nobody notices, so the gap is
	// named here (Included/Total come straight from BoardProject's own
	// DoneShown/DoneTotal, board_get's own accounting) instead of being
	// left for someone to discover as "fewer tasks than I remember" after
	// the fact: runExport also prints one warning line per entry to stderr.
	Truncated []exportTruncationNotice `json:"truncated,omitempty"`
}

// exportTruncationNotice records one project whose done tasks exceeded the
// export's capacity. Total is every done task the project actually has
// (BoardProject.DoneTotal); Included is how many of those made it into this
// export's Projects (BoardProject.DoneShown).
type exportTruncationNotice struct {
	Project  string `json:"project"`
	Included int    `json:"included"`
	Total    int    `json:"total"`
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

// exportJournalEntry is one row of the task lifecycle journal (KANB-30). Its
// ID is deliberately not carried across: task_history.id is a store-internal
// autoincrement with nothing else referencing it (unlike a progress mark or
// chat message id, which a caller could have recorded), so import is free to
// let the destination assign fresh ones. Everything else that matters to a
// replay travels: kind, timestamps, the reconstructed flag, and every value
// field, re-keyed like the rest of the document.
type exportJournalEntry struct {
	Project       string    `json:"project"`
	Task          string    `json:"task,omitempty"`
	Actor         string    `json:"actor"`
	Kind          string    `json:"kind"`
	TS            time.Time `json:"ts"`
	Reconstructed bool      `json:"reconstructed"`
	Archived      *bool     `json:"archived,omitempty"`
	FromColumn    string    `json:"from_column,omitempty"`
	FromKind      string    `json:"from_kind,omitempty"`
	ToColumn      string    `json:"to_column,omitempty"`
	ToKind        string    `json:"to_kind,omitempty"`
	OldEstimate   *float64  `json:"old_estimate,omitempty"`
	NewEstimate   *float64  `json:"new_estimate,omitempty"`
	OldParent     string    `json:"old_parent,omitempty"`
	NewParent     string    `json:"new_parent,omitempty"`
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
	for _, bp := range board.Projects {
		if bp.DoneTotal > bp.DoneShown {
			out.Truncated = append(out.Truncated, exportTruncationNotice{
				Project:  bp.Key,
				Included: bp.DoneShown,
				Total:    bp.DoneTotal,
			})
		}
	}

	// The store is what the append-only tables live behind: pulling the
	// whole history through service.ProgressHistory would still hit the
	// right row set, but a paginated ChatList would cap each page at 100
	// and force the export to handle cursors. The store's List returns
	// the newest page first and accepts a cursor for the previous one,
	// which is the same loop ChatList runs — just here, in one place.
	err = st.Read(ctx, func(tx store.Tx) error {
		// Origin is a single, store-wide row (task_history_origin has
		// exactly one), not per project — read it once here rather than
		// once per project in the loop below.
		origin, err := st.TaskHistory().Origin(tx)
		if err != nil {
			return fmt.Errorf("export journal origin: %w", err)
		}
		out.HistoryStartsAt = origin

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
			if err := collectJournal(tx, st, bp, p.ID, out); err != nil {
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

// collectJournal reads one project's whole lifecycle journal and appends it
// to the export document, re-keyed the way the rest of the document is:
// column ids become column names and task ids become task keys, resolved
// through the store's own GetByID lookups. Unlike collectProgress (which
// walks the board's visible columns) this reads the journal directly by
// project id, so an archived task's history travels too — the journal is
// the one place in this document that does not depend on what the board
// currently chooses to display.
//
// A dangling reference (a column or a parent task that has since been hard-
// deleted — task_history keeps no foreign key on those columns, by design,
// so a later admin delete does not silently rewrite history) resolves to an
// empty string rather than failing the whole export: the entry's kind, time
// and other fields are still worth keeping, and import treats an unresolved
// name as "leave that one field off" rather than an error.
func collectJournal(tx store.Tx, st store.Store, bp service.BoardProject, projectID string, out *exportDocument) error {
	entries, err := st.TaskHistory().ListByProject(tx, projectID)
	if err != nil {
		return fmt.Errorf("export journal (project %s): %w", bp.Key, err)
	}

	taskKeys := make(map[string]string)
	lookupTask := func(id string) string {
		if id == "" {
			return ""
		}
		if k, ok := taskKeys[id]; ok {
			return k
		}
		t, err := st.Tasks().GetByID(tx, id)
		if err != nil {
			// A hard-deleted task: task_history carries no FK on task_id
			// itself in every column (old_parent / new_parent have none at
			// all), so this is an honest gap, not a bug.
			taskKeys[id] = ""
			return ""
		}
		taskKeys[id] = t.Key
		return t.Key
	}

	colNames := make(map[string]string)
	lookupColumn := func(id *string) string {
		if id == nil || *id == "" {
			return ""
		}
		if n, ok := colNames[*id]; ok {
			return n
		}
		c, err := st.Columns().GetByID(tx, *id)
		if err != nil {
			colNames[*id] = ""
			return ""
		}
		colNames[*id] = c.Name
		return c.Name
	}

	for _, e := range entries {
		je := exportJournalEntry{
			Project:       bp.Key,
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
	raw, dropped, err := stripLegacyWIPLimits(raw)
	if err != nil {
		return fmt.Errorf("import parse: %w", err)
	}
	if dropped > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d column(s) in this export carry wip_limit; it was dropped on import because the board has no WIP limits since 26.09.2026\n", dropped)
	}
	doc, err := decodeExportDocument(raw)
	if err != nil {
		return err
	}
	if len(doc.Projects) == 0 {
		return fmt.Errorf("import: no projects found in input")
	}
	if err := validateImportReferences(doc); err != nil {
		return err
	}

	for _, bp := range doc.Projects {
		if err := importProject(ctx, svc, actor, bp, doc); err != nil {
			return err
		}
	}
	// progress + chat come after every project/tasks/link has been
	// re-created, so the references in the marks / messages resolve
	// against the destination board that the prior pass built.
	if err := importDated(ctx, st, actor, doc); err != nil {
		return err
	}
	// The journal comes last: it has to clean up after every service call
	// above, each of which wrote its own (import-time) journal entries as
	// an unavoidable store-layer side effect.
	return importJournal(ctx, st, doc)
}

// stripLegacyWIPLimits removes the retired per-column WIP limit from an
// export made before 26.09.2026, when the board still had WIP limits
// (KANB-59). decodeExportDocument refuses unknown fields — a typo must not
// silently discard history — so without this every backup taken before the
// removal would stop importing. Old exports spell the field the way
// encoding/json wrote an untagged struct field ("WIPLimit", null on every
// column without a limit); the match ignores case and underscores so
// "wip_limit" is covered too, exactly as the decoder would have matched it.
// The returned count is the columns that carried a real (non-null) limit —
// those are reported on stderr, never dropped silently.
func stripLegacyWIPLimits(raw []byte) ([]byte, int, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Not an object at all: let the strict decoder produce the error.
		return raw, 0, nil
	}
	projKey, ok := foldKey(doc, "projects")
	if !ok {
		return raw, 0, nil
	}
	var projects []map[string]json.RawMessage
	if json.Unmarshal(doc[projKey], &projects) != nil {
		return raw, 0, nil
	}
	stripped, limited := 0, 0
	for _, proj := range projects {
		colKey, ok := foldKey(proj, "columns")
		if !ok {
			continue
		}
		var cols []map[string]json.RawMessage
		if json.Unmarshal(proj[colKey], &cols) != nil {
			continue
		}
		changed := false
		for _, col := range cols {
			for k, v := range col {
				if strings.ReplaceAll(strings.ToLower(k), "_", "") != "wiplimit" {
					continue
				}
				if s := strings.TrimSpace(string(v)); s != "" && s != "null" {
					limited++
				}
				delete(col, k)
				stripped++
				changed = true
			}
		}
		if changed {
			b, err := json.Marshal(cols)
			if err != nil {
				return nil, 0, err
			}
			proj[colKey] = b
		}
	}
	if stripped == 0 {
		return raw, 0, nil
	}
	b, err := json.Marshal(projects)
	if err != nil {
		return nil, 0, err
	}
	doc[projKey] = b
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, 0, err
	}
	return out, limited, nil
}

// foldKey finds the key in m that encoding/json would match to name
// (case-insensitively), the way the strict decoder resolves it.
func foldKey(m map[string]json.RawMessage, name string) (string, bool) {
	if _, ok := m[name]; ok {
		return name, true
	}
	for k := range m {
		if strings.EqualFold(k, name) {
			return k, true
		}
	}
	return "", false
}

// decodeExportDocument treats an export as a closed wire contract. Missing
// newer fields remain compatible with old exports; an unknown field is a typo
// and must not silently discard history.
func decodeExportDocument(raw []byte) (exportDocument, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc exportDocument
	if err := dec.Decode(&doc); err != nil {
		return exportDocument{}, fmt.Errorf("import parse: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return exportDocument{}, fmt.Errorf("import parse: multiple JSON documents are not allowed")
		}
		return exportDocument{}, fmt.Errorf("import parse: %w", err)
	}
	return doc, nil
}

// validateImportReferences refuses dangling history before the first service
// write. Full cross-layer atomicity needs a service transaction contract; this
// preflight at least guarantees malformed history cannot leave a new board
// behind before importDated discovers it.
func validateImportReferences(doc exportDocument) error {
	projects := make(map[string]struct{}, len(doc.Projects))
	tasks := make(map[string]struct{})
	for _, bp := range doc.Projects {
		projects[bp.Key] = struct{}{}
		for _, col := range bp.Columns {
			for _, task := range col.Tasks {
				tasks[bp.Key+"/"+task.Key] = struct{}{}
			}
		}
	}
	for _, mark := range doc.ProgressMarks {
		if _, ok := projects[mark.Project]; !ok {
			return fmt.Errorf("import progress mark %s: project %q not in document", mark.ID, mark.Project)
		}
		if mark.Task != "" {
			if _, ok := tasks[mark.Project+"/"+mark.Task]; !ok {
				return fmt.Errorf("import progress mark %s: task %q not in document project %q", mark.ID, mark.Task, mark.Project)
			}
		}
	}
	for _, msg := range doc.ChatMessages {
		if _, ok := projects[msg.Project]; !ok {
			return fmt.Errorf("import chat message %s: project %q not in document", msg.ID, msg.Project)
		}
	}
	return nil
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
			Name: c.Name,
			Kind: c.Kind,
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

// importJournal restores the task lifecycle journal (KANB-30) after every
// project, task and link has been re-created. Each of those creations wrote
// its own 'created' / 'moved' / ... journal entries as a store-layer side
// effect stamped at the IMPORT instant (see TaskHistoryRepo.DeleteByProject
// for why that cannot be suppressed). This clears that noise per project —
// only once each, and only for projects this document actually describes —
// then replays the ORIGINAL entries from the document with their original
// timestamps, re-keyed back to the destination's ids. Finally it lowers the
// store's origin to the source's, if the source started earlier, so a board
// moved to a new machine does not report its history became trustworthy on
// moving day.
//
// A legacy document with neither a journal nor a history_starts_at (an
// export from before KANB-32) leaves everything alone: the import-time
// noise from project/task creation is exactly what every import produced
// before this feature existed, and there is nothing here to restore over
// it.
func importJournal(ctx context.Context, st store.Store, doc exportDocument) error {
	if len(doc.Journal) == 0 && doc.HistoryStartsAt.IsZero() {
		return nil
	}
	return st.Write(ctx, func(tx store.Tx) error {
		projID := make(map[string]string, len(doc.Projects))
		taskID := make(map[string]string)
		colID := make(map[string]string)
		for _, bp := range doc.Projects {
			p, err := st.Projects().GetByKey(tx, bp.Key)
			if err != nil {
				return fmt.Errorf("import journal resolve project %s: %w", bp.Key, err)
			}
			projID[bp.Key] = p.ID

			cols, err := st.Columns().ListByProject(tx, p.ID)
			if err != nil {
				return fmt.Errorf("import journal resolve columns of %s: %w", bp.Key, err)
			}
			for _, c := range cols {
				colID[bp.Key+"/"+c.Name] = c.ID
			}

			for _, col := range bp.Columns {
				for _, tv := range col.Tasks {
					t, err := st.Tasks().GetByKey(tx, tv.Key)
					if err != nil {
						return fmt.Errorf("import journal resolve task %s: %w", tv.Key, err)
					}
					taskID[bp.Key+"/"+tv.Key] = t.ID
				}
			}
		}

		// Every project this document describes gets its import-time noise
		// wiped exactly once, whether or not it turns out to have any
		// journal entries of its own: a project whose SOURCE journal was
		// genuinely empty must end up with an empty journal here too, not
		// with the side-effect rows TaskCreate happened to write.
		for _, bp := range doc.Projects {
			pid, ok := projID[bp.Key]
			if !ok {
				continue
			}
			if err := st.TaskHistory().DeleteByProject(tx, pid); err != nil {
				return fmt.Errorf("import journal: clear project %s: %w", bp.Key, err)
			}
		}

		for _, e := range doc.Journal {
			pid, ok := projID[e.Project]
			if !ok {
				return fmt.Errorf("import journal entry: project %q not in document", e.Project)
			}
			entry := &store.TaskHistoryEntry{
				TS:            e.TS,
				Actor:         e.Actor,
				ProjectID:     pid,
				Kind:          store.TaskHistoryKind(e.Kind),
				Reconstructed: e.Reconstructed,
				Archived:      e.Archived,
				OldEstimate:   e.OldEstimate,
				NewEstimate:   e.NewEstimate,
			}
			if e.Task != "" {
				tid, ok := taskID[e.Project+"/"+e.Task]
				if !ok {
					return fmt.Errorf("import journal entry: task %q not in document (project %s)", e.Task, e.Project)
				}
				entry.TaskID = &tid
			}
			if e.FromColumn != "" {
				if cid, ok := colID[e.Project+"/"+e.FromColumn]; ok {
					entry.FromColumn = &cid
				}
			}
			if e.ToColumn != "" {
				if cid, ok := colID[e.Project+"/"+e.ToColumn]; ok {
					entry.ToColumn = &cid
				}
			}
			if e.FromKind != "" {
				k := domain.Kind(e.FromKind)
				entry.FromKind = &k
			}
			if e.ToKind != "" {
				k := domain.Kind(e.ToKind)
				entry.ToKind = &k
			}
			if e.OldParent != "" {
				if tid, ok := taskID[e.Project+"/"+e.OldParent]; ok {
					entry.OldParent = &tid
				}
			}
			if e.NewParent != "" {
				if tid, ok := taskID[e.Project+"/"+e.NewParent]; ok {
					entry.NewParent = &tid
				}
			}
			if err := st.TaskHistory().Append(tx, entry); err != nil {
				return fmt.Errorf("import journal entry (project %s): %w", e.Project, err)
			}
		}

		if err := st.TaskHistory().LowerOrigin(tx, doc.HistoryStartsAt); err != nil {
			return fmt.Errorf("import journal: lower origin: %w", err)
		}
		return nil
	})
}
