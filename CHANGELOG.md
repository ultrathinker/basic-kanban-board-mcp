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
- Native MCP server over streamable-HTTP with mandatory bearer auth; see
  `docs/MCP-TOOLS.md` for the current tool list.
- Compact board read (~90% fewer tokens than a JSON dump).
- Dependency-aware `task_next` with an atomic claim/start; optimistic
  concurrency (`if_version`) with conflicts carrying the current state.
- Monochrome web UI with a live board, an agent-setup page, ready-to-paste
  agent prompts, and an admin "new project" form.
- Single-binary distribution, a distroless Docker image, and a labeled demo
  seeded on a fresh database.
