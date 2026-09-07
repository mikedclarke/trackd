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

`POST /api/v1/labels` creates a label or recolors an existing one. `color` is
optional and must be a hex color, `#rgb` or `#rrggbb` in either case; anything
else is a 422 `invalid_ref`, and an empty `color` leaves the label's own color
alone.

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

`POST /api/v1/issues/{key}/relations` with `"remove": true` is idempotent: it
answers 200 with `{"relations": [...], "removed": true}` when there was a
relation to remove, and `"removed": false` when there was not, so a retry is
safe. The `removed` field is on a removal only, never on an add or a listing.
Idempotent is not lax: both issue keys and the relation type still have to be
real, so a typo is a 404 or a 400 rather than a quiet `"removed": false`.

## Errors

Errors are always `{"error": "human message", "code": "..."}`. The code set is
closed, and every code the server can emit is here with the status it comes
with and the exit code the CLI turns it into.

| Code | HTTP | CLI exit | When |
|---|---|---|---|
| `validation` | 400 | 2 | The request itself is malformed: an unknown JSON field or query parameter, a body that is not JSON, a value out of range, a patch with nothing in it, an unknown enum value such as a relation type or a project status |
| `unauthorized` | 401 | 4 | No bearer token, or one that is unknown or revoked |
| `forbidden` | 403 | 4 | A real write this token's role may not make: replacing or clearing a description needs an `admin` token |
| `not_found` | 404 | 3 | The issue, project, milestone or comment does not exist |
| `conflict` | 409 | 5 | A uniqueness or exclusivity rule: a project name or slug already taken, a milestone name already used in its project, two labels from one exclusive group |
| `version_conflict` | 409 | 5 | `expected_version` did not match, so another writer got there first |
| `description_replace` | 409 | 5 | The update would overwrite a non-empty description and did not carry `replace_description` |
| `invalid_ref` | 422 | 2 | A value that has to resolve or has a fixed shape does not: a status, project, milestone, parent or label that names nothing; a title or name that is empty or over 500 characters; a label `color` that is not `#rgb` or `#rrggbb` |
| `busy` | 503 | 6 | SQLite was busy. The response carries `Retry-After: 1` and the CLI retries on its own |
| `internal` | 500 | 6 | A fault in trackd or the machine under it. The detail goes to the server log, never into the body |

An unmatched route answers 404 and a method a path does not accept answers 405,
both in this same shape with `not_found`.

Titles and names (`title` on an issue, `name` on a project or milestone) are
trimmed before they are stored, and are rejected as `invalid_ref` when the
trimmed value is empty or longer than 500 characters. The limit is not
configurable.

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
`list_activity`. `save_relation` with `remove` is idempotent in the same way as
the REST endpoint and reports `removed` alongside the relations.
`save_issue` takes a required `mode` of `create` or `update`, so
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
