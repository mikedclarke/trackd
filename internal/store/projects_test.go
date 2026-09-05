package store

import (
	"errors"
	"testing"
)

// S13
func TestProjectStatusVocabulary(t *testing.T) {
	s := openTestStore(t)
	for _, status := range []string{"backlog", "planned", "started", "paused", "completed", "canceled"} {
		if _, err := s.CreateProject(ProjectInput{Name: "P " + status, Status: status}, "pm"); err != nil {
			t.Errorf("status %s rejected: %v", status, err)
		}
	}
	if _, err := s.CreateProject(ProjectInput{Name: "Legacy", Status: "active"}, "pm"); err == nil {
		t.Error("the retired 'active' status was accepted")
	}
	defaulted, err := s.CreateProject(ProjectInput{Name: "Unstated"}, "pm")
	if err != nil || defaulted.Status != "backlog" {
		t.Fatalf("default status = %+v, %v", defaulted, err)
	}
}

// S13
func TestProjectDatesAndCompletion(t *testing.T) {
	s := openTestStore(t)
	p, err := s.CreateProject(ProjectInput{Name: "Rebuild", StartDate: "2026-01-01", TargetDate: "2026-06-30"}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if p.StartDate != "2026-01-01" || p.TargetDate != "2026-06-30" || p.CompletedAt != "" {
		t.Fatalf("project = %+v", p)
	}
	if _, err := s.CreateProject(ProjectInput{Name: "Bad dates", StartDate: "soon"}, "pm"); err == nil {
		t.Error("malformed start date accepted")
	}
	if _, err := s.UpdateProject(p.Slug, ProjectPatch{TargetDate: strptr("whenever")}, "pm"); err == nil {
		t.Error("malformed target date accepted on update")
	}
	completed, err := s.UpdateProject(p.Slug, ProjectPatch{Status: strptr("completed")}, "pm")
	if err != nil || completed.CompletedAt == "" {
		t.Fatalf("completed = %+v, %v", completed, err)
	}
	reopened, err := s.UpdateProject(p.Slug, ProjectPatch{Status: strptr("started")}, "pm")
	if err != nil || reopened.CompletedAt != "" {
		t.Fatalf("reopened = %+v, %v", reopened, err)
	}
	cleared, err := s.UpdateProject(p.Slug, ProjectPatch{StartDate: strptr("")}, "pm")
	if err != nil || cleared.StartDate != "" {
		t.Fatalf("cleared start date = %+v, %v", cleared, err)
	}
}

// S13
func TestProjectLabelsInOneTransaction(t *testing.T) {
	s := openTestStore(t)
	mustLabel(t, s, "client-x", "claude-ready", "needs-mike")
	p, err := s.CreateProject(ProjectInput{Name: "Labelled", Labels: []string{"client-x"}}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Labels) != 1 || p.Labels[0] != "client-x" {
		t.Fatalf("labels = %v", p.Labels)
	}
	// A rejected label must take the whole create with it.
	if _, err := s.CreateProject(ProjectInput{Name: "Doomed", Labels: []string{"ghost"}}, "pm"); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("unknown label on create = %v, want ErrInvalidRef", err)
	}
	if _, err := s.GetProject("doomed"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a failed create left a project behind: %v", err)
	}
	added, err := s.UpdateProject(p.Slug, ProjectPatch{AddLabels: []string{"claude-ready"}}, "pm")
	if err != nil || len(added.Labels) != 2 {
		t.Fatalf("add = %+v, %v", added, err)
	}
	swapped, err := s.UpdateProject(p.Slug, ProjectPatch{AddLabels: []string{"needs-mike"}}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if containsFold(swapped.Labels, "claude-ready") {
		t.Errorf("exclusive group not applied to projects: %v", swapped.Labels)
	}
	removed, err := s.UpdateProject(p.Slug, ProjectPatch{RemoveLabels: []string{"needs-mike"}}, "pm")
	if err != nil || len(removed.Labels) != 1 {
		t.Fatalf("remove = %+v, %v", removed, err)
	}
	both := []string{"claude-ready", "needs-mike"}
	if _, err := s.UpdateProject(p.Slug, ProjectPatch{Labels: &both}, "pm"); !errors.Is(err, ErrConflict) {
		t.Errorf("replace with a whole exclusive group = %v, want ErrConflict", err)
	}
}

// S13
func TestProjectDuplicateName(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateProject(ProjectInput{Name: "Site Rebuild"}, "pm"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject(ProjectInput{Name: "site rebuild", Slug: "other"}, "pm"); !errors.Is(err, ErrConflict) {
		t.Errorf("case-different duplicate name = %v, want ErrConflict", err)
	}
	if _, err := s.CreateProject(ProjectInput{Name: "Another", Slug: "site-rebuild"}, "pm"); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate slug = %v, want ErrConflict", err)
	}
	if _, err := s.CreateProject(ProjectInput{Name: "Second"}, "pm"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateProject("second", ProjectPatch{Name: strptr("SITE REBUILD")}, "pm"); !errors.Is(err, ErrConflict) {
		t.Errorf("rename onto an existing name = %v, want ErrConflict", err)
	}
	// Case-insensitive slug lookup.
	if _, err := s.GetProject("SITE-REBUILD"); err != nil {
		t.Errorf("case-insensitive slug lookup: %v", err)
	}
}

// S14
func TestMilestoneNameUniqueOnlyWhileUnarchived(t *testing.T) {
	s := milestoneFixture(t)
	list, err := s.ListMilestones("rebuild", false)
	if err != nil || len(list) != 1 {
		t.Fatalf("milestones = %+v, %v", list, err)
	}
	original := list[0]
	if _, err := s.CreateMilestone(MilestoneInput{Project: "rebuild", Name: "Launch"}, "pm"); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate live name = %v, want ErrConflict", err)
	}
	archived := true
	if _, err := s.UpdateMilestone(original.ID, MilestonePatch{Archived: &archived}, "pm"); err != nil {
		t.Fatal(err)
	}
	reused, err := s.CreateMilestone(MilestoneInput{Project: "rebuild", Name: "Launch"}, "pm")
	if err != nil {
		t.Fatalf("archiving must free the name: %v", err)
	}
	if reused.ID == original.ID {
		t.Fatal("expected a second milestone row")
	}
	// Un-archiving back into the clash is the branch that used to dereference
	// a nil patch name.
	live := false
	_, err = s.UpdateMilestone(original.ID, MilestonePatch{Archived: &live}, "pm")
	if !errors.Is(err, ErrConflict) {
		t.Errorf("un-archive into a name clash = %v, want ErrConflict", err)
	}
}

// S15
func TestTokenEvents(t *testing.T) {
	s := openTestStore(t)
	plaintext, err := s.CreateToken("pm", "agent")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := s.ListTokens()
	if err != nil || len(tokens) != 1 {
		t.Fatalf("tokens = %+v, %v", tokens, err)
	}
	events, err := s.ListEvents("token", tokens[0].ID, 0)
	if err != nil || len(events) != 1 || events[0].Action != "token.created" {
		t.Fatalf("create events = %+v, %v", events, err)
	}
	if string(events[0].After) == "" || containsFold([]string{string(events[0].After)}, plaintext) {
		t.Error("the token plaintext must never reach the audit trail")
	}
	if err := s.RevokeToken("pm"); err != nil {
		t.Fatal(err)
	}
	events, err = s.ListEvents("token", tokens[0].ID, 0)
	if err != nil || len(events) != 2 || events[0].Action != "token.revoked" {
		t.Fatalf("revoke events = %+v, %v", events, err)
	}
}

// S15: a token stays valid when its last_used_at cannot be written.
func TestVerifyTokenSurvivesUnwritableStore(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/tokens.db"
	rw, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := rw.CreateToken("pm", "agent")
	if err != nil {
		t.Fatal(err)
	}
	rw.Close()

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	token, err := ro.VerifyToken(plaintext)
	if err != nil || token.Name != "pm" {
		t.Fatalf("verify on a read-only store = %+v, %v", token, err)
	}
}
