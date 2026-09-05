package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupRestore(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t)
	if _, _, err := s.CreateIssue(IssueInput{Title: "precious"}, ""); err != nil {
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

func TestBackupContextCanceledBeforeStart(t *testing.T) {
	s := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.BackupContext(ctx, t.TempDir()); err == nil {
		t.Fatal("canceled context reported success")
	}
}

// S16: pruning only ever touches files this code wrote.
func TestPruneBackupsIgnoresForeignFiles(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t)
	for range 3 {
		if _, err := s.Backup(dir); err != nil {
			t.Fatal(err)
		}
	}
	bystanders := []string{"trackd-manual.db", "trackd-2026-01-01.db", "notes.db", "trackd-20260101-000000.db.bak"}
	for _, name := range bystanders {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not a snapshot"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A snapshot-shaped file that is not a valid database: evidence of a bad
	// backup, never something to delete.
	corrupt := filepath.Join(dir, "trackd-20200101-000000.db")
	if err := os.WriteFile(corrupt, []byte("garbage that is not sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := PruneBackups(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed %v, want the two oldest real snapshots", removed)
	}
	for _, name := range append(bystanders, filepath.Base(corrupt)) {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was pruned: %v", name, err)
		}
	}
}

// S16
func TestNewestBackupAge(t *testing.T) {
	dir := t.TempDir()
	if _, ok := NewestBackupAge(dir); ok {
		t.Error("an empty directory reported a backup")
	}
	if _, ok := NewestBackupAge(filepath.Join(dir, "missing")); ok {
		t.Error("a missing directory reported a backup")
	}
	s := openTestStore(t)
	if _, err := s.Backup(dir); err != nil {
		t.Fatal(err)
	}
	age, ok := NewestBackupAge(dir)
	if !ok {
		t.Fatal("a fresh snapshot was not found")
	}
	if age > time.Minute {
		t.Errorf("age = %v, want something close to zero", age)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := NewestBackupAge(dir); !ok {
		t.Error("a foreign file broke the age lookup")
	}
}

// S16: restoring under a live database would be silently undone.
func TestRestoreRefusesWithSidecars(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t)
	if _, _, err := s.CreateIssue(IssueInput{Title: "precious"}, ""); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Backup(filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		target := filepath.Join(dir, "live"+suffix+".db")
		sidecar := target + suffix
		if err := os.WriteFile(sidecar, []byte("sidecar"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := Restore(snap, target)
		if err == nil {
			t.Fatalf("%s: restore ran with a sidecar present", suffix)
		}
		if !strings.Contains(err.Error(), sidecar) {
			t.Errorf("%s: error %q does not name the sidecar", suffix, err)
		}
		if _, err := os.Stat(target); err == nil {
			t.Errorf("%s: a refused restore still wrote the database", suffix)
		}
	}
	// With no sidecars the restore goes through and is verified.
	good := filepath.Join(dir, "restored.db")
	if err := Restore(snap, good); err != nil {
		t.Fatal(err)
	}
	rs, err := OpenReadOnly(good)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	if issue, err := rs.GetIssue("TSK-1"); err != nil || issue.Title != "precious" {
		t.Fatalf("restored issue = %+v, %v", issue, err)
	}
}
