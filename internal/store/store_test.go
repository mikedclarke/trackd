package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenSeedsDefaults(t *testing.T) {
	s := openTestStore(t)
	statuses, err := s.ListStatuses()
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 8 {
		t.Fatalf("expected 8 seeded statuses, got %d", len(statuses))
	}
	prefix, err := s.Setting("issue_prefix")
	if err != nil || prefix != "TSK" {
		t.Fatalf("issue_prefix = %q, %v", prefix, err)
	}
}

func TestIssueLifecycle(t *testing.T) {
	s := openTestStore(t)

	issue, err := s.CreateIssue(IssueInput{
		Title:       "First issue",
		Description: "Body with unicode — ✓ and\nnewlines",
		Labels:      []string{"agent-ready", "b", "agent-ready"},
	}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if issue.Key != "TSK-1" {
		t.Errorf("key = %q, want TSK-1", issue.Key)
	}
	if issue.Status != "Triage" || issue.StatusType != "triage" {
		t.Errorf("default status = %s/%s", issue.Status, issue.StatusType)
	}
	if len(issue.Labels) != 2 {
		t.Errorf("labels = %v, want deduped 2", issue.Labels)
	}

	second, err := s.CreateIssue(IssueInput{Title: "Second"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Key != "TSK-2" {
		t.Errorf("key = %q, want TSK-2", second.Key)
	}

	title := "Renamed"
	status := "In Progress"
	updated, err := s.UpdateIssue("TSK-1", IssuePatch{Title: &title, Status: &status}, "seo")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != "Renamed" || updated.Status != "In Progress" {
		t.Errorf("update not applied: %+v", updated)
	}
	if updated.StartedAt == "" {
		t.Error("started_at not stamped on transition to started status")
	}

	done := "Done"
	updated, err = s.UpdateIssue("TSK-1", IssuePatch{Status: &done}, "seo")
	if err != nil {
		t.Fatal(err)
	}
	if updated.CompletedAt == "" {
		t.Error("completed_at not stamped on transition to completed status")
	}

	archived := true
	if _, err := s.UpdateIssue("TSK-1", IssuePatch{Archived: &archived}, ""); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListIssues(IssueFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Key != "TSK-2" {
		t.Errorf("archived issue still listed: %+v", list)
	}

	events, err := s.ListEvents("issue", issue.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("expected 4 events (create + 3 updates), got %d", len(events))
	}
	last := events[0]
	if last.Action != "issue.updated" || last.Before == nil || last.After == nil {
		t.Errorf("update event missing before/after: %+v", last)
	}
	var before Issue
	if err := json.Unmarshal(last.Before, &before); err != nil {
		t.Fatal(err)
	}
	if before.ArchivedAt != "" {
		t.Error("event before-state should predate archiving")
	}

	if _, err := s.GetIssue("TSK-999"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing issue error = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateIssue(IssueInput{Title: "bad", Priority: 9}, ""); err == nil {
		t.Error("priority 9 accepted")
	}
	if _, err := s.CreateIssue(IssueInput{Title: "bad", Status: "Nope"}, ""); err == nil {
		t.Error("unknown status accepted")
	}
}

func TestListIssueFilters(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateProject(ProjectInput{Name: "Site Rebuild"}, ""); err != nil {
		t.Fatal(err)
	}
	mk := func(title, status, project string, labels ...string) *Issue {
		t.Helper()
		issue, err := s.CreateIssue(IssueInput{Title: title, Status: status, Project: project, Labels: labels}, "")
		if err != nil {
			t.Fatal(err)
		}
		return issue
	}
	mk("Fix header", "Todo", "site-rebuild", "agent-ready")
	mk("Write copy", "Todo", "site-rebuild")
	mk("Audit backlinks", "In Progress", "", "agent-ready")

	cases := []struct {
		name string
		f    IssueFilter
		want int
	}{
		{"all", IssueFilter{}, 3},
		{"status", IssueFilter{Status: "Todo"}, 2},
		{"status type", IssueFilter{StatusType: "started"}, 1},
		{"project", IssueFilter{Project: "site-rebuild"}, 2},
		{"label", IssueFilter{Label: "agent-ready"}, 2},
		{"label and status", IssueFilter{Label: "agent-ready", Status: "Todo"}, 1},
		{"query", IssueFilter{Query: "backlinks"}, 1},
		{"query key", IssueFilter{Query: "TSK-2"}, 1},
		{"no match", IssueFilter{Query: "zzz"}, 0},
		{"limit", IssueFilter{Limit: 2}, 2},
	}
	for _, tc := range cases {
		got, err := s.ListIssues(tc.f)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != tc.want {
			t.Errorf("%s: got %d issues, want %d", tc.name, len(got), tc.want)
		}
	}

	parent, _ := s.GetIssue("TSK-1")
	child := "TSK-1"
	if _, err := s.UpdateIssue("TSK-2", IssuePatch{Parent: &child}, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListIssues(IssueFilter{Parent: parent.Key})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "TSK-2" {
		t.Errorf("parent filter: %+v", got)
	}
}

func TestCommentsAndRelations(t *testing.T) {
	s := openTestStore(t)
	a, _ := s.CreateIssue(IssueInput{Title: "A"}, "")
	b, _ := s.CreateIssue(IssueInput{Title: "B"}, "")

	c, err := s.AddComment(a.Key, "Done, see output/", "engineer")
	if err != nil {
		t.Fatal(err)
	}
	if c.Actor != "engineer" {
		t.Errorf("comment actor = %q", c.Actor)
	}
	comments, err := s.ListComments(a.Key)
	if err != nil || len(comments) != 1 {
		t.Fatalf("comments = %v, %v", comments, err)
	}

	if err := s.AddRelation(a.Key, b.Key, "blocks", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRelation(a.Key, b.Key, "blocks", ""); err != nil {
		t.Fatal(err) // duplicate add is a no-op, not an error
	}
	if err := s.AddRelation(a.Key, a.Key, "blocks", ""); err == nil {
		t.Error("self-relation accepted")
	}
	if err := s.AddRelation(a.Key, b.Key, "nonsense", ""); err == nil {
		t.Error("unknown relation type accepted")
	}
	rels, err := s.ListRelations(b.Key)
	if err != nil || len(rels) != 1 {
		t.Fatalf("relations = %v, %v", rels, err)
	}
	if rels[0].IssueKey != a.Key || rels[0].Type != "blocks" {
		t.Errorf("relation = %+v", rels[0])
	}
	if err := s.RemoveRelation(a.Key, b.Key, "blocks", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRelation(a.Key, b.Key, "blocks", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("second remove = %v, want ErrNotFound", err)
	}
}

func TestProjects(t *testing.T) {
	s := openTestStore(t)
	p, err := s.CreateProject(ProjectInput{Name: "Gerrards Bullion — SEO!"}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if p.Slug != "gerrards-bullion-seo" {
		t.Errorf("slug = %q", p.Slug)
	}
	if _, err := s.CreateProject(ProjectInput{Name: "Gerrards Bullion (SEO)"}, ""); err == nil {
		t.Error("duplicate slug accepted")
	}

	status := "completed"
	updated, err := s.UpdateProject(p.Slug, ProjectPatch{Status: &status}, "pm")
	if err != nil || updated.Status != "completed" {
		t.Fatalf("update = %+v, %v", updated, err)
	}

	withLabels, err := s.SetProjectLabels(p.Slug, []string{"client-x"}, "pm")
	if err != nil || len(withLabels.Labels) != 1 {
		t.Fatalf("labels = %+v, %v", withLabels, err)
	}

	archived := true
	if _, err := s.UpdateProject(p.Slug, ProjectPatch{Archived: &archived}, ""); err != nil {
		t.Fatal(err)
	}
	visible, _ := s.ListProjects(false)
	all, _ := s.ListProjects(true)
	if len(visible) != 0 || len(all) != 1 {
		t.Errorf("visible = %d, all = %d", len(visible), len(all))
	}
}

func TestTokens(t *testing.T) {
	s := openTestStore(t)
	plaintext, err := s.CreateToken("pm", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plaintext, "td_") {
		t.Errorf("plaintext = %q", plaintext)
	}
	tok, err := s.VerifyToken(plaintext)
	if err != nil || tok.Name != "pm" || tok.Role != "agent" {
		t.Fatalf("verify = %+v, %v", tok, err)
	}
	if _, err := s.VerifyToken("td_wrong"); err == nil {
		t.Error("bogus token verified")
	}
	if _, err := s.CreateToken("pm", "agent"); err == nil {
		t.Error("duplicate token name accepted")
	}
	if _, err := s.CreateToken("root", "superuser"); err == nil {
		t.Error("unknown role accepted")
	}
	if err := s.RevokeToken("pm"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyToken(plaintext); err == nil {
		t.Error("revoked token verified")
	}
	if err := s.RevokeToken("pm"); !errors.Is(err, ErrNotFound) {
		t.Errorf("double revoke = %v, want ErrNotFound", err)
	}
}

func TestCustomPrefix(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetSetting("issue_prefix", "GDL"); err != nil {
		t.Fatal(err)
	}
	issue, err := s.CreateIssue(IssueInput{Title: "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if issue.Key != "GDL-1" {
		t.Errorf("key = %q, want GDL-1", issue.Key)
	}
}

func TestMigrationSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateIssue(IssueInput{Title: "survives"}, ""); err != nil {
		t.Fatal(err)
	}

	migs, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	migs = append(migs, migration{name: "9999_test.sql", sql: "CREATE TABLE migration_probe (id INTEGER PRIMARY KEY)"})
	if err := s.applyMigrations(migs); err != nil {
		t.Fatal(err)
	}
	s.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".pre-migrate-v1-") {
			snapshot = filepath.Join(dir, e.Name())
		}
	}
	if snapshot == "" {
		t.Fatal("no pre-migration snapshot created")
	}

	// The snapshot must itself be a valid database holding the pre-migration data.
	snap, err := Open(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	issue, err := snap.GetIssue("TSK-1")
	if err != nil || issue.Title != "survives" {
		t.Fatalf("snapshot data: %+v, %v", issue, err)
	}
}
