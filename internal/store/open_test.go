package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// S1: the pragmas ride in the DSN, so they apply to every connection the
// driver hands out rather than only the first one Open happened to touch.
func TestOpenAppliesDSNPragmas(t *testing.T) {
	s := openTestStore(t)
	cases := []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"},
		{"synchronous", "2"},
		{"foreign_keys", "1"},
		{"busy_timeout", "5000"},
	}
	for _, tc := range cases {
		var got string
		if err := s.db.QueryRow("PRAGMA " + tc.pragma).Scan(&got); err != nil {
			t.Fatalf("%s: %v", tc.pragma, err)
		}
		if got != tc.want {
			t.Errorf("PRAGMA %s = %q, want %q", tc.pragma, got, tc.want)
		}
	}
}

func TestDataSourceName(t *testing.T) {
	rw := dataSourceName("/tmp/a b?c.db", false)
	want := "file:/tmp/a b%3fc.db?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)&_txlock=immediate"
	if rw != want {
		t.Errorf("read-write DSN =\n%s\nwant\n%s", rw, want)
	}
	ro := dataSourceName("/tmp/x.db", true)
	// A read-only handle must not set a journal mode (that writes to the file)
	// and must not begin immediate (that fails outright on mode=ro).
	if !strings.Contains(ro, "mode=ro") || strings.Contains(ro, "journal_mode") || strings.Contains(ro, "_txlock") {
		t.Errorf("read-only DSN = %s", ro)
	}
}

// S2: a second writer gets ErrBusy, not an opaque driver error.
func TestConcurrentWriteIsErrBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	holder, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	// Shorten the second store's wait so the test does not sit out the full
	// five second busy timeout; the pooled single connection keeps it.
	if _, err := other.db.Exec("PRAGMA busy_timeout=50"); err != nil {
		t.Fatal(err)
	}
	tx, err := holder.db.Begin() // BEGIN IMMEDIATE: the write lock is taken here
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE settings SET value = value WHERE key = 'issue_seq'"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := other.CreateIssue(IssueInput{Title: "blocked"}, ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("contended write = %v, want ErrBusy", err)
	}
}

// S3: read-only handles never migrate and never write.
func TestOpenReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ro.db")
	rw, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustCreateIssue(t, rw, IssueInput{Title: "readable"}, "")
	rw.Close()

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if !ro.ReadOnly() {
		t.Error("ReadOnly() false on a read-only handle")
	}
	issue, err := ro.GetIssue("TSK-1")
	if err != nil || issue.Title != "readable" {
		t.Fatalf("read-only get = %+v, %v", issue, err)
	}
	if _, _, err := ro.CreateIssue(IssueInput{Title: "nope"}, ""); err == nil {
		t.Error("read-only handle accepted a write")
	}
	// export and backup both run against a read-only handle.
	if _, err := ro.Backup(filepath.Join(dir, "backups")); err != nil {
		t.Errorf("read-only backup: %v", err)
	}
	if _, err := OpenReadOnly(filepath.Join(dir, "missing.db")); err == nil {
		t.Error("read-only open created a missing database")
	}
}

// S3: an older binary must refuse a database a newer one has migrated.
func TestOpenRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for _, open := range []struct {
		name string
		fn   func(string) (*Store, error)
	}{{"read-write", Open}, {"read-only", OpenReadOnly}} {
		if _, err := open.fn(path); !errors.Is(err, ErrSchemaNewer) {
			t.Errorf("%s open = %v, want ErrSchemaNewer", open.name, err)
		}
	}
}

// S3/S18: a corrupt file is refused at open instead of being migrated.
func TestOpenRefusesCorruptDatabase(t *testing.T) {
	// Two offsets, because a damaged page either makes quick_check report a
	// problem or makes the read itself fail; both are ErrIntegrity.
	for _, offset := range []int64{4096, 8192} {
		dir := t.TempDir()
		path := filepath.Join(dir, "corrupt.db")
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 60 {
			mustCreateIssue(t, s, IssueInput{Title: fmt.Sprintf("filler %d", i)}, "")
		}
		if err := s.QuickCheck(); err != nil {
			t.Fatalf("healthy database failed quick_check: %v", err)
		}
		s.Close()

		f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		junk := make([]byte, 4096)
		for i := range junk {
			junk[i] = 0xFF
		}
		if _, err := f.WriteAt(junk, offset); err != nil {
			t.Fatal(err)
		}
		f.Close()

		if _, err := Open(path); !errors.Is(err, ErrIntegrity) {
			t.Errorf("offset %d: open on a corrupt file = %v, want ErrIntegrity", offset, err)
		}
	}
}

// S4: the advisory lock is what stops a second server, or a migrating CLI,
// opening the live database.
func TestLockExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	release, err := s.LockExclusive()
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.LockExclusive(); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock = %v, want ErrLocked", err)
	}
	release()
	second, err := other.LockExclusive()
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	second()
}

// S5: foreign_keys is off while a migration runs (otherwise the orphan insert
// below would be rejected outright) and foreign_key_check is what catches the
// damage before the transaction commits.
func TestMigrationFailsForeignKeyCheck(t *testing.T) {
	s := openTestStore(t)
	migs, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	migs = append(migs, migration{
		name: "9999_orphan.sql",
		sql:  "INSERT INTO issue_labels (issue_id, label_id) VALUES (999, 999)",
	})
	err = s.applyMigrations(migs)
	if err == nil || !strings.Contains(err.Error(), "foreign_key_check") {
		t.Fatalf("orphan migration = %v, want a foreign_key_check failure", err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migs)-1 {
		t.Errorf("user_version = %d after a failed migration, want %d", version, len(migs)-1)
	}
	var on int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&on); err != nil {
		t.Fatal(err)
	}
	if on != 1 {
		t.Error("foreign_keys left off after a failed migration")
	}
}

// S5 and the 0003 rebuild: a database at user_version 2 migrates with every
// row intact, and the pre-migration snapshot lands in SnapshotDir.
func TestMigrateFromSchema2(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v2.db")
	snapshots := filepath.Join(dir, "snapshots")
	migs, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) < 3 {
		t.Fatalf("expected at least 3 migrations, got %d", len(migs))
	}
	db, err := sql.Open("sqlite", dataSourceName(path, false))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, m := range migs[:2] {
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("%s: %v", m.name, err)
		}
	}
	ts := "2026-08-01T09:00:00Z"
	seed := []struct {
		query string
		args  []any
	}{
		{"PRAGMA user_version=2", nil},
		{"INSERT INTO projects (id, name, slug, description, status, created_at, updated_at) VALUES (1, 'Site Rebuild', 'site-rebuild', 'desc', 'active', ?, ?)", []any{ts, ts}},
		{"INSERT INTO labels (id, name, color) VALUES (1, 'agent-ready', '')", nil},
		{"INSERT INTO project_labels (project_id, label_id) VALUES (1, 1)", nil},
		{"INSERT INTO milestones (id, project_id, name, description, target_date, created_at, updated_at) VALUES (1, 1, 'Launch', '', '2026-09-01', ?, ?)", []any{ts, ts}},
		{"INSERT INTO issues (id, key, title, description, status_id, priority, project_id, assignee, milestone_id, created_at, updated_at) VALUES (1, 'TSK-1', 'Old issue', 'body', 3, 2, 1, 'engineer', 1, ?, ?)", []any{ts, ts}},
		{"INSERT INTO issue_labels (issue_id, label_id) VALUES (1, 1)", nil},
		{"INSERT INTO comments (id, issue_id, body, actor, created_at, updated_at) VALUES (1, 1, 'a note', 'pm', ?, ?)", []any{ts, ts}},
		{"UPDATE settings SET value = '1' WHERE key = 'issue_seq'", nil},
	}
	for _, st := range seed {
		if _, err := db.Exec(st.query, st.args...); err != nil {
			t.Fatalf("%s: %v", st.query, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := OpenWith(path, Options{SnapshotDir: snapshots})
	if err != nil {
		t.Fatalf("migrating a v2 database: %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migs) {
		t.Errorf("user_version = %d, want %d", version, len(migs))
	}
	project, err := s.GetProject("site-rebuild")
	if err != nil {
		t.Fatal(err)
	}
	if project.Status != "started" {
		t.Errorf("project status = %q, want started (was active)", project.Status)
	}
	if len(project.Labels) != 1 || project.Labels[0] != "agent-ready" {
		t.Errorf("project labels = %v", project.Labels)
	}
	issue, err := s.GetIssue("TSK-1")
	if err != nil {
		t.Fatal(err)
	}
	if issue.Title != "Old issue" || issue.Milestone != "Launch" || issue.Assignee != "engineer" {
		t.Errorf("issue = %+v", issue)
	}
	if len(issue.Labels) != 1 || issue.Version != 0 {
		t.Errorf("issue labels = %v, version = %d", issue.Labels, issue.Version)
	}
	comments, err := s.ListComments("TSK-1")
	if err != nil || len(comments) != 1 || comments[0].Body != "a note" {
		t.Fatalf("comments = %+v, %v", comments, err)
	}
	milestones, err := s.ListMilestones("site-rebuild", false)
	if err != nil || len(milestones) != 1 || milestones[0].TargetDate != "2026-09-01" {
		t.Fatalf("milestones = %+v, %v", milestones, err)
	}
	// A migrated database gets no exclusive label groups until an operator
	// sets some.
	groups, err := s.Setting("label_groups")
	if err != nil || groups != "" {
		t.Errorf("label_groups = %q, %v, want empty", groups, err)
	}
	if err := s.tx(foreignKeyCheck); err != nil {
		t.Errorf("migrated database fails foreign_key_check: %v", err)
	}

	entries, err := os.ReadDir(snapshots)
	if err != nil {
		t.Fatalf("snapshot dir: %v", err)
	}
	var found string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".pre-migrate-v2-") {
			found = filepath.Join(snapshots, e.Name())
		}
	}
	if found == "" {
		t.Fatalf("no pre-migration snapshot in %s: %v", snapshots, entries)
	}
	snap, err := OpenReadOnly(found)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	var snapVersion int
	if err := snap.db.QueryRow("PRAGMA user_version").Scan(&snapVersion); err != nil {
		t.Fatal(err)
	}
	if snapVersion != 2 {
		t.Errorf("snapshot user_version = %d, want the pre-migration 2", snapVersion)
	}
}

// S6
func TestTimestamps(t *testing.T) {
	if _, err := time.Parse(timestampFormat, now()); err != nil {
		t.Fatalf("now() = %q: %v", now(), err)
	}
	if len(now()) != len("2006-01-02T15:04:05.000Z") {
		t.Errorf("now() = %q, want millisecond precision", now())
	}
	cases := []struct {
		in   string
		want string
		bad  bool
	}{
		{in: "", want: ""},
		{in: "2026-09-05T11:22:33Z", want: "2026-09-05T11:22:33.000Z"},
		{in: "2026-09-05T11:22:33.123456Z", want: "2026-09-05T11:22:33.123Z"},
		{in: "2026-09-05T12:22:33+01:00", want: "2026-09-05T11:22:33.000Z"},
		{in: "2026-09-05", want: "2026-09-05T00:00:00.000Z"},
		{in: "yesterday", bad: true},
		{in: "05/09/2026", bad: true},
	}
	for _, tc := range cases {
		got, err := ParseTimestamp(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("ParseTimestamp(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseTimestamp(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, ok := range []string{"", "2026-01-31"} {
		if err := validDate(ok); err != nil {
			t.Errorf("validDate(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"next week", "2026-13-01", "2026-1-1", "2026-01-31T00:00:00Z"} {
		if err := validDate(bad); err == nil {
			t.Errorf("validDate(%q) accepted", bad)
		}
	}
}

// S18
func TestQuickCheckAndPing(t *testing.T) {
	s := openTestStore(t)
	if err := s.Ping(); err != nil {
		t.Errorf("Ping: %v", err)
	}
	if err := s.QuickCheck(); err != nil {
		t.Errorf("QuickCheck: %v", err)
	}
}
