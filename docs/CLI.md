# trackd CLI

The client commands talk HTTP to a running server; the server commands operate on the database file directly. `trackd help <command>` prints a group's subcommands and `trackd <command> <sub> --help` the flags.

## Client commands

`issue list|show|get|view|create|update|append|comment|relate|events`,
`comment list|edit`, `events`, `project list|show|create|update`,
`milestone list|create|update`, `label list|add`,
`view list|show|create|update|delete|restore`, `statuses`, `health` (a table
of status, version, schema, backup age and integrity, or the raw report with
`--json`). Connection via `--url`/`--token` or `$TRACKD_URL`/`$TRACKD_TOKEN`, or
a token file named by `$TRACKD_TOKEN_FILE` (tried after `$TRACKD_TOKEN`).
Every command takes `--json` for machine-readable output, on failure too: the
error object goes to stdout and the human line to stderr. Every list command's
`--json` is an envelope keyed by the type (`{"projects": [...]}`,
`{"issues": [...]}`, ...), the same shape the REST API returns. The JSON is
strict: a tab or newline inside a value is written as `\t` or `\n`. Pipe it
straight into the parser, or print a captured copy with `printf '%s\n'`;
zsh's builtin `echo` turns those escapes back into raw control characters,
which a strict parser then rejects. Flags may come
before or after the positional key, so `trackd issue show --json TSK-1` works.
`trackd <command> --help` (and `-h`) prints the flags and exits 0.

`trackd comment <key> --body <text>` adds a comment, an alias for
`trackd issue comment`, and `trackd comment list <key>` lists an issue's
comments with their ids.

`get` and `view` are aliases for `issue show`, and `statuses list` is accepted
as a synonym for `statuses`. `issue list` takes `--search` as an alias for `-q`,
and `--columns <cols>` or `--tsv` for a flat listing of only the named columns
(any of `key,status,priority,project,assignee,labels,title`) instead of the full
table or JSON. `--priority` on `issue create` and `issue update` accepts a word
(`none|urgent|high|medium|low`) as well as `0-4`, and `--milestone` accepts a
milestone id as well as a name.

`issue list` shows one page (`--limit`, default 100, max 500) and says
`more results: --offset N (or --all)` when there is another; `--tsv` says it on
stderr so the records stay clean, and `--json` carries it as `next_offset`.
`--all` follows the pages to the end and prints every result. Without
`--order-by` it sorts oldest created first (not the usual most recently
updated), so an issue updated while the pages are read is neither skipped nor
repeated.

`issue update` takes several keys (`trackd issue update TSK-1 TSK-2 TSK-3
--status Done --add-label shipped`) and sends them as one batch request. Each
issue is updated on its own, with its own audit event; a refused one does not
stop the others. It prints a line per key (`--json`: `{"results": [...]}`) and
exits with the first refusal's code. An item refused as busy can be re-run on
its own; the others are already applied. `--append` and `--expected-version`
take one key. Every bare word must be shaped like an issue key, so an unquoted
flag value (`--title Fix the bug`) is a usage error and nothing is sent.

`events --key <key>` (repeatable, up to 100) reads those issues' events in
one request, oldest first; page on with `--after-id`.

## Views

A view is a saved filter with a name. Create one with any of the filter flags
`issue list` takes (`--status`, `--type`, `--project`, `--label`,
`--exclude-label`, `--assignee`, `--milestone`, `--priority`, `--created-by`,
`-q`, `--order-by`, all as on `issue list`) plus `--updated-within <window>`
(`7d`, `48h`, `2w`: a relative window, resolved each time the view is
applied), then open it by name:

```sh
trackd view create "Waiting on me" --label waiting --status Todo --status "In Progress" \
  --order-by created --description "parked on a person" \
  --quick "Answered: remove=waiting" --quick "Ship: status=Done, add=released"
trackd issue list --view "Waiting on me"                  # the view's rows
trackd issue list --view "Waiting on me" --status Done    # a flag given here overrides the view's
trackd issue list --view "Waiting on me" --label urgent   # except labels, which narrow it
trackd view list                                          # every view this token may open
trackd view show "Waiting on me"
trackd view update "Waiting on me" --exclude-label ready --clear-order-by --rename Court
trackd view delete Court                                  # archives it and frees the name
trackd view restore Court
```

At least one filter flag is required. A view is shared with every token unless
created with `--private` (`view update --shared|--private` changes it later),
and only its owner or an admin token may update or delete it. `--quick` adds a
quick action, a one-tap button on every row of the view in the web board:
`"Name: key=value, ..."` with keys `status`, `priority`, `add` and `remove`
(the label keys repeat), up to four per view. On `view update`, the filter is
read, amended by the flags given and written back whole, so one flag changes
one field; a list flag such as `--status` replaces that list, and each field
has a `--clear-*` flag (`--clear-statuses`, `--clear-types`, `--clear-labels`,
`--clear-exclude-labels`, `--clear-priorities`, `--clear-project`,
`--clear-assignee`, `--clear-milestone`, `--clear-updated-within`,
`--clear-created-by`, `--clear-q`, `--clear-order-by`). `--quick` given once or
more replaces the whole quick-action set; `--clear-quick` empties it.

`issue list` also gained `--priority` (repeatable, `0-4` or a word, matches
any) and `--created-by <actor>`, both usable with or without `--view`.

## Labels and titles

`trackd label add <name> [--color <hex>]` creates a label or recolors one that
already exists. The color must be a hex color, `#rgb` or `#rrggbb` in either
case, and leaving it out keeps whatever color the label has. An issue title
and a project or milestone name are trimmed before they are stored and must be
between 1 and 500 characters. Either rule broken is exit 2.

## Relations

`trackd issue relate <key> <related-key> --type <blocks|relates|duplicate>`
links two issues, and `--remove` unlinks them. Removal is idempotent: it exits 0
whether or not there was a relation there, and says which, so a retry after a
dropped connection is safe. Both issue keys and the type still have to be real
ones, so a typo is still an error.

## Empty values

Two rules protect a field from an empty shell variable:

- A bare empty string to a value flag is a usage error. Clearing is explicit:
  `--clear-description`, `--clear-labels`, `--clear-project`, `--clear-parent`,
  `--clear-assignee`, `--clear-milestone`, `--clear-due`.
- `--description -`, `--body -` and `--text -` read stdin, and fail when stdin is
  empty rather than writing nothing over something. Stdin is kept byte for
  byte, trailing newline included.
- `--text` and `--body` are interchangeable on the body-bearing commands:
  `issue append`, `issue comment` and `comment edit` all accept either name.
  Passing both at once is a usage error.

## Exit codes and retries

Exit codes: 0 ok, 1 unexpected, 2 usage or validation (400, 422), 3 not found
(404), 4 auth (401, 403), 5 conflict (409), 6 server or network (5xx, or the
server could not be reached). The server's error codes and the exit code each
one maps to are tabled in [API.md](API.md). The CLI adds four codes of its own,
for failures that happen instead of a server's answer rather than in it:

| Code | Exit | When |
|---|---|---|
| `usage` | 2 | A mistyped flag, an unknown command or subcommand, a missing or extra positional, an empty string where a value was wanted |
| `unauthorized` | 4 | No token is set at all, caught locally before any request rather than after a wasted round trip |
| `unreachable` | 6 | The server could not be reached, after the retries |
| `degraded` | 6 | `trackd health` against a server reporting degraded health. The report is printed first; the exit code is what a monitor reads |

The client gives a busy server and a refused connection three retries with
250ms, 1s and 3s backoff, and times out a request after 10 seconds. A create is
retried only when it carries an idempotency key, so a repeat can never make a
duplicate.

## Server commands

Server maintenance commands (`serve`, `token`, `setting`, `backup`, `restore`,
`export`, `import`) operate on the database file directly. They take `--db`, or
`$TRACKD_DB`, and fall back to `./trackd.db`.

## Service (macOS)

`trackd service <status|start|stop|restart|install|uninstall>` manages the
launchd job that runs `serve`, in the logged-in user's domain (`gui/<uid>`).
The job label comes from `--label`, then `$TRACKD_SERVICE_LABEL`; with neither
the command exits 2. On any other platform it exits 1 and points at systemd.

| Subcommand | Does | Flags |
|---|---|---|
| `status` | one line: launchd state, pid and runs, then a `/healthz` probe (no token, one attempt). Exits 0 when the job is running and the server answers ok, 6 otherwise | `--url` (default `$TRACKD_URL`, then `http://127.0.0.1:8484`), `--json` |
| `start` | loads the plist when the job is not loaded, kicks it when it is loaded but idle, does nothing when it is running | |
| `stop` | unloads the job (`launchctl bootout`); the plist stays | |
| `restart` | `launchctl kickstart -k`; a job that is not loaded is started instead | |
| `install` | writes `~/Library/LaunchAgents/<label>.plist` and loads it; refuses when that file exists | `--db`, `--addr`, `--backup-dir`, `--log`, `--bin`, `--dry-run` |
| `uninstall` | unloads the job and removes the plist; the database and log are untouched | |

## Settings

```sh
trackd setting list                         # every setting with its value and meaning
trackd setting get issue_prefix
trackd setting set issue_prefix ACME        # new keys become ACME-1, ACME-2, ...
trackd setting set label_groups '[["ready","blocked"]]'
trackd setting set base_url https://trackd.example.com
trackd setting set agent_label source:agent  # auto-tag issues created by non-admin tokens; empty disables
trackd setting set workspace_name "Acme Tasks"  # name shown in the web board header; empty shows just the wordmark
trackd setting set actor_from_token true    # only admin tokens may pass --actor for someone else; false (the default) allows any
```

With `actor_from_token` on, a client command run with an `agent` token and an `--actor` other than the token's own name fails with exit 4 and changes nothing; drop `--actor` (or pass the token's own name) and the write is attributed to the token.

Every `set` records a `setting.updated` event with the old and new value (`trackd events --entity setting`). `set` writes to the database file, so it refuses while a server is running on it. `list` and `get` open the file read-only and are safe at any time.
