package store

import (
	"errors"
	"strings"
	"testing"
)

// S7
func TestCreateIssueIdempotency(t *testing.T) {
	s := openTestStore(t)
	first, created, err := s.CreateIssue(IssueInput{Title: "Only once", IdempotencyKey: "abc"}, "pm")
	if err != nil || !created {
		t.Fatalf("first create = %+v, created %v, %v", first, created, err)
	}
	again, created, err := s.CreateIssue(IssueInput{Title: "Different title", IdempotencyKey: "abc"}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("replay reported created = true")
	}
	if again.Key != first.Key || again.Title != first.Title {
		t.Errorf("replay = %+v, want the original %+v", again, first)
	}
	events, err := s.ListEvents("issue", first.ID, 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("replay recorded %d events, want 1: %v", len(events), err)
	}
	if _, _, err := s.CreateIssue(IssueInput{Title: "Fresh"}, "pm"); err != nil {
		t.Fatalf("a create without a key must still work: %v", err)
	}
}

// S7
func TestCreateIssueDerivedFields(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetSetting("base_url", "https://trackd.example"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		priority int
		want     string
	}{{0, "No priority"}, {1, "Urgent"}, {2, "High"}, {3, "Medium"}, {4, "Low"}}
	for _, tc := range cases {
		issue := mustCreateIssue(t, s, IssueInput{Title: "p", Priority: tc.priority}, "")
		if issue.PriorityLabel != tc.want {
			t.Errorf("priority %d label = %q, want %q", tc.priority, issue.PriorityLabel, tc.want)
		}
		if issue.URL != "https://trackd.example/ui/issue/"+issue.Key {
			t.Errorf("url = %q", issue.URL)
		}
		if issue.Labels == nil {
			t.Error("labels nil on a fresh issue")
		}
	}
	if err := s.SetSetting("base_url", ""); err != nil {
		t.Fatal(err)
	}
	issue := mustCreateIssue(t, s, IssueInput{Title: "no base url"}, "")
	if issue.URL != "" {
		t.Errorf("url = %q with an empty base_url", issue.URL)
	}
}

// S7
func TestCreateIssueRejections(t *testing.T) {
	s := openTestStore(t)
	if _, _, err := s.CreateIssue(IssueInput{Title: "unknown label", Labels: []string{"nope"}}, ""); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("unknown label = %v, want ErrInvalidRef", err)
	}
	if _, _, err := s.CreateIssue(IssueInput{Title: "bad date", DueDate: "next tuesday"}, ""); err == nil {
		t.Error("malformed due date accepted")
	}
	if _, _, err := s.CreateIssue(IssueInput{Title: "no project", Project: "ghost"}, ""); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("unknown project = %v, want ErrInvalidRef", err)
	}
	if _, _, err := s.CreateIssue(IssueInput{Title: "no parent", Parent: "TSK-404"}, ""); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("unknown parent = %v, want ErrInvalidRef", err)
	}
}

// S8
func TestUpdateIssueVersioning(t *testing.T) {
	s := openTestStore(t)
	issue := mustCreateIssue(t, s, IssueInput{Title: "Versioned"}, "pm")
	if issue.Version != 0 {
		t.Fatalf("new issue version = %d, want 0", issue.Version)
	}
	title := "Renamed"
	updated, err := s.UpdateIssue(issue.Key, IssuePatch{Title: &title}, "pm")
	if err != nil || updated.Version != 1 {
		t.Fatalf("update = %+v, %v", updated, err)
	}
	stale := int64(0)
	_, err = s.UpdateIssue(issue.Key, IssuePatch{Title: &title, ExpectedVersion: &stale}, "seo")
	if !errors.Is(err, ErrVersionConflict) {
		t.Errorf("stale expected_version = %v, want ErrVersionConflict", err)
	}
	if !errors.Is(err, ErrConflict) {
		t.Errorf("version conflict must also match ErrConflict: %v", err)
	}
	current := int64(1)
	if _, err := s.UpdateIssue(issue.Key, IssuePatch{Title: &title, ExpectedVersion: &current}, "seo"); err != nil {
		t.Errorf("matching expected_version rejected: %v", err)
	}
}

// S8
func TestUpdateIssueDescriptionIsAppendOnly(t *testing.T) {
	s := openTestStore(t)
	issue := mustCreateIssue(t, s, IssueInput{Title: "Notes"}, "pm")

	first := "the original brief"
	if _, err := s.UpdateIssue(issue.Key, IssuePatch{Description: &first}, "pm"); err != nil {
		t.Fatalf("setting the first description: %v", err)
	}
	overwrite := "wiped"
	_, err := s.UpdateIssue(issue.Key, IssuePatch{Description: &overwrite}, "seo")
	if !errors.Is(err, ErrDescriptionReplace) || !errors.Is(err, ErrConflict) {
		t.Fatalf("silent overwrite = %v, want ErrDescriptionReplace and ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "description replace refused: use append or replace_description") {
		t.Errorf("message = %q", err.Error())
	}
	replaced, err := s.UpdateIssue(issue.Key, IssuePatch{Description: &overwrite, ReplaceDescription: true}, "seo")
	if err != nil || replaced.Description != "wiped" {
		t.Fatalf("explicit replace = %+v, %v", replaced, err)
	}
}

// S8
func TestUpdateIssueStatusTimestamps(t *testing.T) {
	s := openTestStore(t)
	issue := mustCreateIssue(t, s, IssueInput{Title: "Lifecycle"}, "pm")
	set := func(status string) *Issue {
		t.Helper()
		out, err := s.UpdateIssue(issue.Key, IssuePatch{Status: &status}, "pm")
		if err != nil {
			t.Fatalf("status %s: %v", status, err)
		}
		return out
	}
	started := set("In Progress")
	if started.StartedAt == "" {
		t.Fatal("started_at not stamped")
	}
	done := set("Done")
	if done.CompletedAt == "" || done.StartedAt != started.StartedAt {
		t.Fatalf("completed = %+v", done)
	}
	reopened := set("Todo")
	if reopened.CompletedAt != "" {
		t.Error("completed_at survived leaving a completed status")
	}
	if reopened.StartedAt != started.StartedAt {
		t.Error("started_at is meant to be sticky")
	}
	canceled := set("Canceled")
	if canceled.CanceledAt == "" {
		t.Fatal("canceled_at not stamped")
	}
	if back := set("Todo"); back.CanceledAt != "" {
		t.Error("canceled_at survived leaving a canceled status")
	}
}

// S8
func TestUpdateIssueLabelRules(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetSetting("label_groups", `[["ready","blocked"]]`); err != nil {
		t.Fatal(err)
	}
	mustLabel(t, s, "ready", "blocked", "seo")
	issue := mustCreateIssue(t, s, IssueInput{Title: "Routing", Labels: []string{"seo"}}, "pm")

	replace := []string{"seo"}
	if _, err := s.UpdateIssue(issue.Key, IssuePatch{Labels: &replace, AddLabels: []string{"ready"}}, "pm"); err == nil {
		t.Error("labels and add_labels together were accepted")
	}
	ready, err := s.UpdateIssue(issue.Key, IssuePatch{AddLabels: []string{"ready"}}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if len(ready.Labels) != 2 {
		t.Fatalf("labels = %v", ready.Labels)
	}
	// The exclusive group swaps rather than accumulating.
	blocked, err := s.UpdateIssue(issue.Key, IssuePatch{AddLabels: []string{"blocked"}}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if containsFold(blocked.Labels, "ready") || !containsFold(blocked.Labels, "blocked") {
		t.Errorf("labels = %v, want ready swapped out", blocked.Labels)
	}
	removed, err := s.UpdateIssue(issue.Key, IssuePatch{RemoveLabels: []string{"BLOCKED"}}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed.Labels) != 1 || removed.Labels[0] != "seo" {
		t.Errorf("labels after remove = %v", removed.Labels)
	}
	both := []string{"ready", "blocked"}
	if _, err := s.UpdateIssue(issue.Key, IssuePatch{Labels: &both}, "pm"); !errors.Is(err, ErrConflict) {
		t.Errorf("replace with two members of one group = %v, want ErrConflict", err)
	}
	if _, err := s.UpdateIssue(issue.Key, IssuePatch{AddLabels: []string{"invented"}}, "pm"); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("unknown label = %v, want ErrInvalidRef", err)
	}
	empty := []string{}
	cleared, err := s.UpdateIssue(issue.Key, IssuePatch{Labels: &empty}, "pm")
	if err != nil || len(cleared.Labels) != 0 {
		t.Fatalf("clearing labels = %+v, %v", cleared, err)
	}
}

// S8
func TestUpdateIssueParentCycles(t *testing.T) {
	s := openTestStore(t)
	a := mustCreateIssue(t, s, IssueInput{Title: "A"}, "")
	b := mustCreateIssue(t, s, IssueInput{Title: "B"}, "")
	c := mustCreateIssue(t, s, IssueInput{Title: "C"}, "")
	if _, err := s.UpdateIssue(b.Key, IssuePatch{Parent: &a.Key}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateIssue(c.Key, IssuePatch{Parent: &b.Key}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateIssue(a.Key, IssuePatch{Parent: &c.Key}, ""); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("cycle a->b->c->a = %v, want ErrInvalidRef", err)
	}
	if _, err := s.UpdateIssue(a.Key, IssuePatch{Parent: &a.Key}, ""); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("self-parent = %v, want ErrInvalidRef", err)
	}
	if _, err := s.UpdateIssue(a.Key, IssuePatch{DueDate: strptr("someday")}, ""); err == nil {
		t.Error("malformed due date accepted on update")
	}
	if _, err := s.UpdateIssue("TSK-404", IssuePatch{Title: strptr("x")}, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing issue = %v, want ErrNotFound", err)
	}
}

func strptr(s string) *string { return &s }

// S9
func TestAppendDescription(t *testing.T) {
	s := openTestStore(t)
	issue := mustCreateIssue(t, s, IssueInput{Title: "Handoff"}, "pm")
	first, err := s.AppendDescription(issue.Key, "context from pm", "pm")
	if err != nil {
		t.Fatal(err)
	}
	if first.Description != "context from pm" {
		t.Errorf("first append = %q, want no leading separator", first.Description)
	}
	second, err := s.AppendDescription(issue.Key, "result from engineer", "engineer")
	if err != nil {
		t.Fatal(err)
	}
	if second.Description != "context from pm\n\nresult from engineer" {
		t.Errorf("second append = %q", second.Description)
	}
	if second.Version != 2 {
		t.Errorf("version = %d after two appends, want 2", second.Version)
	}
	if _, err := s.AppendDescription(issue.Key, "   ", ""); err == nil {
		t.Error("empty append accepted")
	}
	if _, err := s.AppendDescription("TSK-404", "x", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("append to a missing issue = %v, want ErrNotFound", err)
	}
	events, err := s.ListEvents("issue", issue.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Action != "issue.updated" || events[0].Before == nil || events[0].After == nil {
		t.Errorf("append event = %+v", events[0])
	}
}

// S10
func TestCommentThreadsAndEdits(t *testing.T) {
	s := openTestStore(t)
	issue := mustCreateIssue(t, s, IssueInput{Title: "Discussion"}, "pm")
	other := mustCreateIssue(t, s, IssueInput{Title: "Elsewhere"}, "pm")
	stale := "2020-01-01T00:00:00.000Z"
	touch := func(key string) {
		t.Helper()
		if _, err := s.db.Exec("UPDATE issues SET updated_at = ? WHERE key = ?", stale, key); err != nil {
			t.Fatal(err)
		}
	}

	touch(issue.Key)
	root, created, err := s.AddComment(issue.Key, CommentInput{Body: "first"}, "pm")
	if err != nil || !created {
		t.Fatalf("comment = %+v, %v, %v", root, created, err)
	}
	moved, err := s.GetIssue(issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	if moved.UpdatedAt == stale {
		t.Error("a comment must move the issue's updated_at")
	}
	if moved.Version != issue.Version {
		t.Errorf("a comment must not bump version: %d", moved.Version)
	}

	reply, _, err := s.AddComment(issue.Key, CommentInput{Body: "reply", ParentID: root.ID}, "engineer")
	if err != nil || reply.ParentID != root.ID {
		t.Fatalf("reply = %+v, %v", reply, err)
	}
	if _, _, err := s.AddComment(other.Key, CommentInput{Body: "wrong thread", ParentID: root.ID}, ""); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("cross-issue parent = %v, want ErrInvalidRef", err)
	}
	if _, _, err := s.AddComment(issue.Key, CommentInput{Body: "ghost parent", ParentID: 9999}, ""); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("missing parent = %v, want ErrInvalidRef", err)
	}

	once, created, err := s.AddComment(issue.Key, CommentInput{Body: "only once", IdempotencyKey: "k1"}, "pm")
	if err != nil || !created {
		t.Fatal(err)
	}
	replay, created, err := s.AddComment(issue.Key, CommentInput{Body: "different body", IdempotencyKey: "k1"}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if created || replay.ID != once.ID || replay.Body != once.Body {
		t.Errorf("replay = %+v, created %v", replay, created)
	}

	touch(issue.Key)
	edited, err := s.UpdateComment(root.ID, "first, corrected", "pm")
	if err != nil || edited.Body != "first, corrected" {
		t.Fatalf("edit = %+v, %v", edited, err)
	}
	after, err := s.GetIssue(issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	if after.UpdatedAt == stale {
		t.Error("a comment edit must move the issue's updated_at")
	}
	if _, err := s.UpdateComment(9999, "x", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing comment = %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateComment(root.ID, "  ", ""); err == nil {
		t.Error("empty edit accepted")
	}
	events, err := s.ListEvents("issue", issue.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Action != "comment.updated" || events[0].Before == nil {
		t.Errorf("edit event = %+v", events[0])
	}
	comments, err := s.ListComments(issue.Key)
	if err != nil || len(comments) != 3 {
		t.Fatalf("comments = %+v, %v", comments, err)
	}
}

// S10
func TestRelationsTouchBothIssues(t *testing.T) {
	s := openTestStore(t)
	a := mustCreateIssue(t, s, IssueInput{Title: "A"}, "")
	b := mustCreateIssue(t, s, IssueInput{Title: "B"}, "")
	stale := "2020-01-01T00:00:00.000Z"
	if _, err := s.db.Exec("UPDATE issues SET updated_at = ?", stale); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRelation(a.Key, b.Key, "blocks", "pm"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{a.Key, b.Key} {
		got, err := s.GetIssue(key)
		if err != nil {
			t.Fatal(err)
		}
		if got.UpdatedAt == stale {
			t.Errorf("%s updated_at untouched by a relation", key)
		}
	}
	if _, err := s.db.Exec("UPDATE issues SET updated_at = ?", stale); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRelation(a.Key, b.Key, "blocks", "pm"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{a.Key, b.Key} {
		got, err := s.GetIssue(key)
		if err != nil {
			t.Fatal(err)
		}
		if got.UpdatedAt == stale {
			t.Errorf("%s updated_at untouched by a relation removal", key)
		}
	}
}

// S11
func TestListIssuesFilterMatrix(t *testing.T) {
	s := openTestStore(t)
	mustLabel(t, s, "ready", "seo")
	if _, err := s.CreateProject(ProjectInput{Name: "Rebuild"}, ""); err != nil {
		t.Fatal(err)
	}
	one := mustCreateIssue(t, s, IssueInput{Title: "Fix header", Status: "Todo", Project: "rebuild", Priority: 3, Labels: []string{"ready"}}, "")
	two := mustCreateIssue(t, s, IssueInput{Title: "50% off banner", Status: "Todo", Priority: 1, Labels: []string{"seo"}}, "")
	three := mustCreateIssue(t, s, IssueInput{Title: "Audit", Status: "In Progress"}, "")
	if _, _, err := s.AddComment(three.Key, CommentInput{Body: "found a redirect chain"}, "seo"); err != nil {
		t.Fatal(err)
	}
	done := "Done"
	if _, err := s.UpdateIssue(two.Key, IssuePatch{Status: &done}, ""); err != nil {
		t.Fatal(err)
	}
	archived := true
	if _, err := s.UpdateIssue(one.Key, IssuePatch{Archived: &archived}, ""); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		f    IssueFilter
		want []string
	}{
		{"default hides archived", IssueFilter{}, []string{two.Key, three.Key}},
		{"archived included", IssueFilter{Archived: "true"}, []string{one.Key, two.Key, three.Key}},
		{"archived only", IssueFilter{Archived: "only"}, []string{one.Key}},
		{"exclude label", IssueFilter{ExcludeLabels: []string{"seo"}}, []string{three.Key}},
		{"comment body search", IssueFilter{Query: "redirect chain"}, []string{three.Key}},
		{"literal percent", IssueFilter{Query: "%"}, []string{two.Key}},
		{"completed since", IssueFilter{CompletedSince: "2000-01-01T00:00:00.000Z"}, []string{two.Key}},
		{"two status types", IssueFilter{StatusTypes: []string{"started", "completed"}}, []string{two.Key, three.Key}},
	}
	for _, tc := range cases {
		got, err := s.ListIssues(tc.f)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		keys := map[string]bool{}
		for _, issue := range got {
			keys[issue.Key] = true
			if issue.Labels == nil {
				t.Errorf("%s: %s has nil labels", tc.name, issue.Key)
			}
		}
		if len(keys) != len(tc.want) {
			t.Errorf("%s: got %d issues %v, want %v", tc.name, len(got), keys, tc.want)
			continue
		}
		for _, want := range tc.want {
			if !keys[want] {
				t.Errorf("%s: %s missing from %v", tc.name, want, keys)
			}
		}
	}

	byPriority, err := s.ListIssues(IssueFilter{OrderBy: "priority", Archived: "true"})
	if err != nil {
		t.Fatal(err)
	}
	// Urgent first, then Medium, then "no priority" last.
	wantOrder := []string{two.Key, one.Key, three.Key}
	for i, key := range wantOrder {
		if byPriority[i].Key != key {
			t.Errorf("priority order = %s at %d, want %s", byPriority[i].Key, i, key)
		}
	}
	byCreated, err := s.ListIssues(IssueFilter{OrderBy: "created", Archived: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if byCreated[0].Key != one.Key {
		t.Errorf("created order starts at %s, want %s", byCreated[0].Key, one.Key)
	}
	if _, err := s.ListIssues(IssueFilter{OrderBy: "sideways"}); err == nil {
		t.Error("unknown order_by accepted")
	}
	if _, err := s.ListIssues(IssueFilter{Archived: "maybe"}); err == nil {
		t.Error("unknown archived value accepted")
	}
}

// S11: the page limit is capped, and one query loads every issue's labels.
func TestListIssuesLimits(t *testing.T) {
	s := openTestStore(t)
	mustLabel(t, s, "bulk")
	for range 12 {
		mustCreateIssue(t, s, IssueInput{Title: "bulk", Labels: []string{"bulk"}}, "")
	}
	page, err := s.ListIssues(IssueFilter{Limit: 5})
	if err != nil || len(page) != 5 {
		t.Fatalf("limit 5 = %d issues, %v", len(page), err)
	}
	for _, issue := range page {
		if len(issue.Labels) != 1 || issue.Labels[0] != "bulk" {
			t.Fatalf("labels = %v", issue.Labels)
		}
	}
	offset, err := s.ListIssues(IssueFilter{Limit: 5, Offset: 10})
	if err != nil || len(offset) != 2 {
		t.Fatalf("offset page = %d issues, %v", len(offset), err)
	}
	capped, err := s.ListIssues(IssueFilter{Limit: 5000})
	if err != nil || len(capped) != 12 {
		t.Fatalf("oversized limit = %d issues, %v", len(capped), err)
	}
	empty, err := s.ListIssues(IssueFilter{Query: "nothing matches this"})
	if err != nil {
		t.Fatal(err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("empty page = %v, want a non-nil empty slice", empty)
	}
}

func TestCreateIssueStampsPhaseTimestamps(t *testing.T) {
	s := openTestStore(t)
	done, _, err := s.CreateIssue(IssueInput{Title: "born done", Status: "Done"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if done.CompletedAt == "" || done.StartedAt != "" || done.CanceledAt != "" {
		t.Fatalf("created into Done: completed=%q started=%q canceled=%q", done.CompletedAt, done.StartedAt, done.CanceledAt)
	}
	active, _, err := s.CreateIssue(IssueInput{Title: "born active", Status: "In Progress"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if active.StartedAt == "" || active.CompletedAt != "" {
		t.Fatalf("created into In Progress: started=%q completed=%q", active.StartedAt, active.CompletedAt)
	}
}
