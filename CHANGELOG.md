# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.2] - 2026-09-25

### Fixed

- The exclusive lock is taken before the database file is opened, not after
  its integrity check and migrations have run. A `token add`, `setting set`,
  `import` or `restore` aimed at a database a server is serving now refuses
  before touching the file; until now it migrated the live file first and
  refused afterwards. `store.Options.Exclusive` replaces the separate
  `LockExclusive` step, and `Close` releases the lock.
- A patch that changes nothing (a label the issue already has, its current
  priority or status, an archive of an archived issue) is a no-op: the version
  does not move and no event is recorded, so a repeated or redundant write can
  no longer make another writer's `expected_version` fail.
- Issue keys are matched case-insensitively (`trackd issue show gdl-12`), like
  every other lookup already was.
- `label` and `exclude_label` given with a view are added to the view's own
  labels instead of replacing them, so an explicit label narrows the view as
  the docs said it did.
- Quick action buttons on a view row post the action's name rather than its
  position, so a view edited between the page render and the tap applies the
  action the person read, or refuses, never a neighbour.
- A request that died (context canceled or timed out), a transaction the
  driver gave up on, or a value the JSON encoder could not write is reported
  as a 500 `internal`, not a 400 `validation`, so an agent knows to retry
  rather than blame its input.

## [0.3.1] - 2026-09-21

### Added

- Saved views: a named issue filter stored server-side, so a queue can be
  opened by name from every interface. Migration `0005_views.sql` (schema 5);
  `GET/POST /api/v1/views` and `GET/PATCH /api/v1/views/{name}`;
  `trackd view list|show|create|update|delete|restore`;
  `trackd issue list --view NAME` and `?view=NAME` on the issue list, whose
  explicit filters layer on top of the view's; MCP `list_views` and
  `save_view`, and `view` on `list_issues`. A view filters on statuses, status
  types, project, labels, excluded labels, assignee, milestone, priorities, a
  relative `updated_within` window (`7d`, `48h`), creator, text and order. It
  is shared by default or private to its owner and admins; only the owner or
  an admin may change it; deleting archives it and frees the name. Views ride
  in the JSONL dump and are audited (`view.created|updated|archived|restored`,
  `trackd events --entity view`).
- Quick actions on a view: up to four named one-tap patches (status, priority,
  add or remove labels) shown as buttons on every row of the view in the web
  board (`--quick "Answered: remove=waiting"`).
- The web board works a queue. Every saved view is a tab and a plain URL
  (`/ui/view/<name>`); each row opens inline to the latest comment, a reply
  box, the quick actions, and a status, priority and label form. The issue
  page gains the same action form. Every write goes through the store like
  the API, is attributed to the signed-in token, records the same audit event,
  and carries the issue version the page was rendered with: a change that
  lost a race is refused with a notice (the reply typed with it is still
  saved). Views can be created, edited and archived from the board (`+ view`).
- `issue list --priority` (repeatable, matches any) and `--created-by`, and
  the matching `priority` and `created_by` list parameters on the API and
  `priorities` and `created_by` on MCP `list_issues`.

### Changed

- The board and issue page lay out in one column on a phone: no sideways
  scrolling, full-width controls, tabs that scroll.
- `trackd view` is now the view command group, so it no longer hints at
  `trackd issue view` (which still works as an alias of `issue show`).

## [0.3.0] - 2026-09-20

### Added

- Web board logins are durable: sessions persist in the database (hashed, like
  API tokens), survive server restarts, and slide their 30-day expiry on use,
  so a browser stays signed in until it signs out. Revoking a token ends its
  sessions. Migration `0004_ui_sessions.sql`; /healthz now reports schema 4.
- The issue page takes comments: posted as the signed-in token, attributed and
  audited exactly like an API comment. A "use as reply" button on each comment
  copies its body into the reply box, so reply-with-this-text flows (reviews,
  approvals) work from a phone. Cross-origin form posts are refused.
- `trackd comment <key> --body <text>` now adds a comment, as an alias for
  `trackd issue comment`, so the natural guess works instead of erroring.
- `trackd comment list <key>` lists an issue's comments with their ids, so a
  caller editing or replying to one reads the id off the list instead of
  guessing it.
- A token can be read from a file named by `$TRACKD_TOKEN_FILE`, keeping the
  credential out of the environment and the process table. The order is
  `--token`, then `$TRACKD_TOKEN`, then the file.
- `workspace_name` is a writable setting (`trackd setting set workspace_name`).
  It sets the name shown in the web board header and was previously only
  reachable by editing the database directly.

### Changed

- The web board is no longer described as read-only; commenting is the one
  write it offers. All other writes stay with the API, CLI, and MCP.
- CLI `--json` list output is now enveloped everywhere, matching the REST API:
  `project list`, `label list`, `milestone list`, `statuses`, `issue events`
  and `issue relate` return `{"projects": [...]}`, `{"labels": [...]}` and so
  on rather than a bare array. This is a breaking change for scripts that
  parsed the bare array from those commands.
- `trackd <verb>` where `<verb>` is really an issue subcommand (`show`, `list`,
  `create`, ...) now answers with a `did you mean "trackd issue <verb>"?` hint
  instead of dumping the whole usage screen.
- `trackd issue update` help marks `--replace-description` and
  `--clear-description` as admin-token only, and a 403 refusal now points at
  `trackd issue append` as the path an agent token can take.

### Fixed

- `trackd <command> --help` (and `-h`) exits 0 instead of 2, so a harness or
  script does not read a help request as an error.
- `/healthz` reports the schema version derived from the embedded migrations
  rather than a hand-maintained constant that could drift.
- The server answers `/favicon.ico` with 204 No Content, so a browser board
  visit no longer logs a 404 for it.

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
