package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// UISession is one web-board login: an opaque cookie value mapped to the token
// that signed in. Sessions are durable so a server restart does not sign every
// browser out; they still die on logout, on expiry, and with their token's
// revocation.
type UISession struct {
	TokenName string
	TokenRole string
	ExpiresAt time.Time
}

// CreateUISession records a new web session. Only the SHA-256 of the id is
// stored, so a copy of the database is not a bag of usable sessions.
func (s *Store) CreateUISession(id string, tokenID int64, ttl time.Duration) error {
	if id == "" {
		return errors.New("session id is required")
	}
	if ttl <= 0 {
		return errors.New("session ttl must be positive")
	}
	ts := now()
	expires := time.Now().UTC().Add(ttl).Format(timestampFormat)
	return s.tx(func(tx *sql.Tx) error {
		// Logins are rare enough that this is the natural place to sweep
		// expired rows; nothing else ever needs to iterate them.
		if _, err := tx.Exec("DELETE FROM ui_sessions WHERE expires_at <= ?", ts); err != nil {
			return err
		}
		_, err := tx.Exec(
			"INSERT INTO ui_sessions (id, token_id, created_at, expires_at) VALUES (?, ?, ?, ?)",
			hashToken(id), tokenID, ts, expires,
		)
		return err
	})
}

// UISession resolves a cookie value to the signed-in token's name. An expired
// session, an unknown id, or a session whose token has been revoked resolves
// to ErrNotFound.
func (s *Store) UISession(id string) (*UISession, error) {
	var out UISession
	var expires string
	err := s.db.QueryRow(
		`SELECT t.name, t.role, us.expires_at FROM ui_sessions us
		 JOIN tokens t ON t.id = us.token_id AND t.revoked_at IS NULL
		 WHERE us.id = ? AND us.expires_at > ?`, hashToken(id), now(),
	).Scan(&out.TokenName, &out.TokenRole, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if out.ExpiresAt, err = time.Parse(timestampFormat, expires); err != nil {
		return nil, fmt.Errorf("ui session expiry: %w", err)
	}
	return &out, nil
}

// RenewUISession pushes a live session's expiry out to now+ttl. Renewing a
// session that has already expired or been deleted is a quiet no-op: the next
// lookup fails and the browser lands on the login form.
func (s *Store) RenewUISession(id string, ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("session ttl must be positive")
	}
	expires := time.Now().UTC().Add(ttl).Format(timestampFormat)
	_, err := s.db.Exec(
		"UPDATE ui_sessions SET expires_at = ? WHERE id = ? AND expires_at > ?",
		expires, hashToken(id), now(),
	)
	return err
}

// DeleteUISession forgets one session; deleting an unknown id is not an error.
func (s *Store) DeleteUISession(id string) error {
	_, err := s.db.Exec("DELETE FROM ui_sessions WHERE id = ?", hashToken(id))
	return err
}
