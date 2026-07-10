# trackd

Self-hosted task tracker for AI agents. Single Go binary: SQLite storage, REST API,
CLI, MCP endpoint, embedded read-only web UI.

## Commands

- Build: `go build ./...`
- Test: `go test -race ./...`
- Format: `gofmt -w .` (CI rejects unformatted code)
- Vet: `go vet ./...`

## Architecture

- `main.go` — entry point, subcommand dispatch, server-side commands (serve,
  token, backup, restore, export, import)
- `cli.go` — client commands (issue, project, label, statuses, health) that talk
  HTTP to a running server
- `internal/store` — SQLite storage layer: schema migrations, CRUD, audit events,
  backup/restore, JSONL export/import, Linear CSV importer. All writes go through
  this package.
- `internal/server` — HTTP layer: REST API + bearer auth, MCP endpoint
  (`mcp.go`), embedded zero-JS web UI (`ui.go`, templates in `ui/`), backup
  scheduler
- `internal/client` — thin HTTP client used by the CLI

## Hard rules

- **Data safety over everything.** No hard deletes — rows are archived, canceled, or
  revoked, never destroyed. Every mutation records an audit event with before/after
  state. Never weaken the SQLite durability pragmas. Migrations snapshot the database
  before applying.
- Pure Go, no CGO — releases cross-compile.
- Keep dependencies minimal; justify any new one.

## Style

- Conventional commits (`feat:`, `fix:`, `chore:`), imperative mood, no emoji, no
  AI-attribution or Co-Authored-By trailers.
- Code comments only where they explain a non-obvious constraint or decision.
- Every storage feature ships with tests; prefer table-driven tests.
