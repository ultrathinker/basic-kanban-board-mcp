# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims
to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) from its
first tagged release onward.

## [Unreleased]

Pre-1.0 development. The public surface — the MCP tool set (current list and
count generated at `docs/MCP-TOOLS.md`), the compact board grammar, the CLI,
and the web UI — is settling toward a `v1.0.0` tag. Until then, minor breaking
changes may land without a major-version bump.

### Added
- Executor keys (KANB-60): `executor_key_issue` lets an admin or a project's
  coordinator issue a named, expiring key (default 24h, at most 7 days) for
  one agent. The key's name is the agent's identity and a participant of the
  listed projects; it reads everything but writes only to the cards assigned
  to it (claim, notes, moves outside done columns, progress) and to the
  project feed, so an executor can hand work over but never accept it.
  Expired keys stop authenticating and leave the participant list on their
  own (migration 0010 adds `tokens.expires_at`).
- `task_create` / `task_update` refuse an assignee that is not a participant
  of the project, listing the participants; existing assignees are untouched.
- Native MCP server over streamable-HTTP with mandatory bearer auth; see
  `docs/MCP-TOOLS.md` for the current tool list.
- Compact board read (~90% fewer tokens than a JSON dump).
- Dependency-aware `task_next` with an atomic claim/start; optimistic
  concurrency (`if_version`) with conflicts carrying the current state.
- Monochrome web UI with a live board, an agent-setup page, ready-to-paste
  agent prompts, and an admin "new project" form.
- Single-binary distribution, a distroless Docker image, and a labeled demo
  seeded on a fresh database.
