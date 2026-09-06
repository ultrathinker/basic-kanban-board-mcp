# basic-kanban-board-mcp

A kanban board your coding agents can actually use. One binary, one file, nine tools.

Point Claude Code, Codex or Cursor at it and your agents plan their work as tasks, take the
next one, and report progress — while you watch the board in a browser and drop in new work
without restarting anything.

```
compact_version=1
# TETRIS Tetris Dogfood · focus none · Backlog 2 · Doing 1 · Done 5 (hidden)
## Backlog
- TETRIS-5 [medium feat] Game over detection and restart · est 1h · v1
- TETRIS-6 [medium chore] Final verification pass · est 1h · blocked-by TETRIS-5 · v1
## Doing
- TETRIS-8 [high feat] Add a pause control (P key and a button) · lease claude 59m · #ux · v2 · age 30s
```

That is a whole project, as an agent sees it.

## Why

Most MCP task servers expose 40 to 170 tools and hand back JSON. An agent then spends its
context deciding which of 170 tools to call and parsing the dump it gets back.

This one exposes **nine tools** and answers in the compact text above.

| the same 30-task board | tokens |
| --- | --- |
| this format | **~1 060** |
| minified JSON | ~5 640 |
| pretty-printed JSON (what most servers return) | ~10 650 |

About **90% fewer tokens** than a JSON board dump. Measured, not estimated — the number is
a release gate in the test suite, so it cannot quietly drift.

## Quickstart

```bash
kanban serve
```

That is it. No database to install, no config file, no Docker required. It creates a SQLite
file, prints an admin token once, and serves the board at `http://127.0.0.1:8080`.

Then point your agent at it:

```json
{
  "mcpServers": {
    "kanban": {
      "type": "http",
      "url": "http://127.0.0.1:8080/mcp",
      "headers": { "Authorization": "Bearer kbn_..." }
    }
  }
}
```

Open `/agent-setup` in the browser and it gives you that snippet, filled in, for whichever
client you use.

## The nine tools

| tool | what it does |
| --- | --- |
| `board_get` | the whole board, compact — cheap enough to call every session |
| `task_next` | peek, claim, or claim-and-start the next unblocked task, in one round trip |
| `task_get` | full detail on specific tasks, by key |
| `task_create` | create tasks in a batch, with `@ref` for dependencies inside the batch |
| `task_update` | edit, move, note, tick acceptance — batched, with optimistic concurrency |
| `task_claim` | claim, renew or release a lease |
| `task_link` | add or remove blocking relationships |
| `task_remove` | archive or restore |
| `project_upsert` | create or reconfigure a project and its columns |

There will not be a tenth. Everything else is a plugin.

## What makes it safe for several agents at once

- **Leases.** Claiming a task is a single atomic compare-and-swap. Two agents calling
  `task_next(start)` on the same column cannot both win.
- **Optimistic concurrency.** Edits carry `if_version`; a conflict comes back with the
  current state attached, so the agent can retry without re-reading.
- **Dependencies are enforced, not advisory.** A blocked task is not offered by
  `task_next`, and the response says why it was skipped.
- **WIP limits** are real limits.

## The board is for you

The web UI is deliberately plain: monochrome, no build step, no JavaScript framework. It
exists so a human can see what the agents are doing and steer them — drop a card in the
backlog and the next `task_next` picks it up. No prompt editing, no restart.

## Install

```bash
go install github.com/ultrathinker/basic-kanban-board-mcp/cmd/kanban@latest
```

Or grab a binary from [Releases](../../releases), or `docker run`. Single static binary,
no cgo, no runtime dependencies.

## Not for you if

You want sprints, story points, burndown charts, time tracking, or a Jira replacement.
This is the basic one. That is the whole point.

## License

MIT.
