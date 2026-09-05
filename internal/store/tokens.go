package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

var tokenRoles = map[string]bool{"admin": true, "agent": true}

// CreateToken mints a new API token and returns its plaintext, which is shown
// exactly once; only the SHA-256 hash is stored.
func (s *Store) CreateToken(name, role string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("token name is required")
	}
	if !tokenRoles[role] {
		return "", fmt.Errorf("unknown token role %q", role)
	}
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	plaintext := "td_" + hex.EncodeToString(raw)
	err := s.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			"INSERT INTO tokens (name, hash, role, created_at) VALUES (?, ?, ?, ?)",
			name, hashToken(plaintext), role, now(),
		)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		// The plaintext never reaches the audit trail.
		return recordEvent(tx, "token", id, name, "token.created", nil, map[string]string{"name": name, "role": role})
	})
	if err != nil {
		return "", fmt.Errorf("create token %q: %w", name, err)
	}
	return plaintext, nil
}

func (s *Store) VerifyToken(plaintext string) (*Token, error) {
	hash := hashToken(plaintext)
	var t Token
	var storedHash string
	err := s.db.QueryRow(
		"SELECT id, name, hash, role, created_at, COALESCE(last_used_at, ''), COALESCE(revoked_at, '') "+
			"FROM tokens WHERE hash = ? AND revoked_at IS NULL",
		hash,
	).Scan(&t.ID, &t.Name, &storedHash, &t.Role, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("invalid token")
	}
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(storedHash)) != 1 {
		return nil, errors.New("invalid token")
	}
	// last_used_at is a coarse signal; refreshing at most once a minute avoids
	// a synchronous write on every authenticated request. A failed refresh
	// (a busy database, a read-only handle) must never cost a valid caller
	// their request.
	if stale(t.LastUsedAt, time.Minute) {
		if _, err := s.db.Exec("UPDATE tokens SET last_used_at = ? WHERE id = ?", now(), t.ID); err != nil {
			log.Printf("trackd: token %s last_used_at not refreshed: %v", t.Name, err)
		}
	}
	return &t, nil
}

func stale(ts string, d time.Duration) bool {
	if ts == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, ts)
	return err != nil || time.Since(t) >= d
}

func (s *Store) RevokeToken(name string) error {
	return s.tx(func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRow("SELECT id FROM tokens WHERE name = ? AND revoked_at IS NULL", name).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("token %q: %w", name, ErrNotFound)
		}
		if err != nil {
			return err
		}
		ts := now()
		if _, err := tx.Exec("UPDATE tokens SET revoked_at = ? WHERE id = ?", ts, id); err != nil {
			return err
		}
		return recordEvent(tx, "token", id, name, "token.revoked", map[string]string{"name": name}, nil)
	})
}

func (s *Store) ListTokens() ([]Token, error) {
	rows, err := s.db.Query(
		"SELECT id, name, role, created_at, COALESCE(last_used_at, ''), COALESCE(revoked_at, '') FROM tokens ORDER BY id",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.Name, &t.Role, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
