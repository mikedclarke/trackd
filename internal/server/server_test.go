package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/mikedclarke/trackd/internal/store"
)

type testEnv struct {
	t      *testing.T
	url    string
	token  string
	client *http.Client
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	token, err := st.CreateToken("pm", "agent")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(st, "test").Handler())
	t.Cleanup(ts.Close)
	return &testEnv{t: t, url: ts.URL, token: token, client: ts.Client()}
}

func (e *testEnv) do(method, path string, body any, out any) *http.Response {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.url+path, reader)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			e.t.Fatalf("%s %s: decode response: %v", method, path, err)
		}
	}
	return resp
}

func (e *testEnv) expect(method, path string, body any, wantCode int, out any) {
	e.t.Helper()
	resp := e.do(method, path, body, out)
	if resp.StatusCode != wantCode {
		e.t.Fatalf("%s %s = %d, want %d", method, path, resp.StatusCode, wantCode)
	}
}

func TestAuth(t *testing.T) {
	e := newTestEnv(t)

	resp, err := e.client.Get(e.url + "/api/v1/issues")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", e.url+"/api/v1/issues", nil)
	req.Header.Set("Authorization", "Bearer td_bogus")
	resp, err = e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token = %d, want 401", resp.StatusCode)
	}

	resp, err = e.client.Get(e.url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d, want 200 without auth", resp.StatusCode)
	}
	var health map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if health["status"] != "ok" {
		t.Errorf("health status = %v", health["status"])
	}
}

func TestIssueEndpoints(t *testing.T) {
	e := newTestEnv(t)

	var issue store.Issue
	e.expect("POST", "/api/v1/issues", map[string]any{
		"title":  "Fix header",
		"status": "Todo",
		"labels": []string{"agent-ready"},
	}, http.StatusCreated, &issue)
	if issue.Key != "TSK-1" || issue.Status != "Todo" {
		t.Fatalf("created issue = %+v", issue)
	}

	var got store.Issue
	e.expect("GET", "/api/v1/issues/TSK-1", nil, http.StatusOK, &got)
	if got.Title != "Fix header" {
		t.Errorf("get = %+v", got)
	}

	e.expect("PATCH", "/api/v1/issues/TSK-1", map[string]any{"status": "In Progress"}, http.StatusOK, &got)
	if got.Status != "In Progress" || got.StartedAt == "" {
		t.Errorf("patched = %+v", got)
	}

	// The authenticating token's name is the default actor on the audit trail.
	var events []store.Event
	e.expect("GET", "/api/v1/issues/TSK-1/events", nil, http.StatusOK, &events)
	if len(events) != 2 || events[0].Actor != "pm" {
		t.Errorf("events = %+v", events)
	}

	// An explicit actor in the body overrides the token name.
	var comment store.Comment
	e.expect("POST", "/api/v1/issues/TSK-1/comments", map[string]any{
		"body":  "done",
		"actor": "seo",
	}, http.StatusCreated, &comment)
	if comment.Actor != "seo" {
		t.Errorf("comment actor = %q, want explicit override", comment.Actor)
	}
	var comments []store.Comment
	e.expect("GET", "/api/v1/issues/TSK-1/comments", nil, http.StatusOK, &comments)
	if len(comments) != 1 {
		t.Errorf("comments = %+v", comments)
	}

	var list []store.Issue
	e.expect("GET", "/api/v1/issues?status=In+Progress&label=agent-ready", nil, http.StatusOK, &list)
	if len(list) != 1 {
		t.Errorf("filtered list = %+v", list)
	}
	e.expect("GET", "/api/v1/issues?q=zzz", nil, http.StatusOK, &list)
	if len(list) != 0 {
		t.Errorf("no-match list = %+v", list)
	}

	e.expect("GET", "/api/v1/issues/TSK-999", nil, http.StatusNotFound, nil)
	e.expect("PATCH", "/api/v1/issues/TSK-1", map[string]any{}, http.StatusBadRequest, nil)
	e.expect("PATCH", "/api/v1/issues/TSK-1", map[string]any{"titel": "typo"}, http.StatusBadRequest, nil)
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "x", "priority": 9}, http.StatusBadRequest, nil)
}

func TestRelationEndpoints(t *testing.T) {
	e := newTestEnv(t)
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "A"}, http.StatusCreated, nil)
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "B"}, http.StatusCreated, nil)

	var relations []store.Relation
	e.expect("POST", "/api/v1/issues/TSK-1/relations", map[string]any{
		"related": "TSK-2",
		"type":    "blocks",
	}, http.StatusOK, &relations)
	if len(relations) != 1 || relations[0].Type != "blocks" {
		t.Fatalf("relations = %+v", relations)
	}
	e.expect("GET", "/api/v1/issues/TSK-2/relations", nil, http.StatusOK, &relations)
	if len(relations) != 1 {
		t.Errorf("reverse listing = %+v", relations)
	}
	e.expect("POST", "/api/v1/issues/TSK-1/relations", map[string]any{
		"related": "TSK-2",
		"type":    "blocks",
		"remove":  true,
	}, http.StatusOK, &relations)
	if len(relations) != 0 {
		t.Errorf("after remove = %+v", relations)
	}
	e.expect("POST", "/api/v1/issues/TSK-1/relations", map[string]any{
		"related": "TSK-2",
		"type":    "nonsense",
	}, http.StatusBadRequest, nil)
}

func TestProjectAndLabelEndpoints(t *testing.T) {
	e := newTestEnv(t)

	var project store.Project
	e.expect("POST", "/api/v1/projects", map[string]any{
		"name":   "Site Rebuild",
		"labels": []string{"client-x"},
	}, http.StatusCreated, &project)
	if project.Slug != "site-rebuild" || len(project.Labels) != 1 {
		t.Fatalf("project = %+v", project)
	}

	e.expect("PATCH", "/api/v1/projects/site-rebuild", map[string]any{
		"status": "paused",
		"labels": []string{"client-x", "priority"},
	}, http.StatusOK, &project)
	if project.Status != "paused" || len(project.Labels) != 2 {
		t.Errorf("patched project = %+v", project)
	}
	e.expect("PATCH", "/api/v1/projects/site-rebuild", map[string]any{}, http.StatusBadRequest, nil)
	e.expect("GET", "/api/v1/projects/missing", nil, http.StatusNotFound, nil)

	var projects []store.Project
	e.expect("GET", "/api/v1/projects", nil, http.StatusOK, &projects)
	if len(projects) != 1 {
		t.Errorf("projects = %+v", projects)
	}

	var label store.Label
	e.expect("POST", "/api/v1/labels", map[string]any{"name": "urgent", "color": "#ff0000"}, http.StatusCreated, &label)
	if label.Color != "#ff0000" {
		t.Errorf("label = %+v", label)
	}
	var labels []store.Label
	e.expect("GET", "/api/v1/labels", nil, http.StatusOK, &labels)
	if len(labels) != 3 {
		t.Errorf("labels = %d, want 3 (client-x, priority, urgent)", len(labels))
	}

	var statuses []store.Status
	e.expect("GET", "/api/v1/statuses", nil, http.StatusOK, &statuses)
	if len(statuses) != 8 {
		t.Errorf("statuses = %d", len(statuses))
	}
}

func TestIssueProjectAssignment(t *testing.T) {
	e := newTestEnv(t)
	e.expect("POST", "/api/v1/projects", map[string]any{"name": "P"}, http.StatusCreated, nil)

	var issue store.Issue
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "in project", "project": "p"}, http.StatusCreated, &issue)
	if issue.Project != "p" {
		t.Fatalf("issue project = %q", issue.Project)
	}
	// Clearing takes an explicit empty string, distinct from omitting the field.
	// Decode into a fresh struct: cleared fields are omitted from the response.
	var cleared store.Issue
	e.expect("PATCH", fmt.Sprintf("/api/v1/issues/%s", issue.Key), map[string]any{"project": ""}, http.StatusOK, &cleared)
	if cleared.Project != "" {
		t.Errorf("project not cleared: %+v", cleared)
	}
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "bad", "project": "missing"}, http.StatusNotFound, nil)
}
