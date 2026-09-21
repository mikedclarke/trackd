package server

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/mikedclarke/trackd/internal/store"
)

// signIn logs the cookie-jar client in as the environment's token.
func signIn(t *testing.T, ts string, client *http.Client, token string) {
	t.Helper()
	resp, err := client.PostForm(ts+"/ui/login", url.Values{"token": {token}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func post(t *testing.T, client *http.Client, target string, form url.Values) (int, string) {
	t.Helper()
	noFollow := &http.Client{Jar: client.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.PostForm(target, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

func seedView(t *testing.T, st *store.Store) *store.View {
	t.Helper()
	for _, name := range []string{"waiting", "ready"} {
		if _, err := st.EnsureLabel(name, ""); err != nil {
			t.Fatal(err)
		}
	}
	view, err := st.CreateView(store.ViewInput{
		Name:        "Waiting on me",
		Description: "parked on a person",
		Filter:      store.ViewFilter{Statuses: []string{"Todo", "In Progress"}, Labels: []string{"waiting"}, OrderBy: "created"},
		QuickActions: []store.QuickAction{
			{Name: "Answered", RemoveLabels: []string{"waiting"}},
			{Name: "Ship", Status: "Done"},
		},
	}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func TestUIViewPageAndTabs(t *testing.T) {
	ts, client, token, st := newUIEnv(t)
	seedView(t, st)
	parked, _, err := st.CreateIssue(store.IssueInput{Title: "Parked <b>row</b>", Status: "Todo", Labels: []string{"waiting"}, Priority: 2}, "seo")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateIssue(store.IssueInput{Title: "Free row", Status: "Todo"}, "seo"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddComment(parked.Key, store.CommentInput{Body: "first"}, "seo"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddComment(parked.Key, store.CommentInput{Body: "Can you sign this <i>off</i>?"}, "seo"); err != nil {
		t.Fatal(err)
	}
	signIn(t, ts.URL, client, token)

	// The board carries the view as a tab.
	code, body := fetch(t, client, ts.URL+"/")
	if code != http.StatusOK || !strings.Contains(body, `href="/ui/view/Waiting%20on%20me"`) || !strings.Contains(body, "+ view") {
		t.Fatalf("board tabs missing: %d %q", code, body)
	}

	code, body = fetch(t, client, ts.URL+"/ui/view/Waiting%20on%20me")
	if code != http.StatusOK {
		t.Fatalf("view page = %d", code)
	}
	for _, want := range []string{
		"Waiting on me", "parked on a person", "status: Todo, In Progress", "label: waiting",
		parked.Key, "P2", "Can you sign this", "latest comment", "Answered", "Ship",
		`name="expected_version" value="` + strconv.FormatInt(parked.Version, 10) + `"`,
		"edit view", "1 issues",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("view page missing %q", want)
		}
	}
	if strings.Contains(body, "Free row") {
		t.Error("view page shows an issue outside the filter")
	}
	if strings.Contains(body, "<b>row</b>") || strings.Contains(body, "<i>off</i>") || strings.Contains(body, "first") {
		t.Error("view page leaked markup or showed an older comment")
	}
	// The latest comment is the newest, not the first.
	code, _ = fetch(t, client, ts.URL+"/ui/view/no-such-view")
	if code != http.StatusNotFound {
		t.Errorf("unknown view = %d, want 404", code)
	}
}

func TestUIRowActions(t *testing.T) {
	ts, client, token, st := newUIEnv(t)
	view := seedView(t, st)
	issue, _, err := st.CreateIssue(store.IssueInput{Title: "Parked", Status: "Todo", Labels: []string{"waiting"}}, "seo")
	if err != nil {
		t.Fatal(err)
	}
	signIn(t, ts.URL, client, token)
	action := ts.URL + "/ui/issue/" + issue.Key + "/action"
	back := "/ui/view/Waiting%20on%20me"
	version := func() string {
		t.Helper()
		current, err := st.GetIssue(issue.Key)
		if err != nil {
			t.Fatal(err)
		}
		return strconv.FormatInt(current.Version, 10)
	}

	// A reply with a quick action: the comment lands as the signed-in token,
	// the label goes, and the redirect carries a notice back to the view.
	code, location := post(t, client, action, url.Values{
		"back": {back + "#" + issue.Key}, "view": {view.Name}, "expected_version": {version()},
		"body": {"Signed off.\r\nGo."}, "do": {"quick:0"},
	})
	if code != http.StatusSeeOther || !strings.HasPrefix(location, back) || !strings.Contains(location, "notice=saved") {
		t.Fatalf("quick action = %d -> %q", code, location)
	}
	after, err := st.GetIssue(issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Labels) != 0 || after.Version != issue.Version+1 {
		t.Errorf("after quick action = labels %v version %d", after.Labels, after.Version)
	}
	comments, err := st.ListComments(issue.Key)
	if err != nil || len(comments) != 1 || comments[0].Body != "Signed off.\nGo." || comments[0].Actor != "pm" {
		t.Fatalf("comments = %+v, %v", comments, err)
	}
	events, err := st.ListEvents("issue", issue.ID, 10)
	if err != nil || events[0].Action != "issue.updated" || events[0].Actor != "pm" || events[1].Action != "comment.created" {
		t.Errorf("events = %+v, %v", events, err)
	}
	// The notice renders on the page it points at.
	code, body := fetch(t, client, ts.URL+location)
	if code != http.StatusOK || !strings.Contains(body, issue.Key+" updated.") {
		t.Errorf("notice not rendered: %d", code)
	}

	// Explicit fields: status, priority, add and remove label, on one post.
	if _, err := st.UpdateIssue(issue.Key, store.IssuePatch{AddLabels: []string{"waiting"}}, "seo"); err != nil {
		t.Fatal(err)
	}
	code, location = post(t, client, action, url.Values{
		"back": {back}, "expected_version": {version()},
		"status": {"In Progress"}, "priority": {"1"}, "add_label": {"ready"}, "remove_label": {"waiting"}, "do": {"apply"},
	})
	if code != http.StatusSeeOther || !strings.Contains(location, "notice=saved") {
		t.Fatalf("apply = %d -> %q", code, location)
	}
	after, _ = st.GetIssue(issue.Key)
	if after.Status != "In Progress" || after.Priority != 1 || strings.Join(after.Labels, ",") != "ready" {
		t.Errorf("after apply = %+v", after)
	}

	// A stale version is refused with a conflict notice; the reply typed with
	// it is still saved.
	stale := strconv.FormatInt(after.Version-1, 10)
	code, location = post(t, client, action, url.Values{
		"back": {back}, "expected_version": {stale}, "body": {"late reply"}, "status": {"Done"}, "do": {"apply"},
	})
	if code != http.StatusSeeOther || !strings.Contains(location, "notice=conflict") {
		t.Fatalf("stale apply = %d -> %q", code, location)
	}
	after, _ = st.GetIssue(issue.Key)
	if after.Status != "In Progress" {
		t.Errorf("stale apply changed status to %s", after.Status)
	}
	if comments, _ = st.ListComments(issue.Key); len(comments) != 2 || comments[1].Body != "late reply" {
		t.Errorf("reply lost on conflict: %+v", comments)
	}
	code, body = fetch(t, client, ts.URL+location)
	if code != http.StatusOK || !strings.Contains(body, "changed since this page was loaded") {
		t.Errorf("conflict notice not rendered: %d", code)
	}

	// No version at all: nothing is applied, and the error rides back.
	code, location = post(t, client, action, url.Values{"back": {back}, "status": {"Done"}})
	if code != http.StatusSeeOther || !strings.Contains(location, "notice=error") {
		t.Errorf("versionless apply = %d -> %q", code, location)
	}
	// An empty post is a plain return with no notice and no event.
	code, location = post(t, client, action, url.Values{"back": {back}, "expected_version": {version()}, "do": {"reply"}})
	if code != http.StatusSeeOther || location != back {
		t.Errorf("empty post = %d -> %q", code, location)
	}
	// A quick action index that no longer exists is refused, not applied.
	code, location = post(t, client, action, url.Values{"back": {back}, "view": {view.Name}, "expected_version": {version()}, "do": {"quick:9"}})
	if !strings.Contains(location, "notice=error") {
		t.Errorf("bad quick index = %d -> %q", code, location)
	}
	// The return path is same-site only.
	_, location = post(t, client, action, url.Values{"back": {"https://evil.example/"}, "expected_version": {version()}, "body": {"x"}})
	if !strings.HasPrefix(location, "/ui/issue/"+issue.Key) {
		t.Errorf("open redirect: %q", location)
	}
	// Cross-origin posts are refused, like comments.
	req, _ := http.NewRequest("POST", action, strings.NewReader(url.Values{"expected_version": {version()}, "status": {"Done"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin action = %d", resp.StatusCode)
	}
	// And a signed-out browser is sent to the login page.
	bare := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = bare.PostForm(action, url.Values{"expected_version": {version()}, "status": {"Done"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "/ui/login") {
		t.Errorf("unauthenticated action = %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestUIIssuePageActions(t *testing.T) {
	ts, client, token, st := newUIEnv(t)
	if _, err := st.EnsureLabel("ready", ""); err != nil {
		t.Fatal(err)
	}
	issue, _, err := st.CreateIssue(store.IssueInput{Title: "Target", Status: "Todo"}, "seo")
	if err != nil {
		t.Fatal(err)
	}
	signIn(t, ts.URL, client, token)
	code, body := fetch(t, client, ts.URL+"/ui/issue/"+issue.Key)
	if code != http.StatusOK || !strings.Contains(body, `action="/ui/issue/`+issue.Key+`/action"`) || !strings.Contains(body, "Todo (current)") {
		t.Fatalf("issue page action box missing: %d", code)
	}
	code, location := post(t, client, ts.URL+"/ui/issue/"+issue.Key+"/action", url.Values{
		"expected_version": {strconv.FormatInt(issue.Version, 10)}, "status": {"Done"}, "add_label": {"ready"},
	})
	if code != http.StatusSeeOther || !strings.HasPrefix(location, "/ui/issue/"+issue.Key) || !strings.Contains(location, "notice=saved") {
		t.Fatalf("issue page action = %d -> %q", code, location)
	}
	after, _ := st.GetIssue(issue.Key)
	if after.Status != "Done" || strings.Join(after.Labels, ",") != "ready" {
		t.Errorf("after = %+v", after)
	}
}

func TestUIViewEditor(t *testing.T) {
	ts, client, token, st := newUIEnv(t)
	for _, name := range []string{"waiting", "ready"} {
		if _, err := st.EnsureLabel(name, ""); err != nil {
			t.Fatal(err)
		}
	}
	signIn(t, ts.URL, client, token)
	code, body := fetch(t, client, ts.URL+"/ui/views/new")
	if code != http.StatusOK || !strings.Contains(body, `name="quick_name_3"`) || !strings.Contains(body, "waiting") {
		t.Fatalf("new view form = %d", code)
	}

	// A bad submission re-renders the form with the error and the values.
	code, location := post(t, client, ts.URL+"/ui/views", url.Values{"name": {"Court"}, "shared": {"1"}})
	if code != http.StatusUnprocessableEntity || location != "" {
		t.Fatalf("empty filter submit = %d -> %q", code, location)
	}

	code, location = post(t, client, ts.URL+"/ui/views", url.Values{
		"name": {"Court"}, "description": {"needs an answer"}, "shared": {"1"},
		"status": {"Todo", "In Review"}, "label": {"waiting"}, "priority": {"1", "2"}, "updated_within": {"7d"}, "order_by": {"created"},
		"quick_name_0": {"Answered"}, "quick_remove_0": {"waiting"},
		"quick_name_1": {"Ship"}, "quick_status_1": {"Done"}, "quick_priority_1": {"4"}, "quick_add_1": {"ready"},
	})
	if code != http.StatusSeeOther || location != "/ui/view/Court" {
		t.Fatalf("create view = %d -> %q", code, location)
	}
	view, err := st.GetView("Court")
	if err != nil {
		t.Fatal(err)
	}
	if view.Owner != "pm" || !view.Shared || len(view.Filter.Statuses) != 2 || len(view.Filter.Priorities) != 2 ||
		view.Filter.UpdatedWithin != "7d" || len(view.QuickActions) != 2 || view.QuickActions[1].Priority == nil || *view.QuickActions[1].Priority != 4 {
		t.Fatalf("created view = %+v", view)
	}

	// Edit: the form carries the stored values; a save replaces them.
	code, body = fetch(t, client, ts.URL+"/ui/view/Court/edit")
	if code != http.StatusOK || !strings.Contains(body, `value="Court"`) || !strings.Contains(body, `value="Answered"`) || !strings.Contains(body, "Archive this view") {
		t.Fatalf("edit form = %d", code)
	}
	if !strings.Contains(body, `name="status" value="Todo" checked`) {
		t.Error("edit form does not pre-tick the stored status")
	}
	code, location = post(t, client, ts.URL+"/ui/view/Court", url.Values{
		"name": {"Court of appeal"}, "status": {"Todo"}, "quick_name_0": {"Answered"}, "quick_remove_0": {"waiting"},
	})
	if code != http.StatusSeeOther || location != "/ui/view/Court%20of%20appeal" {
		t.Fatalf("edit save = %d -> %q", code, location)
	}
	view, _ = st.GetView("Court of appeal")
	if view.Shared || len(view.Filter.Statuses) != 1 || len(view.Filter.Labels) != 0 || len(view.QuickActions) != 1 {
		t.Errorf("edited view = %+v", view)
	}

	// Another agent cannot edit it, and cannot see it now it is private.
	other, err := st.CreateToken("seo", "agent")
	if err != nil {
		t.Fatal(err)
	}
	ts2, client2, _, _ := newUIEnvOn(t, st)
	signIn(t, ts2.URL, client2, other)
	if code, _ := fetch(t, client2, ts2.URL+"/ui/view/Court%20of%20appeal"); code != http.StatusNotFound {
		t.Errorf("private view visible to another token: %d", code)
	}
	if _, err := st.UpdateView("Court of appeal", store.ViewPatch{Shared: boolp(true)}, "pm"); err != nil {
		t.Fatal(err)
	}
	if code, _ := fetch(t, client2, ts2.URL+"/ui/view/Court%20of%20appeal/edit"); code != http.StatusForbidden {
		t.Errorf("edit form open to a non-owner: %d", code)
	}
	if code, _ := post(t, client2, ts2.URL+"/ui/view/Court%20of%20appeal", url.Values{"do": {"archive"}}); code != http.StatusForbidden {
		t.Errorf("archive by a non-owner: %d", code)
	}

	// The owner archives from the editor; the tab goes and the page is gone.
	code, location = post(t, client, ts.URL+"/ui/view/Court%20of%20appeal", url.Values{"do": {"archive"}})
	if code != http.StatusSeeOther || location != "/" {
		t.Fatalf("archive = %d -> %q", code, location)
	}
	if code, _ := fetch(t, client, ts.URL+"/ui/view/Court%20of%20appeal"); code != http.StatusNotFound {
		t.Errorf("archived view still served: %d", code)
	}
	_, body = fetch(t, client, ts.URL+"/")
	if strings.Contains(body, "Court%20of%20appeal") {
		t.Error("archived view still a tab")
	}
}

// newUIEnvOn opens a second server and cookie jar over an existing store, for
// a test that needs two signed-in identities.
func newUIEnvOn(t *testing.T, st *store.Store) (*httptest.Server, *http.Client, string, *store.Store) {
	t.Helper()
	ts := httptest.NewServer(New(st, "test").Handler())
	t.Cleanup(ts.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return ts, &http.Client{Jar: jar}, "", st
}

func boolp(b bool) *bool { return &b }
