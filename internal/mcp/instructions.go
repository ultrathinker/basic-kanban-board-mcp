package mcp

import (
	"fmt"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// instructionsText is the `initialize` instructions every client sees before
// it makes a single tool call. PLAN §6 rule 5 requires it to state the
// operating policy up front so an agent never has to infer it from a
// README: how to discover accessible projects, where its identity comes
// from, the lease TTL, the compact grammar version, and the intended loop.
//
// It is built from domain constants (never a hand-copied number) so it can
// never silently drift from the limits the schemas themselves enforce. The
// golden test in instructions_test.go pins the exact rendered text so an
// edit to either the wording or an underlying constant is visible in
// review, per AGENTS.md "no undocumented deviation".
var instructionsText = fmt.Sprintf(`This server is a shared kanban board for AI coding agents (and the humans watching them). Nine tools; no tenth will be added.

Identity: your actor identity is the name of your bearer token. No tool accepts an "actor" parameter — whatever you do is attributed to your token, always.

Projects: you may not see every project on the board. Call board_get with no `+"`project`"+` to list every project your token can access, with a compact summary of each.

The operating loop:
  1. board_get(project: "KEY") at the start of a session — prefer the default compact text; it is cheap enough to call every session.
  2. task_next(project: "KEY", action: "start") to take the next unblocked, unclaimed task in one round trip. Use action:"peek" to look without taking anything.
  3. As you make progress, task_update with only `+"`note`"+` set — notes are append-only and never bump a task's version, so they never conflict.
  4. Whenever you change title, body, type, priority, estimate, tags, assignee, column, rank, parent, acceptance, due_at or metadata, send `+"`if_version`"+` — the version the last read of that task returned. A conflict error carries the current state as `+"`current`"+`; merge into it and retry with the version it reports. No extra read is needed.
  5. %s

Leases: claiming a task grants a lease of %s by default (a project may configure its own default; every lease is clamped to %s–%s). `+"`task_claim`"+` renews or releases it explicitly; letting it expire hands the task back to task_next for anyone.

Compact grammar: board_get's default text output is compact_version=%d. Every task line ends in `+"`v<version>`"+`, so a read-modify-write needs no follow-up task_get just to learn the version to echo back.

Optional research/review fields: a normal task needs only title, body, acceptance and priority. Four fields exist for review and research work and stay empty/unset until they apply — reach for them only when they do. reviewer is who signs off, kept distinct from assignee (who does the work). conclusion is the post-hoc takeaway — kept distinct from the body (the brief written up front) and from notes (the running log). actual is the effort a task really took. outcome is the epistemic status of a result: open, holds, refuted, superseded or moot. It is independent of the column — the column records workflow progress (is the work done?), outcome records whether the result still stands (was it right?). A task can be Done yet refuted. Do NOT set outcome to holds just because ordinary implementation work reached Done; leave it open unless a result was actually judged.

Errors: every failure is `+"`{ok:false, error:{code, message, remediation}}`"+` with `+"`isError`"+` set on the result. `+"`remediation`"+` names the concrete next action — read it instead of guessing.`,
	versionEchoRule,
	formatDuration(domain.ClaimTTLDefault), formatDuration(domain.ClaimTTLMin), formatDuration(domain.ClaimTTLMax), domain.CompactVersion)
