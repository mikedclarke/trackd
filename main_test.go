package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
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

// A run killed after creating its temp file leaves it behind; the next export
// to the same path takes it over instead of failing with "file exists", and
// the stale bytes do not survive into the published file.
func TestWriteNewFileAfterKilledRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no advisory lock on Windows, so a leftover is refused")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "dump.jsonl")
	if err := os.WriteFile(path+".partial", []byte("half of a much longer dump from the killed run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeNewFile(path, func(w io.Writer) error {
		_, err := io.WriteString(w, "complete\n")
		return err
	}); err != nil {
		t.Fatalf("rerun after a killed export: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "complete\n" {
		t.Fatalf("published file = %q, %v", got, err)
	}
	if _, err := os.Stat(path + ".partial"); !os.IsNotExist(err) {
		t.Errorf("temp file still present: %v", err)
	}
}

// An export still writing to the temp file holds its lock, so a second export
// to the same path is refused and leaves the first one's file alone.
func TestWriteNewFileRefusesWhileAnotherRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no advisory lock on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "dump.jsonl")
	held, err := store.LockFile(path + ".partial")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := os.WriteFile(path+".partial", []byte("in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = writeNewFile(path, func(w io.Writer) error {
		_, err := io.WriteString(w, "second\n")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("writeNewFile = %v, want a refusal", err)
	}
	if got, err := os.ReadFile(path + ".partial"); err != nil || string(got) != "in progress\n" {
		t.Errorf("running export's temp file = %q, %v", got, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a file was published under the final name: %v", err)
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

// A server holds the lock on its database's .lock file, which stays put when
// the database itself is moved aside, so a restore to that path is refused
// whether or not a file is there. A restore that ran would be lost: the
// server keeps writing to the moved file.
func TestRestoreRefusedWhileServerHoldsLock(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "trackd.db")
	st, err := store.OpenWith(db, store.Options{Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	snap, err := st.Backup(filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(db + suffix); err == nil {
			if err := os.Rename(db+suffix, filepath.Join(dir, "aside.db"+suffix)); err != nil {
				t.Fatal(err)
			}
		}
	}
	err = run([]string{"restore", "--db", db, snap})
	if err == nil || !strings.Contains(err.Error(), "another trackd is running") {
		t.Fatalf("restore under a live server = %v, want a refusal", err)
	}
	if got := exitCode(err); got != 5 {
		t.Errorf("exit code = %d, want 5", got)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Errorf("the refused restore wrote %s: %v", db, err)
	}
}

// Anything at the temp name that is not a plain file this run created or
// locked is refused, never followed: a link left there would otherwise have
// its target overwritten with the export.
func TestWriteNewFileRefusesLinkAtTempName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need extra privileges on Windows")
	}
	dir := t.TempDir()
	victim := filepath.Join(dir, "precious.db")
	if err := os.WriteFile(victim, []byte("the only copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "dump.jsonl")
	if err := os.Symlink(victim, path+".partial"); err != nil {
		t.Fatal(err)
	}
	err := writeNewFile(path, func(w io.Writer) error {
		_, err := io.WriteString(w, "export\n")
		return err
	})
	if err == nil {
		t.Fatal("writeNewFile followed a link at the temp name")
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "the only copy" {
		t.Errorf("link target = %q, %v; want it untouched", got, err)
	}
	if info, err := os.Lstat(path + ".partial"); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link at the temp name was removed or replaced: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("a file was published under the final name: %v", err)
	}
}

// The export is written through the handle that holds the temp file's lock,
// is the same file that gets published, and is readable by its owner only.
func TestWriteNewFileWritesThroughLockedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dump.jsonl")
	var written os.FileInfo
	err := writeNewFile(path, func(w io.Writer) error {
		f, ok := w.(*os.File)
		if !ok {
			t.Fatalf("writer is %T, want the temp file", w)
		}
		info, err := f.Stat()
		if err != nil {
			return err
		}
		written = info
		named, err := os.Lstat(path + ".partial")
		if err != nil {
			return err
		}
		if !os.SameFile(info, named) {
			t.Error("the writer is not the file at the temp name")
		}
		_, err = io.WriteString(w, "complete\n")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	published, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(written, published) {
		t.Error("the published file is not the one that was written")
	}
	if runtime.GOOS != "windows" && published.Mode().Perm() != 0o600 {
		t.Errorf("export mode = %v, want 0600", published.Mode().Perm())
	}
}

// Where the lock is not real, a leftover temp file cannot be told from a
// running export, so it is refused, as 0.7.0's exclusive create did.
func TestWriteNewFileWithoutLockRefusesLeftover(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the advisory lock is real here")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "dump.jsonl")
	if err := os.WriteFile(path+".partial", []byte("another export\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeNewFile(path, func(w io.Writer) error {
		_, err := io.WriteString(w, "second\n")
		return err
	})
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("writeNewFile = %v, want the leftover refused", err)
	}
	if got, err := os.ReadFile(path + ".partial"); err != nil || string(got) != "another export\n" {
		t.Errorf("leftover = %q, %v; want it untouched", got, err)
	}
}

// A dry run writes nothing: against a path with no database it runs against a
// throwaway one, so neither the database nor its lock file appears, whether
// the export is accepted or refused.
func TestImportDryRunCreatesNothing(t *testing.T) {
	cases := []struct {
		name    string
		csv     string
		wantErr bool
	}{
		{"valid export", "ID,Title,Status,Created,Updated\nACME-1,Ship it,Todo,2026-05-13T11:21:41Z,2026-05-13T11:21:41Z\n", false},
		{"not a Linear export", "ID,Title\nACME-1,Ship it\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			csv := filepath.Join(dir, "export.csv")
			if err := os.WriteFile(csv, []byte(tc.csv), 0o600); err != nil {
				t.Fatal(err)
			}
			db := filepath.Join(dir, "dry.db")
			var err error
			captureStdout(t, func() {
				err = run([]string{"import", "--db", db, "--dry-run", "linear", csv})
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("dry run = %v, want error %v", err, tc.wantErr)
			}
			for _, p := range []string{db, db + ".lock", db + "-wal", db + "-shm"} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Errorf("dry run left %s: %v", filepath.Base(p), err)
				}
			}
			if left, _ := os.ReadDir(scratch); len(left) != 0 {
				t.Errorf("dry run left its throwaway database behind: %v", left)
			}
		})
	}
}

// The commands that change an existing database refuse a path with no
// database, exit 3, instead of creating an empty one and reporting success.
func TestCommandsRefuseMissingDatabase(t *testing.T) {
	cases := []struct {
		name string
		cmd  []string
		args []string
	}{
		{"token add", []string{"token", "add"}, []string{"builder"}},
		{"token revoke", []string{"token", "revoke"}, []string{"builder"}},
		{"setting set", []string{"setting", "set"}, []string{"issue_prefix", "ACME"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := filepath.Join(t.TempDir(), "typo.db")
			args := append(append(append([]string{}, tc.cmd...), "--db", db), tc.args...)
			var err error
			captureStdout(t, func() {
				err = run(args)
			})
			if err == nil || !strings.Contains(err.Error(), "no database at "+db) {
				t.Fatalf("%s = %v, want a refusal naming the path", tc.name, err)
			}
			if got := exitCode(err); got != 3 {
				t.Errorf("exit code = %d, want 3", got)
			}
			for _, p := range []string{db, db + ".lock"} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Errorf("%s left %s: %v", tc.name, filepath.Base(p), err)
				}
			}
		})
	}
}
