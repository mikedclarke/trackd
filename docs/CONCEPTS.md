# Concepts

The data model and the rules trackd enforces on it. The README has the short version; this is the whole of it.

- **Issues** have a stable key (`TSK-1`), a status, a 0-4 priority (1 = urgent,
  4 = low), labels, an optional project, parent, assignee, and milestone,
  relations (`blocks`, `relates`, `duplicate`), comments, an integer `version`,
  and a full activity log. Titles are trimmed and capped at 500 characters.
  Removing a relation is idempotent: `trackd issue relate A B --remove` exits 0
  whether or not the relation was there, and says which, so a retry is safe.
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
  endpoint) is the only place a label is created, so a typo cannot invent one. A
  label's optional `--color` is a hex color, `#rgb` or `#rrggbb`.
  Label groups can be exclusive: configure
  `trackd setting set label_groups '[["ready","blocked"]]'` and an issue holds at
  most one label from each group, so adding one removes the other and a
  replacement set holding both is rejected. No groups are configured by
  default. Label, status, project slug, milestone and issue key lookups are
  case-insensitive.
- **Versions make concurrent updates safe.** Every issue carries a `version` that
  increments on each write. Pass `--expected-version N` (`expected_version` in
  the API) and a write that lost the race fails with a 409 instead of quietly
  overwriting. Leave it out and the last writer wins, as before. A patch that
  changes nothing (a label the issue already has, its current priority) is
  not a write: the version stays and no event is recorded.
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
- **Views** are saved issue filters with a name, so a queue can be opened by
  name from any interface: `trackd view create "Waiting on me" --label
  waiting --status Todo --status "In Progress" --order-by created`, then
  `trackd issue list --view "Waiting on me"`, `list_issues` with `view` over
  MCP, or the tab of the same name on the web board. A view filters on any of
  status, status type, project, labels, excluded labels, assignee, milestone,
  priority, creator, a relative window (`--updated-within 7d`), text and sort
  order. A view is shared with every token unless made `--private`; only its
  owner or an admin can change it. Up to four **quick actions** (`--quick
  "Answered: remove=waiting"`) become one-tap buttons on every row of the view
  in the web board. Deleting a view archives it, which frees its name.
- **Timestamps** are UTC RFC3339 with milliseconds (`2026-01-02T15:04:05.000Z`).
  Every timestamp parameter accepts RFC3339 with any offset and is converted;
  dates (due, start, target) are plain `YYYY-MM-DD`.
- **Tokens are identities.** Mint one per agent (`trackd token add pm`). Writes
  are attributed to the token's name unless the request passes an explicit
  `actor`. Roles: `agent` (the default) or `admin`. Every write is open to both
  except one: replacing or clearing a description, which needs `admin`. Give the
  agents `agent` tokens and keep an `admin` token for yourself.
- **Settings** live in the database and are read and written with
  `trackd setting list|get|set`. Five are writable: `issue_prefix` (the key
  prefix for new issues, `TSK` by default; existing keys keep theirs),
  `label_groups` (the exclusive groups above), `base_url` (the server's
  public URL, used to fill each issue's `url` field for links in agent output),
  `agent_label` (a label auto-applied to issues created by non-admin tokens;
  empty by default, which disables it) and `workspace_name` (the name shown in
  the web board header; empty shows just the wordmark). A sixth, `issue_seq`
  (the last issue number handed out), is read-only.
  Every change is audited. `set` writes to the database file directly, so it
  refuses while a server is running: stop the server, set, start it again.

