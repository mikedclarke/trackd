package store

import (
	"errors"
	"strconv"
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

func TestIssueMilestoneByID(t *testing.T) {
	s := milestoneFixture(t)
	list, err := s.ListMilestones("rebuild", false)
	if err != nil || len(list) != 1 {
		t.Fatalf("milestones = %v, %v", list, err)
	}
	id := list[0].ID

	// A numeric argument resolves to the milestone with that id.
	issue := mustCreateIssue(t, s, IssueInput{Title: "By id", Project: "rebuild", Milestone: strconv.FormatInt(id, 10)}, "pm")
	if issue.Milestone != "Launch" {
		t.Fatalf("milestone by id = %q, want Launch", issue.Milestone)
	}

	// A numeric id that does not exist is an invalid reference, not a name lookup.
	if _, _, err := s.CreateIssue(IssueInput{Title: "Bad id", Project: "rebuild", Milestone: strconv.FormatInt(id+999, 10)}, "pm"); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("unknown milestone id = %v, want ErrInvalidRef", err)
	}

	// An id belonging to another project does not resolve within this one.
	if _, err := s.CreateProject(ProjectInput{Name: "Other"}, "pm"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateIssue(IssueInput{Title: "Wrong project", Project: "other", Milestone: strconv.FormatInt(id, 10)}, "pm"); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("cross-project milestone id = %v, want ErrInvalidRef", err)
	}

	// The name path still works.
	byName := mustCreateIssue(t, s, IssueInput{Title: "By name", Project: "rebuild", Milestone: "Launch"}, "pm")
	if byName.Milestone != "Launch" {
		t.Fatalf("milestone by name = %q", byName.Milestone)
	}
}

func TestIssueAssigneeAndMilestone(t *testing.T) {
	s := milestoneFixture(t)

	issue := mustCreateIssue(t, s, IssueInput{Title: "Ship it", Project: "rebuild", Assignee: "engineer", Milestone: "Launch"}, "pm")
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
	if _, _, err := s.CreateIssue(IssueInput{Title: "No project", Milestone: "Launch"}, "pm"); err == nil {
		t.Error("milestone without project was accepted")
	}
	if _, _, err := s.CreateIssue(IssueInput{Title: "Wrong name", Project: "rebuild", Milestone: "Nope"}, "pm"); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("unknown milestone = %v, want ErrInvalidRef", err)
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
	fresh := mustCreateIssue(t, s, IssueInput{Title: "Late arrival", Project: "rebuild"}, "pm")
	if _, err := s.UpdateIssue(fresh.Key, IssuePatch{Milestone: &launch}, "pm"); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("archived milestone was assignable: %v", err)
	}
}

// Restating an issue's current project, in any case, is not a move: the
// milestone stays and nothing is written.
func TestIssueSameProjectKeepsMilestone(t *testing.T) {
	s := milestoneFixture(t)
	issue := mustCreateIssue(t, s, IssueInput{Title: "Ship it", Project: "rebuild", Milestone: "Launch"}, "pm")
	events, err := s.ListEvents("issue", issue.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"rebuild", "REBUILD"} {
		got, err := s.UpdateIssue(issue.Key, IssuePatch{Project: &project}, "pm")
		if err != nil {
			t.Fatal(err)
		}
		if got.Milestone != "Launch" || got.Version != issue.Version {
			t.Errorf("project %q: milestone %q at version %d, want Launch at version %d", project, got.Milestone, got.Version, issue.Version)
		}
		after, err := s.ListEvents("issue", issue.ID, 0)
		if err != nil || len(after) != len(events) {
			t.Errorf("project %q recorded an event: %d -> %d (%v)", project, len(events), len(after), err)
		}
	}
	// A typo is still a bad reference, not a quiet no-op.
	typo := "rebiuld"
	if _, err := s.UpdateIssue(issue.Key, IssuePatch{Project: &typo}, "pm"); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("unknown project = %v, want ErrInvalidRef", err)
	}
}

// Milestone names are matched without case when an issue names one, so two
// open milestones in a project may not differ only in case: create, rename
// and unarchive all refuse the clash. An archived milestone frees its name,
// and the same name in another project is fine.
func TestMilestoneNameUniqueWithoutCase(t *testing.T) {
	s := milestoneFixture(t)
	if _, err := s.CreateProject(ProjectInput{Name: "Other"}, "pm"); err != nil {
		t.Fatal(err)
	}
	beta, err := s.CreateMilestone(MilestoneInput{Project: "rebuild", Name: "Beta"}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	launch, err := s.ListMilestones("rebuild", false)
	if err != nil || len(launch) != 2 {
		t.Fatalf("milestones = %+v, %v", launch, err)
	}
	launchID := launch[0].ID
	if launch[0].Name != "Launch" {
		launchID = launch[1].ID
	}
	yes, no := true, false
	renameTo := func(name string) *string { return &name }
	cases := []struct {
		name  string
		write func() error
		ok    bool
	}{
		{"create, other case", func() error {
			_, err := s.CreateMilestone(MilestoneInput{Project: "rebuild", Name: "beta"}, "pm")
			return err
		}, false},
		{"create, other project", func() error {
			_, err := s.CreateMilestone(MilestoneInput{Project: "other", Name: "BETA"}, "pm")
			return err
		}, true},
		{"rename into another's name", func() error {
			_, err := s.UpdateMilestone(launchID, MilestonePatch{Name: renameTo("BETA")}, "pm")
			return err
		}, false},
		{"rename to its own name in another case", func() error {
			_, err := s.UpdateMilestone(beta.ID, MilestonePatch{Name: renameTo("beta")}, "pm")
			return err
		}, true},
		{"archive frees the name", func() error {
			if _, err := s.UpdateMilestone(beta.ID, MilestonePatch{Archived: &yes}, "pm"); err != nil {
				return err
			}
			_, err := s.CreateMilestone(MilestoneInput{Project: "rebuild", Name: "Beta"}, "pm")
			return err
		}, true},
		{"unarchive into a clash", func() error {
			_, err := s.UpdateMilestone(beta.ID, MilestonePatch{Archived: &no}, "pm")
			return err
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.write()
			if tc.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.ok && (!errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "already exists in project rebuild")) {
				t.Fatalf("= %v, want ErrConflict naming the project", err)
			}
		})
	}
}
