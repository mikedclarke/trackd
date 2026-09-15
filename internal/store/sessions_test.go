package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newSessionStore(t *testing.T) (*Store, int64) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	plaintext, err := s.CreateToken("pm", "agent")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.VerifyToken(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	return s, token.ID
}

func TestUISessionLifecycle(t *testing.T) {
	s, tokenID := newSessionStore(t)

	if err := s.CreateUISession("cookie-1", tokenID, time.Hour); err != nil {
		t.Fatal(err)
	}
	session, err := s.UISession("cookie-1")
	if err != nil {
		t.Fatal(err)
	}
	if session.TokenName != "pm" {
		t.Errorf("token name = %q, want pm", session.TokenName)
	}
	if remaining := time.Until(session.ExpiresAt); remaining < 50*time.Minute || remaining > time.Hour {
		t.Errorf("expiry %v is not about an hour out", remaining)
	}

	// The raw session id is never stored, so a database row is not a cookie.
	var stored string
	if err := s.db.QueryRow("SELECT id FROM ui_sessions").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "cookie-1" {
		t.Error("session id stored in plaintext")
	}

	if _, err := s.UISession("cookie-2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session = %v, want ErrNotFound", err)
	}

	if err := s.RenewUISession("cookie-1", 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	session, err = s.UISession("cookie-1")
	if err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(session.ExpiresAt); remaining < 110*time.Minute {
		t.Errorf("expiry %v did not move out on renewal", remaining)
	}

	if err := s.DeleteUISession("cookie-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UISession("cookie-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted session = %v, want ErrNotFound", err)
	}
	if err := s.DeleteUISession("cookie-1"); err != nil {
		t.Errorf("deleting an unknown session = %v, want nil", err)
	}
}

func TestUISessionExpiry(t *testing.T) {
	s, tokenID := newSessionStore(t)
	if err := s.CreateUISession("short", tokenID, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := s.UISession("short"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired session = %v, want ErrNotFound", err)
	}
	// Renewal cannot resurrect an expired session.
	if err := s.RenewUISession("short", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UISession("short"); !errors.Is(err, ErrNotFound) {
		t.Errorf("renewed expired session = %v, want ErrNotFound", err)
	}
	// The next login sweeps the dead row.
	if err := s.CreateUISession("fresh", tokenID, time.Hour); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM ui_sessions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("ui_sessions rows = %d, want the expired row swept", count)
	}
}

func TestUISessionDiesWithRevokedToken(t *testing.T) {
	s, tokenID := newSessionStore(t)
	if err := s.CreateUISession("cookie-1", tokenID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeToken("pm"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UISession("cookie-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("session for revoked token = %v, want ErrNotFound", err)
	}
}

func TestUISessionRejectsBadInput(t *testing.T) {
	s, tokenID := newSessionStore(t)
	if err := s.CreateUISession("", tokenID, time.Hour); err == nil {
		t.Error("empty session id accepted")
	}
	if err := s.CreateUISession("cookie-1", tokenID, 0); err == nil {
		t.Error("zero ttl accepted")
	}
	if err := s.RenewUISession("cookie-1", -time.Hour); err == nil {
		t.Error("negative renewal ttl accepted")
	}
}
