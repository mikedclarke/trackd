-- Saved views: a named issue filter anyone can open by name from the board,
-- the API, the CLI or MCP. The filter and the quick actions are JSON so a new
-- filter field is a code change, not a migration. Archiving frees the name.
CREATE TABLE views (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    filter_json TEXT NOT NULL DEFAULT '{}',
    quick_json  TEXT NOT NULL DEFAULT '[]',
    owner       TEXT NOT NULL DEFAULT '',
    shared      INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    archived_at TEXT
);

CREATE UNIQUE INDEX idx_views_name ON views (name COLLATE NOCASE) WHERE archived_at IS NULL;
