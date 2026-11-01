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
		Description: "multi\nline café ✓",
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
		Title:          "Fix header, “quotes” & unicode ✓",
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
	if verified, err := dst.ImportDump(bytes.NewReader(first.Bytes()), false); err != nil || !verified {
		t.Fatalf("import = verified %v, %v", verified, err)
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
	if _, err := src.ImportDump(bytes.NewReader(dump.Bytes()), false); err == nil {
		t.Fatal("import into non-empty database accepted")
	}
}

func TestImportRejectsGarbage(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.ImportDump(strings.NewReader("not json\n"), true); err == nil {
		t.Fatal("garbage accepted as dump")
	}
	if _, err := s.ImportDump(strings.NewReader(`{"record":"other","version":1}`+"\n"), true); err == nil {
		t.Fatal("wrong header accepted")
	}
}

// The header carries the schema, the records the new columns, and the last
// line is the end record.
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
	if header.Version != 3 || header.Schema != len(migs) {
		t.Errorf("header = %+v, want version 3 schema %d", header, len(migs))
	}
	var end dumpEnd
	if err := decodeRecord([]byte(lines[len(lines)-1]), &end); err != nil {
		t.Fatal(err)
	}
	if end.Record != "end" || end.Counts["issue"] != 3 || end.Counts["status"] != 8 || len(end.SHA256) != 64 {
		t.Errorf("end record = %+v", end)
	}
	want := []string{`"idempotency_key":"idem-1"`, `"version":`, `"start_date":"2026-01-01"`, `"target_date":"2026-06-30"`, `"completed_at":`, `"parent_id":`}
	for _, fragment := range want {
		if !strings.Contains(dump.String(), fragment) {
			t.Errorf("dump is missing %s", fragment)
		}
	}
}

// A v1 dump has no end record: refused by default, imported when the caller
// accepts that its completeness cannot be checked.
func TestImportDumpV1Fixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "dump_v1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t)
	if _, err := s.ImportDump(bytes.NewReader(fixture), false); !errors.Is(err, ErrUnverifiedDump) {
		t.Fatalf("v1 dump without the opt-in = %v, want ErrUnverifiedDump", err)
	}
	if verified, err := s.ImportDump(bytes.NewReader(fixture), true); err != nil || verified {
		t.Fatalf("importing a v1 dump = verified %v, %v", verified, err)
	}
	issue, err := s.GetIssue("ACME-1")
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
	comments, err := s.ListComments("ACME-1")
	if err != nil || len(comments) != 1 || comments[0].ParentID != 0 {
		t.Fatalf("comments = %+v, %v", comments, err)
	}
	next, _, err := s.CreateIssue(IssueInput{Title: "after import"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if next.Key != "ACME-3" {
		t.Errorf("next key = %s, want ACME-3", next.Key)
	}
	// The imported dump re-exports as the current version.
	var out bytes.Buffer
	if err := s.ExportDump(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), `{"record":"trackd","version":3,"schema":`) {
		t.Errorf("re-export header = %s", strings.SplitN(out.String(), "\n", 2)[0])
	}
}

// S17: an unknown field means a dump this binary cannot load faithfully.
func TestImportRejectsUnknownFields(t *testing.T) {
	s := openTestStore(t)
	dump := `{"record":"trackd","version":3,"schema":3}` + "\n" +
		`{"record":"setting","key":"issue_prefix","value":"ACME","extra":true}` + "\n"
	_, err := s.ImportDump(strings.NewReader(dump), false)
	if err == nil || !strings.Contains(err.Error(), "extra") {
		t.Fatalf("unknown field = %v, want a rejection naming it", err)
	}
}

// S17: a dump from a newer schema is refused rather than half-loaded.
func TestImportRejectsNewerSchema(t *testing.T) {
	s := openTestStore(t)
	dump := `{"record":"trackd","version":3,"schema":99}` + "\n"
	if _, err := s.ImportDump(strings.NewReader(dump), false); !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("newer schema = %v, want ErrSchemaNewer", err)
	}
}

// A dump cut short on a line boundary, or altered, is refused before the
// destination's seeded settings and statuses are lost.
func TestImportRefusesTruncatedDump(t *testing.T) {
	src := openTestStore(t)
	if err := src.UpdateSetting("issue_prefix", "ACME", "pm"); err != nil {
		t.Fatal(err)
	}
	issue := mustCreateIssue(t, src, IssueInput{Title: "Kept"}, "pm")
	if _, _, err := src.AddComment(issue.Key, CommentInput{Body: "note"}, "pm"); err != nil {
		t.Fatal(err)
	}
	var dump bytes.Buffer
	if err := src.ExportDump(&dump); err != nil {
		t.Fatal(err)
	}
	full := dump.String()
	lines := strings.SplitAfter(full, "\n")
	cutBeforeEvents := full[:strings.Index(full, `{"record":"event"`)]
	dropped := strings.Replace(full, `{"record":"setting","key":"workspace_name","value":"trackd"}`+"\n", "", 1)
	if dropped == full {
		t.Fatal("fixture: workspace_name record not found")
	}
	changed := strings.Replace(full, `"title":"Kept"`, `"title":"Kapt"`, 1)
	if changed == full {
		t.Fatal("fixture: issue title not found")
	}
	cases := []struct {
		name, body, want string
	}{
		{"cut before events", cutBeforeEvents, "no end record"},
		{"header only", lines[0], "no end record"},
		{"line dropped", dropped, "end record counts"},
		{"byte changed", changed, "checksum does not match"},
		{"line after end", full + `{"record":"setting","key":"extra","value":"x"}` + "\n", "after the end record"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := openTestStore(t)
			_, err := dst.ImportDump(strings.NewReader(tc.body), false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("import = %v, want a refusal saying %q", err, tc.want)
			}
			statuses, err := dst.ListStatuses()
			if err != nil || len(statuses) != 8 {
				t.Errorf("statuses after the refusal = %d, %v, want the 8 seeded", len(statuses), err)
			}
			if prefix, err := dst.Setting("issue_prefix"); err != nil || prefix != "TSK" {
				t.Errorf("issue_prefix after the refusal = %q, %v, want the seeded TSK", prefix, err)
			}
		})
	}
}

// A dump whose label groups its own memberships break is refused, so the
// import cannot bring in a clash the setting would have refused.
func TestImportRefusesGroupConflict(t *testing.T) {
	src := openTestStore(t)
	mustLabel(t, src, "ready", "blocked")
	issue := mustCreateIssue(t, src, IssueInput{Title: "Both", Labels: []string{"ready", "blocked"}}, "pm")
	if err := src.SetSetting("label_groups", `[["ready","blocked"]]`); err != nil {
		t.Fatal(err)
	}
	var dump bytes.Buffer
	if err := src.ExportDump(&dump); err != nil {
		t.Fatal(err)
	}
	dst := openTestStore(t)
	_, err := dst.ImportDump(bytes.NewReader(dump.Bytes()), false)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), issue.Key) {
		t.Fatalf("import = %v, want ErrConflict naming %s", err, issue.Key)
	}
	if issues, err := dst.ListIssues(IssueFilter{}); err != nil || len(issues) != 0 {
		t.Errorf("issues after the refusal = %d, %v", len(issues), err)
	}
	if statuses, err := dst.ListStatuses(); err != nil || len(statuses) != 8 {
		t.Errorf("statuses after the refusal = %d, %v", len(statuses), err)
	}
}
