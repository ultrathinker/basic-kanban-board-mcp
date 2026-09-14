package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// commandAcceptanceRepo persists the "command -> tasks -> acceptor" link
// (KANB-47). One row per command message, ever: the UNIQUE(message_id)
// constraint IS the "a command is accepted once per project" rule, enforced by
// the database inside the same transaction that creates the tasks, which is
// what makes acceptance atomic — no half-state can commit.
type commandAcceptanceRepo struct{ s *sqlStore }

const acceptanceSelectSQL = `SELECT id, message_id, project_id, accepted_by,
	task_ids, task_keys, request_hash, created_at
	FROM command_acceptances`

func (r *commandAcceptanceRepo) GetByMessage(tx Tx, messageID string) (*domain.CommandAcceptance, error) {
	if messageID == "" {
		return nil, domain.Invalid("message_id", "message id is empty", "Pass the command message id.")
	}
	tw := tx.(*txWrap)
	row := tw.tx.QueryRowContext(tw.ctx(), acceptanceSelectSQL+` WHERE message_id = ?`, messageID)
	a, err := scanAcceptance(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NotFound("command_acceptance", messageID)
	}
	return a, err
}

func (r *commandAcceptanceRepo) ByMessages(tx Tx, messageIDs []string) (map[string]domain.CommandAcceptance, error) {
	out := make(map[string]domain.CommandAcceptance, len(messageIDs))
	norm := make([]string, 0, len(messageIDs))
	for _, id := range messageIDs {
		if id != "" {
			norm = append(norm, id)
		}
	}
	if len(norm) == 0 {
		return out, nil
	}
	tw := tx.(*txWrap)
	placeholders, args := makeInClause(norm)
	q := acceptanceSelectSQL + ` WHERE message_id IN (` + placeholders + `)`
	rows, err := tw.tx.QueryContext(tw.ctx(), q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list acceptances: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanAcceptance(rows.Scan)
		if err != nil {
			return nil, err
		}
		out[a.MessageID] = *a
	}
	return out, rows.Err()
}

func (r *commandAcceptanceRepo) Put(tx Tx, a *domain.CommandAcceptance) error {
	if a == nil {
		return errors.New("store: acceptance.Put: nil acceptance")
	}
	if a.ID == "" || a.MessageID == "" || a.ProjectID == "" || a.AcceptedByTokenID == "" {
		return domain.Invalid("command_acceptance", "id, message_id, project_id and accepted_by are required",
			"Set those fields before calling acceptance Put.")
	}
	taskIDs, err := encodeJSON(a.TaskIDs)
	if err != nil {
		return err
	}
	taskKeys, err := encodeJSON(a.TaskKeys)
	if err != nil {
		return err
	}
	if a.CreatedAt.IsZero() {
		now, err := tx.Now()
		if err != nil {
			return err
		}
		a.CreatedAt = now
	}
	tw := tx.(*txWrap)
	_, err = tw.tx.ExecContext(tw.ctx(), `
		INSERT INTO command_acceptances(
			id, message_id, project_id, accepted_by, task_ids, task_keys, request_hash, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		a.ID, a.MessageID, a.ProjectID, a.AcceptedByTokenID, taskIDs, taskKeys, a.RequestHash, formatTime(a.CreatedAt),
	)
	if err != nil {
		if IsUniqueViolation(err) {
			return wrapf(domain.Conflict(nil, 0, 0), "command message %s was already accepted", a.MessageID)
		}
		return fmt.Errorf("store: insert acceptance: %w", err)
	}
	return nil
}

// scanAcceptance materializes one row. scanFn abstracts *sql.Row.Scan from
// *sql.Rows.Scan, the same split the project and token repos use.
func scanAcceptance(scanFn func(dest ...any) error) (*domain.CommandAcceptance, error) {
	var (
		a        domain.CommandAcceptance
		taskIDs  string
		taskKeys string
		created  string
	)
	if err := scanFn(&a.ID, &a.MessageID, &a.ProjectID, &a.AcceptedByTokenID,
		&taskIDs, &taskKeys, &a.RequestHash, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: scan acceptance: %w", err)
	}
	ids, err := decodeStrings(taskIDs)
	if err != nil {
		return nil, err
	}
	keys, err := decodeStrings(taskKeys)
	if err != nil {
		return nil, err
	}
	createdAt, err := parseTime(created)
	if err != nil {
		return nil, err
	}
	a.TaskIDs, a.TaskKeys, a.CreatedAt = ids, keys, createdAt
	return &a, nil
}
