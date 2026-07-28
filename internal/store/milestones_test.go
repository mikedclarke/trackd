package store

import (
	"strings"
	"testing"
)

func milestoneFixture(t *testing.T) *Store {
	t.Helper()
	s := openTestStore(t)
	if _, err := s.CreateProject(ProjectInput{Name: "Rebuild"}, "pm"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMilestone(MilestoneInput{Project: "rebuild", Name: "Launch", TargetDate: "2026-09-01"}, "pm"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMilestoneCRUD(t *testing.T) {
	s := milestoneFixture(t)

	list, err := s.ListMilestones("rebuild", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "Launch" || list[0].TargetDate != "2026-09-01" || list[0].Project != "rebuild" {
		t.Fatalf("milestones = %+v", list)
	}

	if _, err := s.CreateMilestone(MilestoneInput{Project: "rebuild", Name: "Launch"}, "pm"); err == nil {
		t.Error("duplicate milestone name in one project was accepted")
	}
	if _, err := s.CreateMilestone(MilestoneInput{Project: "rebuild", Name: "Bad date", TargetDate: "next week"}, "pm"); err == nil {
		t.Error("malformed target date was accepted")
	}
	if _, err := s.CreateMilestone(MilestoneInput{Name: "Orphan"}, "pm"); err == nil {
		t.Error("milestone without a project was accepted")
	}

	newName := "Launch v2"
	updated, err := s.UpdateMilestone(list[0].ID, MilestonePatch{Name: &newName}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Launch v2" {
		t.Fatalf("updated name = %q", updated.Name)
	}

	archived := true
	if _, err := s.UpdateMilestone(list[0].ID, MilestonePatch{Archived: &archived}, "pm"); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListMilestones("rebuild", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("archived milestone still listed: %+v", list)
	}

	events, err := s.ListEvents("milestone", updated.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 milestone events (create, update, archive), got %d", len(events))
	}
}

func TestIssueAssigneeAndMilestone(t *testing.T) {
	s := milestoneFixture(t)

	issue, err := s.CreateIssue(IssueInput{Title: "Ship it", Project: "rebuild", Assignee: "engineer", Milestone: "Launch"}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if issue.Assignee != "engineer" || issue.Milestone != "Launch" {
		t.Fatalf("issue = %+v", issue)
	}

	// Filters.
	byAssignee, err := s.ListIssues(IssueFilter{Assignee: "engineer"})
	if err != nil || len(byAssignee) != 1 {
		t.Fatalf("assignee filter: %v, %v", byAssignee, err)
	}
	byMilestone, err := s.ListIssues(IssueFilter{Milestone: "Launch"})
	if err != nil || len(byMilestone) != 1 {
		t.Fatalf("milestone filter: %v, %v", byMilestone, err)
	}
	assignees, err := s.ListAssignees()
	if err != nil || len(assignees) != 1 || assignees[0] != "engineer" {
		t.Fatalf("assignees = %v, %v", assignees, err)
	}

	// A milestone needs a project, and must exist in that project.
	if _, err := s.CreateIssue(IssueInput{Title: "No project", Milestone: "Launch"}, "pm"); err == nil {
		t.Error("milestone without project was accepted")
	}
	if _, err := s.CreateIssue(IssueInput{Title: "Wrong name", Project: "rebuild", Milestone: "Nope"}, "pm"); err == nil {
		t.Error("unknown milestone was accepted")
	}

	// Clearing and re-setting via patch.
	empty := ""
	patched, err := s.UpdateIssue(issue.Key, IssuePatch{Milestone: &empty, Assignee: &empty}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if patched.Milestone != "" || patched.Assignee != "" {
		t.Fatalf("clear failed: %+v", patched)
	}
	launch := "Launch"
	if _, err := s.UpdateIssue(issue.Key, IssuePatch{Milestone: &launch}, "pm"); err != nil {
		t.Fatal(err)
	}

	// Moving the issue to another project clears a milestone that no longer applies.
	if _, err := s.CreateProject(ProjectInput{Name: "Other"}, "pm"); err != nil {
		t.Fatal(err)
	}
	other := "other"
	moved, err := s.UpdateIssue(issue.Key, IssuePatch{Project: &other}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if moved.Milestone != "" {
		t.Fatalf("milestone survived a project move: %+v", moved)
	}

	// An archived milestone cannot be assigned.
	back := "rebuild"
	if _, err := s.UpdateIssue(issue.Key, IssuePatch{Project: &back, Milestone: &launch}, "pm"); err != nil {
		t.Fatal(err)
	}
	milestones, err := s.ListMilestones("rebuild", false)
	if err != nil {
		t.Fatal(err)
	}
	archived := true
	if _, err := s.UpdateMilestone(milestones[0].ID, MilestonePatch{Archived: &archived}, "pm"); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.CreateIssue(IssueInput{Title: "Late arrival", Project: "rebuild"}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateIssue(fresh.Key, IssuePatch{Milestone: &launch}, "pm"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("archived milestone was assignable: %v", err)
	}
}
