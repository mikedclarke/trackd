package store

import (
	"os"
	"path/filepath"
	"testing"
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
