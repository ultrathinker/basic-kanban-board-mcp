package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// links
// ---------------------------------------------------------------------------

type linkRepo struct{ s *sqlStore }

func (r *linkRepo) Add(tx Tx, l *domain.Link) error {
	if l == nil {
		return errors.New("store: link.Add: nil link")
	}
	if l.BlockerID == "" || l.BlockedID == "" {
		return domain.Invalid("link", "blocker_id and blocked_id are required",
			"Both ids must be set.")
	}
	if l.BlockerID == l.BlockedID {
		return domain.Invalid("link", "a task may not block itself",
			"Drop the link or pick a different target.")
	}
	if !l.Type.Valid() {
		return domain.Invalid("link", fmt.Sprintf("link type %q is invalid", l.Type),
			"Use blocks (the only type in v1).")
	}
	if l.CreatedAt.IsZero() {
		l.CreatedAt = tx.Now().UTC()
	}
	// Reject cycles BEFORE insert (the schema's CHECK only blocks the
	// trivial self-link; transitive cycles and parent-chain violations
	// require a walk).
	wouldCycle, path, err := r.WouldCycle(tx, l.BlockerID, l.BlockedID)
	if err != nil {
		return err
	}
	if wouldCycle {
		return domain.Cycle(path)
	}
	tw := tx.(*txWrap)
	_, err = tw.tx.ExecContext(tw.ctx(), `
		INSERT INTO links(blocker_id, blocked_id, type, created_at, created_by)
		VALUES (?,?,?,?,?)`,
		l.BlockerID, l.BlockedID, string(l.Type), formatTime(l.CreatedAt), l.CreatedBy,
	)
	if err != nil {
		if IsUniqueViolation(err) {
			// Adding the same link is a no-op (idempotent per the schema
			// contract in PLAN §6.6). We do not surface this as an error.
			return nil
		}
		if IsCheckViolation(err) {
			return domain.Invalid("link", "a task may not block itself",
				"Drop the link or pick a different target.")
		}
		if IsForeignKeyViolation(err) {
			return missingRef(
				fmt.Sprintf("link references a task that does not exist: blocker %s or blocked %s",
					l.BlockerID, l.BlockedID),
				"Check both task keys with task_get, then retry task_link.")
		}
		return fmt.Errorf("store: insert link: %w", err)
	}
	return nil
}

func (r *linkRepo) Remove(tx Tx, blockerID, blockedID string, t domain.LinkType) error {
	if blockerID == "" || blockedID == "" {
		return domain.Invalid("link", "blocker_id and blocked_id are required",
			"Both ids must be set.")
	}
	if !t.Valid() {
		return domain.Invalid("link", fmt.Sprintf("link type %q is invalid", t),
			"Use blocks.")
	}
	tw := tx.(*txWrap)
	res, err := tw.tx.ExecContext(tw.ctx(),
		"DELETE FROM links WHERE blocker_id = ? AND blocked_id = ? AND type = ?",
		blockerID, blockedID, string(t))
	if err != nil {
		return fmt.Errorf("store: delete link: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// PLAN §6.6: removing a non-existent link is a no-op success.
		return nil
	}
	return nil
}

// OpenBlockers returns the keys of blockers that are not archived AND not
// in a done column, sorted with the natural-key order. The list is what
// the service uses for both CheckMove and the compact rendering.
func (r *linkRepo) OpenBlockers(tx Tx, taskID string) ([]string, error) {
	if taskID == "" {
		return nil, domain.Invalid("task_id", "task id is empty", "Pass the task UUID.")
	}
	out, err := linksOpenBlockers(tx, taskID)
	if err != nil {
		return nil, err
	}
	sortNatural(out)
	return out, nil
}

// Blocks returns the keys this task blocks, sorted naturally.
func (r *linkRepo) Blocks(tx Tx, taskID string) ([]string, error) {
	if taskID == "" {
		return nil, domain.Invalid("task_id", "task id is empty", "Pass the task UUID.")
	}
	out, err := linksBlocks(tx, taskID)
	if err != nil {
		return nil, err
	}
	sortNatural(out)
	return out, nil
}

// WouldCycle reports whether adding the edge blocker -> blocked would
// close a cycle. It checks three cases:
//  1. self-link (caught at the schema CHECK, repeated here for a clear
//     domain error path)
//  2. parent/child chain — a task may not block anything in its own
//     subtree, and may not be blocked by anything in its own ancestry
//  3. transitive blocks-graph cycles
//
// The returned path is the full closed cycle starting and ending at
// blocker, e.g. ["BMB-3","BMB-7","BMB-3"] for a 2-step cycle, so the
// remediation can name the offender.
func (r *linkRepo) WouldCycle(tx Tx, blockerID, blockedID string) (bool, []string, error) {
	if blockerID == "" || blockedID == "" {
		return false, nil, domain.Invalid("link", "blocker_id and blocked_id are required", "Pass both ids.")
	}
	if blockerID == blockedID {
		// Self-link: the SQL CHECK rejects this with a less helpful
		// message; report the domain-level reason.
		return true, []string{blID(blockerID, tx), blID(blockerID, tx)}, nil
	}
	tw := tx.(*txWrap)
	// Case 2: parent/child chain. Walk up the blocked task's ancestry; if
	// any ancestor is the blocker, we'd be making the blocker block its
	// own ancestor. Similarly walk up the blocker's ancestry: if any
	// ancestor is the blocked, we'd be making the blocked block its own
	// ancestor (i.e. the new edge runs UP the parent chain, which the
	// dependency DAG forbids).
	blockerKey, err := r.taskKey(tw, blockerID)
	if err != nil {
		return false, nil, err
	}
	blockedKey, err := r.taskKey(tw, blockedID)
	if err != nil {
		return false, nil, err
	}
	// blocked ancestor -> blocker
	if path, ok, err := r.ancestorChain(tw, blockedID, blockerID); err != nil {
		return false, nil, err
	} else if ok {
		// path is the ancestor chain from blocked upward; the cycle is
		// blocked -> ... -> blocker -> blocked.
		keys := []string{blockedKey}
		keys = append(keys, path...)
		keys = append(keys, blockedKey)
		return true, keys, nil
	}
	// blocker ancestor -> blocked
	if path, ok, err := r.ancestorChain(tw, blockerID, blockedID); err != nil {
		return false, nil, err
	} else if ok {
		keys := []string{blockerKey}
		keys = append(keys, path...)
		keys = append(keys, blockerKey)
		return true, keys, nil
	}

	// Case 3: transitive blocks-graph cycle. Adding the edge
	// blocker -> blocked would create a cycle iff there is already a
	// path blocked -> ... -> blocker in the EXISTING blocks graph
	// (because the new edge closes the loop: blocker -> blocked -> ...
	// -> blocker). Each step follows the "blocks" direction: if X
	// blocks Y then (blocker_id=X, blocked_id=Y); to walk FROM X we
	// want rows where blocker_id = current, taking the next blocked_id.
	tw_ := tw
	var hit string
	err = tw_.tx.QueryRowContext(tw.ctx(), `
		WITH RECURSIVE walk(curr, depth) AS (
		    SELECT ?, 0
		    UNION ALL
		    SELECT l.blocked_id, w.depth + 1
		      FROM links l JOIN walk w ON l.blocker_id = w.curr
		     WHERE l.type = 'blocks' AND w.depth < 16
		)
	    SELECT 1 FROM walk WHERE curr = ? LIMIT 1`, blockedID, blockerID).Scan(&hit)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("store: walk blocks: %w", err)
	}
	// Build the full path: [blocker, blocked, ..., blocker] using the
	// SAME recursive walk so each step is the actual edge that exists
	// in the graph. The walk starts at `blocked` and follows outgoing
	// blocks edges (blocker_id = current, taking blocked_id as next).
	rows, err := tw_.tx.QueryContext(tw.ctx(), `
		WITH RECURSIVE walk(curr, depth) AS (
		    SELECT ?, 0
		    UNION ALL
		    SELECT l.blocked_id, w.depth + 1
		      FROM links l JOIN walk w ON l.blocker_id = w.curr
		     WHERE l.type = 'blocks' AND w.depth < 16
		)
	    SELECT curr, depth FROM walk ORDER BY depth ASC`, blockedID)
	if err != nil {
		return false, nil, fmt.Errorf("store: walk path: %w", err)
	}
	defer rows.Close()
	keys := []string{blockerKey, blockedKey}
	visited := map[string]bool{blockedID: true}
	for rows.Next() {
		var curr string
		var depth int
		if err := rows.Scan(&curr, &depth); err != nil {
			return false, nil, fmt.Errorf("store: scan walk row: %w", err)
		}
		if depth == 0 {
			continue
		}
		if visited[curr] {
			// The walk can revisit nodes; report the cycle path up
			// to the first repeat, then close the loop.
			keys = append(keys, blockerKey)
			return true, keys, nil
		}
		visited[curr] = true
		k, err := r.taskKey(tw, curr)
		if err != nil {
			return false, nil, err
		}
		keys = append(keys, k)
		if curr == blockerID {
			return true, keys, nil
		}
	}
	// Defensive fallback: cycle was confirmed but the path walk didn't
	// close cleanly. Return what we have, closed at the blocker.
	keys = append(keys, blockerKey)
	return true, keys, nil
}

// ancestorChain walks the parent_id chain of start and returns the
// sequence of ancestor keys (excluding start) that lead to target. If
// target is not in the chain, returns ok=false.
func (r *linkRepo) ancestorChain(tw *txWrap, start, target string) ([]string, bool, error) {
	ids, ok, err := parentChain(tw, start, target)
	if err != nil || !ok {
		return nil, ok, err
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		k, err := r.taskKey(tw, id)
		if err != nil {
			return nil, false, err
		}
		keys = append(keys, k)
	}
	return keys, true, nil
}

// parentChain walks parent_id pointers upward from `start` until it
// either reaches `target` or runs out. Returns the IDs (excluding
// start) on the path to target. Used by both link-cycle detection and
// task reparenting.
func parentChain(tw *txWrap, start, target string) ([]string, bool, error) {
	current := start
	visited := make(map[string]bool)
	var ids []string
	for i := 0; i < 32; i++ {
		if visited[current] {
			return nil, false, fmt.Errorf("store: parent chain cycle at %s", current)
		}
		visited[current] = true
		var parent sql.NullString
		err := tw.tx.QueryRowContext(tw.ctx(),
			"SELECT parent_id FROM tasks WHERE id = ?", current,
		).Scan(&parent)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, false, nil
			}
			return nil, false, fmt.Errorf("store: read parent: %w", err)
		}
		if !parent.Valid {
			return nil, false, nil
		}
		ids = append(ids, parent.String)
		if parent.String == target {
			return ids, true, nil
		}
		current = parent.String
	}
	return nil, false, nil
}

// parentChainDepth returns how many parent_id edges exist from
// `start` upward to the root. Returns 0 if start is top-level.
func parentChainDepth(tw *txWrap, start string) (int, error) {
	current := start
	visited := make(map[string]bool)
	for i := 0; i < 64; i++ {
		if visited[current] {
			return 0, fmt.Errorf("store: parent chain cycle at %s", current)
		}
		visited[current] = true
		var parent sql.NullString
		err := tw.tx.QueryRowContext(tw.ctx(),
			"SELECT parent_id FROM tasks WHERE id = ?", current,
		).Scan(&parent)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return i, nil
			}
			return 0, fmt.Errorf("store: read parent: %w", err)
		}
		if !parent.Valid {
			return i, nil
		}
		current = parent.String
	}
	return 0, fmt.Errorf("store: parent chain too deep from %s", start)
}

// taskKey returns the canonical key of the given task id, or "" if the
// task is missing (which we treat as a non-existent cycle candidate).
func (r *linkRepo) taskKey(tw *txWrap, id string) (string, error) {
	var key string
	err := tw.tx.QueryRowContext(tw.ctx(), "SELECT key FROM tasks WHERE id = ?", id).Scan(&key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", domain.NotFound("task", id)
		}
		return "", fmt.Errorf("store: read task key: %w", err)
	}
	return key, nil
}

// blID resolves a UUID to its key; used in path reporting.
func blID(id string, t Tx) string {
	if t == nil {
		return id
	}
	if k, err := taskKeyFromID(t, id); err == nil {
		return k
	}
	return id
}

func taskKeyFromID(t Tx, id string) (string, error) {
	tw := t.(*txWrap)
	var k string
	err := tw.tx.QueryRowContext(tw.ctx(), "SELECT key FROM tasks WHERE id = ?", id).Scan(&k)
	return k, err
}

// txOf returns the Tx from a txWrap (for helper functions that already
// have a *txWrap on hand). Defined here to keep all the link helpers
// together.
func txOf(tw *txWrap) Tx { return tw }

// ListForTasks returns all links grouped by task, with the task acting
// in either the blocker or blocked role.
func (r *linkRepo) ListForTasks(tx Tx, taskIDs []string) (map[string][]domain.Link, error) {
	if len(taskIDs) == 0 {
		return map[string][]domain.Link{}, nil
	}
	tw := tx.(*txWrap)
	placeholders, args := makeInClause(taskIDs)
	q := `
	SELECT blocker_id, blocked_id, type, created_at, created_by
	FROM links
	WHERE blocker_id IN (` + placeholders + `) OR blocked_id IN (` + placeholders + `)`
	args = append(args, args...)
	rows, err := tw.tx.QueryContext(tw.ctx(), q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list links: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]domain.Link)
	for rows.Next() {
		var l domain.Link
		var typ, created string
		if err := rows.Scan(&l.BlockerID, &l.BlockedID, &typ, &created, &l.CreatedBy); err != nil {
			return nil, fmt.Errorf("store: scan link: %w", err)
		}
		l.Type = domain.LinkType(typ)
		t, err := parseTime(created)
		if err != nil {
			return nil, err
		}
		l.CreatedAt = t
		out[l.BlockerID] = append(out[l.BlockerID], l)
		out[l.BlockedID] = append(out[l.BlockedID], l)
	}
	return out, rows.Err()
}

// _ keeps strings imported for query building.
var _ = strings.HasPrefix

// (wrapf now lives in scan.go.)
