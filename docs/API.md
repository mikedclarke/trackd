# trackd REST API

Everything lives under `/api/v1` with bearer auth (`Authorization: Bearer td_...`). Mint tokens with `trackd token add <name>`.

## Endpoints

Issues: `GET/POST /api/v1/issues`, `GET/PATCH /api/v1/issues/{key}`,
`POST /api/v1/issues/{key}/description` (append), comments, relations and
per-issue events under the issue path, `PATCH /api/v1/comments/{id}`.
Everything else: `GET /api/v1/events` (the global feed),
`GET/POST /api/v1/projects`, `GET/PATCH /api/v1/projects/{slug}`,
`GET/POST /api/v1/milestones`, `GET/PATCH /api/v1/milestones/{id}`,
per-entity event feeds under projects and milestones,
`GET/POST /api/v1/labels`, `GET /api/v1/statuses`.

## Issue list filters

Issue list filters: `status`, `status_type`, `label` and `exclude_label` (all
repeatable), `project`, `parent`, `assignee`, `milestone`, `q`, `updated_since`,
`completed_since`, `archived` (`true` includes archived issues, `only` restricts
to them), `order_by` (`updated`, `created`, `priority`), `limit` (default 100,
max 500), `offset`. An unknown parameter is a 400. A filter value that names
nothing is a 422 `invalid_ref` rather than an empty page, so a typo in a label
or a project slug cannot read as "no work": `status`, `status_type`, `project`,
`label`, `exclude_label`, `milestone` and `parent` all resolve before the query.
Assignees are free-form names, so an unknown one is simply an empty result.
`GET /api/v1/events` takes `entity` of `issue`, `project`, `milestone`,
`token` or `setting` (a comment or a relation is recorded against its issue);
any other value is a 404.

## Responses

Every list response is an envelope, never a bare array:
`{"issues": [...], "next_offset": N|null}`, and `{"comments": [...]}`,
`{"relations": [...]}`, `{"projects": [...]}` and so on for the rest.
`GET /api/v1/events` pages with `{"events": [...], "next_after_id": N|null}`.

## Writes

PATCH bodies change only the fields they include. `labels` replaces the set;
`add_labels` and `remove_labels` amend it; sending both forms in one request is a
400. `replace_description: true` permits an overwrite and requires an `admin`
token (clearing a description is that field with `"description": ""` beside it,
so it is the same rule); `expected_version` guards against a lost update. There
are no DELETE endpoints by design.

## Errors

Errors are always `{"error": "human message", "code": "..."}` where the code is
one of `validation` (400), `unauthorized` (401), `forbidden` (403, a write this
token's role may not make: replacing a description), `not_found` (404),
`conflict`, `version_conflict` or `description_replace` (409), `invalid_ref`
(422, a referenced or filtered status, project, milestone, parent or label does
not resolve), `busy` (503, with `Retry-After: 1`) and `internal` (500).
Unmatched routes and methods use the same shape.

## Health

`GET /healthz` is unauthenticated and reports
`{"status": "ok"|"degraded", "version", "schema", "backup": {...},
"integrity": {...}}`. Status is `degraded`, and the HTTP status 503, when the
integrity check failed, the last backup errored or is older than twice the
configured interval, or the scheduler is off. The body carries no filesystem
paths.

`backup` reports the server's own scheduled snapshots in `--backup-dir`, and
nothing else. A copy taken by another tool, on a schedule of its own or into
another directory, never reaches this counter, so a stale `last_at` here means
"the scheduler has not run", not "there is no recent backup". If an external job
is your real backup, point it at `--backup-dir` or monitor it separately, and
read this number as what it is: the health of the scheduler.

## MCP

Streamable HTTP at `/mcp`, same bearer auth. Twelve tools: `list_issues`,
`get_issue` (returns the issue with comments and relations), `save_issue`,
`add_comment`, `list_projects`, `save_project`, `list_milestones`,
`save_milestone`, `list_labels`, `list_statuses`, `save_relation`,
`list_activity`. `save_issue` takes a required `mode` of `create` or `update`, so
a missing key can never turn an update into a new issue, and carries the same
`append_description`, `add_labels`, `remove_labels`, `replace_description`
(admin tokens only, as over REST), `expected_version` and `idempotency_key`
fields as the API. The read tools are
annotated read-only, and nothing in the tool set is destructive.

For Claude Code, add to `.mcp.json`:

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
