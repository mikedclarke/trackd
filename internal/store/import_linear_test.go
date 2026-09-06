package store

import (
	"strings"
	"testing"
)

// linearFixture mirrors the column set and value formats of a real Linear
// issue export.
const linearFixture = `"ID","Team","Title","Description","Status","Estimate","Priority","Project ID","Project","Creator","Assignee","Labels","Cycle Number","Cycle Name","Cycle Start","Cycle End","Created","Updated","Started","Triaged","Completed","Canceled","Archived","Due Date","Parent issue","Initiatives","Project Milestone ID","Project Milestone","SLA Status","UUID","Time in status (minutes)","Related to","Blocked by","Duplicate of"
"ACME-1","ACME","Rebuild service pages","Line one
line two, “quotes” ✓","In Progress","","High","uuid-1","Site Rebuild","Alex","Alex","agent-ready, seo","","","","","Wed May 13 2026 11:21:41 GMT+0000 (GMT+00:00)","Wed Jul 01 2026 11:51:28 GMT+0000 (GMT+00:00)","Thu May 14 2026 09:00:00 GMT+0000 (GMT+00:00)","","","","","Thu Jun 25 2026 23:00:00 GMT+0000 (GMT+00:00)","","","m-uuid-1","Launch","","u1","10","ACME-2","",""
"ACME-2","ACME","Child task","","Done","","Medium","uuid-1","Site Rebuild","Alex","Alex","","","","","","Wed May 13 2026 11:25:00 GMT+0000 (GMT+00:00)","Wed Jul 01 2026 12:10:54 GMT+0000 (GMT+00:00)","","","Wed Jul 01 2026 12:10:54 GMT+0000 (GMT+00:00)","","","","ACME-1","","","","","u2","5","ACME-1","ACME-4",""
"ACME-3","ACME","Old duplicate","","Duplicate","","No priority","","","Alex","","","","","","","Wed May 13 2026 11:30:00 GMT+0000 (GMT+00:00)","Wed May 20 2026 10:00:00 GMT+0000 (GMT+00:00)","","","","Wed May 20 2026 10:00:00 GMT+0000 (GMT+00:00)","","","","","","","","u3","1","","","ACME-1"
"ACME-4","ACME","Archived one","","QA Check","","Urgent","uuid-2","Other Project","Alex","","ops","","","","","Wed May 13 2026 11:31:00 GMT+0000 (GMT+00:00)","Thu May 21 2026 14:10:53 GMT+0000 (GMT+00:00)","","","","","Thu May 21 2026 14:10:53 GMT+0000 (GMT+00:00)","","","","","","","u4","2","","",""
"PER-1","Personal","Personal errand","","Todo","","Low","","","Alex","","","","","","","Wed May 13 2026 11:32:00 GMT+0000 (GMT+00:00)","Wed May 13 2026 11:32:00 GMT+0000 (GMT+00:00)","","","","","","","","","","","","u5","0","","ACME-99",""
`

func TestImportLinearCSV(t *testing.T) {
	s := openTestStore(t)
	stats, err := s.ImportLinearCSV(strings.NewReader(linearFixture), "linear-import", false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Issues != 5 || stats.Projects != 2 || stats.Labels != 3 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.Milestones != 1 || stats.Assignees != 1 {
		t.Errorf("milestones = %d, assignees = %d, want 1 and 1", stats.Milestones, stats.Assignees)
	}
	project, err := s.GetProject("site-rebuild")
	if err != nil {
		t.Fatal(err)
	}
	if project.Status != "planned" {
		t.Errorf("imported project status = %q, want planned", project.Status)
	}
	imported, err := s.GetIssue("ACME-1")
	if err != nil {
		t.Fatal(err)
	}
	if imported.Assignee != "Alex" || imported.Milestone != "Launch" {
		t.Errorf("ACME-1 assignee=%q milestone=%q, want Alex and Launch", imported.Assignee, imported.Milestone)
	}
	// ACME-1 related ACME-2 appears on both rows but must import once; ACME-2 is
	// blocked by ACME-4; ACME-3 duplicates ACME-1.
	if stats.Relations["relates"] != 1 || stats.Relations["blocks"] != 1 || stats.Relations["duplicate"] != 1 {
		t.Errorf("relations = %+v", stats.Relations)
	}
	if len(stats.StatusesCreated) != 1 || stats.StatusesCreated[0] != "QA Check" {
		t.Errorf("statuses created = %v", stats.StatusesCreated)
	}
	if stats.Prefix != "ACME" || stats.Seq != 4 {
		t.Errorf("sequence = %s-%d", stats.Prefix, stats.Seq)
	}
	if len(stats.Skipped) != 1 || !strings.Contains(stats.Skipped[0], "ACME-99") {
		t.Errorf("skipped = %v", stats.Skipped)
	}

	issue, err := s.GetIssue("ACME-1")
	if err != nil {
		t.Fatal(err)
	}
	if issue.Status != "In Progress" || issue.Priority != 2 || issue.Project != "site-rebuild" {
		t.Errorf("ACME-1 = %+v", issue)
	}
	if issue.CreatedAt != "2026-05-13T11:21:41Z" || issue.StartedAt != "2026-05-14T09:00:00Z" {
		t.Errorf("ACME-1 timestamps = created %s started %s", issue.CreatedAt, issue.StartedAt)
	}
	// Midnight-local due dates round to the intended calendar date.
	if issue.DueDate != "2026-06-26" {
		t.Errorf("ACME-1 due = %s, want 2026-06-26", issue.DueDate)
	}
	if len(issue.Labels) != 2 || issue.Labels[0] != "agent-ready" {
		t.Errorf("ACME-1 labels = %v", issue.Labels)
	}
	if !strings.Contains(issue.Description, "line two, “quotes” ✓") {
		t.Errorf("ACME-1 description = %q", issue.Description)
	}

	child, _ := s.GetIssue("ACME-2")
	if child.Parent != "ACME-1" || child.CompletedAt == "" {
		t.Errorf("ACME-2 = %+v", child)
	}
	archived, _ := s.GetIssue("ACME-4")
	if archived.ArchivedAt == "" || archived.Status != "QA Check" || archived.Priority != 1 {
		t.Errorf("ACME-4 = %+v", archived)
	}
	relations, _ := s.ListRelations("ACME-1")
	if len(relations) != 2 { // relates ACME-2, duplicate from ACME-3
		t.Errorf("ACME-1 relations = %+v", relations)
	}

	// Archived issues are excluded from default listings but retrievable.
	visible, err := s.ListIssues(IssueFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 4 {
		t.Errorf("visible issues = %d, want 4", len(visible))
	}

	// The key sequence continues after the dominant prefix's maximum.
	next, _, err := s.CreateIssue(IssueInput{Title: "post-import"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if next.Key != "ACME-5" {
		t.Errorf("next key = %s, want ACME-5", next.Key)
	}

	events, err := s.ListEvents("issue", issue.ID, 0)
	if err != nil || len(events) != 1 || events[0].Action != "issue.imported" {
		t.Errorf("events = %+v, %v", events, err)
	}
}

func TestImportLinearDryRun(t *testing.T) {
	s := openTestStore(t)
	stats, err := s.ImportLinearCSV(strings.NewReader(linearFixture), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Issues != 5 {
		t.Errorf("dry-run stats = %+v", stats)
	}
	issues, err := s.ListIssues(IssueFilter{Archived: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("dry run wrote %d issues", len(issues))
	}
	// A real import must still work afterwards.
	if _, err := s.ImportLinearCSV(strings.NewReader(linearFixture), "", false); err != nil {
		t.Fatal(err)
	}
}

func TestImportLinearRefusals(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.ImportLinearCSV(strings.NewReader("a,b,c\n1,2,3\n"), "", false); err == nil ||
		!strings.Contains(err.Error(), "missing column") {
		t.Errorf("bad header error = %v", err)
	}
	if _, _, err := s.CreateIssue(IssueInput{Title: "existing"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportLinearCSV(strings.NewReader(linearFixture), "", false); err == nil {
		t.Error("import into non-empty database accepted")
	}
}
