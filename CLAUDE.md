# trackd

Self-hosted task tracker for AI agents. Single Go binary: SQLite storage, REST API,
CLI, MCP endpoint, embedded web UI (the board, saved views as tabs, an issue
page; writes are comments, replies, status, priority, label and quick-action
changes from a view row or the issue page, plus creating and editing views).

## Commands

- Build: `make build` (compiles to `bin/trackd`); `go build ./...` for a quick
  compile check. The version is a hand-bumped `const` in `main.go`; `make dist`
  builds the release archives and refuses a dirty tree unless `ALLOW_DIRTY=1`.
  Versioning: this is a long-lived 0.x project, so bump the patch (0.0.1) for
  fixes and hotfixes and the minor (0.1.0) for a release that adds features or
  changes a contract; several patch releases between minors is the normal
  rhythm, and a roadmap phase never maps to a version number
- Test: `make test` (`go test -race ./...`)
- Lint: `make lint` (gofmt check, `go vet ./...`, golangci-lint when installed)
- Format: `gofmt -w .`

## Architecture

- `main.go`: entry point, subcommand dispatch, server-side commands (serve,
  token, setting, backup, restore, export, import), exit-code mapping
- `cli.go`: client commands (issue, comment, events, project, milestone, label,
  statuses, health) that talk HTTP to a running server; `cli_views.go` is the
  `view` group
- `internal/store`: SQLite storage layer. Schema migrations, CRUD, audit events,
  backup/restore, JSONL export/import, Linear CSV importer, the advisory file
  lock, and the operator settings (`settings.go`: the known keys and their
  validation). All writes go through this package.
- `internal/server`: HTTP layer. REST API and bearer auth (`handlers.go`,
  `views.go`), MCP endpoint (`mcp.go`), embedded web UI (`ui.go` for auth,
  board and issue page; `ui_views.go` for view pages, the view editor and the
  one form-post write path `POST /ui/issue/{key}/action`; templates in `ui/`),
  backup scheduler, health checker
- `internal/client`: HTTP client used by the CLI. `client.go` is the transport
  (retries, timeout, `APIError`); `api.go` holds the typed calls that own the
  JSON field names and unwrap the list envelopes.

## Hard rules

- **Data safety over everything.** No hard deletes: rows are archived, canceled,
  or revoked, never destroyed. Every mutation records an audit event with
  before/after state. Never weaken the SQLite durability pragmas. Migrations
  snapshot the database before applying.
- **Descriptions are append-only.** An update that would overwrite a non-empty
  description is refused unless the request carries `replace_description`, which
  in turn needs an `admin` token (403 `forbidden` otherwise). Keep it that way:
  it is what stops one agent erasing another's context.
- **The web UI writes through the store like the API.** A form post goes
  through the same store method, carries `expected_version` from the page it
  was rendered on, and records the same audit event. No UI-only write paths.
- **Views are owned.** Only the owner token or an admin changes a view; a
  private view answers 404 to anyone else. Archiving frees the name.
- **One writer.** `serve` holds an exclusive advisory lock on the database file.
  Commands that only read (`export`, `backup`, `token list`, `setting list|get`)
  open read-only; commands that write (`token add`, `token revoke`,
  `setting set`, `import`, `restore`) take the lock and refuse when a server
  holds it.
- Nothing personal ships in the product or its tests: no default label names,
  key prefixes, people or client names from the author's own setup. Fixtures
  use neutral names (`ACME`, `ready`, `blocked`, `Alex`).
- Pure Go, no CGO, so releases cross-compile.
- Keep dependencies minimal; justify any new one.
- Keep it simple. No abstraction, layer, option or flag without a present need
  in the code; the plain version that is easy to read and test beats the
  general one. A refactor earns its place by removing duplication that exists
  today, not by preparing for features that might come.

## Contracts worth knowing before you change anything

- Every list response is an envelope (`{"issues": [...], "next_offset": ...}`,
  `{"comments": [...]}`, and so on), never a bare array. Changing one breaks the
  CLI, the MCP tools and the UI together.
- Errors are `{"error": ..., "code": ...}`. The CLI maps the status to an exit
  code: 0 ok, 1 unexpected, 2 usage or validation, 3 not found, 4 auth,
  5 conflict, 6 server or network. Scripts branch on those numbers.
- Timestamps are UTC RFC3339 with milliseconds. Parse every incoming timestamp
  through `store.ParseTimestamp`.
- The CLI never accepts a bare empty string as a value: clearing a field is an
  explicit `--clear-*` flag, and `-` reads stdin but fails when stdin is empty.
- A create is only retried when it carries an idempotency key.

## Style

- Conventional commits (`feat:`, `fix:`, `chore:`), imperative mood, no emoji, no
  AI-attribution or Co-Authored-By trailers.
- Code comments only where they explain a non-obvious constraint or decision.
- No em dashes in prose, in code comments, or in user-facing strings.
- Every storage feature ships with tests; prefer table-driven tests.
- Client and CLI tests speak the REST contract by hand against an `httptest`
  server, so the packages can be built and tested in either order.
