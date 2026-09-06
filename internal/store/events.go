package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

type eventRepo struct{ s *sqlStore }

func (r *eventRepo) Append(tx Tx, e *domain.Event) error {
	if e == nil {
		return errors.New("store: event.Append: nil event")
	}
	if e.Type == "" {
		return domain.Invalid("event", "type is required", "Set EventType before Append.")
	}
	if e.ProjectID == "" {
		return domain.Invalid("event", "project_id is required", "Events are scoped to a project.")
	}
	if e.TS.IsZero() {
		now, err := tx.Now()
		if err != nil {
			return err
		}
		e.TS = now
	}
	payload, err := encodeJSON(e.Payload)
	if err != nil {
		return err
	}
	tw := tx.(*txWrap)
	var taskID any
	if e.TaskID != nil {
		taskID = *e.TaskID
	}
	res, err := tw.tx.ExecContext(tw.ctx(), `
		INSERT INTO events(ts, actor, type, project_id, task_id, payload)
		VALUES (?,?,?,?,?,?)`,
		formatTime(e.TS), e.Actor, string(e.Type), e.ProjectID, taskID, payload,
	)
	if err != nil {
		return fmt.Errorf("store: append event: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: last insert id: %w", err)
	}
	e.ID = id
	return nil
}

// Since returns events with id > afterID, oldest first, capped at limit.
// A null projectID scopes the read to a project; an empty afterID returns
// the earliest retained events. The caller checks MinID for a resync.
func (r *eventRepo) Since(tx Tx, projectID string, afterID int64, limit int) ([]domain.Event, error) {
	if limit <= 0 {
		limit = 200
	}
	tw := tx.(*txWrap)
	var (
		rows *sql.Rows
		err  error
	)
	if projectID == "" {
		rows, err = tw.tx.QueryContext(tw.ctx(), `
			SELECT id, ts, actor, type, project_id, task_id, payload
			FROM events WHERE id > ? ORDER BY id ASC LIMIT ?`, afterID, limit)
	} else {
		rows, err = tw.tx.QueryContext(tw.ctx(), `
			SELECT id, ts, actor, type, project_id, task_id, payload
			FROM events WHERE project_id = ? AND id > ? ORDER BY id ASC LIMIT ?`,
			projectID, afterID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("store: events since: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

func (r *eventRepo) Latest(tx Tx, projectID string, limit int) ([]domain.Event, error) {
	if limit <= 0 {
		limit = 200
	}
	tw := tx.(*txWrap)
	var (
		rows *sql.Rows
		err  error
	)
	if projectID == "" {
		rows, err = tw.tx.QueryContext(tw.ctx(), `
			SELECT id, ts, actor, type, project_id, task_id, payload
			FROM events ORDER BY id DESC LIMIT ?`, limit)
	} else {
		rows, err = tw.tx.QueryContext(tw.ctx(), `
			SELECT id, ts, actor, type, project_id, task_id, payload
			FROM events WHERE project_id = ? ORDER BY id DESC LIMIT ?`, projectID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("store: events latest: %w", err)
	}
	defer rows.Close()
	out, err := scanEvents(rows)
	if err != nil {
		return nil, err
	}
	// Reverse to oldest first; Latest fetched descending for index friendliness.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (r *eventRepo) MinID(tx Tx) (int64, error) {
	tw := tx.(*txWrap)
	var id sql.NullInt64
	err := tw.tx.QueryRowContext(tw.ctx(), "SELECT MIN(id) FROM events").Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: min event id: %w", err)
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}

func scanEvents(rows *sql.Rows) ([]domain.Event, error) {
	var out []domain.Event
	for rows.Next() {
		var (
			e        domain.Event
			ts, kind string
			taskID   sql.NullString
			payload  string
		)
		if err := rows.Scan(&e.ID, &ts, &e.Actor, &kind, &e.ProjectID, &taskID, &payload); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		t, err := parseTime(ts)
		if err != nil {
			return nil, err
		}
		e.TS = t
		e.Type = domain.EventType(kind)
		if taskID.Valid {
			s := taskID.String
			e.TaskID = &s
		}
		if payload != "" {
			pl, err := decodePayload(payload)
			if err != nil {
				return nil, err
			}
			e.Payload = pl
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
