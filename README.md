# trackd

Self-hosted task and project tracking for AI agents. One binary, one SQLite file,
three interfaces: REST API, CLI, and MCP — plus a read-only web board for humans.

![the trackd board](docs/board.jpeg)

> **Status: pre-release.** Feature-complete for v1, being battle-tested before the
> first tagged release.

## Why

AI agents need durable, queryable task storage more than they need another project
management app. trackd is that storage: a tracker whose primary users are agents —
over MCP, a CLI, or plain HTTP — with a minimal web UI for humans who want to
glance at the board.

- **Never lose data.** Full-synchronous WAL writes. No hard-delete endpoints —
  issues are archived or canceled, never destroyed, so no agent bug can wipe your
  history. Every mutation records an audit event with before/after state. Built-in
  scheduled backups with integrity checks, plain-text JSONL export, and automatic
  snapshots before schema migrations.
- **Single binary.** No runtime, no containers required, no external database, no
  build step. Pure Go, cross-compiled for macOS, Linux, and Windows.
- **Agent-native.** Give each agent its own API token and every comment and audit
  event is attributed automatically. Strict request validation: unknown JSON
  fields are rejected, so an agent's typo fails loudly instead of silently doing
  nothing.
- **Import from Linear.** One command migrates a Linear CSV export — issues, keys,
  projects, labels, relations, timestamps — with a `--dry-run` mode that validates
  everything first.

## Quickstart

```sh
go build -o trackd .          # or grab a release binary once tagged
./trackd serve --db trackd.db --backup-dir backups
```

The first run prints an admin API token — store it, it is never shown again.
Everything lives under `/api/v1` with bearer auth:

```sh
export TRACKD_URL=http://127.0.0.1:8484 TRACKD_TOKEN=td_...

curl -s $TRACKD_URL/api/v1/issues \
  -H "Authorization: Bearer $TRACKD_TOKEN" \
  -d '{"title": "First issue", "status": "Todo", "labels": ["agent-ready"]}'
```

Or use the CLI, which talks to the same server:

```sh
trackd issue create --title "First issue" --status Todo --label agent-ready
trackd issue list --label agent-ready
trackd issue update TSK-1 --status "In Progress" --actor builder
trackd issue comment TSK-1 --body "done, see the PR" --actor builder
trackd issue show TSK-1
```

Open the same URL in a browser for the read-only board (sign in with a token).

## Concepts

- **Issues** have a stable key (`TSK-1`), a status, a 0–4 priority (1 = urgent,
  4 = low), labels, an optional project, parent, assignee, and milestone,
  relations (`blocks`, `relates`, `duplicate`), comments, and a full activity
  log.
- **Assignees** are free-form names — a person or an agent. Filter by them
  everywhere (`trackd issue list --assignee pm`); routing work between humans
  and agents is the point.
- **Milestones** belong to a project and carry an optional target date. Issues
  reference one by name within their project; moving an issue to another
  project clears a milestone that no longer applies.
- **Statuses** are workflow states with a type: `triage`, `backlog`, `unstarted`,
  `started`, `completed`, `canceled`. The defaults mirror a common agent workflow
  (Triage → Backlog → Todo → In Progress → In Review → Done); importing from
  Linear carries any extra statuses across.
- **Projects** group issues and carry their own labels and status.
- **Tokens are identities.** Mint one per agent (`trackd token add pm`). Writes
  are attributed to the token's name unless the request passes an explicit
  `actor`. Roles: `agent` or `admin` (reserved for future privileged endpoints).

## Interfaces

### REST

`GET/POST /api/v1/issues`, `GET/PATCH /api/v1/issues/{key}`, comments, relations
and per-issue events under the issue path, `GET/POST /api/v1/projects`,
`GET/PATCH /api/v1/projects/{slug}`, `GET/POST /api/v1/milestones`,
`GET/PATCH /api/v1/milestones/{id}`, `GET/POST /api/v1/labels`,
`GET /api/v1/statuses`. Issue list filters: `status`, `status_type`, `project`,
`label`, `parent`, `assignee`, `milestone`, `q`, `updated_since`, `archived`,
`limit`, `offset`.
PATCH bodies change only the fields they include; an explicit empty string
clears a field. There are no DELETE endpoints by design.

`GET /healthz` is unauthenticated and reports database health plus the age of
the last verified backup.

### CLI

`issue list|show|create|update|comment|relate|events`, `project
list|show|create|update`, `milestone list|create|update`, `label list|add`,
`statuses`, `health`. Connection via
`--url`/`--token` or `$TRACKD_URL`/`$TRACKD_TOKEN`. Every command takes `--json`
for machine-readable output; `--description -` and `--body -` read stdin. Server
maintenance commands (`serve`, `token`, `backup`, `restore`, `export`, `import`)
operate on the database file directly.

### MCP

Streamable HTTP at `/mcp`, same bearer auth. Tools: `list_issues`, `get_issue`
(returns the issue with comments and relations), `save_issue` (create or update),
`add_comment`, `list_projects`, `save_project`, `list_milestones`,
`save_milestone`, `list_labels`. For Claude Code, add to `.mcp.json`:

```json
{
  "mcpServers": {
    "trackd": {
      "type": "http",
      "url": "http://your-host:8484/mcp",
      "headers": { "Authorization": "Bearer td_..." }
    }
  }
}
```

### Web UI

Server-rendered, zero JavaScript, embedded in the binary. A board grouped by
status with project/label/assignee filters, and an issue page with description,
comments, relations, and the audit trail. Sign in once with any API token (30-day cookie).
Read-only: agents do the writing.

## Data safety

- `trackd serve --backup-dir <dir> [--backup-every 24h] [--backup-keep 14]
  [--backup-timeout 10m]` snapshots on startup and on the interval. Every
  snapshot is `VACUUM INTO` plus `PRAGMA integrity_check` — a backup that
  doesn't verify is deleted and reported, never silently kept. Backups run on
  their own database connection and are bounded by `--backup-timeout`, so a
  slow or hung backup destination can never block the API; failures show up in
  `/healthz` alongside the last good snapshot.
- `trackd backup` / `trackd restore <snapshot>` for manual operation. Restore
  refuses to overwrite an existing database.
- `trackd export > dump.jsonl` writes the complete database as human-readable
  JSONL; `trackd import trackd dump.jsonl` restores it into a fresh file. The
  round-trip is byte-identical and enforced by tests. This is the no-lock-in
  guarantee.
- Schema migrations snapshot the database automatically before applying.
- The audit log (`events`) records every mutation with actor and before/after
  state.

Treat snapshots and JSONL dumps as sensitive: they contain your full issue
history and the (hashed) token table.

## Import from Linear

```sh
trackd import --db trackd.db --dry-run linear export.csv   # validate, write nothing
trackd import --db trackd.db linear export.csv
```

Imports issues (original keys preserved, key sequence continues), statuses,
text priorities, projects, assignees, milestones, labels, parent/child,
blocked-by / related / duplicate relations, and timestamps. Cycles and
initiatives are ignored. Unknown statuses are created. Dangling references are
skipped and reported, never fatal. Note: Linear's CSV export does not include
comments, so comments cannot be migrated this way.

## Running as a service

trackd binds `:8484` by default. It has no TLS of its own — run it on a private
network (Tailscale works well) or behind a reverse proxy.

systemd (Linux):

```ini
[Service]
ExecStart=/usr/local/bin/trackd serve --db /var/lib/trackd/trackd.db --backup-dir /var/lib/trackd/backups
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

launchd (macOS): a `LaunchAgent` plist with
`ProgramArguments = [trackd, serve, --db, ..., --backup-dir, ...]` and
`KeepAlive = true`. Point `--backup-dir` at a directory that is itself synced or
copied off the machine — but note that macOS privacy protection (TCC) blocks
launchd-spawned processes from privacy-protected folders such as `~/Documents`
unless you grant the binary access in System Settings → Privacy & Security.
trackd detects a blocked backup directory and reports it in `/healthz` instead
of hanging.

## Development

```sh
go test -race ./...   # includes the export/import round-trip and full API tests
gofmt -l . && go vet ./... && golangci-lint run
```

Pure Go, no CGO (`modernc.org/sqlite`), two direct dependencies (the SQLite
driver and the official MCP SDK).

## License

MIT
