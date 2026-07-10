# trackd

Self-hosted task and project tracking for AI agents. One binary, one SQLite file,
three interfaces: REST API, CLI, and MCP.

> **Status: pre-release.** Under active development — not yet ready for use.

## Why

AI agents need durable, queryable task storage more than they need another project
management app. trackd is that storage: a tracker whose primary users are agents
(over MCP, a CLI, or plain HTTP), with a minimal read-only web UI for humans who
want to glance at the board.

Design principles:

- **Never lose data.** Full-synchronous WAL writes, no hard-delete endpoints, an
  audit trail with before/after values for every mutation, built-in scheduled
  backups with integrity checks, and plain-text JSONL export/import so your data
  is never locked in.
- **Single binary.** No runtime, no build step, no external database. Pure Go,
  cross-compiled for macOS, Linux, and Windows.
- **Agent-native.** Issues, projects, labels, comments, and relations exposed over
  REST, CLI, and MCP. Optional actor attribution lets multi-agent setups record
  who did what.
- **Import from Linear.** One command to migrate a Linear CSV export.

## Quickstart

Pre-release: build from source with `go build`, then:

    trackd serve --db trackd.db --backup-dir backups

The first run prints an admin API token (shown exactly once). Everything speaks
JSON over `/api/v1` with bearer auth:

    curl -s localhost:8484/api/v1/issues \
      -H "Authorization: Bearer $TRACKD_TOKEN" \
      -d '{"title": "First issue", "status": "Todo", "labels": ["agent-ready"]}'

Add a token per agent with `trackd token add <name>` and each one's writes are
attributed automatically; pass an explicit `"actor"` field to override.
`/healthz` reports database health and the age of the last verified backup.

## License

MIT
