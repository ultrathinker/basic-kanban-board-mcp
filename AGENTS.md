# AGENTS.md — instructions for agents implementing this project

Read this first, then `docs/PLAN.md` (the spec of record), then your own task brief.

## What this project is

**basic-kanban-board-mcp** — a minimal, self-hosted kanban board for AI coding agents.
One static Go binary, one SQLite file, one port. A native MCP server over remote
streamable-HTTP with mandatory bearer auth, so many agents on many machines share **one**
board while a human watches it live in a browser.

**Goal #1 is adoption.** Thousands of installs. Every decision is judged first by: does this
make the product easier to install, understand and recommend? Goal #2: the owner dogfoods it
daily across three machines with several agent CLIs.

**The four differentiators** — no competitor combines more than two, so they are not
negotiable:
1. batch task writes (arrays, not one-item calls)
2. a compact board read cheap enough to call every session (≤ 600 tokens for 30 tasks)
3. dependency-aware `task_next` with an atomic claim/start
4. real optimistic concurrency (`if_version`, conflicts carry the current state)

## Non-negotiables

- **Go 1.27, `CGO_ENABLED=0`, stdlib `net/http`.** No web framework, no ORM.
- **Nine MCP tools. Do not add a tenth.** Extra capability goes into parameters.
- **English everywhere**: code, comments, docs, UI strings, commit messages.
- **No npm, no node_modules.** Tailwind runs from its standalone binary; the generated CSS is
  committed so a plain `go build` always works.
- **Actor identity comes from the token.** No tool, endpoint or form takes an `actor` parameter.
- **Fail loud.** Never silently ignore, downgrade or "fix up" a caller's request. If something
  is refused, say which rule refused it and what to do instead (`Remediation` on every error).
- **`internal/domain` stays pure** — no database, no HTTP, no MCP imports. It must be testable
  with plain Go values.
- **Only `internal/service` may call `internal/store`.** MCP, web and CLI call the service.

## Do not disturb the human's desktop

A person is working at this machine while you run. Anything that opens a window, steals focus or
runs unattended in the background is disruptive, so:

- **Never start a long-running or foreground process**: no dev servers, no `go run` of anything
  that listens, no watchers, no `docker run`, no browsers. If a task needs a server to be
  verified, write the code and the test, and say in your final message how to run it — the
  coordinator will.
- **Never install anything system-wide** — no `winget`, `choco`, `apt`, `npm -g`, no installers,
  no toolchain downloads. If a tool you need is missing, stop and report it instead.
- **Do not run `docker build` or any container command.** CI does that.
- Compiling and running unit tests is fine (`go build`, `go vet`, `go test`) — keep it to that.
- **`go test -race` works here — use it.** It needs cgo, which the shipped binary does not
  (we release with `CGO_ENABLED=0`), so run it with the toolchain explicitly:

  ```bash
  CGO_ENABLED=1 PATH="/c/msys64/mingw64/bin:$PATH" go test ./your/package/... -race
  ```

  This paragraph used to say the opposite — that there was no C toolchain here on purpose —
  and three separate agents wasted words reconciling that with a brief telling them to run
  it. MSYS2 is installed and gcc is at that path. **Do not install anything to make this
  work**; if the toolchain is genuinely missing, stop and report it.
- Concurrency correctness is this product's headline claim, so a package that touches shared
  state without a `-race` run is not finished.
- Prefer one combined command over many small ones; every shell invocation flashes a console
  window on Windows.

## Your boundaries

Your brief names the packages you own. **Do not create or edit files outside them**, and in
particular never touch: `go.mod`/`go.sum` (all dependencies are already pinned),
`internal/domain/*`, `internal/store/store.go`, `internal/service/service.go`,
`internal/store/migrations/*`, `docs/PLAN.md`. These are frozen contracts that other agents are
building against in parallel. If you believe a contract is wrong, **stop and say so in your
final message** rather than changing it — a unilateral change breaks four other agents.

## Definition of done for your chunk

1. `go build ./...` succeeds.
2. `go vet ./...` is clean.
3. `go test ./... -race` passes, and **your package has its own tests** — you write them, they
   are part of the chunk, not a later phase.
4. Table-driven tests for anything rule-shaped; a test per error code you can produce.
5. No `TODO`, no stub returning `nil, nil`, no panic-on-unimplemented left behind.
6. Public identifiers have doc comments. Comments explain **why**, never what the line does.

## House style

- Errors: return `*domain.Error` via the constructors in `internal/domain/errors.go` so every
  surface reports the same code and remediation. Wrap lower-level errors, never swallow them.
- Time: the database clock (`Tx.Now()`) inside a transaction; `time.Time` in UTC everywhere.
  Never compare a lease against a client-supplied timestamp.
- Transactions: all checks (WIP, dependencies, cycles, sequence allocation, versions, claims,
  idempotency) happen **inside** the write transaction. A Go pre-check followed by a separate
  write is a race, and this product's whole pitch is that it does not have those.
- Versions: use `domain.BumpsTaskVersion` / `domain.BumpsProjectVersion`. Leases and notes
  deliberately do **not** bump a task's version — a background lease renewal must never
  invalidate another agent's `if_version` edit.
- Naming: `internal/...` packages are lowercase single words. Exported names read as English.
- Keep functions small enough to test. Prefer a pure function plus a thin I/O caller.

## Deviating from the plan

The plan was written before any code existed, so reality wins — **but never silently**. If you
must depart from what `docs/PLAN.md` says, then in your final message report: the section, what
it planned, what you did, and the fact that forced it. The coordinator records it in the
plan's §18 Deviations log. An undocumented deviation is treated as a bug.

## Reference layout

```
cmd/kanban/            CLI entry point and subcommands
internal/domain/       FROZEN. entities, enums, limits, errors, pure rules
internal/store/        store.go is FROZEN (interfaces); the SQLite implementation is code
internal/service/      service.go is FROZEN (interfaces); the use-cases are code
internal/mcp/          the nine tools, schemas, instructions, compact renderer
internal/web/          handlers, templates, SSE, sessions
internal/events/       in-process bus
internal/auth/         tokens, sessions, middleware, trusted proxies, rate limit
internal/config/       flags/env and the startup refusal rules
web/templates/ web/static/   embedded assets
docs/PLAN.md           the spec of record — read it, do not edit it
```
