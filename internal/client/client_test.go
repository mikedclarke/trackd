package client

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The tests speak the REST contract by hand rather than running the real
// server, so the client is tested against the contract it is written to and
// the two packages can be built in either order.

// fast strips the backoff so a test that only proves the retry count does not
// pay for it. Tests about the backoff itself leave the delays alone.
func fast(c *Client) *Client {
	c.delays = []time.Duration{0, 0, 0}
	return c
}

func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return New(ts.URL+"/", "td_test") // the trailing slash must be tolerated
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("writing response: %v", err)
	}
}

func TestAuthHeaderAndListEnvelope(t *testing.T) {
	var gotAuth, gotQuery string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		writeJSON(t, w, 200, map[string]any{
			"issues":      []map[string]any{{"key": "TSK-1", "title": "one", "version": 3}},
			"next_offset": 100,
		})
	})
	issues, next, err := c.ListIssues(IssueQuery{
		Statuses: []string{"Todo", "In Progress"},
		Labels:   []string{"ready"},
		Archived: "only",
		OrderBy:  "priority",
		Limit:    100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer td_test" {
		t.Errorf("auth header = %q", gotAuth)
	}
	q, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatal(err)
	}
	if got := q["status"]; len(got) != 2 || got[0] != "Todo" || got[1] != "In Progress" {
		t.Errorf("status params = %v, want both statuses", got)
	}
	for key, want := range map[string]string{"label": "ready", "archived": "only", "order_by": "priority", "limit": "100"} {
		if q.Get(key) != want {
			t.Errorf("%s = %q, want %q", key, q.Get(key), want)
		}
	}
	if len(issues) != 1 || issues[0].Key != "TSK-1" || issues[0].Version != 3 {
		t.Fatalf("issues = %+v", issues)
	}
	if next == nil || *next != 100 {
		t.Errorf("next_offset = %v, want 100", next)
	}
}

func TestListEnvelopeEndOfResults(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 200, map[string]any{"issues": []any{}, "next_offset": nil})
	})
	issues, next, err := c.ListIssues(IssueQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 || next != nil {
		t.Errorf("issues = %v, next = %v, want empty and nil", issues, next)
	}
}

func TestEveryListIsUnwrapped(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/issues/TSK-1/comments":
			writeJSON(t, w, 200, map[string]any{"comments": []map[string]any{{"id": 7, "body": "hi", "parent_id": 3}}})
		case "/api/v1/issues/TSK-1/relations":
			writeJSON(t, w, 200, map[string]any{"relations": []map[string]any{{"issue_key": "TSK-1", "related_key": "TSK-2", "type": "blocks"}}})
		case "/api/v1/issues/TSK-1/events":
			writeJSON(t, w, 200, map[string]any{"events": []map[string]any{{"id": 4, "action": "issue.created"}}})
		case "/api/v1/projects":
			writeJSON(t, w, 200, map[string]any{"projects": []map[string]any{{"slug": "cutover", "status": "started"}}})
		case "/api/v1/milestones":
			writeJSON(t, w, 200, map[string]any{"milestones": []map[string]any{{"id": 2, "name": "beta"}}})
		case "/api/v1/labels":
			writeJSON(t, w, 200, map[string]any{"labels": []map[string]any{{"id": 1, "name": "ready"}}})
		case "/api/v1/statuses":
			writeJSON(t, w, 200, map[string]any{"statuses": []map[string]any{{"id": 3, "name": "Todo", "type": "unstarted"}}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})
	comments, err := c.ListComments("TSK-1")
	if err != nil || len(comments) != 1 || comments[0].ParentID != 3 {
		t.Errorf("comments = %+v, %v", comments, err)
	}
	relations, err := c.ListRelations("TSK-1")
	if err != nil || len(relations) != 1 || relations[0].Type != "blocks" {
		t.Errorf("relations = %+v, %v", relations, err)
	}
	events, err := c.ListIssueEvents("TSK-1", 0)
	if err != nil || len(events) != 1 || events[0].Action != "issue.created" {
		t.Errorf("events = %+v, %v", events, err)
	}
	projects, err := c.ListProjects(false)
	if err != nil || len(projects) != 1 || projects[0].Status != "started" {
		t.Errorf("projects = %+v, %v", projects, err)
	}
	milestones, err := c.ListMilestones("", false)
	if err != nil || len(milestones) != 1 || milestones[0].Name != "beta" {
		t.Errorf("milestones = %+v, %v", milestones, err)
	}
	labels, err := c.ListLabels()
	if err != nil || len(labels) != 1 || labels[0].Name != "ready" {
		t.Errorf("labels = %+v, %v", labels, err)
	}
	statuses, err := c.ListStatuses()
	if err != nil || len(statuses) != 1 || statuses[0].Type != "unstarted" {
		t.Errorf("statuses = %+v, %v", statuses, err)
	}
}

func TestEventFeedCursor(t *testing.T) {
	var gotQuery url.Values
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		writeJSON(t, w, 200, map[string]any{
			"events":        []map[string]any{{"id": 11, "entity": "issue", "entity_key": "TSK-1", "action": "issue.updated"}},
			"next_after_id": 11,
		})
	})
	events, next, err := c.ListEvents(EventQuery{Since: "2026-09-01T00:00:00.000Z", AfterID: 4, Entity: "issue", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"since": "2026-09-01T00:00:00.000Z", "after_id": "4", "entity": "issue", "limit": "50",
	} {
		if gotQuery.Get(key) != want {
			t.Errorf("%s = %q, want %q", key, gotQuery.Get(key), want)
		}
	}
	if len(events) != 1 || events[0].EntityKey != "TSK-1" {
		t.Fatalf("events = %+v", events)
	}
	if next == nil || *next != 11 {
		t.Errorf("next_after_id = %v, want 11", next)
	}
}

func TestErrorBodyCarriesCode(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		contentTyp string
		wantCode   string
		wantMsg    string
	}{
		{"not found", 404, `{"error":"issue TSK-9 not found","code":"not_found"}`, "application/json", "not_found", "issue TSK-9 not found"},
		{"conflict", 409, `{"error":"description replace refused: use append or replace_description","code":"description_replace"}`, "application/json", "description_replace", "description replace refused: use append or replace_description"},
		{"invalid ref", 422, `{"error":"unknown label \"nope\"","code":"invalid_ref"}`, "application/json", "invalid_ref", `unknown label "nope"`},
		{"non-JSON body", 502, "<html>bad gateway</html>", "text/html", "internal", "502 Bad Gateway"},
		{"empty body", 500, "", "application/json", "internal", "500 Internal Server Error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := serve(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentTyp)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := fast(c).GetIssue("TSK-9")
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want *APIError", err)
			}
			if apiErr.Status != tc.status || apiErr.Code != tc.wantCode || apiErr.Message != tc.wantMsg {
				t.Errorf("got %+v, want status %d code %q message %q", apiErr, tc.status, tc.wantCode, tc.wantMsg)
			}
		})
	}
}

func TestRetriesServiceUnavailableThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			writeJSON(t, w, 503, map[string]any{"error": "database is busy", "code": "busy"})
			return
		}
		writeJSON(t, w, 200, map[string]any{"key": "TSK-1"})
	})
	start := time.Now()
	issue, err := c.GetIssue("TSK-1")
	if err != nil {
		t.Fatal(err)
	}
	if issue.Key != "TSK-1" {
		t.Errorf("issue = %+v", issue)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Errorf("elapsed = %v, want at least the first 250ms backoff", elapsed)
	}
}

func TestGivesUpAfterFourAttempts(t *testing.T) {
	var calls atomic.Int32
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(t, w, 503, map[string]any{"error": "database is busy", "code": "busy"})
	})
	_, err := fast(c).GetIssue("TSK-1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 503 || apiErr.Code != "busy" {
		t.Fatalf("err = %v, want a 503 busy APIError", err)
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("attempts = %d, want the first plus three retries", got)
	}
}

func TestCreateWithoutIdempotencyKeyIsNeverRetried(t *testing.T) {
	var calls atomic.Int32
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(t, w, 503, map[string]any{"error": "database is busy", "code": "busy"})
	})
	// A repeat could create a second issue, so one attempt is all it gets.
	if _, err := fast(c).CreateIssue(IssueCreate{Title: "no key"}); err == nil {
		t.Fatal("want an error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestCreateWithIdempotencyKeyIsRetried(t *testing.T) {
	var calls atomic.Int32
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeJSON(t, w, 503, map[string]any{"error": "database is busy", "code": "busy"})
			return
		}
		body, _ := io.ReadAll(r.Body)
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("body: %v", err)
		}
		if got["idempotency_key"] != "abc123" {
			t.Errorf("idempotency_key = %v", got["idempotency_key"])
		}
		writeJSON(t, w, 200, map[string]any{"key": "TSK-2"})
	})
	issue, err := fast(c).CreateIssue(IssueCreate{Title: "keyed", IdempotencyKey: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if issue.Key != "TSK-2" {
		t.Errorf("issue = %+v", issue)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

func TestAppendIsNeverRetried(t *testing.T) {
	var calls atomic.Int32
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(t, w, 503, map[string]any{"error": "database is busy", "code": "busy"})
	})
	// Repeating an append would write the text twice.
	if _, err := fast(c).AppendDescription("TSK-1", "more", "pm"); err == nil {
		t.Fatal("want an error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestUnreachableServerIsATransportError(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	addr := ts.URL
	ts.Close() // nothing is listening now
	c := fast(New(addr, "td_test"))
	_, err := c.GetIssue("TSK-1")
	var transport *TransportError
	if !errors.As(err, &transport) {
		t.Fatalf("err = %v, want *TransportError", err)
	}
	if !strings.Contains(transport.Op, "GET ") {
		t.Errorf("op = %q", transport.Op)
	}
}

func TestIssuePatchSendsContractFieldNames(t *testing.T) {
	var got map[string]any
	var method string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("body: %v", err)
		}
		writeJSON(t, w, 200, map[string]any{"key": "TSK-1", "version": 4})
	})
	version := int64(3)
	empty := ""
	labels := []string{"a", "b"}
	issue, err := c.UpdateIssue("TSK-1", IssuePatch{
		Description:        &empty,
		ReplaceDescription: true,
		Project:            &empty,
		Labels:             &labels,
		ExpectedVersion:    &version,
		Actor:              "pm",
	})
	if err != nil {
		t.Fatal(err)
	}
	if method != "PATCH" {
		t.Errorf("method = %s", method)
	}
	if issue.Version != 4 {
		t.Errorf("version = %d", issue.Version)
	}
	if got["replace_description"] != true {
		t.Errorf("replace_description = %v", got["replace_description"])
	}
	if got["expected_version"] != float64(3) {
		t.Errorf("expected_version = %v", got["expected_version"])
	}
	if v, ok := got["description"]; !ok || v != "" {
		t.Errorf("description = %v, present = %v; a clear must send the empty string", v, ok)
	}
	if v, ok := got["project"]; !ok || v != "" {
		t.Errorf("project = %v, present = %v", v, ok)
	}
	if got["actor"] != "pm" {
		t.Errorf("actor = %v", got["actor"])
	}
	if _, ok := got["title"]; ok {
		t.Error("title was sent although it was never set")
	}
}

func TestLabelAddRemoveAndAppendBodies(t *testing.T) {
	var path, method string
	var got map[string]any
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("body: %v", err)
		}
		writeJSON(t, w, 200, map[string]any{"key": "TSK-1"})
	})
	if _, err := c.UpdateIssue("TSK-1", IssuePatch{AddLabels: []string{"ready"}, RemoveLabels: []string{"blocked"}}); err != nil {
		t.Fatal(err)
	}
	if fmtAny(got["add_labels"]) != "[ready]" || fmtAny(got["remove_labels"]) != "[blocked]" {
		t.Errorf("label ops = %v / %v", got["add_labels"], got["remove_labels"])
	}
	if _, ok := got["labels"]; ok {
		t.Error("labels replace was sent alongside the add and remove lists")
	}

	if _, err := c.AppendDescription("TSK-1", "another line", "pm"); err != nil {
		t.Fatal(err)
	}
	if path != "/api/v1/issues/TSK-1/description" || method != "POST" {
		t.Errorf("append went to %s %s", method, path)
	}
	if got["append"] != "another line" || got["actor"] != "pm" {
		t.Errorf("append body = %v", got)
	}
}

func TestCommentThreadAndEdit(t *testing.T) {
	var path, method string
	var got map[string]any
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("body: %v", err)
		}
		writeJSON(t, w, 200, map[string]any{"id": 12, "issue_key": "TSK-1", "body": "text"})
	})
	comment, err := c.AddComment("TSK-1", CommentCreate{Body: "a reply", ParentID: 7, Actor: "pm", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if comment.ID != 12 {
		t.Errorf("comment = %+v", comment)
	}
	if got["parent_id"] != float64(7) || got["idempotency_key"] != "k1" {
		t.Errorf("comment body = %v", got)
	}

	if _, err := c.UpdateComment(12, "edited", "pm"); err != nil {
		t.Fatal(err)
	}
	if path != "/api/v1/comments/12" || method != "PATCH" {
		t.Errorf("edit went to %s %s", method, path)
	}
	if got["body"] != "edited" || got["actor"] != "pm" {
		t.Errorf("edit body = %v", got)
	}
}

func TestProjectDatesAndLabelOps(t *testing.T) {
	var got map[string]any
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("body: %v", err)
		}
		writeJSON(t, w, 200, map[string]any{"slug": "cutover", "status": "started", "start_date": "2026-09-01"})
	})
	project, err := c.CreateProject(ProjectCreate{
		Name: "Cutover", Status: "started", StartDate: "2026-09-01", TargetDate: "2026-10-01", Labels: []string{"infra"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if project.StartDate != "2026-09-01" {
		t.Errorf("project = %+v", project)
	}
	if got["start_date"] != "2026-09-01" || got["target_date"] != "2026-10-01" {
		t.Errorf("create body = %v", got)
	}

	target := "2026-11-01"
	if _, err := c.UpdateProject("cutover", ProjectPatch{TargetDate: &target, AddLabels: []string{"infra"}, RemoveLabels: []string{"old"}}); err != nil {
		t.Fatal(err)
	}
	if got["target_date"] != "2026-11-01" || fmtAny(got["add_labels"]) != "[infra]" || fmtAny(got["remove_labels"]) != "[old]" {
		t.Errorf("patch body = %v", got)
	}
}

func TestHealthReturnsTheReportEvenWhenDegraded(t *testing.T) {
	var calls atomic.Int32
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/healthz" {
			t.Errorf("path = %s", r.URL.Path)
		}
		writeJSON(t, w, 503, map[string]any{"status": "degraded", "schema": 3})
	})
	health, status, err := c.Health()
	if err != nil {
		t.Fatal(err)
	}
	if health["status"] != "degraded" || status != 503 {
		t.Errorf("health = %v, status = %d", health, status)
	}
	// A health check that retried a degraded answer would take four seconds
	// to tell a monitor what it already knew.
	if got := calls.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func fmtAny(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	s := string(b)
	s = strings.ReplaceAll(s, `"`, "")
	return s
}
