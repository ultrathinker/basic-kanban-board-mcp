package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"

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
		now, err := tx.Now()
		if err != nil {
			return err
		}
		l.CreatedAt = now
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
		if IsProjectLocalityViolation(err) {
			return crossProjectEdge("link")
		}
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
//  3. transitive wait-for cycles, over blocks links AND the parent
//     hierarchy — see waitsForEdgesCTE for why the hierarchy belongs in
//     the same walk
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

	// Case 3: a transitive wait-for cycle. Adding "blocker blocks blocked"
	// says blocked waits on blocker, so it closes a loop iff blocker
	// already waits — directly or transitively — on blocked.
	closes, err := waitsForReaches(tw, blockedID, blockerID)
	if err != nil {
		return false, nil, err
	}
	if !closes {
		return false, nil, nil
	}
	// The edge being added closes the loop, so the reported cycle starts at
	// the blocker and ends at it too.
	ids, err := waitsForPath(tw, blockedID, blockerID)
	if err != nil {
		return false, nil, err
	}
	if len(ids) == 0 {
		// The two walks read the same edges in the same transaction, so
		// this cannot normally happen. If it ever does, the link is still
		// refused — an unhelpful path is a worse outcome than a committed
		// cycle only for the person reading the message.
		return true, []string{blockerKey, blockedKey, blockerKey}, nil
	}
	keys := []string{blockerKey}
	for _, id := range ids {
		k, err := r.taskKey(tw, id)
		if err != nil {
			return false, nil, err
		}
		keys = append(keys, k)
	}
	return true, keys, nil
}

// waitsForReaches reports whether `to` is reachable from `from` over the
// wait-for relation.
//
// The recursive term is UNION, not UNION ALL, and that is the whole point.
// UNION makes SQLite drop rows it has already produced, so each task enters
// the walk at most once and the recursion ends when the visited set is
// exhausted — no depth argument, and cost linear in the reachable
// subgraph.
//
// This replaces a walk that carried the path in a string and tested it for
// membership. That is per-path bookkeeping rather than a global visited
// set: on a graph with parallel routes it enumerates exponentially many
// simple paths, and the `depth < 16` cutoff was what kept it from running
// away. Bounding the search that way also quietly bounded the product
// invariant — a cycle whose shortest closing route ran longer than 16 hops
// was not detected and committed, and dependency-aware task_next then had
// no runnable leaf anywhere in that component, permanently and across
// restarts. The batch API takes 100 tasks per call, so describing such a
// chain took one request.
func waitsForReaches(tw *txWrap, from, to string) (bool, error) {
	var found int
	err := tw.tx.QueryRowContext(tw.ctx(), `
		WITH RECURSIVE `+waitsForEdgesCTE+`,
		reach(id) AS (
		    SELECT ?
		    UNION
		    SELECT e.dst FROM edges e JOIN reach r ON e.src = r.id
		)
		SELECT 1 FROM reach WHERE id = ? LIMIT 1`, from, to).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: walk wait-for graph: %w", err)
	}
	return true, nil
}

// waitsForPath returns the shortest chain of task ids leading from `from`
// to `to` over the wait-for relation, inclusive at both ends, or nil when
// there is none.
//
// It only runs after waitsForReaches has already decided to refuse the
// link, so it can afford to read the whole edge relation and walk it here:
// this is the error path of a rejected write, not the hot path. Doing the
// walk breadth-first in Go — rather than asking SQL for a path — keeps the
// cost linear where a path-carrying recursive query is exponential, and
// reports the shortest cycle, which is the one a caller can most easily
// break.
func waitsForPath(tw *txWrap, from, to string) ([]string, error) {
	rows, err := tw.tx.QueryContext(tw.ctx(),
		`WITH RECURSIVE `+waitsForEdgesCTE+` SELECT src, dst FROM edges`)
	if err != nil {
		return nil, fmt.Errorf("store: read wait-for edges: %w", err)
	}
	defer rows.Close()
	adj := make(map[string][]string)
	for rows.Next() {
		var src, dst string
		if err := rows.Scan(&src, &dst); err != nil {
			return nil, fmt.Errorf("store: scan wait-for edge: %w", err)
		}
		adj[src] = append(adj[src], dst)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Sorted successors make the reported cycle stable across runs when
	// several shortest paths exist; an error message that changes between
	// identical calls is one nobody can write a test against.
	for _, succ := range adj {
		sort.Strings(succ)
	}
	prev := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == to {
			return tracePath(prev, from, to), nil
		}
		for _, next := range adj[cur] {
			if _, seen := prev[next]; seen {
				continue
			}
			prev[next] = cur
			queue = append(queue, next)
		}
	}
	return nil, nil
}

// tracePath rebuilds the from→to chain recorded in a BFS predecessor map.
func tracePath(prev map[string]string, from, to string) []string {
	var out []string
	for at := to; at != ""; at = prev[at] {
		out = append(out, at)
		if at == from {
			break
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// waitsForEdgesCTE is the "Y waits on X" relation that cycle detection
// walks, as a non-recursive CTE body meant to be spliced into a
// WITH RECURSIVE list. It is wider than the links table alone because
// task_next gates readiness on the parent hierarchy too (domain.NextReady):
//
//	E1  X blocks Y                    → Y waits on X
//	E2  X blocks P, C is a child of P → C waits on X, because NextReady's
//	    ParentBlocked rule never offers a subtask while its parent has
//	    open blockers
//	E3  C is a child of P             → P waits on C, because NextReady's
//	    NotLeaf rule never offers a parent with unfinished subtasks
//
// Following E1 alone — as this walk used to — misses "A blocks B, C is a
// subtask of B, C blocks A": all three tasks become permanently unofferable
// and nothing refused the link that caused it. PLAN §11 invariant 5 asks
// for cycle detection including parent chains.
//
// E2 and E3 are deliberately not one hierarchy edge traversed both ways. A
// bare parent→child edge would collapse a subtree into a single node and
// reject "A blocks C1, C2 blocks A" for two siblings under one parent —
// which schedules fine as C2, A, C1. E2 reaches a child only through a
// blocks edge into its parent, which is precisely what ParentBlocked
// propagates.
const waitsForEdgesCTE = `
		edges(src, dst) AS (
		    SELECT l.blocker_id, l.blocked_id
		      FROM links l
		     WHERE l.type = 'blocks'
		    UNION
		    SELECT l.blocker_id, c.id
		      FROM links l
		      JOIN tasks c ON c.parent_id = l.blocked_id
		     WHERE l.type = 'blocks'
		    UNION
		    SELECT t.id, t.parent_id
		      FROM tasks t
		     WHERE t.parent_id IS NOT NULL
		)`

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
//
// The walk is bounded by the visited set, not by a hop count. The domain
// caps hierarchies at MaxSubtaskDepth, so any constant here would only ever
// fire on a chain the domain says cannot exist — which is exactly the case
// where answering "no ancestor found" is wrong, because a hierarchy that
// deep came from a corrupt row or an import and is the thing most in need
// of a cycle check. Revisiting a task still ends the walk with a loud error.
func parentChain(tw *txWrap, start, target string) ([]string, bool, error) {
	current := start
	visited := make(map[string]bool)
	var ids []string
	for {
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
}

// parentChainDepth returns how many parent_id edges exist from
// `start` upward to the root. Returns 0 if start is top-level.
//
// Like parentChain, the visited set is the bound: the old hop limit could
// report a depth of 0 for a chain that was merely long, and depth 0 is the
// answer that says "this task may take children".
func parentChainDepth(tw *txWrap, start string) (int, error) {
	current := start
	visited := make(map[string]bool)
	for i := 0; ; i++ {
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

// (wrapf now lives in scan.go.)
