-- Web-board logins move out of server memory so a restart no longer signs
-- every browser out. The id column holds the SHA-256 of the cookie value,
-- mirroring how API tokens are stored: a copy of the database is not a bag
-- of usable sessions.
CREATE TABLE ui_sessions (
    id         TEXT PRIMARY KEY,
    token_id   INTEGER NOT NULL REFERENCES tokens (id),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

CREATE INDEX idx_ui_sessions_expires ON ui_sessions (expires_at);
