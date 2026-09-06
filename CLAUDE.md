# trackd

Self-hosted task tracker for AI agents. Single Go binary: SQLite storage, REST API,
CLI, MCP endpoint, embedded read-only web UI.

## Commands

- Build: `make build` (stamps the version, refuses a dirty tree unless
  `ALLOW_DIRTY=1`); `go build ./...` for a quick compile check
- Test: `make test` (`go test -race ./...`)
- Lint: `make lint` (gofmt check, `go vet ./...`, golangci-lint when installed)
- Format: `gofmt -w .`

## Architecture

- `main.go`: entry point, subcommand dispatch, server-side commands (serve,
  token, setting, backup, restore, export, import), exit-code mapping
- `cli.go`: client commands (issue, comment, events, project, milestone, label,
  statuses, health) that talk HTTP to a running server
- `internal/store`: SQLite storage layer. Schema migrations, CRUD, audit events,
  backup/restore, JSONL export/import, Linear CSV importer, the advisory file
  lock, and the operator settings (`settings.go`: the known keys and their
  validation). All writes go through this package.
- `internal/server`: HTTP layer. REST API and bearer auth, MCP endpoint
  (`mcp.go`), embedded zero-JS web UI (`ui.go`, templates in `ui/`), backup
  scheduler, health checker
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
