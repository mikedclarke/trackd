package store

import (
	"errors"
	"strings"
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

// Issue keys narrow the global feed to those issues' events, oldest first with
// the same cursor, so one request reads several issues' histories.
func TestListAllEventsByIssueKeys(t *testing.T) {
	s := openTestStore(t)
	a := mustCreateIssue(t, s, IssueInput{Title: "alpha"}, "pm")
	b := mustCreateIssue(t, s, IssueInput{Title: "beta"}, "pm")
	mustCreateIssue(t, s, IssueInput{Title: "gamma"}, "pm")
	if _, err := s.UpdateIssue(a.Key, IssuePatch{Status: strptr("Todo")}, "pm"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject(ProjectInput{Name: "Noise"}, "pm"); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListAllEvents(EventFilter{IssueKeys: []string{a.Key, strings.ToLower(b.Key)}})
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, e := range got {
		seen = append(seen, e.EntityKey+" "+e.Action)
	}
	want := []string{a.Key + " issue.created", b.Key + " issue.created", a.Key + " issue.updated"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("keyed feed = %v, want %v", seen, want)
	}

	// The cursor and the limit page through a keyed feed like the plain one.
	page, err := s.ListAllEvents(EventFilter{IssueKeys: []string{a.Key, b.Key}, Limit: 2})
	if err != nil || len(page) != 2 {
		t.Fatalf("first keyed page = %+v, %v", page, err)
	}
	rest, err := s.ListAllEvents(EventFilter{IssueKeys: []string{a.Key, b.Key}, AfterID: page[1].ID})
	if err != nil || len(rest) != 1 || rest[0].Action != "issue.updated" {
		t.Fatalf("keyed page after cursor = %+v, %v", rest, err)
	}
	if both, err := s.ListAllEvents(EventFilter{IssueKeys: []string{a.Key}, Entity: "issue"}); err != nil || len(both) != 2 {
		t.Errorf("keys with entity issue = %+v, %v", both, err)
	}

	if _, err := s.ListAllEvents(EventFilter{IssueKeys: []string{a.Key, "TSK-999"}}); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("unknown key = %v, want ErrInvalidRef", err)
	}
	if _, err := s.ListAllEvents(EventFilter{IssueKeys: []string{a.Key}, Entity: "project"}); err == nil {
		t.Error("keys with entity project were accepted")
	}
	tooMany := make([]string, MaxEventIssueKeys+1)
	for i := range tooMany {
		tooMany[i] = a.Key
	}
	if _, err := s.ListAllEvents(EventFilter{IssueKeys: tooMany}); err == nil {
		t.Error("more than MaxEventIssueKeys keys were accepted")
	}
}

// A private view's events reach only its owner and an admin's read. The
// clause sits in the query, so a page is never cut short by a hidden row.
func TestListAllEventsHidesPrivateViews(t *testing.T) {
	s := openTestStore(t)
	first := mustCreateIssue(t, s, IssueInput{Title: "First"}, "pm")
	no := false
	filter := ViewFilter{Statuses: []string{"Todo"}}
	if _, err := s.CreateView(ViewInput{Name: "Alex only", Filter: filter, Shared: &no}, "alex"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateView(ViewInput{Name: "Everyone", Filter: filter}, "alex"); err != nil {
		t.Fatal(err)
	}
	second := mustCreateIssue(t, s, IssueInput{Title: "Second"}, "pm")

	keys := func(f EventFilter) string {
		t.Helper()
		events, err := s.ListAllEvents(f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range events {
			out = append(out, e.EntityKey)
		}
		return strings.Join(out, ",")
	}
	everyone := first.Key + ",Everyone," + second.Key
	all := first.Key + ",Alex only,Everyone," + second.Key
	for _, tc := range []struct {
		name   string
		filter EventFilter
		want   string
	}{
		{"another viewer", EventFilter{Viewer: "sam"}, everyone},
		{"no viewer", EventFilter{}, everyone},
		{"another viewer, views only", EventFilter{Viewer: "sam", Entity: "view"}, "Everyone"},
		{"the owner", EventFilter{Viewer: "alex"}, all},
		{"an admin", EventFilter{Viewer: "sam", AllViews: true}, all},
	} {
		if got := keys(tc.filter); got != tc.want {
			t.Errorf("%s: feed = %s, want %s", tc.name, got, tc.want)
		}
	}

	// One event a page: every page another viewer reads is full until the
	// last, and together they are the unpaged feed.
	var paged []string
	var after int64
	for range 10 {
		page, err := s.ListAllEvents(EventFilter{Viewer: "sam", AfterID: after, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		paged = append(paged, page[0].EntityKey)
		after = page[0].ID
	}
	if got := strings.Join(paged, ","); got != everyone {
		t.Errorf("paged feed = %s, want %s", got, everyone)
	}
}
