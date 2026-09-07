# Contributing to basic-kanban-board-mcp

Thanks for your interest. This is a deliberately small project — one static Go
binary, a native MCP server, and a plain web UI — and it intends to stay that
way. Please read this before opening a PR.

## Ground rules

- **Nine MCP tools. There is no tenth.** New capability goes into parameters of
  the existing tools, not a new tool. A PR that adds a tool will be declined.
- **English everywhere**: code, comments, docs, UI strings, commit messages.
- **No web framework, no ORM, no npm.** Standard library `net/http`, the pure-Go
  SQLite driver, and hand-authored CSS/JS. The generated Tailwind CSS is
  committed so a plain `go build` always works.
- **Fail loud.** Never silently ignore or "fix up" a caller's request; refuse it
  with a message that names the rule and the remedy.

## Development setup

Requires **Go 1.27+**. No other toolchain.

```bash
go build ./cmd/kanban      # build the binary
go vet ./...
go test ./...              # full suite
CGO_ENABLED=1 go test ./... -race   # race detector (needs a C toolchain)
gofmt -l .                 # must print nothing
```

Run it locally:

```bash
go run ./cmd/kanban serve   # http://127.0.0.1:8080, admin token printed once
```

## Web assets: edit the canonical tree, then mirror

The web assets exist twice: `web/{static,templates}` is the **canonical source
you edit**, and `internal/web/{static,templates}` is a byte-identical mirror
that `//go:embed` compiles into the binary. After editing anything under `web/`:

```bash
make sync-embed     # copies web/ -> internal/web/
```

CI fails if the two trees drift. The stylesheet is also held to a monochrome,
zero-elevation palette by tests in `internal/web/view` — greyscale only.

## Pull requests

1. Branch from `master`.
2. Keep the change focused; one concern per PR.
3. `go build`, `go vet`, `go test ./...`, and `gofmt -l .` must all be clean.
4. Add or update tests for behavior changes.
5. Use conventional-commit style subjects: `feat:`, `fix:`, `docs:`, `test:`,
   `refactor:`, `chore:`.
6. Update `docs/` and `CHANGELOG.md` when behavior or the public surface changes.

## Reporting issues

Use the issue templates. For anything security-sensitive, follow
[SECURITY](docs/SECURITY.md) instead of opening a public issue.
