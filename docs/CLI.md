# trackd CLI

The client commands talk HTTP to a running server; the server commands operate on the database file directly. `trackd help <command>` prints a group's subcommands and `trackd <command> <sub> --help` the flags.

## Client commands

`issue list|show|create|update|append|comment|relate|events`, `comment edit`,
`events`, `project list|show|create|update`, `milestone list|create|update`,
`label list|add`, `statuses`, `health` (a table of status, version, schema,
backup age and integrity, or the raw report with `--json`). Connection via
`--url`/`--token` or `$TRACKD_URL`/`$TRACKD_TOKEN`. Every command takes
`--json` for machine-readable output, on failure too: the error object goes to
stdout and the human line to stderr. Flags may come before or after the
positional key, so `trackd issue show --json TSK-1` works.

## Empty values

Two rules protect a field from an empty shell variable:

- A bare empty string to a value flag is a usage error. Clearing is explicit:
  `--clear-description`, `--clear-labels`, `--clear-project`, `--clear-parent`,
  `--clear-assignee`, `--clear-milestone`, `--clear-due`.
- `--description -`, `--body -` and `--text -` read stdin, and fail when stdin is
  empty rather than writing nothing over something.
- `--text` and `--body` are interchangeable on the body-bearing commands:
  `issue append`, `issue comment` and `comment edit` all accept either name.
  Passing both at once is a usage error.

## Exit codes and retries

Exit codes: 0 ok, 1 unexpected, 2 usage or validation (400, 422), 3 not found
(404), 4 auth (401, 403), 5 conflict (409), 6 server or network (5xx, or the
server could not be reached). A mistyped flag, like an unknown command, is a
usage error: exit 2, `code: "usage"`. The client gives a busy server and a
refused connection three retries with 250ms, 1s and 3s backoff, and times out a
request after 10 seconds. A create is retried only when it carries an
idempotency key, so a repeat can never make a duplicate.

## Server commands

Server maintenance commands (`serve`, `token`, `setting`, `backup`, `restore`,
`export`, `import`) operate on the database file directly. They take `--db`, or
`$TRACKD_DB`, and fall back to `./trackd.db`.

## Settings

```sh
trackd setting list                         # every setting with its value and meaning
trackd setting get issue_prefix
trackd setting set issue_prefix ACME        # new keys become ACME-1, ACME-2, ...
trackd setting set label_groups '[["ready","blocked"]]'
trackd setting set base_url https://trackd.example.com
```

Every `set` records a `setting.updated` event with the old and new value (`trackd events --entity setting`). `set` writes to the database file, so it refuses while a server is running on it. `list` and `get` open the file read-only and are safe at any time.
