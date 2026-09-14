package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// GenerateMCPToolsDocs renders the canonical docs/MCP-TOOLS.md content
// from the registry. The committed file on disk is the golden; the test
// in docgen_test.go diff-fails on any divergence, so a registry change
// without a re-run shows up as a red build.
//
// The doc is built top-down: the static prose (operating principles,
// compact grammar, error envelope) lives in the *Body constants below;
// the dynamic parts (tool count, per-tool table, per-tool parameter
// list) come from the registry. Mixing them inline would let a typo in
// either side slip past review — the doc as a whole is "did the
// registry and the prose agree?", not "did either side alone change".
func GenerateMCPToolsDocs() string {
	var b bytes.Buffer
	writeDocHeader(&b)
	writeDocQuickReference(&b)
	writeDocCorePrinciples(&b)
	writeDocCompactGrammar(&b)
	writeDocToolReference(&b, fullToolFactories)
	writeDocErrorHandling(&b)
	return b.String()
}

func writeDocHeader(b *bytes.Buffer) {
	fmt.Fprintf(b, "# MCP Tools Reference\n\n")
	fmt.Fprintf(b, "basic-kanban-board-mcp exposes %d native Model Context Protocol (MCP) tools over streamable-HTTP with mandatory bearer token authentication.\n\n", len(fullToolFactories))
	fmt.Fprintf(b, "Every task write is a batch, every read defaults to a token-efficient compact text format, and every task carries an authoritative version for optimistic concurrency.\n\n")
	fmt.Fprintf(b, "---\n\n")
}

func writeDocQuickReference(b *bytes.Buffer) {
	fmt.Fprintf(b, "## Quick Reference: The %d Tools\n\n", len(fullToolFactories))
	fmt.Fprintf(b, "| Tool | Purpose |\n")
	fmt.Fprintf(b, "|---|---|\n")
	for i, f := range fullToolFactories {
		t := f()
		fmt.Fprintf(b, "| [`%s`](#%d-%s) | %s |\n",
			t.Name, i+1, t.Name, oneLineForDoc(t.Description))
	}
	fmt.Fprintf(b, "\n")
	fmt.Fprintf(b, "> **Tool count:** %d tools on the full server (this file). %d on `/mcp/readonly`: %s. The count and the list both come from the registry, not a constant or a hand-written name.\n\n", len(fullToolFactories), len(readOnlyToolFactories), readOnlyToolNamesForDoc())
	fmt.Fprintf(b, "---\n\n")
}

// readOnlyToolNamesForDoc renders the read-only server's tool list from
// readOnlyToolFactories — the same registry.go slice NewReadOnlyServer and
// TestRegistryMatchesTheRunningServer use — rather than a name string typed
// into this file by hand. task_next carries an annotation because its
// claim/start behaviour differs on the read-only server (registerTaskNext's
// readOnly flag); every other name is printed as the registry has it.
func readOnlyToolNamesForDoc() string {
	names := toolNames(readOnlyToolFactories)
	parts := make([]string, len(names))
	for i, n := range names {
		if n == "task_next" {
			parts[i] = n + " with claim/start disabled"
			continue
		}
		parts[i] = n
	}
	return strings.Join(parts, ", ")
}

func writeDocCorePrinciples(b *bytes.Buffer) {
	fmt.Fprintf(b, "## Core Protocol Principles\n\n")
	b.WriteString(corePrinciplesBody)
	fmt.Fprintf(b, "\n---\n\n")
}

func writeDocCompactGrammar(b *bytes.Buffer) {
	fmt.Fprintf(b, "## Compact Board Grammar (`compact_version=1`)\n\n")
	b.WriteString(compactGrammarBody)
	fmt.Fprintf(b, "\n---\n\n")
}

func writeDocToolReference(b *bytes.Buffer, factories []func() *gomcp.Tool) {
	fmt.Fprintf(b, "## Tool Reference\n\n")
	for i, f := range factories {
		t := f()
		fmt.Fprintf(b, "### %d. `%s`\n\n", i+1, t.Name)
		if t.Description != "" {
			fmt.Fprintf(b, "%s\n\n", strings.TrimRight(t.Description, "\n"))
		}
		writeDocParams(b, t)
		writeDocExample(b, t)
		fmt.Fprintf(b, "---\n\n")
	}
}

func writeDocParams(b *bytes.Buffer, t *gomcp.Tool) {
	props := schemaProperties(t.InputSchema)
	if len(props) == 0 {
		return
	}
	required := schemaRequiredSet(t.InputSchema)
	fmt.Fprintf(b, "#### Parameters\n\n")
	fmt.Fprintf(b, "| Name | Type | Required | Description |\n")
	fmt.Fprintf(b, "|---|---|---|---|\n")
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := props[n]
		req := "Optional"
		if required[n] {
			req = "Required"
		}
		fmt.Fprintf(b, "| `%s` | %s | %s | %s |\n", n, schemaTypeOf(p), req, schemaDescOf(p))
	}
	fmt.Fprintf(b, "\n")
}

func writeDocExample(b *bytes.Buffer, t *gomcp.Tool) {
	example := exampleCallFromSchema(t)
	if example == "" {
		return
	}
	fmt.Fprintf(b, "#### Example Call\n\n")
	fmt.Fprintf(b, "```jsonc\n%s\n```\n\n", example)
}

func writeDocErrorHandling(b *bytes.Buffer) {
	fmt.Fprintf(b, "## Error Handling & Domain Codes\n\n")
	b.WriteString(errorHandlingBody)
}

// oneLineForDoc returns the first sentence of a tool description, so
// the Quick Reference table stays one line per tool. The full prose
// stays in the per-tool section, where it is the proper context.
func oneLineForDoc(desc string) string {
	desc = strings.TrimSpace(desc)
	if i := strings.IndexAny(desc, "\n"); i > 0 {
		desc = desc[:i]
	}
	if i := strings.Index(desc, "."); i > 0 && i < len(desc)-1 {
		desc = desc[:i+1]
	}
	return strings.TrimSpace(desc)
}

// schemaProperties / schemaRequiredSet / schemaTypeOf / schemaDescOf
// walk the published InputSchema the same way paramsFromSchema does
// (board_guide.go); kept here as a separate copy because the docgen
// test imports this file independently of board_guide's helpers and
// the two should not be made to share implementation across the
// package boundary — a future tools-only refactor would otherwise
// collapse them and lose the per-tool trace.
func schemaProperties(s any) map[string]any {
	m := asMap(s)
	props, _ := m["properties"].(map[string]any)
	return props
}

func schemaRequiredSet(s any) map[string]bool {
	m := asMap(s)
	req, _ := m["required"].([]any)
	out := make(map[string]bool, len(req))
	for _, r := range req {
		if name, ok := r.(string); ok {
			out[name] = true
		}
	}
	return out
}

func schemaTypeOf(p any) string {
	m, _ := p.(map[string]any)
	if t, ok := m["type"].(string); ok && t != "" {
		return t
	}
	if types, ok := m["types"].([]any); ok {
		parts := make([]string, 0, len(types))
		for _, x := range types {
			if s, ok := x.(string); ok {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "|")
		}
	}
	return "any"
}

func schemaDescOf(p any) string {
	m, _ := p.(map[string]any)
	if d, ok := m["description"].(string); ok {
		return d
	}
	return ""
}

func asMap(s any) map[string]any {
	// InputSchema is `any`; every tool here sets it via schemaFor, so
	// the assertion is safe. For a tool that ever sets a non-map, the
	// helper returns nil and the doc skips the parameter table — the
	// failure mode is "less doc", not "wrong doc".
	if m, ok := s.(map[string]any); ok {
		return m
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// exampleCallFromSchema produces a minimal call from the schema: it
// always names the tool and every required property. The values are
// placeholders (`<value>`) because the doc is the wire contract, not a
// worked example — that lives in /agent-setup.
func exampleCallFromSchema(t *gomcp.Tool) string {
	props := schemaProperties(t.InputSchema)
	if len(props) == 0 {
		return fmt.Sprintf("%s()", t.Name)
	}
	required := schemaRequiredSet(t.InputSchema)
	if len(required) == 0 {
		// No required fields: name the first property alphabetically so
		// the example is deterministic across regenerations.
		names := make([]string, 0, len(props))
		for n := range props {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) > 0 {
			required = map[string]bool{names[0]: true}
		}
	}
	parts := make([]string, 0, len(required))
	// Stable order: alphabetical by name.
	keys := make([]string, 0, len(required))
	for k := range required {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%q: <value>", k))
	}
	return fmt.Sprintf("%s({ %s })", t.Name, strings.Join(parts, ", "))
}

// corePrinciplesBody is the human-written half of the doc — the prose
// the registry does not generate. The two halves are concatenated by
// GenerateMCPToolsDocs so a regression in either side fails the docgen
// test (golden comparison).
//
// Built from concatenated double-quoted literals so backticks (used
// heavily in inline-code spans like `board_get`) survive verbatim
// without closing a raw-string literal.
var corePrinciplesBody = buildPrinciples()

func buildPrinciples() string {
	var b strings.Builder
	b.WriteString("1. **Actor Identity Comes From the Bearer Token:**\n")
	b.WriteString("   No tool accepts an `actor` parameter. Your identity is automatically derived from the name of the bearer token configured in your client. Every task creation, claim, lease renewal, note, and update event is permanently attributed to that token identity.\n")
	b.WriteString("\n")
	b.WriteString("2. **The Agent Operating Loop:**\n")
	b.WriteString("   - **Step 1:** Call `board_get(project: \"KEY\")` at session start. Use the default compact format; it costs ~1 064 tokens for 30 tasks (~90% fewer tokens than standard indented JSON).\n")
	b.WriteString("   - **Step 2:** Call `task_next(project: \"KEY\", action: \"start\")` to claim the top ready task and transition it to the active column in a single round trip. Use `action: \"peek\"` if you want to inspect without claiming.\n")
	b.WriteString("   - **Step 3:** While working, log incremental progress by calling `task_update` with only `key` and `note`. Notes are append-only, never bump `version`, and never conflict with concurrent teammates.\n")
	b.WriteString("   - **Step 4:** When editing mutable fields (`title`, `body`, `type`, `priority`, `estimate`, `tags`, `assignee`, `column`, `rank`, `parent`, `acceptance`, `due_at`, `metadata`), always supply `if_version` set to the version returned by your last read.\n")
	b.WriteString("   - **Step 5:** When finished, transition the task to Done: `task_update(patches: [{key: \"KEY-1\", column: \"Done\", if_version: N}])`.\n")
	b.WriteString("\n")
	b.WriteString("   **Canonical loop — one task, zero wasted reads.** The next `if_version` always comes from the `version` field of your most recent response for that task, never from a fresh `task_get`:\n")
	b.WriteString("   ```jsonc\n")
	b.WriteString("   // 1. take the next ready task; data.tasks[0] is the started task, post-mutation\n")
	b.WriteString("   task_next({ \"project\": \"BMB\", \"action\": \"start\" })\n")
	b.WriteString("   //   -> data.tasks[0] = { \"key\": \"BMB-14\", \"column\": \"Doing\", \"version\": 6, ... }\n")
	b.WriteString("\n")
	b.WriteString("   // 2. log progress — note never bumps the version, so you still hold v6\n")
	b.WriteString("   task_update({ \"patches\": [{ \"key\": \"BMB-14\", \"note\": \"scaffolding done\" }] })\n")
	b.WriteString("\n")
	b.WriteString("   // 3. finish — chain if_version from the version you last saw (6)\n")
	b.WriteString("   task_update({ \"patches\": [{ \"key\": \"BMB-14\", \"column\": \"Done\", \"if_version\": 6 }] })\n")
	b.WriteString("   //   -> data.items[0].task.version = 7   (chain your NEXT edit from 7)\n")
	b.WriteString("   ```\n")
	b.WriteString("\n")
	b.WriteString("3. **Authoritative Post-Write Versioning (`versionEchoRule`):**\n")
	b.WriteString("   The `version` integer returned on every task object is the value **AFTER** the call completes, and it is **authoritative**. Chain your next `if_version` directly from this value without re-reading the task.\n")
	b.WriteString("\n")
	b.WriteString("   Under `domain.BumpsTaskVersion`, appending a `note`, changing a lease (`task_claim`), or setting project `focus` **deliberately do not bump a task's version**. Therefore, if an update response echoes the identical version number you sent in `if_version`, the write succeeded and the version legitimately did not change. Do not issue a redundant `task_get`.\n")
	b.WriteString("\n")
	b.WriteString("4. **Two Read Tool Output Shapes:**\n")
	b.WriteString("   - `task_next` and `task_create` return a flat list: `data.tasks[]`.\n")
	b.WriteString("   - `task_get` and `task_update` return a keyed list: `data.items[]`, where each element is `{ \"key\": string, \"ok\": bool, \"task\": {...}, \"error\": {...} }`. For `task_get`, any key that does not exist on the board is preserved in place with `ok: false` and a `not_found` error envelope.\n")
	b.WriteString("\n")
	b.WriteString("5. **Envelope Structure:**\n")
	b.WriteString("   All successful calls return:\n")
	b.WriteString("   ```json\n")
	b.WriteString("   {\n")
	b.WriteString("     \"ok\": true,\n")
	b.WriteString("     \"op\": \"task_update\",\n")
	b.WriteString("     \"data\": { ... },\n")
	b.WriteString("     \"meta\": {\n")
	b.WriteString("       \"count\": 1,\n")
	b.WriteString("       \"warnings\": []\n")
	b.WriteString("     }\n")
	b.WriteString("   }\n")
	b.WriteString("   ```\n")
	b.WriteString("   All domain failures return `ok: false` with `isError: true` on the tool response:\n")
	b.WriteString("   ```json\n")
	b.WriteString("   {\n")
	b.WriteString("     \"ok\": false,\n")
	b.WriteString("     \"op\": \"task_update\",\n")
	b.WriteString("     \"error\": {\n")
	b.WriteString("       \"code\": \"conflict\",\n")
	b.WriteString("       \"message\": \"version mismatch: you sent if_version=2, current is 3\",\n")
	b.WriteString("       \"remediation\": \"The current server state is attached as `current` -- merge your change into it and retry with if_version=3. No re-read needed.\",\n")
	b.WriteString("       \"current\": { ... }\n")
	b.WriteString("     }\n")
	b.WriteString("   }\n")
	b.WriteString("   ```\n")
	return b.String()
}

var compactGrammarBody = buildGrammar()

func buildGrammar() string {
	var b strings.Builder
	b.WriteString("The default output format for `board_get` is compact text (`format: \"compact\"`). The grammar is line-oriented, human-readable, and designed to fit within tight context windows.\n")
	b.WriteString("\n")
	b.WriteString("### Measured Token Comparison (30 Active Tasks)\n")
	b.WriteString("\n")
	b.WriteString("| Format | Measured Size | Tokens (`len/4`) | Savings vs Compact |\n")
	b.WriteString("|---|---|---|---|\n")
	b.WriteString("| **Compact text (`compact_version=1`)** | **~4 258 bytes** | **~1 064 tokens** | **Baseline** |\n")
	b.WriteString("| Minified JSON (`Marshal`) | ~22 560 bytes | ~5 640 tokens | ~81% reduction |\n")
	b.WriteString("| Indented JSON (`MarshalIndent`) | ~42 600 bytes | ~10 650 tokens | ~90% reduction |\n")
	b.WriteString("\n")
	b.WriteString("### Grammar Rules\n")
	b.WriteString("\n")
	b.WriteString("1. **Version header:** Line 1 must be `compact_version=1`.\n")
	b.WriteString("2. **Project header:** `# KEY Name -- focus KEY|none -- Col1 count[/wip] -- Col2 count -- Done total (shown shown|hidden) -- v<version>`\n")
	b.WriteString("   - Each non-done column is formatted as `Name count` or `Name count/wip` if a WIP limit is configured.\n")
	b.WriteString("   - The done segment reports total archived/done tasks and whether any are currently rendered.\n")
	b.WriteString("   - `v<version>` is the **project** configuration version and always comes last.\n")
	b.WriteString("3. **Column header:** `## ColumnName` (the Done section is omitted when `done_limit=0`).\n")
	b.WriteString("4. **Task line:** `- KEY [priority type] Title -- est <n><unit> -- @<assignee> -- lease <actor> <remaining>|expired -- sub <done>/<total> -- blocked-by <KEY>,<KEY> -- #tag #tag -- v<version> -- age <duration>`\n")
	b.WriteString("   - `[priority type]`: If priority is `none`, only `[type]` is rendered. When non-zero, rendered as `[high bug]`, `[critical feat]`.\n")
	b.WriteString("   - Title: internal whitespace is collapsed, newlines stripped, and literal ` -- ` replaced with ` - `.\n")
	b.WriteString("   - Labeled suffixes appear in fixed order, separated by ` -- `, and are omitted when empty:\n")
	b.WriteString("     - `est <n><unit>`: e.g. `est 2h`, `est 1.5d`. `<unit>` is the project's configured `estimate_unit`.\n")
	b.WriteString("     - `@<assignee>`: assignee username or agent name.\n")
	b.WriteString("     - `lease <actor> <remaining>|expired`: time remaining on lease (`43m`, `2h`, `45s`, `3d`) or `expired`.\n")
	b.WriteString("     - `sub <done>/<total>`: subtasks completion status.\n")
	b.WriteString("     - `blocked-by <KEY>,<KEY>`: comma-separated open blocker task keys.\n")
	b.WriteString("     - `#tag #tag`: tags prefixed with `#`, sorted lexicographically.\n")
	b.WriteString("     - `v<version>`: **Always present** on every task line. Enables direct read-modify-write without extra reads.\n")
	b.WriteString("     - `age <duration>`: time elapsed since task entered the current column.\n")
	b.WriteString("\n")
	b.WriteString("### Worked Example\n")
	b.WriteString("\n")
	b.WriteString("```text\n")
	b.WriteString("compact_version=1\n")
	b.WriteString("# BMB BeeMemoryBank -- focus BMB-14 -- Doing 2/3 -- Review 1 -- Backlog 1 -- Done 40 (hidden) -- v9\n")
	b.WriteString("## Doing\n")
	b.WriteString("- BMB-14 [high bug] Fix WAL checkpoint race -- est 2h -- @alex -- lease claude@rog 43m -- sub 1/3 -- #sync -- v7 -- age 2h\n")
	b.WriteString("- BMB-17 [medium feat] Encrypted FTS index -- est 8h -- lease codex@desk expired -- blocked-by BMB-14,BMB-9 -- v3 -- age 3d\n")
	b.WriteString("## Review\n")
	b.WriteString("- BMB-12 [task] Squash migrations 41-44 -- est 1h -- @alex -- v2 -- age 1d\n")
	b.WriteString("## Backlog\n")
	b.WriteString("- BMB-18 [critical bug] Sync loses tombstones -- #sync -- v1\n")
	b.WriteString("```\n")
	return b.String()
}

var errorHandlingBody = buildErrorHandling()

func buildErrorHandling() string {
	var b strings.Builder
	b.WriteString("All failures return `{ok: false, error: {code, message, remediation}}` with `isError` set on the tool response. `remediation` names the concrete next action.\n")
	b.WriteString("\n")
	b.WriteString("### Error Codes (`domain.Code`)\n")
	b.WriteString("\n")
	b.WriteString("| Code | Meaning | Typical Remediation |\n")
	b.WriteString("|---|---|---|\n")
	b.WriteString("| `not_found` | The target key does not exist on a board the actor can see. | Re-list with board_get and retry with the correct key. |\n")
	b.WriteString("| `validation` | An input field failed validation (type, format, length, enum). | See the `message`; the named field is the offender. |\n")
	b.WriteString("| `conflict` | `if_version` does not match the current version, or another writer raced. | Merge your change into `error.current` and retry with `if_version=N+1`. |\n")
	b.WriteString("| `blocked` | An open `blocks` dependency prevents the operation. | Finish or remove the open blocker and retry. |\n")
	b.WriteString("| `wip_exceeded` | The destination column has no WIP slot. | Finish another task in that column, or wait for a lease to expire. |\n")
	b.WriteString("| `claimed` | Another actor holds a live lease. | Wait for the lease to expire, or use `task_claim force: true` if you are admin. |\n")
	b.WriteString("| `forbidden` | Token lacks the required scope, project access, or admin role. | Use a token with the right scope or project_keys. |\n")
	b.WriteString("| `cycle` | The proposed link or parent chain would create a cycle. | Restructure the chain so it is acyclic. |\n")
	b.WriteString("| `rate_limited` | The token exceeded its per-minute budget. | Slow down; the budget resets every minute. |\n")
	b.WriteString("| `payload_too_large` | The request body exceeded 1 MiB. | Split the batch; the cap is `MaxRequestBodyBytes`. |\n")
	b.WriteString("| `idempotency_mismatch` | The `idempotency_key` was reused with different content. Task-item keys expire after 24 hours; a `project_post` send key and a command acceptance live as long as the message they belong to. The original is intact and is named in the message. | Replay the exact original request to get the original response back, or pick a fresh key for the new content. |\n")
	return b.String()
}
