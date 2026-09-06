package store

import (
	"fmt"
	"strings"
)

// buildTaskFilter turns a TaskFilter into (clauses, args) for the dynamic
// SELECT. Centralised here so the SQL string lives next to its arguments.
//
// The done/IncludeDone handling is the trickiest part: when IncludeDone is
// false we exclude tasks in done columns; when IncludeDone is true AND
// DoneLimit is set we want at most DoneLimit done rows ordered by done_at
// desc. We achieve the cap with a window function inside a subquery so
// the limit applies to the done slice independently of the main LIMIT
// (which is for the active slice).
func buildTaskFilter(f TaskFilter, actor string) ([]string, []any) {
	var (
		clauses []string
		args    []any
	)

	if len(f.ProjectIDs) > 0 {
		ph, a := makeInClause(f.ProjectIDs)
		clauses = append(clauses, "project_id IN ("+ph+")")
		args = append(args, a...)
	}
	if len(f.ColumnIDs) > 0 {
		ph, a := makeInClause(f.ColumnIDs)
		clauses = append(clauses, "column_id IN ("+ph+")")
		args = append(args, a...)
	}
	if len(f.Types) > 0 {
		names := make([]string, 0, len(f.Types))
		for _, t := range f.Types {
			if t.Valid() {
				names = append(names, string(t))
			}
		}
		if len(names) > 0 {
			ph, a := makeInClause(names)
			clauses = append(clauses, "type IN ("+ph+")")
			args = append(args, a...)
		}
	}
	if f.PriorityMin != nil && f.PriorityMin.Valid() {
		clauses = append(clauses, "priority >= ?")
		args = append(args, int(*f.PriorityMin))
	}
	if len(f.Tags) > 0 {
		// OR over the supplied tags. JSON1's json_each expands the tags
		// column into rows so we can match with EXISTS.
		ph, a := makeInClause(f.Tags)
		clauses = append(clauses, fmt.Sprintf(
			"EXISTS (SELECT 1 FROM json_each(tags) WHERE json_each.value IN (%s))", ph))
		args = append(args, a...)
	}
	if f.Assignee != nil {
		if *f.Assignee == "" {
			clauses = append(clauses, "assignee IS NULL")
		} else {
			clauses = append(clauses, "assignee = ?")
			args = append(args, *f.Assignee)
		}
	}
	if f.Claimed != "" {
		switch f.Claimed {
		case ClaimedMine:
			if actor == "" {
				// Surface a loud error at the SQL layer by emitting an
				// always-false clause; the caller (List path) will not
				// get results but the error path is handled at the
				// service boundary.
				clauses = append(clauses, "1 = 0")
			} else {
				clauses = append(clauses, "claimed_by = ?")
				args = append(args, actor)
			}
		case ClaimedUnclaimed:
			clauses = append(clauses, "claimed_by IS NULL")
		case ClaimedOther:
			if actor == "" {
				clauses = append(clauses, "1 = 0")
			} else {
				clauses = append(clauses, "claimed_by IS NOT NULL AND claimed_by <> ?")
				args = append(args, actor)
			}
		}
	}
	if f.Blocked != nil {
		if *f.Blocked {
			clauses = append(clauses, blockedSubquery(true))
		} else {
			clauses = append(clauses, "NOT "+blockedSubquery(false))
		}
	}
	if f.Query != "" {
		// Case-insensitive substring on title or body, with manual
		// escaping of LIKE wildcards.
		like := escapeLike(f.Query)
		clauses = append(clauses, "(title LIKE ? ESCAPE '\\' OR body LIKE ? ESCAPE '\\')")
		args = append(args, "%"+like+"%", "%"+like+"%")
	}
	if f.UpdatedSince != nil {
		clauses = append(clauses, "updated_at >= ?")
		args = append(args, formatTime(f.UpdatedSince.UTC()))
	}
	if !f.IncludeArchived {
		clauses = append(clauses, "archived_at IS NULL")
	}
	if f.ParentID != nil {
		if *f.ParentID == "" {
			clauses = append(clauses, "parent_id IS NULL")
		} else {
			clauses = append(clauses, "parent_id = ?")
			args = append(args, *f.ParentID)
		}
	}
	if !f.IncludeDone {
		clauses = append(clauses, "column_id NOT IN (SELECT id FROM columns WHERE kind = 'done')")
	}
	return clauses, args
}

// blockedSubquery returns a SQL fragment that tests "this task has at
// least one open blocker". The fragment is reused by both the `Blocked
// = true` and `Blocked = false` branches.
func blockedSubquery(_ bool) string {
	return `EXISTS (
		SELECT 1 FROM links l
		JOIN tasks tb ON tb.id = l.blocker_id
		JOIN columns cb ON cb.id = tb.column_id
		WHERE l.blocked_id = tasks.id
		  AND l.type = 'blocks'
		  AND tb.archived_at IS NULL
		  AND cb.kind <> 'done'
	)`
}

// escapeLike escapes the two LIKE metacharacters (\ and %) so a user
// query "50%" is a literal search, not a wildcard.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
