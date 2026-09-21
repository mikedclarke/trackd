package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestViewLifecycle(t *testing.T) {
	s := openTestStore(t)
	mustLabel(t, s, "waiting", "ready")
	if _, err := s.CreateProject(ProjectInput{Name: "Site"}, "pm"); err != nil {
		t.Fatal(err)
	}
	two := 2
	view, err := s.CreateView(ViewInput{
		Name:        " Waiting on me ",
		Description: "rows an agent parked on a person",
		Filter: ViewFilter{
			Statuses: []string{"todo", "In Progress"}, Labels: []string{"WAITING"},
			Project: "site", OrderBy: "created", UpdatedWithin: "7d",
		},
		QuickActions: []QuickAction{
			{Name: "Answered", RemoveLabels: []string{"waiting"}},
			{Name: "Ship", Status: "Done", Priority: &two},
		},
	}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if view.Name != "Waiting on me" || view.Owner != "pm" || !view.Shared || view.ID == 0 {
		t.Fatalf("view = %+v", view)
	}
	// Names are stored as given (trimmed); the filter keeps the caller's
	// spellings, which resolve case-insensitively when the view is applied.
	if len(view.QuickActions) != 2 || view.QuickActions[1].Status != "Done" {
		t.Fatalf("quick actions = %+v", view.QuickActions)
	}

	got, err := s.GetView("waiting ON me")
	if err != nil || got.ID != view.ID {
		t.Fatalf("GetView = %+v, %v", got, err)
	}

	// Applying the view resolves the relative window against now and leaves
	// explicit fields alone.
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	f, err := view.Filter.Apply(IssueFilter{OrderBy: "priority", Limit: 5}, now)
	if err != nil {
		t.Fatal(err)
	}
	if f.OrderBy != "priority" || f.Limit != 5 || f.Project != "site" || len(f.Labels) != 1 {
		t.Errorf("applied filter = %+v", f)
	}
	if f.UpdatedSince != "2026-09-14T12:00:00.000Z" {
		t.Errorf("updated_since = %q", f.UpdatedSince)
	}

	// The applied filter lists what it should.
	mustCreateIssue(t, s, IssueInput{Title: "parked", Status: "Todo", Project: "site", Labels: []string{"waiting"}}, "seo")
	mustCreateIssue(t, s, IssueInput{Title: "free", Status: "Todo", Project: "site"}, "seo")
	f, err = view.Filter.Apply(IssueFilter{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	issues, err := s.ListIssues(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Title != "parked" {
		t.Fatalf("view issues = %+v", issues)
	}

	// Update replaces the filter whole, renames, and audits.
	private := false
	updated, err := s.UpdateView("Waiting on me", ViewPatch{
		Name:   strptr("Court"),
		Filter: &ViewFilter{ExcludeLabels: []string{"ready"}},
		Shared: &private,
	}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Court" || updated.Shared || len(updated.Filter.Labels) != 0 || len(updated.Filter.ExcludeLabels) != 1 {
		t.Fatalf("updated = %+v", updated)
	}
	if _, err := s.GetView("Waiting on me"); !errors.Is(err, ErrNotFound) {
		t.Errorf("old name still resolves: %v", err)
	}

	// Archiving frees the name; the archived view stays readable and can be
	// restored, and the events name the view.
	yes := true
	archived, err := s.UpdateView("court", ViewPatch{Archived: &yes}, "pm")
	if err != nil || archived.ArchivedAt == "" {
		t.Fatalf("archive = %+v, %v", archived, err)
	}
	if _, err := s.CreateView(ViewInput{Name: "Court", Filter: ViewFilter{Statuses: []string{"Todo"}}}, "seo"); err != nil {
		t.Fatalf("name not freed by archive: %v", err)
	}
	// The live view wins the name; the archived one is reachable only through
	// the list with archived included.
	live, err := s.GetView("Court")
	if err != nil || live.ArchivedAt != "" || live.Owner != "seo" {
		t.Fatalf("GetView after re-create = %+v, %v", live, err)
	}
	all, err := s.ListViews("pm", false, true)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListViews archived = %d, %v", len(all), err)
	}
	events, err := s.ListEvents("view", view.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range events {
		actions = append(actions, e.Action)
	}
	if strings.Join(actions, ",") != "view.archived,view.updated,view.created" {
		t.Errorf("events = %v", actions)
	}
	feed, err := s.ListAllEvents(EventFilter{Entity: "view"})
	if err != nil || len(feed) != 4 || feed[0].EntityKey != "Court" {
		t.Errorf("view feed = %+v, %v", feed, err)
	}
}

func TestViewVisibility(t *testing.T) {
	s := openTestStore(t)
	no := false
	for _, v := range []ViewInput{
		{Name: "team", Filter: ViewFilter{Statuses: []string{"Todo"}}},
		{Name: "mine", Filter: ViewFilter{Statuses: []string{"Todo"}}, Shared: &no},
	} {
		if _, err := s.CreateView(v, "pm"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateView(ViewInput{Name: "theirs", Filter: ViewFilter{Statuses: []string{"Todo"}}, Shared: &no}, "seo"); err != nil {
		t.Fatal(err)
	}
	names := func(viewer string, all bool) string {
		views, err := s.ListViews(viewer, all, false)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, v := range views {
			out = append(out, v.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names("pm", false); got != "mine,team" {
		t.Errorf("pm sees %q", got)
	}
	if got := names("seo", false); got != "team,theirs" {
		t.Errorf("seo sees %q", got)
	}
	if got := names("owner", true); got != "mine,team,theirs" {
		t.Errorf("admin sees %q", got)
	}
}

func TestViewValidation(t *testing.T) {
	s := openTestStore(t)
	mustLabel(t, s, "waiting")
	todo := ViewFilter{Statuses: []string{"Todo"}}
	cases := []struct {
		name string
		in   ViewInput
		want error
	}{
		{"no name", ViewInput{Filter: todo}, ErrInvalidRef},
		{"long name", ViewInput{Name: strings.Repeat("x", 61), Filter: todo}, ErrInvalidRef},
		{"slash in name", ViewInput{Name: "a/b", Filter: todo}, ErrInvalidRef},
		{"empty filter", ViewInput{Name: "all"}, ErrInvalidRef},
		{"unknown status", ViewInput{Name: "v", Filter: ViewFilter{Statuses: []string{"Nope"}}}, ErrInvalidRef},
		{"unknown label", ViewInput{Name: "v", Filter: ViewFilter{Labels: []string{"nope"}}}, ErrInvalidRef},
		{"unknown project", ViewInput{Name: "v", Filter: ViewFilter{Project: "nope"}}, ErrInvalidRef},
		{"bad priority", ViewInput{Name: "v", Filter: ViewFilter{Priorities: []int{7}}}, ErrInvalidRef},
		{"bad window", ViewInput{Name: "v", Filter: ViewFilter{UpdatedWithin: "soon"}}, ErrInvalidRef},
		{"bad order", ViewInput{Name: "v", Filter: ViewFilter{Statuses: []string{"Todo"}, OrderBy: "title"}}, ErrInvalidRef},
		{"nameless quick action", ViewInput{Name: "v", Filter: todo, QuickActions: []QuickAction{{Status: "Done"}}}, ErrInvalidRef},
		{"empty quick action", ViewInput{Name: "v", Filter: todo, QuickActions: []QuickAction{{Name: "Noop"}}}, ErrInvalidRef},
		{"quick action unknown label", ViewInput{Name: "v", Filter: todo, QuickActions: []QuickAction{{Name: "x", AddLabels: []string{"nope"}}}}, ErrInvalidRef},
		{"quick action unknown status", ViewInput{Name: "v", Filter: todo, QuickActions: []QuickAction{{Name: "x", Status: "Nope"}}}, ErrInvalidRef},
		{"duplicate quick action", ViewInput{Name: "v", Filter: todo, QuickActions: []QuickAction{{Name: "x", Status: "Done"}, {Name: "X", Status: "Todo"}}}, ErrInvalidRef},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.CreateView(tc.in, "pm"); !errors.Is(err, tc.want) {
				t.Errorf("CreateView = %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := s.CreateView(ViewInput{Name: "taken", Filter: todo}, "pm"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateView(ViewInput{Name: "TAKEN", Filter: todo}, "pm"); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate name = %v, want conflict", err)
	}
	if _, err := s.UpdateView("taken", ViewPatch{Filter: &ViewFilter{}}, "pm"); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("empty filter on update = %v", err)
	}
	if _, err := s.UpdateView("missing", ViewPatch{Description: strptr("x")}, "pm"); !errors.Is(err, ErrNotFound) {
		t.Errorf("update missing = %v", err)
	}
}

func TestParseWithin(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, true}, {"30m", 30 * time.Minute, true}, {"48h", 48 * time.Hour, true},
		{"7d", 7 * 24 * time.Hour, true}, {"2w", 14 * 24 * time.Hour, true},
		{"0d", 0, false}, {"7", 0, false}, {"7 d", 0, false}, {"1.5h", 0, false}, {"1y", 0, false},
	} {
		got, err := ParseWithin(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ParseWithin(%q) = %v, %v; want %v, ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestListIssuesPriorityAndCreator(t *testing.T) {
	s := openTestStore(t)
	mustCreateIssue(t, s, IssueInput{Title: "urgent", Status: "Todo", Priority: 1}, "pm")
	mustCreateIssue(t, s, IssueInput{Title: "low", Status: "Todo", Priority: 4}, "seo")
	mustCreateIssue(t, s, IssueInput{Title: "none", Status: "Todo"}, "seo")

	issues, err := s.ListIssues(IssueFilter{Priorities: []int{1, 4}, OrderBy: "created"})
	if err != nil || len(issues) != 2 || issues[0].Title != "urgent" || issues[1].Title != "low" {
		t.Fatalf("priority filter = %+v, %v", issues, err)
	}
	issues, err = s.ListIssues(IssueFilter{CreatedBy: "SEO", OrderBy: "created"})
	if err != nil || len(issues) != 2 || issues[0].Title != "low" {
		t.Fatalf("created_by filter = %+v, %v", issues, err)
	}
	if _, err := s.ListIssues(IssueFilter{Priorities: []int{9}}); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("bad priority filter = %v", err)
	}
}
