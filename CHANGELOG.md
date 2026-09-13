# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.0] - 2026-09-13

### Added

- `issue get` and `issue view` as aliases for `issue show`.
- `issue list --search` as an alias for `-q`.
- `issue list --columns <cols>` and `--tsv` for a lightweight, flat list output
  (a subset of `key,status,priority,project,assignee,labels,title`), so a queue
  read does not need to parse the full JSON. The default table and default
  `--json` output are unchanged.
- `--priority` on `issue create` and `issue update` accepts a word
  (`none|urgent|high|medium|low`) as well as the `0-4` integer.
- `issue create --milestone` (and `update`) accepts a milestone id as well as a
  name, matching how `milestone update` addresses one.
- `agent_label` setting: a label the server auto-applies to issues created by
  non-admin tokens (empty by default, which disables it).

### Changed

- `statuses list` is accepted as a synonym for `statuses` (the `list` verb from
  `project list` / `label list` is tolerated rather than rejected).
- `trackd comment create` / `add` / `new` now point at `trackd issue comment`
  rather than returning a bare "unknown subcommand".

## [0.1.1] - 2026-09-07

### Added

- `setting set` records a `setting.updated` audit event (entity `setting`, the
  key as `entity_key`, with `--actor`).
- Error-code reference tables in `docs/API.md` (every code with its HTTP status
  and CLI exit code) and `docs/CLI.md` (the four codes the CLI raises itself).

### Changed

- `issue append`, `issue comment` and `comment edit` accept `--text` and
  `--body` interchangeably. Passing both at once is a usage error.
- `issue relate --remove` is idempotent: the server answers 200 with
  `"removed": true|false` rather than 404, and the CLI exits 0 either way and
  says which happened. A mistyped key or relation type is still an error.

### Fixed

- `trackd comment <ISSUE-KEY>` now points at `trackd issue comment` instead of
  the opaque "unknown comment subcommand".
- The admin and server commands (`setting`, `token`, `export`, `import`,
  `backup`, `restore`, `serve`) report an unknown flag as a usage error
  (exit 2, `code: "usage"`) with a JSON error object, instead of exit 1 /
  `internal`.
- A missing API token is caught locally and reported as exit 4 (`unauthorized`)
  with an actionable message, instead of after a wasted request.
- `label add --color` rejects a value that is not a `#rgb` or `#rrggbb` hex
  color (422) instead of storing it and drawing nothing.
- Issue titles and project and milestone names are trimmed and must be 1 to 500
  characters, on create and on update (422 otherwise).
- errcheck findings across the store and server.

## [0.1.0] - 2026-09-06

Initial public release: self-hosted task tracker for AI agents. Single Go
binary with SQLite storage, a REST API, a CLI, an MCP endpoint, and an embedded
read-only dashboard. Append-only descriptions, add/remove labels with optional
exclusive groups, issue versions and idempotency keys, a global activity feed,
comment threads and edits, assignees, project milestones, stable exit codes,
and a configurable `setting` command (issue prefix, label groups, base URL).
