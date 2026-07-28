package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
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

func TestMCPToolList(t *testing.T) {
	session, _ := newMCPSession(t)
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := []string{"add_comment", "get_issue", "list_issues", "list_labels", "list_milestones", "list_projects", "save_issue", "save_milestone", "save_project"}
	if len(names) != len(want) {
		t.Fatalf("tools = %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("tools = %v, want %v", names, want)
		}
	}
}

func TestMCPWorkflow(t *testing.T) {
	session, st := newMCPSession(t)

	var project store.Project
	callTool(t, session, "save_project", map[string]any{
		"name":   "Site Rebuild",
		"labels": []string{"client-x"},
	}, &project)
	if project.Slug != "site-rebuild" || len(project.Labels) != 1 {
		t.Fatalf("project = %+v", project)
	}

	var issue store.Issue
	callTool(t, session, "save_issue", map[string]any{
		"title":   "Fix header",
		"status":  "Todo",
		"project": "site-rebuild",
		"labels":  []string{"agent-ready"},
	}, &issue)
	if issue.Key != "TSK-1" || issue.Project != "site-rebuild" {
		t.Fatalf("issue = %+v", issue)
	}

	callTool(t, session, "save_issue", map[string]any{
		"key":    "TSK-1",
		"status": "In Progress",
	}, &issue)
	if issue.Status != "In Progress" || issue.StartedAt == "" {
		t.Errorf("updated issue = %+v", issue)
	}

	var comment store.Comment
	callTool(t, session, "add_comment", map[string]any{
		"key":  "TSK-1",
		"body": "работа done — ✓",
	}, &comment)
	// Attribution defaults to the authenticated token's name.
	if comment.Actor != "pm" {
		t.Errorf("comment actor = %q, want pm", comment.Actor)
	}

	var detail struct {
		Issue     store.Issue      `json:"issue"`
		Comments  []store.Comment  `json:"comments"`
		Relations []store.Relation `json:"relations"`
	}
	callTool(t, session, "get_issue", map[string]any{"key": "TSK-1"}, &detail)
	if detail.Issue.Key != "TSK-1" || len(detail.Comments) != 1 {
		t.Errorf("detail = %+v", detail)
	}

	var issues struct {
		Issues []store.Issue `json:"issues"`
	}
	callTool(t, session, "list_issues", map[string]any{"label": "agent-ready", "status_type": "started"}, &issues)
	if len(issues.Issues) != 1 {
		t.Errorf("filtered issues = %+v", issues)
	}

	var labels struct {
		Labels []store.Label `json:"labels"`
	}
	callTool(t, session, "list_labels", nil, &labels)
	if len(labels.Labels) != 2 {
		t.Errorf("labels = %+v", labels)
	}

	// Errors surface as tool errors, not protocol failures.
	res := callTool(t, session, "get_issue", map[string]any{"key": "TSK-99"}, nil)
	if !res.IsError {
		t.Error("missing issue did not produce a tool error")
	}
	res = callTool(t, session, "save_issue", map[string]any{"title": "bad", "priority": 9}, nil)
	if !res.IsError {
		t.Error("invalid priority did not produce a tool error")
	}

	// The audit trail records the MCP writes with the token actor.
	events, err := st.ListEvents("issue", detail.Issue.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Actor != "pm" {
		t.Errorf("events = %d, first actor %q", len(events), events[0].Actor)
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
