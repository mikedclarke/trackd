package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
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
	_, err := s.db.Exec(
		"INSERT INTO tokens (name, hash, role, created_at) VALUES (?, ?, ?, ?)",
		name, hashToken(plaintext), role, now(),
	)
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
	// a synchronous write on every authenticated request.
	if stale(t.LastUsedAt, time.Minute) {
		if _, err := s.db.Exec("UPDATE tokens SET last_used_at = ? WHERE id = ?", now(), t.ID); err != nil {
			return nil, err
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
	res, err := s.db.Exec("UPDATE tokens SET revoked_at = ? WHERE name = ? AND revoked_at IS NULL", now(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("token %q: %w", name, ErrNotFound)
	}
	return nil
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
