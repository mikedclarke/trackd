package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mikedclarke/trackd/internal/store"
)

// An export that fails midway leaves nothing under the final name, and no
// temp file beside it.
func TestWriteNewFileLeavesNoFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dump.jsonl")
	failed := errors.New("disk went away")
	err := writeNewFile(path, func(w io.Writer) error {
		if _, err := io.WriteString(w, `{"record":"trackd","version":3}`+"\n"); err != nil {
			return err
		}
		return failed
	})
	if !errors.Is(err, failed) {
		t.Fatalf("writeNewFile = %v, want the write's error", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("left behind after a failed write: %s", e.Name())
	}
}

func TestWriteNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dump.jsonl")
	write := func(w io.Writer) error {
		_, err := io.WriteString(w, "complete\n")
		return err
	}
	if err := writeNewFile(path, write); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "complete\n" {
		t.Fatalf("published file = %q, %v", got, err)
	}
	if _, err := os.Stat(path + ".partial"); !os.IsNotExist(err) {
		t.Errorf("temp file still present: %v", err)
	}

	// An existing file is never overwritten.
	if err := writeNewFile(path, func(w io.Writer) error {
		_, err := io.WriteString(w, "other\n")
		return err
	}); err == nil {
		t.Fatal("an existing file was overwritten")
	}
	got, err = os.ReadFile(path)
	if err != nil || string(got) != "complete\n" {
		t.Errorf("existing file now = %q, %v", got, err)
	}
	if _, err := os.Stat(path + ".partial"); !os.IsNotExist(err) {
		t.Errorf("temp file left after a refused publish: %v", err)
	}
}

// A label group refused over existing clashes is a conflict: exit 5.
func TestSettingSetGroupConflictExitsFive(t *testing.T) {
	db := filepath.Join(t.TempDir(), "trackd.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ready", "blocked"} {
		if _, err := st.EnsureLabel(name, ""); err != nil {
			t.Fatal(err)
		}
	}
	issue, _, err := st.CreateIssue(store.IssueInput{Title: "Both", Labels: []string{"ready", "blocked"}}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	err = run([]string{"setting", "set", "--db", db, "label_groups", `[["ready","blocked"]]`})
	if err == nil || !strings.Contains(err.Error(), issue.Key) {
		t.Fatalf("setting set = %v, want a refusal naming %s", err, issue.Key)
	}
	if got := exitCode(err); got != 5 {
		t.Errorf("exit code = %d, want 5", got)
	}
}
