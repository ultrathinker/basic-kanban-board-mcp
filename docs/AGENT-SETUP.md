# Agent Setup & MCP Client Configuration

This guide explains how to connect AI coding agents to basic-kanban-board-mcp over streamable-HTTP with bearer authentication.

---

## Verified Client Compatibility Matrix

| Client | Verification Status | Transport | Auth Support |
|---|---|---|---|
| **Claude Code** | **Verified Live** (tested end-to-end today) | Streamable-HTTP (`/mcp`) | `Authorization: Bearer kbn_…` |
| **Codex CLI** | **Not Verified** (configuration provided) | Streamable-HTTP (`/mcp`) | `Authorization: Bearer kbn_…` |
| **Cursor** | **Not Verified** (configuration provided) | Streamable-HTTP (`/mcp`) | `Authorization: Bearer kbn_…` |
| **MCP Inspector** | **Not Verified** (configuration provided) | Streamable-HTTP (`/mcp`) | `Authorization: Bearer kbn_…` |

> **Honesty Notice:** Claude Code was verified live today across a full plan-build-close dogfood cycle. Codex, Cursor, and MCP Inspector configs match their respective MCP documentation and schemas, but have not yet been tested end-to-end against a live server.

---

## Step 1: Mint an Agent Bearer Token

Every agent authenticates using an opaque bearer token starting with `kbn_`. The token name defines the **actor identity** on the board — all tasks created, updated, leased, or commented on by this token will display this name.

### Create a Token

Run the `kanban token create` CLI command:

```bash
# Mint a write token for an agent named "claude@workstation"
kanban token create --name claude@workstation --scope write

# Mint a token restricted to a single project
kanban token create --name codex@laptop --scope write --project BMB

# Mint an admin token for human orchestration
kanban token create --name lead-admin --scope admin
```

The CLI prints the token secret once:
```text
Token: claude@workstation
Secret: kbn_mzxw6ytboi2dknrugq2tqmbrgiydimjrgq3tqmbq
Scope: write
The secret is shown only this once — store it now or pass it directly to your MCP client.
```

### Understanding Token Scopes

| Scope | Permissions | Intended Role |
|---|---|---|
| `read` | Read board state (`board_get`), fetch task details (`task_get`), peek ready work (`task_next` with `action: "peek"`), and request SSE connection tickets (`POST /events/ticket`). Mutations are rejected. | Observer agents, documentation generators, auditors. |
| `write` | **Implies `read`**. Full task operations: create (`task_create`), update (`task_update`), atomic claim and start (`task_next`), leases (`task_claim`), dependency linking (`task_link`), and archiving (`task_remove`). | **Standard coding agents** (Claude Code, Codex, Cursor). |
| `admin` | **Implies `write` and `read`**. Manage tokens (`create`, `rotate`, `revoke`), create/modify project definitions and columns (`project_upsert`), bypass transition checks using `force: true` with a mandatory reason, steal active leases held by other agents, and execute hard task purges via CLI (`kanban task purge`). | Human administrators, pipeline orchestrators. |

### Project Restrictions

By default, an empty `--project` allows the token to access all projects. Supplying `--project KEY` restricts the token strictly to that project key; attempts to read or mutate other projects fail with `code: "forbidden"`.

### Token Management Commands

```bash
# List all minted tokens, their scopes, status, and last used timestamps
kanban token list

# Rotate an existing token secret (updates in place, invalidates previous secret)
kanban token rotate claude@workstation

# Revoke an agent token immediately
kanban token revoke claude@workstation
```

---

## Step 2: Configure Your MCP Client

Replace `http://127.0.0.1:8080` with your actual server base URL (or `https://kanban.example.com` if hosted remotely) and replace `kbn_…` with your generated token secret.

### 1. Claude Code

Claude Code reads MCP servers from either a project-level `.mcp.json` (recommended for repository check-in) or user-level `~/.claude.json`.

#### Project-level `.mcp.json`:
```json
{
  "mcpServers": {
    "kanban": {
      "type": "http",
      "url": "http://127.0.0.1:8080/mcp",
      "headers": {
        "Authorization": "Bearer kbn_mzxw6ytboi2dknrugq2tqmbrgiydimjrgq3tqmbq"
      }
    }
  }
}
```

Or add it interactively via Claude Code CLI:
```bash
claude mcp add kanban http://127.0.0.1:8080/mcp --header "Authorization: Bearer kbn_..."
```

---

### 2. Codex CLI

Add the server definition to `~/.codex/config.toml`:

```toml
[mcp_servers.kanban]
type = "http"
url = "http://127.0.0.1:8080/mcp"
headers = { "Authorization" = "Bearer kbn_mzxw6ytboi2dknrugq2tqmbrgiydimjrgq3tqmbq" }
```

---

### 3. Cursor

1. Open **Cursor Settings** (`Ctrl+,` or `Cmd+,`).
2. Navigate to **Features** → **MCP**.
3. Click **Add New MCP Server**.
4. Enter the server details:
   - **Name:** `kanban`
   - **Type:** `http`
   - **URL:** `http://127.0.0.1:8080/mcp`
   - **Headers:** `Authorization: Bearer kbn_mzxw6ytboi2dknrugq2tqmbrgiydimjrgq3tqmbq`

---

### 4. MCP Inspector

To inspect the tools and test schemas interactively:

```bash
npx @modelcontextprotocol/inspector
```

In the Inspector UI:
- **Transport Type:** `Streamable HTTP`
- **URL:** `http://127.0.0.1:8080/mcp`
- **Request Headers:**
  - Key: `Authorization`
  - Value: `Bearer kbn_mzxw6ytboi2dknrugq2tqmbrgiydimjrgq3tqmbq`
- Click **Connect**.

---

### 5. Generic HTTP Clients (Curl / Scripts)

Any client supporting streamable-HTTP can interact with `/mcp` directly:

```bash
curl -X POST http://127.0.0.1:8080/mcp \
  -H "Authorization: Bearer kbn_mzxw6ytboi2dknrugq2tqmbrgiydimjrgq3tqmbq" \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "tools/call",
    "params": {
      "name": "board_get",
      "arguments": { "project": "BMB", "format": "compact" }
    }
  }'
```

---

## Automated Config Generation via CLI

The `kanban` binary can generate paste-ready configuration snippets directly:

```bash
# Print configuration snippet for a named client
kanban agent-config --client claude --url http://127.0.0.1:8080 --token kbn_...
kanban agent-config --client codex --url http://127.0.0.1:8080 --token kbn_...
kanban agent-config --client cursor --url http://127.0.0.1:8080 --token kbn_...

# Print recommended agent instructions for AGENTS.md / CLAUDE.md
kanban agent-md
```

You can also view these snippets live in your browser by opening `http://127.0.0.1:8080/agent-setup`.

---

## The Read-Only Endpoint (`/mcp/readonly`)

In addition to `/mcp`, the server exposes `/mcp/readonly`.

### What It Is For
`/mcp/readonly` is designed for agents that only observe, review code, or report status. It eliminates any risk of an agent accidentally creating, updating, or deleting tasks.

### Features & Tool Set
- Only **three tools** are mounted on `/mcp/readonly`:
  1. `board_get`
  2. `task_get`
  3. `task_next`
- All mutating tools (`task_create`, `task_update`, `task_link`, `task_claim`, `task_remove`, `project_upsert`) are completely absent from `tools/list`.
- On `task_next`, mutating actions (`claim` and `start`) are refused loudly:
  ```json
  {
    "ok": false,
    "op": "task_next",
    "error": {
      "code": "forbidden",
      "message": "claim and start are disabled on the read-only endpoint",
      "remediation": "Use action:\"peek\" here, or call this tool against /mcp with a write-scope token to claim or start work."
    }
  }
  ```
- Any valid token (`read`, `write`, or `admin`) can connect to `/mcp/readonly`.

To configure an agent for read-only access, point its URL to `/mcp/readonly`:
```json
{
  "mcpServers": {
    "kanban-readonly": {
      "type": "http",
      "url": "http://127.0.0.1:8080/mcp/readonly",
      "headers": {
        "Authorization": "Bearer kbn_..."
      }
    }
  }
}
```

---

## Recommended `AGENTS.md` Snippet

Add this section to your project repository's `AGENTS.md` or `CLAUDE.md` so coding agents understand how to interact with the board:

````markdown
## Kanban Board Operating Loop

This project uses basic-kanban-board-mcp for task coordination.

1. **Check the board at session start:**
   Call `board_get(project: "PROJ")` to review current column state (default compact format).
2. **Claim your work:**
   Call `task_next(project: "PROJ", action: "start")` to claim the top ready task and move it to Doing in one step.
3. **Log progress as you work:**
   Call `task_update(patches: [{key: "PROJ-1", note: "Added unit tests for WAL checkpoint"}])`.
   Notes are append-only and never conflict.
4. **Mutate fields with optimistic concurrency:**
   When editing fields (title, body, column, acceptance criteria), always include `if_version` from your previous read. If a conflict occurs, merge with `error.current` and retry.
5. **Mark complete:**
   When all acceptance criteria are verified, call `task_update(patches: [{key: "PROJ-1", column: "Done", if_version: N}])`.
````
