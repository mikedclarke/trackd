package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mikedclarke/trackd/internal/store"
)

type testEnv struct {
	t      *testing.T
	url    string
	token  string
	client *http.Client
	store  *store.Store
	server *Server
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
	srv := New(st, "test")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{t: t, url: ts.URL, token: token, client: ts.Client(), store: st, server: srv}
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

// expectError asserts both halves of the error contract: the status and the
// machine-readable code the CLI switches on.
func (e *testEnv) expectError(method, path string, body any, wantCode int, wantClass string) errorBody {
	e.t.Helper()
	var got errorBody
	resp := e.do(method, path, body, &got)
	if resp.StatusCode != wantCode || got.Code != wantClass {
		e.t.Fatalf("%s %s = %d %q, want %d %q (%s)", method, path, resp.StatusCode, got.Code, wantCode, wantClass, got.Error)
	}
	return got
}

// label creates a label, which every issue and project write needs before it
// may name one.
func (e *testEnv) label(names ...string) {
	e.t.Helper()
	for _, name := range names {
		e.expect("POST", "/api/v1/labels", map[string]any{"name": name}, http.StatusCreated, nil)
	}
}

// adminToken mints an admin identity on demand. It is not part of the default
// environment because minting a token records an event, and the feed tests
// count them.
func (e *testEnv) adminToken() string {
	e.t.Helper()
	token, err := e.store.CreateToken("owner", "admin")
	if err != nil {
		e.t.Fatal(err)
	}
	return token
}

// withToken runs fn as another identity, for the writes a role decides.
func (e *testEnv) withToken(token string, fn func()) {
	e.t.Helper()
	saved := e.token
	e.token = token
	defer func() { e.token = saved }()
	fn()
}

func (e *testEnv) issues(query string) issueListResponse {
	e.t.Helper()
	var list issueListResponse
	e.expect("GET", "/api/v1/issues"+query, nil, http.StatusOK, &list)
	return list
}

func TestAuth(t *testing.T) {
	e := newTestEnv(t)

	resp, err := e.client.Get(e.url + "/api/v1/issues")
	if err != nil {
		t.Fatal(err)
	}
	var body errorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || body.Code != codeUnauthorized {
		t.Errorf("no token = %d %q", resp.StatusCode, body.Code)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q, want Bearer", got)
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

	// /healthz answers without a token; its status is asserted separately.
	resp, err = e.client.Get(e.url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("healthz = %d without auth", resp.StatusCode)
	}
}

func (e *testEnv) health() (int, healthResponse) {
	e.t.Helper()
	resp, err := e.client.Get(e.url + "/healthz")
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	if strings.Contains(string(raw), "path") {
		e.t.Errorf("healthz body leaks a filesystem path: %s", raw)
	}
	var body healthResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		e.t.Fatal(err)
	}
	return resp.StatusCode, body
}

func TestHealthzDegradedWithoutScheduler(t *testing.T) {
	e := newTestEnv(t)
	code, body := e.health()
	if code != http.StatusServiceUnavailable || body.Status != "degraded" {
		t.Errorf("no scheduler = %d %q, want 503 degraded", code, body.Status)
	}
	if body.Schema != schemaVersion || body.Version != "test" {
		t.Errorf("health = %+v", body)
	}
	// The integrity checker runs once as the handler is built.
	if !body.Integrity.OK || body.Integrity.CheckedAt == "" {
		t.Errorf("integrity = %+v", body.Integrity)
	}
	if body.Backup.LastAt != "" || body.Backup.AgeSeconds != nil {
		t.Errorf("backup reported without a scheduler: %+v", body.Backup)
	}
}

func TestHealthzOKWithScheduler(t *testing.T) {
	e := newTestEnv(t)
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go e.server.RunBackups(ctx, BackupConfig{Dir: dir, Every: time.Hour, Keep: 3})

	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body := e.health()
		if code == http.StatusOK && body.Status == "ok" && body.Backup.LastAt != "" {
			if body.Backup.AgeSeconds == nil || body.Backup.Error != "" {
				t.Errorf("backup block = %+v", body.Backup)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("health never went ok: %d %+v", code, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHealthzDegradedPaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(s *Server)
	}{
		{"backup errored", func(s *Server) {
			s.backupDir, s.backupEvery = "somewhere", time.Hour
			s.lastBackup = backupStatus{At: time.Now(), Error: "disk full"}
		}},
		{"backup stale", func(s *Server) {
			s.backupDir, s.backupEvery = "somewhere", time.Hour
			s.lastBackup = backupStatus{At: time.Now().Add(-3 * time.Hour)}
		}},
		{"integrity failed", func(s *Server) {
			s.backupDir, s.backupEvery = "somewhere", time.Hour
			s.lastBackup = backupStatus{At: time.Now()}
			s.integrity = integrityStatus{At: time.Now(), OK: false}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.server.mu.Lock()
			tc.setup(e.server)
			e.server.mu.Unlock()
			code, body := e.health()
			if code != http.StatusServiceUnavailable || body.Status != "degraded" {
				t.Errorf("%s = %d %q, want 503 degraded", tc.name, code, body.Status)
			}
		})
	}
}

func TestIssueEndpoints(t *testing.T) {
	e := newTestEnv(t)
	e.label("agent-ready")

	var issue store.Issue
	e.expect("POST", "/api/v1/issues", map[string]any{
		"title":  "Fix header",
		"status": "Todo",
		"labels": []string{"agent-ready"},
	}, http.StatusCreated, &issue)
	if issue.Key != "TSK-1" || issue.Status != "Todo" || issue.Version != 0 || issue.PriorityLabel != "No priority" {
		t.Fatalf("created issue = %+v", issue)
	}

	var got store.Issue
	e.expect("GET", "/api/v1/issues/TSK-1", nil, http.StatusOK, &got)
	if got.Title != "Fix header" {
		t.Errorf("get = %+v", got)
	}

	e.expect("PATCH", "/api/v1/issues/TSK-1", map[string]any{"status": "In Progress"}, http.StatusOK, &got)
	if got.Status != "In Progress" || got.StartedAt == "" || got.Version != 1 {
		t.Errorf("patched = %+v", got)
	}

	// The authenticating token's name is the default actor on the audit trail.
	var events eventListResponse
	e.expect("GET", "/api/v1/issues/TSK-1/events", nil, http.StatusOK, &events)
	if len(events.Events) != 2 || events.Events[0].Actor != "pm" || events.Events[0].EntityKey != "TSK-1" {
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
	var comments commentListResponse
	e.expect("GET", "/api/v1/issues/TSK-1/comments", nil, http.StatusOK, &comments)
	if len(comments.Comments) != 1 {
		t.Errorf("comments = %+v", comments)
	}

	if list := e.issues("?status=In+Progress&label=agent-ready"); len(list.Issues) != 1 {
		t.Errorf("filtered list = %+v", list)
	}
	if list := e.issues("?q=zzz"); len(list.Issues) != 0 || list.NextOffset != nil {
		t.Errorf("no-match list = %+v", list)
	}

	e.expectError("GET", "/api/v1/issues/TSK-999", nil, http.StatusNotFound, codeNotFound)
	e.expectError("PATCH", "/api/v1/issues/TSK-1", map[string]any{}, http.StatusBadRequest, codeValidation)
	e.expectError("PATCH", "/api/v1/issues/TSK-1", map[string]any{"titel": "typo"}, http.StatusBadRequest, codeValidation)
	e.expectError("POST", "/api/v1/issues", map[string]any{"title": "x", "priority": 9}, http.StatusBadRequest, codeValidation)
	e.expectError("POST", "/api/v1/issues", map[string]any{"title": "x", "status": "todo!"}, http.StatusUnprocessableEntity, codeInvalidRef)
}

func TestIssueListFiltersAndPaging(t *testing.T) {
	e := newTestEnv(t)
	e.label("alpha", "beta")
	for _, spec := range []map[string]any{
		{"title": "one", "status": "Todo", "labels": []string{"alpha"}},
		{"title": "two", "status": "In Progress", "labels": []string{"alpha", "beta"}},
		{"title": "three"},
	} {
		e.expect("POST", "/api/v1/issues", spec, http.StatusCreated, nil)
	}
	// completed_at is stamped when an issue moves into a completed status, so
	// the third issue reaches Done by a patch rather than at creation.
	e.expect("PATCH", "/api/v1/issues/TSK-3", map[string]any{"status": "Done"}, http.StatusOK, nil)

	// Repeatable status is an OR; repeatable label is an AND.
	if list := e.issues("?status=Todo&status=Done"); len(list.Issues) != 2 {
		t.Errorf("two statuses = %d issues", len(list.Issues))
	}
	if list := e.issues("?label=alpha&label=beta"); len(list.Issues) != 1 {
		t.Errorf("two labels = %d issues", len(list.Issues))
	}
	if list := e.issues("?exclude_label=beta"); len(list.Issues) != 2 {
		t.Errorf("exclude_label = %d issues", len(list.Issues))
	}
	if list := e.issues("?status_type=completed"); len(list.Issues) != 1 {
		t.Errorf("status_type = %d issues", len(list.Issues))
	}

	// A full page carries the cursor for the next one; a short page does not.
	first := e.issues("?limit=2&order_by=created")
	if len(first.Issues) != 2 || first.NextOffset == nil || *first.NextOffset != 2 {
		t.Fatalf("first page = %+v", first)
	}
	second := e.issues("?limit=2&offset=2&order_by=created")
	if len(second.Issues) != 1 || second.NextOffset != nil {
		t.Errorf("second page = %+v", second)
	}

	e.expectError("GET", "/api/v1/issues?stauts=Todo", nil, http.StatusBadRequest, codeValidation)
	e.expectError("GET", "/api/v1/issues?limit=lots", nil, http.StatusBadRequest, codeValidation)
	e.expectError("GET", "/api/v1/issues?limit=-1", nil, http.StatusBadRequest, codeValidation)
	e.expectError("GET", "/api/v1/issues?updated_since=yesterday", nil, http.StatusBadRequest, codeValidation)
	e.expectError("GET", "/api/v1/issues?completed_since=2026-13-40", nil, http.StatusBadRequest, codeValidation)
	e.expectError("GET", "/api/v1/issues?order_by=sideways", nil, http.StatusBadRequest, codeValidation)
	e.expectError("GET", "/api/v1/issues?archived=perhaps", nil, http.StatusBadRequest, codeValidation)

	// A timestamp filter is canonicalised, not compared as text.
	if list := e.issues("?updated_since=2000-01-01T00:00:00%2B01:00"); len(list.Issues) != 3 {
		t.Errorf("updated_since = %d issues", len(list.Issues))
	}
	if list := e.issues("?completed_since=2000-01-01"); len(list.Issues) != 1 {
		t.Errorf("completed_since = %d issues", len(list.Issues))
	}
}

// A filter value that resolves to nothing is a typo, and a typo must not come
// back as an empty page: a session reads that as "no work" and stops.
func TestIssueListRejectsUnresolvableFilters(t *testing.T) {
	e := newTestEnv(t)
	e.label("alpha")
	e.expect("POST", "/api/v1/projects", map[string]any{"name": "Rebuild"}, http.StatusCreated, nil)
	e.expect("POST", "/api/v1/milestones", map[string]any{"project": "rebuild", "name": "Launch"}, http.StatusCreated, nil)
	e.expect("POST", "/api/v1/issues", map[string]any{
		"title": "parent", "project": "rebuild", "milestone": "Launch",
		"labels": []string{"alpha"}, "assignee": "pm",
	}, http.StatusCreated, nil)
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "child", "parent": "TSK-1"}, http.StatusCreated, nil)

	for _, tc := range []struct {
		name  string
		good  string
		found int
		typo  string
		names string // the message has to name the value the caller got wrong
	}{
		{"status", "?status=Triage", 2, "?status=Triageee", "Triageee"},
		{"status_type", "?status_type=triage", 2, "?status_type=triaged", "triaged"},
		{"project", "?project=rebuild", 1, "?project=rebiuld", "rebiuld"},
		{"label", "?label=alpha", 1, "?label=alphaa", "alphaa"},
		{"exclude_label", "?exclude_label=alpha", 1, "?exclude_label=alphaa", "alphaa"},
		{"milestone", "?milestone=Launch", 1, "?milestone=Launched", "Launched"},
		{"parent", "?parent=TSK-1", 1, "?parent=TSK-11", "TSK-11"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if list := e.issues(tc.good); len(list.Issues) != tc.found {
				t.Fatalf("%s = %d issues, want %d", tc.good, len(list.Issues), tc.found)
			}
			got := e.expectError("GET", "/api/v1/issues"+tc.typo, nil, http.StatusUnprocessableEntity, codeInvalidRef)
			if !strings.Contains(got.Error, tc.names) {
				t.Errorf("%s error = %q, want it to name %q", tc.name, got.Error, tc.names)
			}
		})
	}

	// Assignees are free-form names, so an unknown one is still an empty page.
	if list := e.issues("?assignee=nobody"); len(list.Issues) != 0 {
		t.Errorf("unknown assignee = %+v", list)
	}
}

func TestIssueIdempotentCreate(t *testing.T) {
	e := newTestEnv(t)
	var first, replay store.Issue
	body := map[string]any{"title": "only once", "idempotency_key": "abc123"}
	e.expect("POST", "/api/v1/issues", body, http.StatusCreated, &first)
	e.expect("POST", "/api/v1/issues", body, http.StatusOK, &replay)
	if replay.Key != first.Key {
		t.Fatalf("replay = %s, want %s", replay.Key, first.Key)
	}
	if list := e.issues(""); len(list.Issues) != 1 {
		t.Errorf("replay created a duplicate: %d issues", len(list.Issues))
	}
}

func TestIssueVersionConflict(t *testing.T) {
	e := newTestEnv(t)
	var issue store.Issue
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "contested"}, http.StatusCreated, &issue)
	e.expect("PATCH", "/api/v1/issues/"+issue.Key, map[string]any{
		"title":            "renamed",
		"expected_version": 0,
	}, http.StatusOK, &issue)
	if issue.Version != 1 {
		t.Fatalf("version = %d, want 1", issue.Version)
	}
	e.expectError("PATCH", "/api/v1/issues/"+issue.Key, map[string]any{
		"title":            "stale writer",
		"expected_version": 0,
	}, http.StatusConflict, codeVersionConflict)
}

func TestDescriptionIsAppendOnly(t *testing.T) {
	e := newTestEnv(t)
	var issue store.Issue
	e.expect("POST", "/api/v1/issues", map[string]any{
		"title":       "handoff",
		"description": "first note",
	}, http.StatusCreated, &issue)

	e.expectError("PATCH", "/api/v1/issues/"+issue.Key, map[string]any{
		"description": "overwritten",
	}, http.StatusConflict, codeDescriptionReplace)

	e.expect("POST", "/api/v1/issues/"+issue.Key+"/description", map[string]any{
		"append": "second note",
	}, http.StatusOK, &issue)
	if issue.Description != "first note\n\nsecond note" {
		t.Fatalf("description = %q", issue.Description)
	}
	e.expectError("POST", "/api/v1/issues/"+issue.Key+"/description", map[string]any{
		"append": "  ",
	}, http.StatusBadRequest, codeValidation)
	e.expectError("POST", "/api/v1/issues/TSK-99/description", map[string]any{
		"append": "x",
	}, http.StatusNotFound, codeNotFound)

	// The deliberate overwrite is an admin-token action; TestReplaceDescription
	// NeedsAdmin owns that rule.
	e.withToken(e.adminToken(), func() {
		e.expect("PATCH", "/api/v1/issues/"+issue.Key, map[string]any{
			"description":         "deliberate replacement",
			"replace_description": true,
		}, http.StatusOK, &issue)
	})
	if issue.Description != "deliberate replacement" {
		t.Errorf("replaced description = %q", issue.Description)
	}
}

// Replacing a description is the one write a role decides: an agent token is
// refused with a 403 that names the rule, an admin token goes through. The
// CLI's --clear-description is the same field with an empty description beside
// it, so it is refused the same way.
func TestReplaceDescriptionNeedsAdmin(t *testing.T) {
	e := newTestEnv(t)
	var issue store.Issue
	e.expect("POST", "/api/v1/issues", map[string]any{
		"title":       "runbook",
		"description": "the steps",
	}, http.StatusCreated, &issue)
	key := "/api/v1/issues/" + issue.Key

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"replace", map[string]any{"description": "overwritten", "replace_description": true}},
		{"clear", map[string]any{"description": "", "replace_description": true}},
	} {
		t.Run(tc.name+" as an agent", func(t *testing.T) {
			got := e.expectError("PATCH", key, tc.body, http.StatusForbidden, codeForbidden)
			if !strings.Contains(got.Error, "admin token") {
				t.Errorf("message = %q, want the rule named", got.Error)
			}
		})
	}

	// The append-only path an agent is meant to use still works.
	e.expect("POST", key+"/description", map[string]any{"append": "a note"}, http.StatusOK, &issue)
	if issue.Description != "the steps\n\na note" {
		t.Fatalf("append as an agent = %q", issue.Description)
	}

	admin := e.adminToken()
	e.withToken(admin, func() {
		e.expect("PATCH", key, map[string]any{
			"description":         "rewritten by the owner",
			"replace_description": true,
		}, http.StatusOK, &issue)
	})
	if issue.Description != "rewritten by the owner" {
		t.Errorf("replaced description = %q", issue.Description)
	}
	e.withToken(admin, func() {
		var cleared store.Issue
		e.expect("PATCH", key, map[string]any{
			"description":         "",
			"replace_description": true,
		}, http.StatusOK, &cleared)
		if cleared.Description != "" {
			t.Errorf("cleared description = %q", cleared.Description)
		}
	})
}

func TestLabelOperations(t *testing.T) {
	e := newTestEnv(t)
	if err := e.store.SetSetting("label_groups", `[["ready","blocked"]]`); err != nil {
		t.Fatal(err)
	}
	e.label("alpha", "beta", "ready", "blocked")
	var issue store.Issue
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "labelled"}, http.StatusCreated, &issue)
	key := "/api/v1/issues/" + issue.Key

	e.expect("PATCH", key, map[string]any{"add_labels": []string{"alpha", "ready"}}, http.StatusOK, &issue)
	if len(issue.Labels) != 2 {
		t.Fatalf("after add = %v", issue.Labels)
	}
	// The exclusive group swaps rather than accumulating.
	e.expect("PATCH", key, map[string]any{"add_labels": []string{"blocked"}}, http.StatusOK, &issue)
	if !slicesEqual(issue.Labels, []string{"alpha", "blocked"}) {
		t.Errorf("after exclusive add = %v", issue.Labels)
	}
	e.expect("PATCH", key, map[string]any{"remove_labels": []string{"alpha"}}, http.StatusOK, &issue)
	if !slicesEqual(issue.Labels, []string{"blocked"}) {
		t.Errorf("after remove = %v", issue.Labels)
	}
	// Case-insensitive resolution onto the stored spelling.
	e.expect("PATCH", key, map[string]any{"labels": []string{"BETA"}}, http.StatusOK, &issue)
	if !slicesEqual(issue.Labels, []string{"beta"}) {
		t.Errorf("after replace = %v", issue.Labels)
	}

	e.expectError("PATCH", key, map[string]any{"add_labels": []string{"nope"}}, http.StatusUnprocessableEntity, codeInvalidRef)
	e.expectError("PATCH", key, map[string]any{
		"labels":     []string{"alpha"},
		"add_labels": []string{"beta"},
	}, http.StatusBadRequest, codeValidation)
	e.expectError("PATCH", key, map[string]any{
		"labels": []string{"ready", "blocked"},
	}, http.StatusConflict, codeConflict)
}

func TestCommentThreadAndEdit(t *testing.T) {
	e := newTestEnv(t)
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "discussed"}, http.StatusCreated, nil)
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "elsewhere"}, http.StatusCreated, nil)

	var parent, reply store.Comment
	e.expect("POST", "/api/v1/issues/TSK-1/comments", map[string]any{"body": "root"}, http.StatusCreated, &parent)
	e.expect("POST", "/api/v1/issues/TSK-1/comments", map[string]any{
		"body":      "reply",
		"parent_id": parent.ID,
	}, http.StatusCreated, &reply)
	if reply.ParentID != parent.ID {
		t.Fatalf("reply = %+v", reply)
	}
	// A parent on another issue is a bad reference, not a missing one.
	e.expectError("POST", "/api/v1/issues/TSK-2/comments", map[string]any{
		"body":      "wrong thread",
		"parent_id": parent.ID,
	}, http.StatusUnprocessableEntity, codeInvalidRef)

	var replayed store.Comment
	body := map[string]any{"body": "once", "idempotency_key": "cmt-1"}
	e.expect("POST", "/api/v1/issues/TSK-1/comments", body, http.StatusCreated, &replayed)
	original := replayed.ID
	e.expect("POST", "/api/v1/issues/TSK-1/comments", body, http.StatusOK, &replayed)
	if replayed.ID != original {
		t.Errorf("replayed comment = %d, want %d", replayed.ID, original)
	}

	var edited store.Comment
	e.expect("PATCH", fmt.Sprintf("/api/v1/comments/%d", parent.ID), map[string]any{"body": "root, revised"}, http.StatusOK, &edited)
	if edited.Body != "root, revised" {
		t.Errorf("edited = %+v", edited)
	}
	e.expectError("PATCH", "/api/v1/comments/999", map[string]any{"body": "x"}, http.StatusNotFound, codeNotFound)
	e.expectError("PATCH", "/api/v1/comments/abc", map[string]any{"body": "x"}, http.StatusBadRequest, codeValidation)
	e.expectError("PATCH", fmt.Sprintf("/api/v1/comments/%d", parent.ID), map[string]any{"body": " "}, http.StatusBadRequest, codeValidation)
}

func TestEventFeeds(t *testing.T) {
	e := newTestEnv(t)
	e.expect("POST", "/api/v1/projects", map[string]any{"name": "Feed"}, http.StatusCreated, nil)
	e.expect("POST", "/api/v1/milestones", map[string]any{"project": "feed", "name": "M1"}, http.StatusCreated, nil)
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "watched"}, http.StatusCreated, nil)

	// Minting the test token was itself an event, so the feed holds four.
	var all eventListResponse
	e.expect("GET", "/api/v1/events", nil, http.StatusOK, &all)
	if len(all.Events) != 4 || all.NextAfterID != nil {
		t.Fatalf("global feed = %+v", all)
	}
	// Oldest first, so a consumer can page forward with the cursor.
	if all.Events[0].Action != "token.created" || all.Events[1].EntityKey != "feed" {
		t.Errorf("feed order = %+v", all.Events)
	}

	var page eventListResponse
	e.expect("GET", "/api/v1/events?limit=1", nil, http.StatusOK, &page)
	if len(page.Events) != 1 || page.NextAfterID == nil || *page.NextAfterID != page.Events[0].ID {
		t.Fatalf("first page = %+v", page)
	}
	var rest eventListResponse
	e.expect("GET", fmt.Sprintf("/api/v1/events?after_id=%d", *page.NextAfterID), nil, http.StatusOK, &rest)
	if len(rest.Events) != 3 {
		t.Errorf("after cursor = %d events", len(rest.Events))
	}

	var issuesOnly eventListResponse
	e.expect("GET", "/api/v1/events?entity=issue", nil, http.StatusOK, &issuesOnly)
	if len(issuesOnly.Events) != 1 {
		t.Errorf("entity filter = %+v", issuesOnly)
	}

	var perEntity eventListResponse
	e.expect("GET", "/api/v1/projects/feed/events", nil, http.StatusOK, &perEntity)
	if len(perEntity.Events) != 1 || perEntity.NextAfterID != nil {
		t.Errorf("project feed = %+v", perEntity)
	}
	e.expect("GET", "/api/v1/milestones/1/events", nil, http.StatusOK, &perEntity)
	if len(perEntity.Events) != 1 {
		t.Errorf("milestone feed = %+v", perEntity)
	}
	e.expectError("GET", "/api/v1/projects/missing/events", nil, http.StatusNotFound, codeNotFound)
	e.expectError("GET", "/api/v1/milestones/99/events", nil, http.StatusNotFound, codeNotFound)
	// An entity kind that does not exist is a typo, not an empty feed.
	e.expectError("GET", "/api/v1/events?entity=issu", nil, http.StatusNotFound, codeNotFound)
	e.expectError("GET", "/api/v1/events?entitiy=issue", nil, http.StatusBadRequest, codeValidation)
	e.expectError("GET", "/api/v1/events?since=never", nil, http.StatusBadRequest, codeValidation)
	e.expectError("GET", "/api/v1/events?after_id=x", nil, http.StatusBadRequest, codeValidation)
}

func TestRelationEndpoints(t *testing.T) {
	e := newTestEnv(t)
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "A"}, http.StatusCreated, nil)
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "B"}, http.StatusCreated, nil)

	var relations relationListResponse
	e.expect("POST", "/api/v1/issues/TSK-1/relations", map[string]any{
		"related": "TSK-2",
		"type":    "blocks",
	}, http.StatusOK, &relations)
	if len(relations.Relations) != 1 || relations.Relations[0].Type != "blocks" {
		t.Fatalf("relations = %+v", relations)
	}
	e.expect("GET", "/api/v1/issues/TSK-2/relations", nil, http.StatusOK, &relations)
	if len(relations.Relations) != 1 {
		t.Errorf("reverse listing = %+v", relations)
	}
	e.expect("POST", "/api/v1/issues/TSK-1/relations", map[string]any{
		"related": "TSK-2",
		"type":    "blocks",
		"remove":  true,
	}, http.StatusOK, &relations)
	if len(relations.Relations) != 0 {
		t.Errorf("after remove = %+v", relations)
	}
	if relations.Removed == nil || !*relations.Removed {
		t.Errorf("removed = %v, want true", relations.Removed)
	}
	// D9: removing a relation that is not there is the state the caller asked
	// for. It answers 200 with removed false, so a retry is safe.
	e.expect("POST", "/api/v1/issues/TSK-1/relations", map[string]any{
		"related": "TSK-2",
		"type":    "blocks",
		"remove":  true,
	}, http.StatusOK, &relations)
	if relations.Removed == nil || *relations.Removed {
		t.Errorf("second removed = %v, want false", relations.Removed)
	}
	// Idempotent is not lax: a mistyped type or key still fails.
	e.expectError("POST", "/api/v1/issues/TSK-1/relations", map[string]any{
		"related": "TSK-2",
		"type":    "nonsense",
		"remove":  true,
	}, http.StatusBadRequest, codeValidation)
	e.expectError("POST", "/api/v1/issues/TSK-1/relations", map[string]any{
		"related": "TSK-99",
		"type":    "blocks",
		"remove":  true,
	}, http.StatusNotFound, codeNotFound)
	// A listing carries no removed field at all, so it decodes into a fresh
	// value with the pointer still nil.
	var listed relationListResponse
	e.expect("GET", "/api/v1/issues/TSK-1/relations", nil, http.StatusOK, &listed)
	if listed.Removed != nil {
		t.Errorf("listing removed = %v, want absent", listed.Removed)
	}
	e.expectError("POST", "/api/v1/issues/TSK-1/relations", map[string]any{
		"related": "TSK-2",
		"type":    "nonsense",
	}, http.StatusBadRequest, codeValidation)
}

// D5 and D7: the two field-shape rules the store enforces reach the API as 422
// invalid_ref, on every entity that shares the pattern and on update as well
// as create.
func TestFieldValidationEndpoints(t *testing.T) {
	e := newTestEnv(t)
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "seed"}, http.StatusCreated, nil)
	e.expect("POST", "/api/v1/projects", map[string]any{"name": "Holder"}, http.StatusCreated, nil)
	e.expect("POST", "/api/v1/milestones", map[string]any{"project": "holder", "name": "seed"}, http.StatusCreated, nil)

	long := strings.Repeat("a", 501)
	cases := []struct {
		name   string
		method string
		path   string
		body   map[string]any
	}{
		{"issue create empty title", "POST", "/api/v1/issues", map[string]any{"title": "   "}},
		{"issue create long title", "POST", "/api/v1/issues", map[string]any{"title": long}},
		{"issue update empty title", "PATCH", "/api/v1/issues/TSK-1", map[string]any{"title": ""}},
		{"issue update long title", "PATCH", "/api/v1/issues/TSK-1", map[string]any{"title": long}},
		{"project create empty name", "POST", "/api/v1/projects", map[string]any{"name": " "}},
		{"project create long name", "POST", "/api/v1/projects", map[string]any{"name": long}},
		{"project update empty name", "PATCH", "/api/v1/projects/holder", map[string]any{"name": ""}},
		{"project update long name", "PATCH", "/api/v1/projects/holder", map[string]any{"name": long}},
		{"milestone create empty name", "POST", "/api/v1/milestones", map[string]any{"project": "holder", "name": " "}},
		{"milestone create long name", "POST", "/api/v1/milestones", map[string]any{"project": "holder", "name": long}},
		{"milestone update empty name", "PATCH", "/api/v1/milestones/1", map[string]any{"name": ""}},
		{"milestone update long name", "PATCH", "/api/v1/milestones/1", map[string]any{"name": long}},
		{"label color without a hash", "POST", "/api/v1/labels", map[string]any{"name": "ready", "color": "ff0000"}},
		{"label color as a word", "POST", "/api/v1/labels", map[string]any{"name": "ready", "color": "red"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e.expectError(tc.method, tc.path, tc.body, http.StatusUnprocessableEntity, codeInvalidRef)
		})
	}

	// The accepted shapes still work, and a title is stored trimmed.
	var issue store.Issue
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "  padded  "}, http.StatusCreated, &issue)
	if issue.Title != "padded" {
		t.Errorf("title = %q, want it trimmed", issue.Title)
	}
	var label store.Label
	e.expect("POST", "/api/v1/labels", map[string]any{"name": "ready", "color": "#F0A"}, http.StatusCreated, &label)
	if label.Color != "#F0A" {
		t.Errorf("color = %q", label.Color)
	}
}

func TestProjectAndLabelEndpoints(t *testing.T) {
	e := newTestEnv(t)
	e.label("client-x", "priority")

	var project store.Project
	e.expect("POST", "/api/v1/projects", map[string]any{
		"name":        "Site Rebuild",
		"labels":      []string{"client-x"},
		"status":      "planned",
		"start_date":  "2026-09-01",
		"target_date": "2026-12-01",
	}, http.StatusCreated, &project)
	if project.Slug != "site-rebuild" || len(project.Labels) != 1 ||
		project.StartDate != "2026-09-01" || project.TargetDate != "2026-12-01" {
		t.Fatalf("project = %+v", project)
	}

	e.expect("PATCH", "/api/v1/projects/site-rebuild", map[string]any{
		"status":     "started",
		"add_labels": []string{"priority"},
	}, http.StatusOK, &project)
	if project.Status != "started" || len(project.Labels) != 2 {
		t.Errorf("patched project = %+v", project)
	}
	// completed_at follows the status in both directions. Each response is
	// decoded into a fresh struct: a cleared field is omitted, not sent empty.
	var completed store.Project
	e.expect("PATCH", "/api/v1/projects/site-rebuild", map[string]any{"status": "completed"}, http.StatusOK, &completed)
	if completed.CompletedAt == "" {
		t.Errorf("completed project = %+v", completed)
	}
	var reopened store.Project
	e.expect("PATCH", "/api/v1/projects/site-rebuild", map[string]any{"status": "paused"}, http.StatusOK, &reopened)
	if reopened.CompletedAt != "" {
		t.Errorf("reopened project kept completed_at: %+v", reopened)
	}

	e.expectError("PATCH", "/api/v1/projects/site-rebuild", map[string]any{}, http.StatusBadRequest, codeValidation)
	e.expectError("PATCH", "/api/v1/projects/site-rebuild", map[string]any{"status": "active"}, http.StatusBadRequest, codeValidation)
	e.expectError("PATCH", "/api/v1/projects/site-rebuild", map[string]any{"start_date": "01/09/2026"}, http.StatusBadRequest, codeValidation)
	e.expectError("GET", "/api/v1/projects/missing", nil, http.StatusNotFound, codeNotFound)
	e.expectError("POST", "/api/v1/projects", map[string]any{"name": "site rebuild"}, http.StatusConflict, codeConflict)

	var projects projectListResponse
	e.expect("GET", "/api/v1/projects", nil, http.StatusOK, &projects)
	if len(projects.Projects) != 1 {
		t.Errorf("projects = %+v", projects)
	}
	e.expectError("GET", "/api/v1/projects?archvied=true", nil, http.StatusBadRequest, codeValidation)

	var label store.Label
	e.expect("POST", "/api/v1/labels", map[string]any{"name": "urgent", "color": "#ff0000"}, http.StatusCreated, &label)
	if label.Color != "#ff0000" {
		t.Errorf("label = %+v", label)
	}
	var labels labelListResponse
	e.expect("GET", "/api/v1/labels", nil, http.StatusOK, &labels)
	if len(labels.Labels) != 3 {
		t.Errorf("labels = %d, want 3 (client-x, priority, urgent)", len(labels.Labels))
	}

	var statuses statusListResponse
	e.expect("GET", "/api/v1/statuses", nil, http.StatusOK, &statuses)
	if len(statuses.Statuses) != 8 {
		t.Errorf("statuses = %d", len(statuses.Statuses))
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
	e.expect("PATCH", "/api/v1/issues/"+issue.Key, map[string]any{"project": ""}, http.StatusOK, &cleared)
	if cleared.Project != "" {
		t.Errorf("project not cleared: %+v", cleared)
	}
	// A reference that does not resolve is 422, not 404: the issue itself is
	// findable, the project it names is not.
	e.expectError("POST", "/api/v1/issues", map[string]any{"title": "bad", "project": "missing"},
		http.StatusUnprocessableEntity, codeInvalidRef)
}

func TestMuxErrorsAreJSON(t *testing.T) {
	e := newTestEnv(t)
	got := e.expectError("GET", "/api/v1/nonsense", nil, http.StatusNotFound, codeNotFound)
	if got.Error == "" {
		t.Error("404 body has no message")
	}
	e.expectError("DELETE", "/api/v1/issues", nil, http.StatusMethodNotAllowed, codeNotFound)

	// A UI path is for a browser, so it keeps the plain reply.
	resp, err := e.client.Get(e.url + "/ui/nowhere")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
		t.Errorf("UI 404 = %q, want plain text", ct)
	}
}

func TestClassifyErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"version conflict", fmt.Errorf("x: %w", store.ErrVersionConflict), http.StatusConflict, codeVersionConflict},
		{"description replace", fmt.Errorf("x: %w", store.ErrDescriptionReplace), http.StatusConflict, codeDescriptionReplace},
		{"plain conflict", fmt.Errorf("x: %w", store.ErrConflict), http.StatusConflict, codeConflict},
		{"invalid ref", fmt.Errorf("x: %w", store.ErrInvalidRef), http.StatusUnprocessableEntity, codeInvalidRef},
		{"not found", fmt.Errorf("x: %w", store.ErrNotFound), http.StatusNotFound, codeNotFound},
		{"busy", fmt.Errorf("x: %w", store.ErrBusy), http.StatusServiceUnavailable, codeBusy},
		{"integrity", fmt.Errorf("x: %w", store.ErrIntegrity), http.StatusInternalServerError, codeInternal},
		{"filesystem", &os.PathError{Op: "open", Path: "/nope", Err: errors.New("no such file")}, http.StatusInternalServerError, codeInternal},
		{"plain validation", errors.New("priority 9 out of range 0-4"), http.StatusBadRequest, codeValidation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, code := classify(tc.err)
			if status != tc.wantStatus || code != tc.wantCode {
				t.Errorf("classify = %d %q, want %d %q", status, code, tc.wantStatus, tc.wantCode)
			}
		})
	}
}

func TestBusyCarriesRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/issues", nil)
	writeStoreError(rec, req, fmt.Errorf("locked: %w", store.ErrBusy))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("busy = %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

// An internal fault must not leak its detail into the body.
func TestInternalErrorBodyIsGeneric(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/issues", nil)
	writeStoreError(rec, req, fmt.Errorf("snapshot: %w", &os.PathError{Op: "open", Path: "/secret/place", Err: errors.New("denied")}))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "/secret/place") {
		t.Fatalf("internal error body = %d %s", rec.Code, rec.Body.String())
	}
}

func TestBackupSkipsRecentSnapshot(t *testing.T) {
	e := newTestEnv(t)
	dir := t.TempDir()
	if _, err := e.store.Backup(dir); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go e.server.RunBackups(ctx, BackupConfig{Dir: dir, Every: time.Hour})
	time.Sleep(200 * time.Millisecond)
	cancel()
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("startup snapshot not skipped: %d files before, %d after", len(before), len(after))
	}
	// The existing snapshot still dates the backup for /healthz.
	code, body := e.health()
	if code != http.StatusOK || body.Backup.LastAt == "" {
		t.Errorf("health after skip = %d %+v", code, body.Backup)
	}
}

// After a skipped startup run the next snapshot is due when the existing one
// ages out, not a full interval after this process started: a restart must not
// stretch the gap to almost twice the interval.
func TestFirstBackupDelay(t *testing.T) {
	for _, tc := range []struct {
		name  string
		every time.Duration
		age   time.Duration
		want  time.Duration
	}{
		{"fresh snapshot", 24 * time.Hour, 0, 24 * time.Hour},
		{"part-way through the interval", 24 * time.Hour, 6 * time.Hour, 18 * time.Hour},
		{"already due", 24 * time.Hour, 25 * time.Hour, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstBackupDelay(tc.every, tc.age); got != tc.want {
				t.Errorf("firstBackupDelay(%s, %s) = %s, want %s", tc.every, tc.age, got, tc.want)
			}
		})
	}
}

// The scheduler keeps running after a skipped startup run: the aligned first
// run fires, and the interval ticker takes over from there.
func TestBackupRunsAfterSkippedStartup(t *testing.T) {
	e := newTestEnv(t)
	dir := t.TempDir()
	snapshot, err := e.store.Backup(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Age the snapshot by an eighth of the interval, so the startup run is
	// skipped and the first scheduled run is due a moment later.
	every := 400 * time.Millisecond
	aged := time.Now().Add(-every / 8)
	if err := os.Chtimes(snapshot, aged, aged); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go e.server.RunBackups(ctx, BackupConfig{Dir: dir, Every: every})

	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) > 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no scheduled snapshot after the startup run was skipped: %d files", len(entries))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A zero or negative interval must not mean "back up in a tight loop".
func TestBackupIntervalFallback(t *testing.T) {
	e := newTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	go e.server.RunBackups(ctx, BackupConfig{Dir: t.TempDir(), Every: 0})
	time.Sleep(100 * time.Millisecond)
	cancel()
	e.server.mu.Lock()
	every := e.server.backupEvery
	e.server.mu.Unlock()
	if every != defaultBackupEvery {
		t.Fatalf("interval = %s, want %s", every, defaultBackupEvery)
	}
}

// Regression: a backup wedged on a blocked directory (macOS TCC, dead mount)
// used to hold the store's single connection and freeze every request until
// the process was killed. The API must keep answering while a backup hangs.
func TestAPIRespondsWhileBackupWedged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mkfifo unavailable on windows")
	}
	e := newTestEnv(t)

	backupDir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(backupDir, ".trackd-probe"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go e.server.RunBackups(ctx, BackupConfig{Dir: backupDir, Every: time.Hour, Timeout: time.Hour})
	time.Sleep(100 * time.Millisecond) // let the first run enter the wedge

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(e.url + "/healthz")
	if err != nil {
		t.Fatalf("healthz while backup wedged: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d while backup wedged, want 200", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", e.url+"/api/v1/issues", nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("issue list while backup wedged: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("issue list = %d while backup wedged, want 200", resp.StatusCode)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
