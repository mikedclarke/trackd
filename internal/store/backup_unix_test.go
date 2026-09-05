//go:build unix

package store

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO at the probe path makes the probe's open block until a reader
// appears, reproducing directories that hang on file operations (macOS
// privacy-protected folders, dead network mounts). The regression this
// guards: a wedged backup used to hold the store's only connection and
// freeze every API request.
func TestBackupContextWedgedDirFailsFast(t *testing.T) {
	s := openTestStore(t)
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, ".trackd-probe"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.BackupContext(ctx, dir)
	if err == nil {
		t.Fatal("backup into a wedged dir reported success")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("BackupContext blocked for %v; must honor ctx", elapsed)
	}
	// The store's own connection must be unaffected while the probe goroutine
	// is still wedged.
	if _, _, err := s.CreateIssue(IssueInput{Title: "still alive"}, ""); err != nil {
		t.Fatalf("store blocked after wedged backup: %v", err)
	}
}
