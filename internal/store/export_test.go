package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// populate fills a store with one of everything the dump format carries.
func populate(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.CreateProject(ProjectInput{
		Name:        "Site Rebuild",
		Description: "multi\nline — ✓",
		Status:      "started",
		StartDate:   "2026-01-01",
		TargetDate:  "2026-06-30",
	}, "pm"); err != nil {
		t.Fatal(err)
	}
	mustLabel(t, s, "client-x", "agent-ready")
	labels := []string{"client-x"}
	if _, err := s.UpdateProject("site-rebuild", ProjectPatch{Labels: &labels}, "pm"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMilestone(MilestoneInput{Project: "site-rebuild", Name: "Launch", TargetDate: "2026-09-01"}, "pm"); err != nil {
		t.Fatal(err)
	}
	a, _, err := s.CreateIssue(IssueInput{
		Title:          "Fix header — “quotes” & unicode ✓",
		Description:    "line one\nline two\t<html> {\"json\": true}",
		Status:         "Todo",
		Priority:       2,
		Project:        "site-rebuild",
		Assignee:       "engineer",
		Milestone:      "Launch",
		DueDate:        "2026-08-01",
		Labels:         []string{"agent-ready"},
		IdempotencyKey: "create-a",
	}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.CreateIssue(IssueInput{Title: "Child task", Parent: a.Key}, "seo")
	if err != nil {
		t.Fatal(err)
	}
	parent, _, err := s.AddComment(a.Key, CommentInput{Body: "handoff note\nwith newline"}, "engineer")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddComment(a.Key, CommentInput{Body: "threaded reply", ParentID: parent.ID, IdempotencyKey: "reply-1"}, "pm"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRelation(a.Key, b.Key, "blocks", "pm"); err != nil {
		t.Fatal(err)
	}
	done := "Done"
	if _, err := s.UpdateIssue(b.Key, IssuePatch{Status: &done}, "seo"); err != nil {
		t.Fatal(err)
	}
	archived := true
	if _, err := s.UpdateIssue(b.Key, IssuePatch{Archived: &archived}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken("pm", "agent"); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeToken("pm"); err != nil {
		t.Fatal(err)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	src := openTestStore(t)
	populate(t, src)

	var first bytes.Buffer
	if err := src.ExportDump(&first); err != nil {
		t.Fatal(err)
	}

	dst, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := dst.ImportDump(bytes.NewReader(first.Bytes())); err != nil {
		t.Fatal(err)
	}

	var second bytes.Buffer
	if err := dst.ExportDump(&second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("round-trip export is not byte-identical")
	}

	issue, err := dst.GetIssue("TSK-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(issue.Title, "unicode ✓") || issue.Project != "site-rebuild" {
		t.Errorf("restored issue = %+v", issue)
	}
	child, err := dst.GetIssue("TSK-2")
	if err != nil {
		t.Fatal(err)
	}
	if child.Version == 0 {
		t.Error("issue version did not survive the round trip")
	}
	project, err := dst.GetProject("site-rebuild")
	if err != nil {
		t.Fatal(err)
	}
	if project.StartDate != "2026-01-01" || project.TargetDate != "2026-06-30" {
		t.Errorf("project dates did not survive the round trip: %+v", project)
	}
	comments, err := dst.ListComments("TSK-1")
	if err != nil || len(comments) != 2 || comments[0].Actor != "engineer" {
		t.Fatalf("restored comments = %+v, %v", comments, err)
	}
	if comments[1].ParentID != comments[0].ID {
		t.Errorf("comment thread lost: %+v", comments)
	}
	events, err := dst.ListEvents("issue", issue.ID, 0)
	if err != nil || len(events) < 3 {
		t.Fatalf("restored events = %d, %v", len(events), err)
	}

	// The imported sequence must continue from the dump, not restart.
	next, _, err := dst.CreateIssue(IssueInput{Title: "post-import"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if next.Key != "TSK-3" {
		t.Errorf("next key after import = %q, want TSK-3", next.Key)
	}
}

func TestImportRefusesNonEmpty(t *testing.T) {
	src := openTestStore(t)
	populate(t, src)
	var dump bytes.Buffer
	if err := src.ExportDump(&dump); err != nil {
		t.Fatal(err)
	}
	if err := src.ImportDump(bytes.NewReader(dump.Bytes())); err == nil {
		t.Fatal("import into non-empty database accepted")
	}
}

func TestImportRejectsGarbage(t *testing.T) {
	s := openTestStore(t)
	if err := s.ImportDump(strings.NewReader("not json\n")); err == nil {
		t.Fatal("garbage accepted as dump")
	}
	if err := s.ImportDump(strings.NewReader(`{"record":"other","version":1}` + "\n")); err == nil {
		t.Fatal("wrong header accepted")
	}
}

// S17: v2 carries the schema in its header and the new columns in its records.
func TestExportDumpHeaderAndNewFields(t *testing.T) {
	s := openTestStore(t)
	mustLabel(t, s, "client-x", "agent-ready")
	populate(t, s)
	if _, err := s.CreateProject(ProjectInput{Name: "Dated", StartDate: "2026-01-01", TargetDate: "2026-06-30", Status: "completed"}, "pm"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateIssue(IssueInput{Title: "keyed", IdempotencyKey: "idem-1"}, "pm"); err != nil {
		t.Fatal(err)
	}
	var dump bytes.Buffer
	if err := s.ExportDump(&dump); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(dump.String(), "\n"), "\n")
	var header dumpHeader
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatal(err)
	}
	migs, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if header.Version != 2 || header.Schema != len(migs) {
		t.Errorf("header = %+v, want version 2 schema %d", header, len(migs))
	}
	want := []string{`"idempotency_key":"idem-1"`, `"version":`, `"start_date":"2026-01-01"`, `"target_date":"2026-06-30"`, `"completed_at":`, `"parent_id":`}
	for _, fragment := range want {
		if !strings.Contains(dump.String(), fragment) {
			t.Errorf("dump is missing %s", fragment)
		}
	}
}

// S17: a v1 dump written before the cutover still imports.
func TestImportDumpV1Fixture(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "dump_v1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s := openTestStore(t)
	if err := s.ImportDump(f); err != nil {
		t.Fatalf("importing a v1 dump: %v", err)
	}
	issue, err := s.GetIssue("GDL-1")
	if err != nil {
		t.Fatal(err)
	}
	if issue.Title != "Old issue" || issue.Milestone != "Launch" || issue.DueDate != "2026-08-01" {
		t.Errorf("issue = %+v", issue)
	}
	if issue.Version != 0 || len(issue.Labels) != 1 {
		t.Errorf("issue version = %d, labels = %v", issue.Version, issue.Labels)
	}
	project, err := s.GetProject("site-rebuild")
	if err != nil {
		t.Fatal(err)
	}
	if project.Status != "started" {
		t.Errorf("v1 project status = %q, want the migrated started", project.Status)
	}
	comments, err := s.ListComments("GDL-1")
	if err != nil || len(comments) != 1 || comments[0].ParentID != 0 {
		t.Fatalf("comments = %+v, %v", comments, err)
	}
	next, _, err := s.CreateIssue(IssueInput{Title: "after import"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if next.Key != "GDL-3" {
		t.Errorf("next key = %s, want GDL-3", next.Key)
	}
	// The imported dump re-exports as v2.
	var out bytes.Buffer
	if err := s.ExportDump(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), `{"record":"trackd","version":2,"schema":`) {
		t.Errorf("re-export header = %s", strings.SplitN(out.String(), "\n", 2)[0])
	}
}

// S17: an unknown field means a dump this binary cannot load faithfully.
func TestImportRejectsUnknownFields(t *testing.T) {
	s := openTestStore(t)
	dump := `{"record":"trackd","version":2,"schema":3}` + "\n" +
		`{"record":"setting","key":"issue_prefix","value":"GDL","extra":true}` + "\n"
	err := s.ImportDump(strings.NewReader(dump))
	if err == nil || !strings.Contains(err.Error(), "extra") {
		t.Fatalf("unknown field = %v, want a rejection naming it", err)
	}
}

// S17: a dump from a newer schema is refused rather than half-loaded.
func TestImportRejectsNewerSchema(t *testing.T) {
	s := openTestStore(t)
	dump := `{"record":"trackd","version":2,"schema":99}` + "\n"
	if err := s.ImportDump(strings.NewReader(dump)); !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("newer schema = %v, want ErrSchemaNewer", err)
	}
}
