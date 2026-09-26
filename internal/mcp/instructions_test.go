package mcp

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// TestInstructions_ExactRender is a golden pinning of the exact bytes
// `initialize` returns to every MCP client. Any edit to either the wording
// in instructions.go or any of the constants it interpolates from
// internal/domain shows up here, per AGENTS.md "no undocumented deviation".
func TestInstructions_ExactRender(t *testing.T) {
	t.Parallel()
	want := strings.Join([]string{
		// The count is interpolated from the registry, so this line derives it
		// too. Writing "13" here would reintroduce exactly the hand-maintained
		// number KANB-42 exists to remove — the golden would then pin a literal
		// and fail on the day the surface legitimately grows.
		fmt.Sprintf("This server is a shared kanban board for AI coding agents (and the humans watching them). %d tools; the set is deliberately small.", len(fullToolFactories)),
		"",
		"Identity: your actor identity is the name of your bearer token. No tool accepts an \"actor\" parameter — whatever you do is attributed to your token, always.",
		"",
		"Projects: you may not see every project on the board. Call board_get with no `project` to list every project your token can access, with a compact summary of each.",
		"",
		"The operating loop:",
		"  1. board_get(project: \"KEY\") at the start of a session — prefer the default compact text; it is cheap enough to call every session.",
		"  2. task_next(project: \"KEY\", action: \"start\") to take the next unblocked, unclaimed task in one round trip. Use action:\"peek\" to look without taking anything.",
		"  3. As you make progress, task_update with only `note` set — notes are append-only and never bump a task's version, so they never conflict.",
		"  4. Whenever you change title, body, type, priority, estimate, tags, assignee, column, rank, parent, acceptance, due_at or metadata, send `if_version` — the version the last read of that task returned. A conflict error carries the current state as `current`; merge into it and retry with the version it reports. No extra read is needed.",
		"  5. The `version` on every returned task is the value AFTER the call and is authoritative — chain your next `if_version` from it and never re-read a task just to learn its version. `note`, lease operations and `focus` do not move a task's version by design, so a result echoing the same version you sent means the write landed and the version legitimately did not change.",
		"",
		"Leases: claiming a task grants a lease of " + formatDuration(domain.ClaimTTLDefault) + " by default (a project may configure its own default; every lease is clamped to " + formatDuration(domain.ClaimTTLMin) + "–" + formatDuration(domain.ClaimTTLMax) + "). `task_claim` renews or releases it explicitly; letting it expire hands the task back to task_next for anyone.",
		"",
		"Communication: every project has a message feed. Read it with board_get(project: \"KEY\", view: \"messages\") — it starts at the BEGINNING of history, not at the current moment, so a command written before you started is still delivered; page with the returned next_cursor. Post with project_post: kind is update (default), scope_change, question or command; a question or command is addressed with recipient (a participant's tokens.id or \"all\"), or with no recipient to the project's coordinator. `author` is only a display signature — attribution always follows your token. A command names one resolved_executor, fixed when it was sent; if that is you, turn it into work with task_create(source_message: \"<command id>\"): the tasks and the acceptance commit atomically, a repeat returns the original task keys with meta.already_accepted=true, and nobody else may accept. CAUTION: that guarantee is about the board only — it does NOT prevent an external command, deploy or other side effect from running twice; guard those separately.",
		"",
		"Compact grammar: board_get's default text output is compact_version=" + strconv.Itoa(domain.CompactVersion) + ". Every task line ends in `v<version>`, so a read-modify-write needs no follow-up task_get just to learn the version to echo back.",
		"",
		"Optional research/review fields: a normal task needs only title, body, acceptance and priority. Four fields exist for review and research work and stay empty/unset until they apply — reach for them only when they do. reviewer is who signs off, kept distinct from assignee (who does the work). conclusion is the post-hoc takeaway — kept distinct from the body (the brief written up front) and from notes (the running log). actual is the effort a task really took. outcome is the epistemic status of a result: open, holds, refuted, superseded or moot. It is independent of the column — the column records workflow progress (is the work done?), outcome records whether the result still stands (was it right?). A task can be Done yet refuted. Do NOT set outcome to holds just because ordinary implementation work reached Done; leave it open unless a result was actually judged.",
		"",
		"Executor keys: an orchestrator gives each agent it launches its own key with executor_key_issue; the key's name is the agent's identity, and assignee and reviewer must name a participant exactly (participant_only:true adds a name that needs no key, such as the owner; renew:true extends a key and keeps its secret). An executor key writes only to cards assigned to its name — claim, note, progress_set, and moves between columns other than done ones — and to the feed. It hands work over by moving the card into the review column; the reviewer, not the executor, moves it to done.",
		"",
		"Errors: every failure is `{ok:false, error:{code, message, remediation}}` with `isError` set on the result. `remediation` names the concrete next action — read it instead of guessing.",
	}, "\n")
	if instructionsText != want {
		t.Errorf("instructions drift:\n--- want ---\n%s\n--- got ---\n%s", want, instructionsText)
	}
}
