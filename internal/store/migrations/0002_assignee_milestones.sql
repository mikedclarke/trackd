ALTER TABLE issues ADD COLUMN assignee TEXT NOT NULL DEFAULT '';

CREATE TABLE milestones (
    id          INTEGER PRIMARY KEY,
    project_id  INTEGER NOT NULL REFERENCES projects (id),
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    target_date TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    archived_at TEXT,
    UNIQUE (project_id, name)
);

ALTER TABLE issues ADD COLUMN milestone_id INTEGER REFERENCES milestones (id);

CREATE INDEX idx_issues_assignee ON issues (assignee);
CREATE INDEX idx_issues_milestone ON issues (milestone_id);
