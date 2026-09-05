ALTER TABLE issues ADD COLUMN version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE issues ADD COLUMN idempotency_key TEXT;
CREATE UNIQUE INDEX idx_issues_idem ON issues (idempotency_key) WHERE idempotency_key IS NOT NULL;

ALTER TABLE comments ADD COLUMN parent_id INTEGER REFERENCES comments (id);
ALTER TABLE comments ADD COLUMN idempotency_key TEXT;
CREATE UNIQUE INDEX idx_comments_idem ON comments (idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX idx_comments_parent ON comments (issue_id, parent_id);

-- SQLite cannot alter a CHECK constraint, so projects, milestones and labels
-- are rebuilt: create, copy, drop, rename, recreate indexes. The runner holds
-- foreign_keys OFF around this file and runs foreign_key_check before commit.
CREATE TABLE projects_new (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL,
    slug         TEXT NOT NULL UNIQUE,
    description  TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'backlog' CHECK (status IN ('backlog', 'planned', 'started', 'paused', 'completed', 'canceled')),
    start_date   TEXT,
    target_date  TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    completed_at TEXT,
    archived_at  TEXT
);

INSERT INTO projects_new (id, name, slug, description, status, created_at, updated_at, completed_at, archived_at)
SELECT id, name, slug, description,
       CASE status WHEN 'active' THEN 'started' ELSE status END,
       created_at, updated_at,
       CASE status WHEN 'completed' THEN updated_at END,
       archived_at
FROM projects;

DROP TABLE projects;
ALTER TABLE projects_new RENAME TO projects;

CREATE TABLE milestones_new (
    id          INTEGER PRIMARY KEY,
    project_id  INTEGER NOT NULL REFERENCES projects (id),
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    target_date TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    archived_at TEXT
);

INSERT INTO milestones_new (id, project_id, name, description, target_date, created_at, updated_at, archived_at)
SELECT id, project_id, name, description, target_date, created_at, updated_at, archived_at FROM milestones;

DROP TABLE milestones;
ALTER TABLE milestones_new RENAME TO milestones;

CREATE UNIQUE INDEX idx_milestones_name ON milestones (project_id, name) WHERE archived_at IS NULL;

CREATE TABLE labels_new (
    id    INTEGER PRIMARY KEY,
    name  TEXT NOT NULL UNIQUE COLLATE NOCASE,
    color TEXT NOT NULL DEFAULT ''
);

INSERT INTO labels_new (id, name, color) SELECT id, name, color FROM labels;

DROP TABLE labels;
ALTER TABLE labels_new RENAME TO labels;

CREATE INDEX idx_events_created ON events (created_at);

INSERT OR IGNORE INTO settings (key, value) VALUES
    ('label_groups', '[["claude-ready","needs-mike"]]'),
    ('base_url', '');
