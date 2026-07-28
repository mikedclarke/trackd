package store

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestBackupRestore(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t)
	if _, err := s.CreateIssue(IssueInput{Title: "precious"}, ""); err != nil {
		t.Fatal(err)
	}

	backupDir := filepath.Join(dir, "backups")
	snap, err := s.Backup(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(snap); err != nil {
		t.Fatal(err)
	}

	// Same-second backups must not collide.
	snap2, err := s.Backup(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if snap2 == snap {
		t.Fatal("second backup reused the same filename")
	}

	restored := filepath.Join(dir, "restored.db")
	if err := Restore(snap, restored); err != nil {
		t.Fatal(err)
	}
	rs, err := Open(restored)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	issue, err := rs.GetIssue("TSK-1")
	if err != nil || issue.Title != "precious" {
		t.Fatalf("restored issue = %+v, %v", issue, err)
	}

	if err := Restore(snap, restored); err == nil {
		t.Fatal("restore overwrote an existing database")
	}
	if err := Restore(filepath.Join(dir, "missing.db"), filepath.Join(dir, "x.db")); err == nil {
		t.Fatal("restore accepted a missing snapshot")
	}
}

func TestPruneBackups(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t)

	var snaps []string
	for range 3 {
		snap, err := s.Backup(dir)
		if err != nil {
			t.Fatal(err)
		}
		snaps = append(snaps, snap)
	}
	removed, err := PruneBackups(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Fatalf("removed %d snapshots, want 1", len(removed))
	}
	if _, err := os.Stat(snaps[len(snaps)-1]); err != nil {
		t.Error("newest snapshot was pruned")
	}
	if _, err := PruneBackups(dir, 0); err == nil {
		t.Error("keep=0 accepted; pruning everything must be impossible")
	}
}

// A FIFO at the probe path makes the probe's open block until a reader
// appears, reproducing directories that hang on file operations (macOS
// privacy-protected folders, dead network mounts). The regression this
// guards: a wedged backup used to hold the store's only connection and
// freeze every API request.
func TestBackupContextWedgedDirFailsFast(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mkfifo unavailable on windows")
	}
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
	if _, err := s.CreateIssue(IssueInput{Title: "still alive"}, ""); err != nil {
		t.Fatalf("store blocked after wedged backup: %v", err)
	}
}

func TestBackupContextCanceledBeforeStart(t *testing.T) {
	s := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.BackupContext(ctx, t.TempDir()); err == nil {
		t.Fatal("canceled context reported success")
	}
}
