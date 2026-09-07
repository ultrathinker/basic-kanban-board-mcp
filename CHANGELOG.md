# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims
to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) from its
first tagged release onward.

## [Unreleased]

Pre-1.0 development. The public surface — nine MCP tools, the compact board
grammar, the CLI, and the web UI — is settling toward a `v1.0.0` tag. Until
then, minor breaking changes may land without a major-version bump.

### Added
- Native MCP server over streamable-HTTP with mandatory bearer auth.
- Nine MCP tools: `board_get`, `task_next`, `task_get`, `task_create`,
  `task_update`, `task_claim`, `task_link`, `task_remove`, `project_upsert`.
- Compact board read (~90% fewer tokens than a JSON dump).
- Dependency-aware `task_next` with an atomic claim/start; optimistic
  concurrency (`if_version`) with conflicts carrying the current state.
- Monochrome web UI with a live board, an agent-setup page, ready-to-paste
  agent prompts, and an admin "new project" form.
- Single-binary distribution, a distroless Docker image, and a labeled demo
  seeded on a fresh database.
