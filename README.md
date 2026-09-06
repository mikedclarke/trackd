# trackd

Self-hosted task and project tracking for AI agents. One binary, one SQLite file,
three interfaces: REST API, CLI, and MCP, plus a read-only web board for humans.

![the trackd board](docs/board.jpeg)

> **Status: early release.** trackd is feature-complete for its purpose and runs
> a real multi-agent workload daily, but it is young: expect rough edges, and
> read the [Data safety](#data-safety) section before trusting it with the only
> copy of anything.

## Why

AI agents need durable, queryable task storage more than they need another project
management app. trackd is that storage: a tracker whose primary users are agents,
over MCP, a CLI, or plain HTTP, with a minimal web UI for humans who want to
glance at the board.

- **Never lose data.** Full-synchronous WAL writes. No hard-delete endpoints:
  issues are archived or canceled, never destroyed, so no agent bug can wipe your
  history. Descriptions are append-only unless an overwrite is asked for in so
  many words. Every mutation records an audit event with before/after state.
  Built-in scheduled backups with integrity checks, plain-text JSONL export, and
  automatic snapshots before schema migrations.
- **Single binary.** No runtime, no containers required, no external database, no
  build step. Pure Go, cross-compiled for macOS, Linux, and Windows.
- **Agent-native.** Give each agent its own API token and every comment and audit
  event is attributed automatically. Strict request validation: unknown JSON
  fields and unknown query parameters are rejected, so an agent's typo fails
  loudly instead of silently doing nothing.
- **Safe to retry.** Creates take an idempotency key, updates take an expected
  version, and every error carries a machine-readable code. An agent that loses
  its connection mid-write can repeat the call without wondering what happened.
- **Import from Linear.** One command migrates a Linear CSV export (issues, keys,
  projects, labels, relations, timestamps) with a `--dry-run` mode that validates
  everything first.

## Install

Prebuilt binaries for macOS, Linux and Windows are on the
[releases page](https://github.com/mikedclarke/trackd/releases): download the
archive for your platform, unpack it, and put `trackd` on your `PATH`.

With Go 1.26 or newer installed:

```sh
go install github.com/mikedclarke/trackd@latest
```

Or from a checkout:

```sh
make build                    # writes bin/trackd, or: go build -o trackd .
make install                  # moves it to ~/.local/bin/trackd
```

## Quickstart

```sh
trackd serve --db trackd.db --backup-dir backups
```

The first run prints an admin API token to stderr. Store it, it is never shown
again. Everything lives under `/api/v1` with bearer auth:

```sh
export TRACKD_URL=http://127.0.0.1:8484 TRACKD_TOKEN=td_...

curl -s $TRACKD_URL/api/v1/issues \
  -H "Authorization: Bearer $TRACKD_TOKEN" \
  -d '{"title": "First issue", "status": "Todo", "labels": ["agent-ready"]}'
```

Or use the CLI, which talks to the same server:

```sh
trackd label add agent-ready
trackd issue create --title "First issue" --status Todo --label agent-ready
trackd issue list --label agent-ready --order-by priority
trackd issue update TSK-1 --status "In Progress" --actor builder
trackd issue append TSK-1 --text "found the cause, see the comment" --actor builder
trackd issue comment TSK-1 --body "done, see the PR" --actor builder
trackd issue show TSK-1
```

Open the same URL in a browser for the read-only board (sign in with a token).

## Concepts

- **Issues** have a stable key (`TSK-1`), a status, a 0-4 priority (1 = urgent,
  4 = low), labels, an optional project, parent, assignee, and milestone,
  relations (`blocks`, `relates`, `duplicate`), comments, an integer `version`,
  and a full activity log.
- **Descriptions are append-only.** Once an issue has a description, an update
  that would overwrite it is refused with a 409. Add to it with
  `trackd issue append KEY --text ...`, or pass `--replace-description` to say
  you meant to overwrite. This is the rule that stops one agent erasing another
  agent's context. Overwriting is also the one write a role decides:
  `--replace-description` and `--clear-description` need an `admin` token, and
  an `agent` token is refused with a 403 (`forbidden`, CLI exit 4).
- **Labels are added and removed, not replaced.** `--add-label` and
  `--remove-label` leave the rest of the set alone; `--labels` still replaces the
  whole set when that is what you want, and `--clear-labels` empties it. Labels
  must exist before they can be applied: `trackd label add <name>` (or the label
  endpoint) is the only place a label is created, so a typo cannot invent one.
  Label groups can be exclusive: configure
  `trackd setting set label_groups '[["ready","blocked"]]'` and an issue holds at
  most one label from each group, so adding one removes the other and a
  replacement set holding both is rejected. No groups are configured by
  default. Label, status, project slug and milestone lookups are
  case-insensitive.
- **Versions make concurrent updates safe.** Every issue carries a `version` that
  increments on each write. Pass `--expected-version N` (`expected_version` in
  the API) and a write that lost the race fails with a 409 instead of quietly
  overwriting. Leave it out and the last writer wins, as before.
- **Idempotency keys make creates safe to repeat.** Pass `--idempotency-key` when
  creating an issue or a comment; a second create with the same key returns the
  first record rather than making a duplicate.
- **Assignees** are free-form names, a person or an agent. Filter by them
  everywhere (`trackd issue list --assignee pm`); routing work between humans
  and agents is the point.
- **Milestones** belong to a project and carry an optional target date. Issues
  reference one by name within their project; moving an issue to another
  project clears a milestone that no longer applies. Milestone names are unique
  among a project's unarchived milestones.
- **Statuses** are workflow states with a type: `triage`, `backlog`, `unstarted`,
  `started`, `completed`, `canceled`. The defaults mirror a common agent workflow
  (Triage, Backlog, Todo, In Progress, In Review, Done); importing from Linear
  carries any extra statuses across. Entering a completed or canceled status
  stamps `completed_at` or `canceled_at`, and leaving it clears the stamp;
  `started_at` is sticky once set.
- **Projects** group issues and carry their own labels, a status from `backlog`,
  `planned`, `started`, `paused`, `completed` and `canceled`, and optional
  `start_date`, `target_date` and `completed_at` dates.
- **Comments** can reply to another comment (`--parent ID`) and can be edited
  (`trackd comment edit ID --body ...`). An edit keeps the original in the audit
  trail.
- **Timestamps** are UTC RFC3339 with milliseconds (`2026-01-02T15:04:05.000Z`).
  Every timestamp parameter accepts RFC3339 with any offset and is converted;
  dates (due, start, target) are plain `YYYY-MM-DD`.
- **Tokens are identities.** Mint one per agent (`trackd token add pm`). Writes
  are attributed to the token's name unless the request passes an explicit
  `actor`. Roles: `agent` (the default) or `admin`. Every write is open to both
  except one: replacing or clearing a description, which needs `admin`. Give the
  agents `agent` tokens and keep an `admin` token for yourself.
- **Settings** live in the database and are read and written with
  `trackd setting list|get|set`. There are three: `issue_prefix` (the key
  prefix for new issues, `TSK` by default; existing keys keep theirs),
  `label_groups` (the exclusive groups above) and `base_url` (the server's
  public URL, used to fill each issue's `url` field for links in agent output).
  Every change is audited. `set` writes to the database file directly, so it
  refuses while a server is running: stop the server, set, start it again.

## Interfaces

### REST

Everything is under `/api/v1` with bearer auth. Issues, comments, relations, projects, milestones, labels, statuses and a global activity feed; list responses are envelopes with paging cursors; PATCH changes only the fields it includes; every error is `{"error", "code"}`; `GET /healthz` is unauthenticated. There are no DELETE endpoints by design. The full reference, filters and error codes included, is in [docs/API.md](docs/API.md).

### CLI

`trackd issue|comment|events|project|milestone|label|statuses|health` talk to a running server via `--url`/`--token` or `$TRACKD_URL`/`$TRACKD_TOKEN`; `trackd serve|token|setting|backup|restore|export|import` operate on the database file. Every client command takes `--json`, exit codes are stable (0 ok, 2 usage, 3 not found, 4 auth, 5 conflict, 6 server or network), and a bare empty string is never accepted as a value, so a shell variable that did not expand cannot blank a field. Details in [docs/CLI.md](docs/CLI.md).

### MCP

Streamable HTTP at `/mcp`, same bearer auth, twelve tools (`list_issues`, `get_issue`, `save_issue`, `add_comment`, `list_projects`, `save_project`, `list_milestones`, `save_milestone`, `list_labels`, `list_statuses`, `save_relation`, `list_activity`). Read tools are annotated read-only and nothing in the set is destructive. For Claude Code, add to `.mcp.json`:

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
comments, relations, and the audit trail. Sign in once with any API token.
Read-only: agents do the writing.

![an issue page](docs/issue.jpeg)

## Data safety

- **One writer.** `serve` takes an exclusive advisory lock on the database file.
  A second server, or a `token add`, `token revoke`, `setting set`, `import` or
  `restore` aimed at a database a server is already serving, refuses with
  `another trackd is running on <path>`. `export`, `backup`, `token list` and
  `setting list|get` open the file read-only, so they are always safe to run
  against a live database.
- **A damaged or too-new file is refused.** `serve` runs `PRAGMA quick_check`
  before opening a database read-write and refuses a file that fails it, and an
  older binary refuses a database written by a newer one rather than migrating it
  backwards.
- `trackd serve --backup-dir <dir> [--backup-every 24h] [--backup-keep 14]
  [--backup-timeout 10m]` snapshots on startup and on the interval. Every
  snapshot is `VACUUM INTO` plus `PRAGMA integrity_check`: a backup that does not
  verify is deleted and reported, never silently kept. Backups run on their own
  database connection and are bounded by `--backup-timeout`, so a slow or hung
  backup destination can never block the API; failures show up in `/healthz`
  alongside the last good snapshot. The startup snapshot is skipped when a recent
  one already exists.
- `trackd backup` / `trackd restore <snapshot>` for manual operation. Restore
  refuses to overwrite an existing database, and refuses a destination that still
  has `-wal` or `-shm` sidecars beside it, because those hold writes the snapshot
  does not; move them aside deliberately. The restored file is verified before the
  command reports success.
- `trackd export > dump.jsonl` writes the complete database as human-readable
  JSONL; `trackd import trackd dump.jsonl` loads it into a fresh file. The
  round-trip is byte-identical and enforced by tests. This is the no-lock-in
  guarantee.
- Schema migrations snapshot the database automatically before applying, into the
  backup directory when the server has one.
- The audit log (`events`) records every mutation with actor and before/after
  state, and `trackd events` reads it as one ordered feed with a cursor.

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

trackd binds `:8484` by default. It has no TLS of its own, so run it on a private
network (Tailscale works well) or behind a reverse proxy.

systemd (Linux):

```ini
[Service]
ExecStart=/usr/local/bin/trackd serve --db /var/lib/trackd/trackd.db --backup-dir /var/lib/trackd/backups
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

launchd (macOS), as `~/Library/LaunchAgents/com.example.trackd.plist`, then
`launchctl load` it:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.example.trackd</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/trackd</string>
    <string>serve</string>
    <string>--db</string><string>/Users/you/trackd/trackd.db</string>
    <string>--backup-dir</string><string>/Users/you/trackd/backups</string>
  </array>
  <key>KeepAlive</key><true/>
  <key>RunAtLoad</key><true/>
  <key>StandardOutPath</key><string>/Users/you/trackd/trackd.log</string>
  <key>StandardErrorPath</key><string>/Users/you/trackd/trackd.log</string>
</dict>
</plist>
```

Point `--backup-dir` at a directory that is itself synced or copied off the
machine, but note that macOS privacy protection (TCC) blocks launchd-spawned
processes from privacy-protected folders such as `~/Documents` unless you grant
the binary access in System Settings, Privacy & Security. trackd detects a
blocked backup directory and reports it in `/healthz` instead of hanging.

## Development

```sh
make build          # stamps the version from git describe, refuses a dirty tree
make test           # go test -race ./...
make lint           # gofmt, go vet, and golangci-lint when it is installed
make install        # builds, then moves the binary to ~/.local/bin/trackd
```

`make build` will not build from a tree with uncommitted changes unless you pass
`ALLOW_DIRTY=1`, so a binary's `trackd version` string always names a commit you
can go back to. `make install` moves a freshly built binary into place rather
than copying over the old one, which would corrupt a trackd already running from
that path.

Pure Go, no CGO (`modernc.org/sqlite`), two direct dependencies (the SQLite
driver and the official MCP SDK). Needs Go 1.26 or newer to build.

Releases are built locally with [goreleaser](https://goreleaser.com)
(`goreleaser release --clean` on a tag) or with the plain cross-compile loop in
`Makefile` (`make dist`), then attached to a GitHub release by hand. There is
no CI.

## Contributing

Bug reports and questions are welcome as issues. For anything beyond a small fix, open an issue first so the approach can be agreed before code is written; see [CONTRIBUTING.md](CONTRIBUTING.md). Security reports: see [SECURITY.md](SECURITY.md).

## License

MIT
