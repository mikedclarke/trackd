CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE statuses (
    id       INTEGER PRIMARY KEY,
    name     TEXT NOT NULL UNIQUE,
    type     TEXT NOT NULL CHECK (type IN ('triage', 'backlog', 'unstarted', 'started', 'completed', 'canceled')),
    position INTEGER NOT NULL
);

CREATE TABLE projects (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused', 'completed', 'canceled')),
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    archived_at TEXT
);

CREATE TABLE issues (
    id           INTEGER PRIMARY KEY,
    key          TEXT NOT NULL UNIQUE,
    title        TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    status_id    INTEGER NOT NULL REFERENCES statuses (id),
    priority     INTEGER NOT NULL DEFAULT 0 CHECK (priority BETWEEN 0 AND 4),
    project_id   INTEGER REFERENCES projects (id),
    parent_id    INTEGER REFERENCES issues (id),
    due_date     TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    started_at   TEXT,
    completed_at TEXT,
    canceled_at  TEXT,
    archived_at  TEXT
);

CREATE INDEX idx_issues_status ON issues (status_id);
CREATE INDEX idx_issues_project ON issues (project_id);
CREATE INDEX idx_issues_updated ON issues (updated_at);

CREATE TABLE labels (
    id    INTEGER PRIMARY KEY,
    name  TEXT NOT NULL UNIQUE,
    color TEXT NOT NULL DEFAULT ''
);

CREATE TABLE issue_labels (
    issue_id INTEGER NOT NULL REFERENCES issues (id),
    label_id INTEGER NOT NULL REFERENCES labels (id),
    PRIMARY KEY (issue_id, label_id)
);

CREATE TABLE project_labels (
    project_id INTEGER NOT NULL REFERENCES projects (id),
    label_id   INTEGER NOT NULL REFERENCES labels (id),
    PRIMARY KEY (project_id, label_id)
);

CREATE TABLE comments (
    id         INTEGER PRIMARY KEY,
    issue_id   INTEGER NOT NULL REFERENCES issues (id),
    body       TEXT NOT NULL,
    actor      TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX idx_comments_issue ON comments (issue_id);

CREATE TABLE issue_relations (
    issue_id   INTEGER NOT NULL REFERENCES issues (id),
    related_id INTEGER NOT NULL REFERENCES issues (id),
    type       TEXT NOT NULL CHECK (type IN ('blocks', 'relates', 'duplicate')),
    PRIMARY KEY (issue_id, related_id, type)
);

CREATE TABLE tokens (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    hash         TEXT NOT NULL,
    role         TEXT NOT NULL CHECK (role IN ('admin', 'agent')),
    created_at   TEXT NOT NULL,
    last_used_at TEXT,
    revoked_at   TEXT
);

CREATE TABLE events (
    id          INTEGER PRIMARY KEY,
    entity      TEXT NOT NULL,
    entity_id   INTEGER NOT NULL,
    actor       TEXT NOT NULL DEFAULT '',
    action      TEXT NOT NULL,
    before_json TEXT,
    after_json  TEXT,
    created_at  TEXT NOT NULL
);

CREATE INDEX idx_events_entity ON events (entity, entity_id);

INSERT INTO settings (key, value) VALUES
    ('workspace_name', 'trackd'),
    ('issue_prefix', 'TSK'),
    ('issue_seq', '0');

INSERT INTO statuses (name, type, position) VALUES
    ('Triage', 'triage', 1),
    ('Backlog', 'backlog', 2),
    ('Todo', 'unstarted', 3),
    ('In Progress', 'started', 4),
    ('In Review', 'started', 5),
    ('Done', 'completed', 6),
    ('Canceled', 'canceled', 7),
    ('Duplicate', 'canceled', 8);
