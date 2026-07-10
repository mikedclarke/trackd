package server

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mikedclarke/trackd/internal/store"
)

func newUIEnv(t *testing.T) (*httptest.Server, *http.Client, string, *store.Store) {
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
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return ts, &http.Client{Jar: jar}, token, st
}

func fetch(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestUILoginFlow(t *testing.T) {
	ts, client, token, _ := newUIEnv(t)

	// Unauthenticated board requests land on the login page.
	code, body := fetch(t, client, ts.URL+"/")
	if code != http.StatusOK || !strings.Contains(body, "Paste an API token") {
		t.Fatalf("unauthenticated / -> %d, login page not shown", code)
	}

	resp, err := client.PostForm(ts.URL+"/ui/login", url.Values{"token": {"td_wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token login = %d, want 401", resp.StatusCode)
	}

	resp, err = client.PostForm(ts.URL+"/ui/login", url.Values{"token": {token}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	code, body = fetch(t, client, ts.URL+"/")
	if code != http.StatusOK || !strings.Contains(body, "No issues yet") {
		t.Fatalf("board after login = %d", code)
	}

	// Sign out clears the cookie.
	code, _ = fetch(t, client, ts.URL+"/ui/logout")
	if code != http.StatusOK {
		t.Fatalf("logout = %d", code)
	}
	_, body = fetch(t, client, ts.URL+"/")
	if !strings.Contains(body, "Paste an API token") {
		t.Error("board reachable after logout")
	}
}

func TestUIBoardAndIssue(t *testing.T) {
	ts, client, token, st := newUIEnv(t)
	if _, err := st.CreateProject(store.ProjectInput{Name: "Site Rebuild"}, ""); err != nil {
		t.Fatal(err)
	}
	issue, err := st.CreateIssue(store.IssueInput{
		Title:       "Fix <script>alert(1)</script> header",
		Description: "line one\n<b>not bold</b>",
		Status:      "Todo",
		Priority:    1,
		Project:     "site-rebuild",
		Labels:      []string{"agent-ready"},
	}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddComment(issue.Key, "handoff <i>note</i>", "seo"); err != nil {
		t.Fatal(err)
	}

	resp, err := client.PostForm(ts.URL+"/ui/login", url.Values{"token": {token}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	code, body := fetch(t, client, ts.URL+"/")
	if code != http.StatusOK {
		t.Fatalf("board = %d", code)
	}
	for _, want := range []string{"TSK-1", "P1", "agent-ready", "site-rebuild", "Todo"} {
		if !strings.Contains(body, want) {
			t.Errorf("board missing %q", want)
		}
	}
	// html/template must escape hostile titles.
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("board did not escape issue title")
	}

	code, body = fetch(t, client, ts.URL+"/ui/issue/TSK-1")
	if code != http.StatusOK {
		t.Fatalf("issue page = %d", code)
	}
	for _, want := range []string{"TSK-1", "P1 urgent", "handoff", "issue.created", "seo"} {
		if !strings.Contains(body, want) {
			t.Errorf("issue page missing %q", want)
		}
	}
	if strings.Contains(body, "<b>not bold</b>") || strings.Contains(body, "<i>note</i>") {
		t.Error("issue page did not escape description or comment")
	}

	code, _ = fetch(t, client, ts.URL+"/ui/issue/TSK-99")
	if code != http.StatusNotFound {
		t.Errorf("missing issue page = %d, want 404", code)
	}

	// Project filter narrows the board.
	code, body = fetch(t, client, ts.URL+"/?project=site-rebuild")
	if code != http.StatusOK || !strings.Contains(body, "TSK-1") {
		t.Errorf("filtered board = %d", code)
	}

	// The stylesheet is served without auth (it is static and harmless).
	code, body = fetch(t, client, ts.URL+"/ui/static/style.css")
	if code != http.StatusOK || !strings.Contains(body, "--accent") {
		t.Errorf("stylesheet = %d", code)
	}
}
