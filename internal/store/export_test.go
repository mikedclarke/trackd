package store

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// populate fills a store with one of everything the dump format carries.
func populate(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.CreateProject(ProjectInput{Name: "Site Rebuild", Description: "multi\nline — ✓"}, "pm"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetProjectLabels("site-rebuild", []string{"client-x"}, "pm"); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateIssue(IssueInput{
		Title:       "Fix header — “quotes” & unicode ✓",
		Description: "line one\nline two\t<html> {\"json\": true}",
		Status:      "Todo",
		Priority:    2,
		Project:     "site-rebuild",
		DueDate:     "2026-08-01",
		Labels:      []string{"agent-ready"},
	}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateIssue(IssueInput{Title: "Child task", Parent: a.Key}, "seo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddComment(a.Key, "handoff note\nwith newline", "engineer"); err != nil {
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
	comments, err := dst.ListComments("TSK-1")
	if err != nil || len(comments) != 1 || comments[0].Actor != "engineer" {
		t.Fatalf("restored comments = %+v, %v", comments, err)
	}
	events, err := dst.ListEvents("issue", issue.ID, 0)
	if err != nil || len(events) < 3 {
		t.Fatalf("restored events = %d, %v", len(events), err)
	}

	// The imported sequence must continue from the dump, not restart.
	next, err := dst.CreateIssue(IssueInput{Title: "post-import"}, "")
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
