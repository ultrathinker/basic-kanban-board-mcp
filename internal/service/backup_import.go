package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// Import restores an export document into this board in ONE write
// transaction: every project, column, card, link, note, progress mark,
// message, acceptance and journal row lands together or not at all. A backup
// that half-restores is worse than none — the owner finds out at the moment
// they need it — so a failure anywhere, however late, rolls everything back.
//
// Import never merges. If any project of the document already exists here,
// it refuses before writing anything and names those projects.
//
// Cards come back as they were, not as new cards: key, version, created /
// updated / column-entered / started / done / archived times and authors are
// the source's, so idle time and the attention line are right on the first
// read after a restore. Two things are deliberately NOT restored, and both
// are reported rather than hidden: a lease (it belonged to an agent process
// of the source board; restoring it would lock the card to someone who is
// not running), and any token reference the destination cannot resolve (see
// ImportResult.Warnings).
func (s *svc) Import(ctx context.Context, a Actor, doc *ExportDocument) (*ImportResult, error) {
	if err := requireAdmin(a, "import a board export"); err != nil {
		return nil, err
	}
	if doc == nil || len(doc.Projects) == 0 {
		return nil, domain.Invalid("projects", "the export contains no projects",
			"Pass a document written by kanban export or the web Export button.")
	}
	if err := validateImportDocument(doc); err != nil {
		return nil, err
	}
	chat, err := importChatOrder(doc.ChatMessages)
	if err != nil {
		return nil, err
	}

	ctx = store.WithActor(ctx, a.Name)
	var result *ImportResult
	var pending []domain.Event
	err = s.store.Write(ctx, func(tx store.Tx) error {
		pending = pending[:0]
		if err := s.refuseExistingProjects(tx, doc); err != nil {
			return err
		}
		im := &importer{
			s: s, tx: tx, a: a, doc: doc, pending: &pending,
			projectID: map[string]string{},
			taskID:    map[string]string{},
			tokens:    map[string]bool{},
		}
		for _, bp := range doc.Projects {
			if err := im.project(bp); err != nil {
				return err
			}
		}
		if err := im.dated(chat); err != nil {
			return err
		}
		if err := im.acceptances(); err != nil {
			return err
		}
		if err := im.journal(); err != nil {
			return err
		}
		result = &ImportResult{Warnings: im.warnings}
		for _, bp := range doc.Projects {
			result.Projects = append(result.Projects, strings.ToUpper(bp.Key))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishAll(pending)
	return result, nil
}

// refuseExistingProjects is the no-merge rule, checked inside the import's
// own transaction so no project can appear between the check and the write.
func (s *svc) refuseExistingProjects(tx store.Tx, doc *ExportDocument) error {
	var existing []string
	for _, bp := range doc.Projects {
		p, err := s.store.Projects().GetByKey(tx, bp.Key)
		if isNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		existing = append(existing, fmt.Sprintf("%s (%s)", p.Key, p.Name))
	}
	if len(existing) == 0 {
		return nil
	}
	return &domain.Error{
		Code: domain.CodeConflict,
		Message: fmt.Sprintf("this board already has %d of the export's projects: %s; nothing was imported",
			len(existing), strings.Join(existing, ", ")),
		Remediation: "Import restores whole projects and never merges into an existing one. Import into an empty data directory (kanban import --data <new dir>), or export without those projects.",
	}
}

// importer carries the id maps one import builds as it goes. Every map is
// keyed by the document's own spelling, "PROJECT/TASK-KEY" for tasks, so the
// history streams resolve exactly the names the document uses.
type importer struct {
	s       *svc
	tx      store.Tx
	a       Actor
	doc     *ExportDocument
	pending *[]domain.Event

	projectID map[string]string // doc project key -> new project id
	taskID    map[string]string // "PROJECT/KEY" -> new task id
	tokens    map[string]bool   // tokens.id -> exists on this board
	warnings  []string
}

func (im *importer) warnf(format string, args ...any) {
	im.warnings = append(im.warnings, fmt.Sprintf(format, args...))
}

// tokenExists answers whether a tokens.id resolves on this board, once per id.
func (im *importer) tokenExists(id string) (bool, error) {
	if ok, seen := im.tokens[id]; seen {
		return ok, nil
	}
	_, err := im.s.store.Tokens().GetByID(im.tx, id)
	if err != nil && !isNotFound(err) {
		return false, err
	}
	im.tokens[id] = err == nil
	return err == nil, nil
}

// importCard is one card of a project, live or archived, and where it goes.
type importCard struct {
	tv       domain.TaskView
	column   *domain.Column
	archived bool
	position int // 1-based position in its column, for a card without a rank
}

// project restores one project: the project and its columns through the same
// projectCreate a project_upsert uses, then its cards, hierarchy, links,
// focus and card counter.
func (im *importer) project(bp BoardProject) error {
	s, tx := im.s, im.tx
	key, err := domain.ValidateProjectKey(bp.Key)
	if err != nil {
		return fmt.Errorf("import project %q: %w", bp.Key, err)
	}
	specs := make([]ColumnSpec, len(bp.Columns))
	for i, c := range bp.Columns {
		specs[i] = ColumnSpec{Name: c.Name, Kind: c.Kind}
	}
	desc := bp.Description
	settings := &ProjectSettings{
		EstimateUnit:        &bp.EstimateUnit,
		EnforceDependencies: &bp.EnforceDependencies,
		StrictDone:          &bp.StrictDone,
		ClaimTTLSeconds:     &bp.ClaimTTLSeconds,
		IdleAfterSeconds:    optionalSeconds(bp.IdleAfterSeconds),
	}
	if err := im.coordinator(key, bp.Coordinator, settings); err != nil {
		return err
	}
	p, cols, err := s.projectCreate(tx, im.pending, im.a, key, ProjectUpsertInput{
		Mode: UpsertCreate, Key: key, Name: bp.Name, Description: &desc, Columns: specs, Settings: settings,
	})
	if err != nil {
		return fmt.Errorf("import project %s: %w", key, err)
	}
	im.projectID[bp.Key] = p.ID
	colByName := make(map[string]*domain.Column, len(cols))
	for i := range cols {
		colByName[cols[i].Name] = &cols[i]
	}

	var cards []importCard
	for _, bc := range bp.Columns {
		col := colByName[bc.Name]
		for i, tv := range bc.Tasks {
			cards = append(cards, importCard{tv: tv, column: col, position: i + 1})
		}
	}
	for _, at := range im.doc.ArchivedTasks {
		if at.Project != bp.Key {
			continue
		}
		// An archived card goes back to the column it was archived from;
		// a column removed since then falls back to the first one, which
		// changes nothing visible because the card stays archived.
		col, ok := colByName[at.Task.ColumnName]
		if !ok {
			col = &cols[0]
		}
		cards = append(cards, importCard{tv: at.Task, column: col, archived: true})
	}

	keyOfOldID := make(map[string]string, len(cards))
	for _, c := range cards {
		im.taskID[bp.Key+"/"+c.tv.Key] = newID()
		if c.tv.ID != "" {
			keyOfOldID[c.tv.ID] = c.tv.Key
		}
	}
	// A parent must exist before its subtask references it. Subtasks are one
	// level deep (MaxSubtaskDepth), so "parents first" is a single split.
	sort.SliceStable(cards, func(i, j int) bool {
		return cards[i].tv.ParentID == nil && cards[j].tv.ParentID != nil
	})

	maxSeq := 0
	for _, c := range cards {
		t, seq, err := im.card(bp.Key, p, c, keyOfOldID)
		if err != nil {
			return err
		}
		if err := s.store.Tasks().Create(tx, t); err != nil {
			return fmt.Errorf("import task %s: %w", c.tv.Key, err)
		}
		if seq > maxSeq {
			maxSeq = seq
		}
	}

	// Edges are rebuilt from the blocker's side: Blocks lists every edge,
	// while BlockedBy lists only blockers still open (KANB-62).
	for _, c := range cards {
		for _, blocked := range c.tv.Blocks {
			blockedID, ok := im.taskID[bp.Key+"/"+blocked]
			if !ok {
				im.warnf("task %s: the link to %s was not restored because %s is not in the export", c.tv.Key, blocked, blocked)
				continue
			}
			if err := s.store.Links().Add(tx, &domain.Link{
				BlockerID: im.taskID[bp.Key+"/"+c.tv.Key], BlockedID: blockedID,
				Type: domain.LinkBlocks, CreatedBy: im.a.Name,
			}); err != nil {
				return fmt.Errorf("import link %s -> %s: %w", c.tv.Key, blocked, err)
			}
		}
	}

	if bp.FocusKey != "" {
		if id, ok := im.liveTaskID(bp, bp.FocusKey); ok {
			if err := s.store.Projects().SetFocus(tx, p.ID, &id); err != nil {
				return fmt.Errorf("import project %s: focus: %w", key, err)
			}
		} else {
			im.warnf("project %s: focus %s was not restored because it is not a live card of the export", key, bp.FocusKey)
		}
	}

	next := im.doc.NextTaskSeq[bp.Key]
	if next <= maxSeq {
		next = maxSeq + 1
	}
	if err := s.store.Projects().SetNextTaskSeq(tx, p.ID, next); err != nil {
		return fmt.Errorf("import project %s: next task number: %w", key, err)
	}
	if bp.Archived {
		if err := s.store.Projects().Archive(tx, p.ID, true); err != nil {
			return fmt.Errorf("import project %s: archive: %w", key, err)
		}
	}
	return nil
}

// liveTaskID resolves a key among the project's live (board) cards only.
func (im *importer) liveTaskID(bp BoardProject, key string) (string, bool) {
	for _, c := range bp.Columns {
		for _, tv := range c.Tasks {
			if strings.EqualFold(tv.Key, key) {
				return im.taskID[bp.Key+"/"+tv.Key], true
			}
		}
	}
	return "", false
}

// coordinator restores the project's coordinator when its token exists here
// and could be appointed today; otherwise the project is created without one
// and the import says so. Refusing the whole restore over it would make a
// backup unrestorable on any board that does not share the source's tokens.
func (im *importer) coordinator(key string, c *Participant, settings *ProjectSettings) error {
	if c == nil || c.TokenID == "" {
		return nil
	}
	id := c.TokenID
	probe := &domain.Project{Key: key, CoordinatorTokenID: id}
	err := im.s.validateCoordinator(im.tx, probe, &ProjectSettings{Coordinator: &id})
	if err == nil {
		settings.Coordinator = &id
		return nil
	}
	de := domain.AsError(err)
	if de == nil {
		return err
	}
	im.warnf("project %s: coordinator %s was not restored: %s", key, c.Name, de.Message)
	return nil
}

// card builds the stored row of one exported card. It runs the same field
// validation task_create does, so a hand-edited export cannot plant a card
// the tools would refuse, and it keeps every stored value the source had
// except the id (fresh, mapped by key) and the lease (see Import).
func (im *importer) card(projectKey string, p *domain.Project, c importCard, keyOfOldID map[string]string) (*domain.Task, int, error) {
	tv := c.tv
	keyProject, seq, err := domain.ParseTaskKey(tv.Key)
	if err != nil || !strings.EqualFold(keyProject, p.Key) {
		return nil, 0, domain.Invalid("task", fmt.Sprintf("task %s does not belong to project %s", tv.Key, p.Key),
			"Every card of a project must carry that project's key prefix; import an unmodified export.")
	}
	typ := tv.Type
	if typ == "" {
		typ = domain.TypeTask
	}
	texts := make([]string, len(tv.Acceptance))
	for i, it := range tv.Acceptance {
		texts[i] = it.Text
	}
	if err := validateNewTask(NewTask{
		ProjectKey: p.Key, Title: tv.Title, Body: tv.Body, Type: typ, Priority: tv.Priority,
		Tags: tv.Tags, Assignee: tv.Assignee, Reviewer: tv.Reviewer, Acceptance: texts,
		Outcome: outcomePtr(tv.Outcome),
	}); err != nil {
		return nil, 0, fmt.Errorf("import task %s: %w", tv.Key, err)
	}
	tags, err := domain.NormalizeTags(tv.Tags)
	if err != nil {
		return nil, 0, fmt.Errorf("import task %s: %w", tv.Key, err)
	}

	t := &domain.Task{
		ID:              im.taskID[projectKey+"/"+tv.Key],
		Key:             domain.TaskKey(p.Key, seq),
		ProjectID:       p.ID,
		ColumnID:        c.column.ID,
		Rank:            tv.Rank,
		Title:           tv.Title,
		Body:            tv.Body,
		Type:            typ,
		Priority:        tv.Priority,
		Estimate:        tv.Estimate,
		Actual:          tv.Actual,
		Tags:            tags,
		Assignee:        tv.Assignee,
		Reviewer:        tv.Reviewer,
		Outcome:         outcomeOrOpen(outcomePtr(tv.Outcome)),
		Conclusion:      tv.Conclusion,
		Acceptance:      append([]domain.AcceptanceItem(nil), tv.Acceptance...),
		DueAt:           tv.DueAt,
		ColumnEnteredAt: tv.ColumnEnteredAt,
		StartedAt:       tv.StartedAt,
		DoneAt:          tv.DoneAt,
		Version:         tv.Version,
		Metadata:        tv.Metadata,
		CreatedAt:       tv.CreatedAt,
		UpdatedAt:       tv.UpdatedAt,
		CreatedBy:       tv.CreatedBy,
		UpdatedBy:       tv.UpdatedBy,
	}
	if t.Rank == 0 {
		// A document without ranks still keeps its column order.
		t.Rank = int64(c.position) * domain.RankStep
	}
	if t.CreatedBy == "" {
		t.CreatedBy = im.a.Name
	}
	if c.archived {
		t.ArchivedAt = tv.ArchivedAt
		if t.ArchivedAt == nil {
			// Listed as archived but without the instant: the only honest
			// time left is the restore itself.
			now, err := im.tx.Now()
			if err != nil {
				return nil, 0, err
			}
			t.ArchivedAt = &now
		}
	}
	if tv.ParentID != nil {
		if parentKey, ok := keyOfOldID[*tv.ParentID]; ok {
			id := im.taskID[projectKey+"/"+parentKey]
			t.ParentID = &id
		} else {
			im.warnf("task %s: its parent was not restored because the parent card is not in the export", tv.Key)
		}
	}
	return t, seq, nil
}

// dated writes the append-only streams with their original timestamps:
// progress marks, notes and the message feed. This is the only path that
// may backdate these rows; the tools stamp the database clock.
func (im *importer) dated(chat []ExportChatMessage) error {
	s, tx := im.s, im.tx
	for _, m := range im.doc.ProgressMarks {
		var taskID *string
		if m.Task != "" {
			id := im.taskID[m.Project+"/"+m.Task]
			taskID = &id
		}
		if err := s.store.Progress().Add(tx, &domain.ProgressMark{
			ID: m.ID, ProjectID: im.projectID[m.Project], TaskID: taskID,
			Assessor: m.Assessor, Percent: m.Percent, ETA: m.ETA, CreatedAt: m.CreatedAt,
		}); err != nil {
			return fmt.Errorf("import progress mark %s: %w", m.ID, err)
		}
	}
	for _, n := range im.doc.Notes {
		if err := s.store.Notes().Add(tx, &domain.Note{
			ID: n.ID, TaskID: im.taskID[n.Project+"/"+n.Task], Author: n.Author, Body: n.Body, CreatedAt: n.CreatedAt,
		}); err != nil {
			return fmt.Errorf("import note %s: %w", n.ID, err)
		}
	}

	unknownAuthors := 0
	for _, m := range chat {
		author := ""
		if m.AuthorTokenID != "" {
			ok, err := im.tokenExists(m.AuthorTokenID)
			if err != nil {
				return err
			}
			if ok {
				author = m.AuthorTokenID
			} else {
				unknownAuthors++
			}
		}
		if err := s.store.Chat().Add(tx, &domain.ChatMessage{
			ID: m.ID, ProjectID: im.projectID[m.Project], Author: m.Author, Body: m.Body, CreatedAt: m.CreatedAt,
			Kind: m.Kind, AuthorTokenID: author, Recipient: m.Recipient, ResolvedExecutor: m.ResolvedExecutor,
			ReplyToID: m.ReplyTo, IdempotencyKey: m.IdempotencyKey,
		}); err != nil {
			return fmt.Errorf("import chat message %s: %w", m.ID, err)
		}
	}
	if unknownAuthors > 0 {
		im.warnf("%d chat message(s) were posted by a token that does not exist on this board; they keep their signature, but not the author token", unknownAuthors)
	}
	return nil
}

// acceptances restores which commands were already accepted, so a restored
// command cannot be accepted a second time. The acceptor is a foreign key to
// tokens: a record whose acceptor does not exist here cannot be stored, and
// the import names that command instead of hiding the gap.
func (im *importer) acceptances() error {
	for _, ea := range im.doc.CommandAcceptances {
		ok, err := im.tokenExists(ea.AcceptedBy)
		if err != nil {
			return err
		}
		if !ok {
			im.warnf("command %s: its acceptance was not restored because the accepting token does not exist on this board; the command can be accepted again", ea.Message)
			continue
		}
		var ids, keys []string
		for _, k := range ea.TaskKeys {
			id, found := im.taskID[ea.Project+"/"+k]
			if !found {
				im.warnf("command %s: accepted task %s is not in the export and is left out of the restored acceptance", ea.Message, k)
				continue
			}
			ids = append(ids, id)
			keys = append(keys, k)
		}
		id := ea.ID
		if id == "" {
			id = newID()
		}
		if err := im.s.store.Acceptances().Put(im.tx, &domain.CommandAcceptance{
			ID: id, MessageID: ea.Message, ProjectID: im.projectID[ea.Project], AcceptedByTokenID: ea.AcceptedBy,
			TaskIDs: ids, TaskKeys: keys, RequestHash: ea.RequestHash, CreatedAt: ea.CreatedAt,
		}); err != nil {
			return fmt.Errorf("import acceptance of command %s: %w", ea.Message, err)
		}
	}
	return nil
}

// journal replaces the journal rows the card inserts above wrote as a store
// side effect (stamped with the source's creation instants but describing a
// card created straight into its final column) with the source's own journal,
// then lowers the store's origin to the source's.
//
// A legacy document with neither a journal nor a history_starts_at (before
// KANB-32) leaves those side-effect rows alone: there is nothing to restore
// over them.
func (im *importer) journal() error {
	s, tx, doc := im.s, im.tx, im.doc
	if len(doc.Journal) == 0 && doc.HistoryStartsAt.IsZero() {
		return nil
	}
	colID := make(map[string]string)
	for _, bp := range doc.Projects {
		pid := im.projectID[bp.Key]
		cols, err := s.store.Columns().ListByProject(tx, pid)
		if err != nil {
			return fmt.Errorf("import journal: columns of %s: %w", bp.Key, err)
		}
		for _, c := range cols {
			colID[bp.Key+"/"+c.Name] = c.ID
		}
		if err := s.store.TaskHistory().DeleteByProject(tx, pid); err != nil {
			return fmt.Errorf("import journal: clear project %s: %w", bp.Key, err)
		}
	}
	optional := func(m map[string]string, k string) *string {
		if v, ok := m[k]; ok {
			return &v
		}
		return nil
	}
	for _, e := range doc.Journal {
		pid, ok := im.projectID[e.Project]
		if !ok {
			return domain.Invalid("journal", fmt.Sprintf("a journal entry names project %q, which is not in the export", e.Project),
				"Import an unmodified export.")
		}
		entry := &store.TaskHistoryEntry{
			TS: e.TS, Actor: e.Actor, ProjectID: pid, Kind: store.TaskHistoryKind(e.Kind),
			Reconstructed: e.Reconstructed, Archived: e.Archived,
			OldEstimate: e.OldEstimate, NewEstimate: e.NewEstimate,
		}
		if e.Task != "" {
			tid, ok := im.taskID[e.Project+"/"+e.Task]
			if !ok {
				return domain.Invalid("journal", fmt.Sprintf("a journal entry names task %q, which is not in project %s of the export", e.Task, e.Project),
					"Import an unmodified export.")
			}
			entry.TaskID = &tid
		}
		if e.FromColumn != "" {
			entry.FromColumn = optional(colID, e.Project+"/"+e.FromColumn)
		}
		if e.ToColumn != "" {
			entry.ToColumn = optional(colID, e.Project+"/"+e.ToColumn)
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
			entry.OldParent = optional(im.taskID, e.Project+"/"+e.OldParent)
		}
		if e.NewParent != "" {
			entry.NewParent = optional(im.taskID, e.Project+"/"+e.NewParent)
		}
		if err := s.store.TaskHistory().Append(tx, entry); err != nil {
			return fmt.Errorf("import journal entry (project %s): %w", e.Project, err)
		}
	}
	if err := s.store.TaskHistory().LowerOrigin(tx, doc.HistoryStartsAt); err != nil {
		return fmt.Errorf("import journal: lower origin: %w", err)
	}
	return nil
}

// validateImportDocument refuses a document whose references do not close
// before the transaction even opens: a mark, note, message or acceptance that
// names a project or card the document does not contain, a card listed twice,
// or a card key that does not belong to its project.
func validateImportDocument(doc *ExportDocument) error {
	bad := func(field, msg string) error {
		return domain.Invalid(field, msg, "Import an unmodified export; a hand-edited document must keep every reference inside itself.")
	}
	projects := make(map[string]bool, len(doc.Projects))
	tasks := make(map[string]bool)
	for _, bp := range doc.Projects {
		if projects[bp.Key] {
			return bad("projects", fmt.Sprintf("project %s is listed twice", bp.Key))
		}
		if strings.TrimSpace(bp.Name) == "" {
			return bad("projects", fmt.Sprintf("project %s has no name", bp.Key))
		}
		projects[bp.Key] = true
		for _, col := range bp.Columns {
			for _, tv := range col.Tasks {
				if tasks[bp.Key+"/"+tv.Key] {
					return bad("projects", fmt.Sprintf("task %s is listed twice", tv.Key))
				}
				tasks[bp.Key+"/"+tv.Key] = true
			}
		}
	}
	for _, at := range doc.ArchivedTasks {
		if !projects[at.Project] {
			return bad("archived_tasks", fmt.Sprintf("archived task %s names project %q, which is not in the export", at.Task.Key, at.Project))
		}
		if tasks[at.Project+"/"+at.Task.Key] {
			return bad("archived_tasks", fmt.Sprintf("task %s is listed twice", at.Task.Key))
		}
		tasks[at.Project+"/"+at.Task.Key] = true
	}
	for _, m := range doc.ProgressMarks {
		if !projects[m.Project] {
			return bad("progress_marks", fmt.Sprintf("progress mark %s names project %q, which is not in the export", m.ID, m.Project))
		}
		if m.Task != "" && !tasks[m.Project+"/"+m.Task] {
			return bad("progress_marks", fmt.Sprintf("progress mark %s names task %q, which is not in project %s of the export", m.ID, m.Task, m.Project))
		}
	}
	for _, n := range doc.Notes {
		if !tasks[n.Project+"/"+n.Task] {
			return bad("notes", fmt.Sprintf("note %s names task %q, which is not in project %s of the export", n.ID, n.Task, n.Project))
		}
	}
	commands := make(map[string]string)
	for _, m := range doc.ChatMessages {
		if !projects[m.Project] {
			return bad("chat_messages", fmt.Sprintf("chat message %s names project %q, which is not in the export", m.ID, m.Project))
		}
		if m.Kind == domain.MessageCommand {
			commands[m.ID] = m.Project
		}
	}
	for _, ea := range doc.CommandAcceptances {
		if p, ok := commands[ea.Message]; !ok || p != ea.Project {
			return bad("command_acceptances", fmt.Sprintf("acceptance %s names command %s, which is not a command of project %s in the export", ea.ID, ea.Message, ea.Project))
		}
	}
	return nil
}

// importChatOrder returns the messages in the order import must insert them:
// the source feed's own order. A fresh board gives every message a new feed
// position in insertion order, so inserting in any other order would reverse
// the feed and break every reply's reference to an earlier message.
//
// An export written after KANB-70 carries seq on every message. One written
// before carries none and listed each project's feed newest first, so it is
// reversed. A document mixing the two has been edited by hand, and there is
// no honest order to pick for it.
func importChatOrder(msgs []ExportChatMessage) ([]ExportChatMessage, error) {
	withSeq := 0
	for _, m := range msgs {
		if m.Seq > 0 {
			withSeq++
		}
	}
	out := make([]ExportChatMessage, len(msgs))
	switch withSeq {
	case 0:
		for i, m := range msgs {
			out[len(msgs)-1-i] = m
		}
	case len(msgs):
		copy(out, msgs)
		sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	default:
		return nil, domain.Invalid("chat_messages",
			fmt.Sprintf("%d of %d chat messages carry a feed position (seq) and the rest do not, so the feed order cannot be restored", withSeq, len(msgs)),
			"Import an unmodified export: either every message carries seq (current exports) or none does (exports made before seq existed).")
	}
	seen := make(map[string]string, len(out))
	for _, m := range out {
		if _, dup := seen[m.ID]; dup {
			return nil, domain.Invalid("chat_messages", fmt.Sprintf("chat message %s is listed twice", m.ID),
				"Import an unmodified export.")
		}
		if m.ReplyTo != "" {
			if p, ok := seen[m.ReplyTo]; !ok || p != m.Project {
				return nil, domain.Invalid("chat_messages",
					fmt.Sprintf("chat message %s replies to %s, which is not an earlier message of project %s in the export", m.ID, m.ReplyTo, m.Project),
					"Import an unmodified export; a reply must follow the message it answers.")
			}
		}
		seen[m.ID] = m.Project
	}
	return out, nil
}

// outcomePtr turns a stored outcome into NewTask's optional one; empty stays
// nil, which task creation reads as open.
func outcomePtr(o domain.Outcome) *domain.Outcome {
	if o == "" {
		return nil
	}
	return &o
}

// optionalSeconds sends an optional project setting only when the exported
// board had one: 0 is "not set", and an export written before the setting
// existed carries no value at all, so the restored project keeps the default.
func optionalSeconds(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}
