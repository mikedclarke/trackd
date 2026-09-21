package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mikedclarke/trackd/internal/store"
)

type authTransport struct {
	token string
	base  http.RoundTripper
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(req)
}

func newMCPSession(t *testing.T) (*mcp.ClientSession, *store.Store) {
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

	transport := &mcp.StreamableClientTransport{
		Endpoint:   ts.URL + "/mcp",
		HTTPClient: &http.Client{Transport: &authTransport{token: token, base: http.DefaultTransport}},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, st
}

// mcpSessionAs opens a second MCP session against an existing store under a
// token of the given name and role, for tests that need to act as more than
// one identity.
func mcpSessionAs(t *testing.T, st *store.Store, name, role string) *mcp.ClientSession {
	t.Helper()
	token, err := st.CreateToken(name, role)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(st, "test").Handler())
	t.Cleanup(ts.Close)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   ts.URL + "/mcp",
		HTTPClient: &http.Client{Transport: &authTransport{token: token, base: http.DefaultTransport}},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if out != nil && !res.IsError {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: decode structured content: %v", name, err)
		}
	}
	return res
}

// callErr runs a tool that is expected to fail and returns the message text.
func callErr(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		// Schema validation failures come back as a protocol error rather than
		// a tool result; either way the message is what the caller reads.
		return err.Error()
	}
	if !res.IsError {
		t.Fatalf("%s(%v) succeeded, want an error", name, args)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(text.Text)
		}
	}
	return sb.String()
}

// The twelve tools of the cutover contract plus the two view tools, and
// nothing else.
var wantMCPTools = []string{
	"add_comment", "get_issue", "list_activity", "list_issues", "list_labels",
	"list_milestones", "list_projects", "list_statuses", "list_views", "save_issue",
	"save_milestone", "save_project", "save_relation", "save_view",
}

func TestMCPToolList(t *testing.T) {
	session, _ := newMCPSession(t)
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		byName[tool.Name] = tool
	}
	slices.Sort(names)
	if !slices.Equal(names, wantMCPTools) {
		t.Fatalf("tools = %v, want %v", names, wantMCPTools)
	}

	for name, tool := range byName {
		if tool.Description == "" || !strings.HasSuffix(tool.Description, ".") {
			t.Errorf("%s description = %q, want one sentence", name, tool.Description)
		}
		a := tool.Annotations
		if a == nil {
			t.Fatalf("%s has no annotations", name)
		}
		if a.DestructiveHint == nil || *a.DestructiveHint {
			t.Errorf("%s destructiveHint = %v, want false", name, a.DestructiveHint)
		}
		if a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("%s openWorldHint = %v, want false", name, a.OpenWorldHint)
		}
		readOnly := strings.HasPrefix(name, "list_") || strings.HasPrefix(name, "get_")
		if a.ReadOnlyHint != readOnly {
			t.Errorf("%s readOnlyHint = %v, want %v", name, a.ReadOnlyHint, readOnly)
		}
	}
	// The save tools addressed by an identity are idempotent; a bare comment
	// append is not.
	for _, name := range []string{"save_issue", "save_project", "save_milestone", "save_relation"} {
		if !byName[name].Annotations.IdempotentHint {
			t.Errorf("%s idempotentHint = false, want true", name)
		}
	}
	if byName["add_comment"].Annotations.IdempotentHint {
		t.Error("add_comment idempotentHint = true, want false")
	}

	// mode is a required enum on save_issue.
	schema, err := json.Marshal(byName["save_issue"].InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"required"`, `"mode"`, `"enum"`, `"create"`, `"update"`} {
		if !strings.Contains(string(schema), want) {
			t.Errorf("save_issue schema missing %s: %s", want, schema)
		}
	}
}

func TestMCPWorkflow(t *testing.T) {
	session, st := newMCPSession(t)
	if _, err := st.EnsureLabel("client-x", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureLabel("agent-ready", ""); err != nil {
		t.Fatal(err)
	}

	var project store.Project
	callTool(t, session, "save_project", map[string]any{
		"name":       "Site Rebuild",
		"labels":     []string{"client-x"},
		"status":     "started",
		"start_date": "2026-09-01",
	}, &project)
	if project.Slug != "site-rebuild" || len(project.Labels) != 1 || project.StartDate != "2026-09-01" {
		t.Fatalf("project = %+v", project)
	}

	var issue store.Issue
	callTool(t, session, "save_issue", map[string]any{
		"mode":    "create",
		"title":   "Fix header",
		"status":  "Todo",
		"project": "site-rebuild",
		"labels":  []string{"agent-ready"},
	}, &issue)
	if issue.Key != "TSK-1" || issue.Project != "site-rebuild" {
		t.Fatalf("issue = %+v", issue)
	}

	callTool(t, session, "save_issue", map[string]any{
		"mode":   "update",
		"key":    "TSK-1",
		"status": "In Progress",
	}, &issue)
	if issue.Status != "In Progress" || issue.StartedAt == "" || issue.Version != 1 {
		t.Errorf("updated issue = %+v", issue)
	}

	// Appending is a first-class update, not a read-then-write.
	callTool(t, session, "save_issue", map[string]any{
		"mode":               "update",
		"key":                "TSK-1",
		"append_description": "handoff notes",
	}, &issue)
	if issue.Description != "handoff notes" {
		t.Errorf("appended = %q", issue.Description)
	}

	var comment store.Comment
	callTool(t, session, "add_comment", map[string]any{
		"key":  "TSK-1",
		"body": "работа done ✓",
	}, &comment)
	// Attribution defaults to the authenticated token's name.
	if comment.Actor != "pm" {
		t.Errorf("comment actor = %q, want pm", comment.Actor)
	}

	var detail mcpIssueDetailOut
	callTool(t, session, "get_issue", map[string]any{"key": "TSK-1"}, &detail)
	if detail.Issue.Key != "TSK-1" || len(detail.Comments) != 1 {
		t.Errorf("detail = %+v", detail)
	}

	var issues mcpIssuesOut
	callTool(t, session, "list_issues", map[string]any{
		"labels":       []string{"agent-ready"},
		"status_types": []string{"started"},
	}, &issues)
	if len(issues.Issues) != 1 {
		t.Errorf("filtered issues = %+v", issues)
	}

	var labels mcpLabelsOut
	callTool(t, session, "list_labels", nil, &labels)
	if len(labels.Labels) != 2 {
		t.Errorf("labels = %+v", labels)
	}

	var statuses mcpStatusesOut
	callTool(t, session, "list_statuses", nil, &statuses)
	if len(statuses.Statuses) != 8 || statuses.Statuses[0].Name != "Triage" {
		t.Errorf("statuses = %+v", statuses)
	}
}

func TestMCPAgentLabelAutoApply(t *testing.T) {
	session, st := newMCPSession(t) // agent token
	if err := st.SetSetting("agent_label", "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureLabel("other", ""); err != nil {
		t.Fatal(err)
	}

	var issue store.Issue
	callTool(t, session, "save_issue", map[string]any{"mode": "create", "title": "agent made"}, &issue)
	if len(issue.Labels) != 1 || issue.Labels[0] != "agent" {
		t.Fatalf("agent create = %v, want [agent]", issue.Labels)
	}

	// An explicit pass of the same label is not doubled.
	callTool(t, session, "save_issue", map[string]any{"mode": "create", "title": "explicit", "labels": []string{"agent", "other"}}, &issue)
	if len(issue.Labels) != 2 || issue.Labels[0] != "agent" || issue.Labels[1] != "other" {
		t.Fatalf("explicit pass = %v, want [agent other]", issue.Labels)
	}

	// An admin session on the same store does not get the label.
	admin := mcpSessionAs(t, st, "owner", "admin")
	var adminIssue store.Issue
	callTool(t, admin, "save_issue", map[string]any{"mode": "create", "title": "admin made"}, &adminIssue)
	if len(adminIssue.Labels) != 0 {
		t.Fatalf("admin create = %v, want no label", adminIssue.Labels)
	}
}

func TestMCPSaveIssueMode(t *testing.T) {
	session, _ := newMCPSession(t)

	if msg := callErr(t, session, "save_issue", map[string]any{"title": "no mode"}); !strings.Contains(msg, "mode") {
		t.Errorf("missing mode = %q", msg)
	}
	if msg := callErr(t, session, "save_issue", map[string]any{"mode": "upsert", "title": "x"}); !strings.Contains(msg, "mode") {
		t.Errorf("bad mode = %q", msg)
	}
	if msg := callErr(t, session, "save_issue", map[string]any{"mode": "update", "title": "x"}); !strings.Contains(msg, "key") {
		t.Errorf("update without key = %q", msg)
	}

	var issue store.Issue
	callTool(t, session, "save_issue", map[string]any{"mode": "create", "title": "made"}, &issue)
	if msg := callErr(t, session, "save_issue", map[string]any{
		"mode": "create", "title": "x", "key": issue.Key,
	}); !strings.Contains(msg, "key") {
		t.Errorf("create with key = %q", msg)
	}

	// An idempotency key makes a repeated create safe to retry.
	var first, replay store.Issue
	args := map[string]any{"mode": "create", "title": "once", "idempotency_key": "k1"}
	callTool(t, session, "save_issue", args, &first)
	callTool(t, session, "save_issue", args, &replay)
	if replay.Key != first.Key {
		t.Errorf("replay = %s, want %s", replay.Key, first.Key)
	}
}

// An empty labels array is the one silently destructive input, so it needs the
// caller to say it meant it.
func TestMCPClearLabelsIsExplicit(t *testing.T) {
	session, st := newMCPSession(t)
	if _, err := st.EnsureLabel("alpha", ""); err != nil {
		t.Fatal(err)
	}
	var issue store.Issue
	callTool(t, session, "save_issue", map[string]any{
		"mode": "create", "title": "labelled", "labels": []string{"alpha"},
	}, &issue)

	msg := callErr(t, session, "save_issue", map[string]any{
		"mode": "update", "key": issue.Key, "labels": []string{},
	})
	if !strings.Contains(msg, "clear_labels") {
		t.Errorf("empty labels without confirmation = %q", msg)
	}

	callTool(t, session, "save_issue", map[string]any{
		"mode": "update", "key": issue.Key, "labels": []string{}, "clear_labels": true,
	}, &issue)
	if len(issue.Labels) != 0 {
		t.Errorf("labels after clear = %v", issue.Labels)
	}
}

// Tool errors name their class, so an agent can tell a bad reference from a
// conflict without reading the prose.
func TestMCPErrorsCarryTheirCode(t *testing.T) {
	session, _ := newMCPSession(t)
	var issue store.Issue
	callTool(t, session, "save_issue", map[string]any{
		"mode": "create", "title": "described", "description": "first note",
	}, &issue)

	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"missing issue", "get_issue", map[string]any{"key": "TSK-99"}, "[not_found]"},
		{"unknown status", "save_issue", map[string]any{"mode": "create", "title": "x", "status": "todo!"}, "[invalid_ref]"},
		{"unknown label", "save_issue", map[string]any{"mode": "create", "title": "x", "labels": []string{"nope"}}, "[invalid_ref]"},
		{"bad priority", "save_issue", map[string]any{"mode": "create", "title": "x", "priority": 9}, "[validation]"},
		{"description replace", "save_issue", map[string]any{"mode": "update", "key": issue.Key, "description": "overwrite"}, "[description_replace]"},
		{"replace as an agent", "save_issue", map[string]any{"mode": "update", "key": issue.Key, "description": "overwrite", "replace_description": true}, "[forbidden]"},
		{"version conflict", "save_issue", map[string]any{"mode": "update", "key": issue.Key, "title": "y", "expected_version": 99}, "[version_conflict]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if msg := callErr(t, session, tc.tool, tc.args); !strings.Contains(msg, tc.want) {
				t.Errorf("%s = %q, want %s", tc.name, msg, tc.want)
			}
		})
	}
}

func TestMCPSaveRelation(t *testing.T) {
	session, _ := newMCPSession(t)
	callTool(t, session, "save_issue", map[string]any{"mode": "create", "title": "A"}, nil)
	callTool(t, session, "save_issue", map[string]any{"mode": "create", "title": "B"}, nil)

	var out mcpRelationsOut
	callTool(t, session, "save_relation", map[string]any{
		"key": "TSK-1", "related": "TSK-2", "type": "blocks",
	}, &out)
	if len(out.Relations) != 1 || out.Relations[0].Type != "blocks" {
		t.Fatalf("relations = %+v", out)
	}
	callTool(t, session, "save_relation", map[string]any{
		"key": "TSK-1", "related": "TSK-2", "type": "blocks", "remove": true,
	}, &out)
	if len(out.Relations) != 0 {
		t.Errorf("after remove = %+v", out)
	}
	if out.Removed == nil || !*out.Removed {
		t.Errorf("removed = %v, want true", out.Removed)
	}
	// Removing again is the same state, so it succeeds and says there was
	// nothing there.
	out = mcpRelationsOut{}
	callTool(t, session, "save_relation", map[string]any{
		"key": "TSK-1", "related": "TSK-2", "type": "blocks", "remove": true,
	}, &out)
	if out.Removed == nil || *out.Removed {
		t.Errorf("second removed = %v, want false", out.Removed)
	}
	if msg := callErr(t, session, "save_relation", map[string]any{
		"key": "TSK-1", "related": "TSK-2", "type": "sideways",
	}); msg == "" {
		t.Error("unknown relation type was accepted")
	}
	if msg := callErr(t, session, "save_relation", map[string]any{
		"key": "TSK-1", "related": "TSK-2", "type": "sideways", "remove": true,
	}); msg == "" {
		t.Error("unknown relation type was accepted on a removal")
	}
}

func TestMCPListActivity(t *testing.T) {
	session, _ := newMCPSession(t)
	callTool(t, session, "save_project", map[string]any{"name": "Feed"}, nil)
	callTool(t, session, "save_issue", map[string]any{"mode": "create", "title": "watched"}, nil)

	// Minting the test token was itself an event, so the feed holds three.
	var out mcpActivityOut
	callTool(t, session, "list_activity", nil, &out)
	if len(out.Events) != 3 || out.Events[0].Action != "token.created" || out.Events[1].EntityKey != "feed" {
		t.Fatalf("activity = %+v", out.Events)
	}
	// before and after are objects, not the byte arrays a raw message would be.
	if _, ok := out.Events[1].After.(map[string]any); !ok {
		t.Errorf("event after = %T, want an object", out.Events[1].After)
	}
	var after mcpActivityOut
	callTool(t, session, "list_activity", map[string]any{"after_id": out.Events[1].ID}, &after)
	if len(after.Events) != 1 || after.Events[0].EntityKey != "TSK-1" {
		t.Errorf("after cursor = %+v", after.Events)
	}
	var issuesOnly mcpActivityOut
	callTool(t, session, "list_activity", map[string]any{"entity": "issue"}, &issuesOnly)
	if len(issuesOnly.Events) != 1 {
		t.Errorf("entity filter = %+v", issuesOnly.Events)
	}
	if msg := callErr(t, session, "list_activity", map[string]any{"since": "never"}); msg == "" {
		t.Error("unparseable since was accepted")
	}
}

func TestMCPRequiresAuth(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ts := httptest.NewServer(New(st, "test").Handler())
	t.Cleanup(ts.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	_, err = client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp"}, nil)
	if err == nil {
		t.Fatal("connected without a token")
	}
}

func TestMCPViews(t *testing.T) {
	session, st := newMCPSession(t)
	if _, err := st.EnsureLabel("waiting", ""); err != nil {
		t.Fatal(err)
	}
	for _, in := range []store.IssueInput{
		{Title: "parked", Status: "Todo", Labels: []string{"waiting"}, Priority: 1},
		{Title: "free", Status: "Todo"},
	} {
		if _, _, err := st.CreateIssue(in, "pm"); err != nil {
			t.Fatal(err)
		}
	}
	var view store.View
	callTool(t, session, "save_view", map[string]any{
		"mode": "create", "name": "Waiting on me",
		"filter":        map[string]any{"statuses": []string{"Todo"}, "labels": []string{"waiting"}},
		"quick_actions": []map[string]any{{"name": "Answered", "remove_labels": []string{"waiting"}}},
	}, &view)
	if view.Owner != "pm" || !view.Shared || len(view.QuickActions) != 1 {
		t.Fatalf("view = %+v", view)
	}
	var views mcpViewsOut
	callTool(t, session, "list_views", map[string]any{}, &views)
	if len(views.Views) != 1 || views.Views[0].Name != "Waiting on me" {
		t.Fatalf("list_views = %+v", views)
	}
	var issues mcpIssuesOut
	callTool(t, session, "list_issues", map[string]any{"view": "waiting on me"}, &issues)
	if len(issues.Issues) != 1 || issues.Issues[0].Title != "parked" {
		t.Fatalf("list_issues via view = %+v", issues.Issues)
	}
	callTool(t, session, "list_issues", map[string]any{"priorities": []int{1}, "created_by": "pm"}, &issues)
	if len(issues.Issues) != 1 {
		t.Fatalf("list_issues by priority and creator = %+v", issues.Issues)
	}
	if msg := callErr(t, session, "save_view", map[string]any{"mode": "create", "name": "x"}); !strings.Contains(msg, "[invalid_ref]") {
		t.Errorf("create without filter = %q", msg)
	}
	if msg := callErr(t, session, "list_issues", map[string]any{"view": "nope"}); !strings.Contains(msg, "[not_found]") {
		t.Errorf("unknown view = %q", msg)
	}

	// Another agent may read a shared view but not change it.
	other := mcpSessionAs(t, st, "seo", "agent")
	callTool(t, other, "list_views", map[string]any{}, &views)
	if len(views.Views) != 1 {
		t.Errorf("shared view hidden from another token: %+v", views)
	}
	if msg := callErr(t, other, "save_view", map[string]any{"mode": "update", "name": "Waiting on me", "description": "mine"}); !strings.Contains(msg, "[forbidden]") {
		t.Errorf("update by non-owner = %q", msg)
	}
	yes := true
	callTool(t, session, "save_view", map[string]any{"mode": "update", "name": "Waiting on me", "archived": yes}, &view)
	if view.ArchivedAt == "" {
		t.Errorf("archive via save_view = %+v", view)
	}
}
