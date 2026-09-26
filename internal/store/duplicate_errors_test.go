package store

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// Review of 14.09, D7 and 7.2: a UNIQUE violation used to come back either as
// wrapf(domain.Conflict(nil, 0, 0), ...) — "version mismatch: you sent
// if_version=0, current is 0", with advice to retry with if_version=0 — or as a
// bare "store: insert ...: constraint failed" with no code at all. Every
// duplicate must say what already exists and what to do instead.
func TestDuplicateRowsAreNamedNotVersionConflicts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	p, cols := seedProject(t, s)
	task := seedTask(t, s, p, cols["Backlog"], "x", "tester")

	check := func(label string, err error, code domain.Code, mustSay string) {
		t.Helper()
		de := domain.AsError(err)
		if de == nil {
			t.Errorf("%s: err = %v, want a *domain.Error", label, err)
			return
		}
		if de.Code != code {
			t.Errorf("%s: code = %s, want %s", label, de.Code, code)
		}
		all := de.Message + " " + de.Remediation
		if strings.Contains(all, "if_version") || strings.Contains(all, "version mismatch") {
			t.Errorf("%s: a duplicate is described as a version conflict: %q", label, all)
		}
		if !strings.Contains(de.Message, mustSay) {
			t.Errorf("%s: message %q does not say %q", label, de.Message, mustSay)
		}
		if strings.TrimSpace(de.Remediation) == "" {
			t.Errorf("%s: no remediation", label)
		}
	}

	err := s.Write(ctx, func(tx Tx) error {
		dup := &domain.Project{ID: uuid.NewString(), Key: p.Key, Name: "again", Version: 1, NextTaskSeq: 1, EstimateUnit: "h", ClaimTTLSeconds: 3600}
		return s.Projects().Create(tx, dup)
	})
	check("project key", err, domain.CodeConflict, "already exists")

	err = s.Write(ctx, func(tx Tx) error {
		n := &domain.Note{ID: "note-dup", TaskID: task.ID, Author: "a", Body: "b"}
		if err := s.Notes().Add(tx, n); err != nil {
			return err
		}
		return s.Notes().Add(tx, &domain.Note{ID: "note-dup", TaskID: task.ID, Author: "a", Body: "b"})
	})
	check("note id", err, domain.CodeConflict, "note note-dup already exists")

	err = s.Write(ctx, func(tx Tx) error {
		m := &domain.ProgressMark{ID: "mark-dup", ProjectID: p.ID, TaskID: &task.ID, Assessor: "a", Percent: 10}
		if err := s.Progress().Add(tx, m); err != nil {
			return err
		}
		return s.Progress().Add(tx, &domain.ProgressMark{ID: "mark-dup", ProjectID: p.ID, TaskID: &task.ID, Assessor: "a", Percent: 20})
	})
	check("progress mark id", err, domain.CodeConflict, "progress mark mark-dup already exists")

	err = s.Write(ctx, func(tx Tx) error {
		m := &domain.ChatMessage{ID: "msg-dup", ProjectID: p.ID, Author: "a", Body: "b", Kind: domain.MessageKind("update")}
		if err := s.Chat().Add(tx, m); err != nil {
			return err
		}
		return s.Chat().Add(tx, &domain.ChatMessage{ID: "msg-dup", ProjectID: p.ID, Author: "a", Body: "b", Kind: domain.MessageKind("update")})
	})
	check("chat message id", err, domain.CodeConflict, "chat message msg-dup already exists")
}
