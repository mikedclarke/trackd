package store

import (
	"testing"
)

// S12
func TestListAllEvents(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateProject(ProjectInput{Name: "Rebuild"}, "pm"); err != nil {
		t.Fatal(err)
	}
	milestone, err := s.CreateMilestone(MilestoneInput{Project: "rebuild", Name: "Launch"}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	issue := mustCreateIssue(t, s, IssueInput{Title: "Ship", Project: "rebuild"}, "pm")

	all, err := s.ListAllEvents(EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("events = %d, want 3: %+v", len(all), all)
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatalf("global feed is not ascending by id: %+v", all)
		}
	}
	keys := map[string]string{}
	for _, e := range all {
		keys[e.Entity] = e.EntityKey
	}
	want := map[string]string{"project": "rebuild", "milestone": "rebuild/Launch", "issue": issue.Key}
	for entity, wantKey := range want {
		if keys[entity] != wantKey {
			t.Errorf("%s entity_key = %q, want %q", entity, keys[entity], wantKey)
		}
	}

	after, err := s.ListAllEvents(EventFilter{AfterID: all[0].ID})
	if err != nil || len(after) != 2 || after[0].ID != all[1].ID {
		t.Fatalf("after_id page = %+v, %v", after, err)
	}
	onlyIssues, err := s.ListAllEvents(EventFilter{Entity: "issue"})
	if err != nil || len(onlyIssues) != 1 || onlyIssues[0].Entity != "issue" {
		t.Fatalf("entity filter = %+v, %v", onlyIssues, err)
	}
	limited, err := s.ListAllEvents(EventFilter{Limit: 1})
	if err != nil || len(limited) != 1 || limited[0].ID != all[0].ID {
		t.Fatalf("limit = %+v, %v", limited, err)
	}
	since, err := s.ListAllEvents(EventFilter{Since: "2000-01-01"})
	if err != nil || len(since) != 3 {
		t.Fatalf("since = %d events, %v", len(since), err)
	}
	future, err := s.ListAllEvents(EventFilter{Since: "2999-01-01T00:00:00Z"})
	if err != nil || len(future) != 0 {
		t.Fatalf("future since = %+v, %v", future, err)
	}
	if _, err := s.ListAllEvents(EventFilter{Since: "not a time"}); err == nil {
		t.Error("unparseable since accepted")
	}

	// The per-entity feeds keep their newest-first order.
	perIssue, err := s.ListEvents("issue", issue.ID, 0)
	if err != nil || len(perIssue) != 1 || perIssue[0].EntityKey != issue.Key {
		t.Fatalf("per-issue feed = %+v, %v", perIssue, err)
	}
	perMilestone, err := s.ListEvents("milestone", milestone.ID, 0)
	if err != nil || len(perMilestone) != 1 || perMilestone[0].EntityKey != "rebuild/Launch" {
		t.Fatalf("per-milestone feed = %+v, %v", perMilestone, err)
	}
}
