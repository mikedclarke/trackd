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

// The cookie must hold an opaque session id, never the API token itself: a
// stolen cookie is then worth a session this server can forget, not a
// credential that works against every interface.
func TestUISessionCookieIsOpaque(t *testing.T) {
	ts, client, token, st := newUIEnv(t)
	resp, err := client.PostForm(ts.URL+"/ui/login", url.Values{"token": {token}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	base, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	cookies := client.Jar.Cookies(base)
	if len(cookies) != 1 || cookies[0].Name != uiCookie {
		t.Fatalf("cookies = %+v", cookies)
	}
	value := cookies[0].Value
	if value == token || strings.HasPrefix(value, "td_") {
		t.Fatal("session cookie carries the API token")
	}
	if _, err := st.VerifyToken(value); err == nil {
		t.Fatal("session cookie value is a usable token")
	}
	// Signing out forgets the session, so replaying the cookie fails.
	if _, err := client.Get(ts.URL + "/ui/logout"); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.AddCookie(&http.Cookie{Name: uiCookie, Value: value})
	bare := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	replay, err := bare.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	replay.Body.Close()
	if replay.StatusCode != http.StatusSeeOther {
		t.Errorf("replayed session = %d, want a redirect to login", replay.StatusCode)
	}
}

// A proxy that terminates TLS tells us over X-Forwarded-Proto, and the cookie
// has to be marked Secure when it does.
func TestUICookieSecureBehindTLSProxy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		header     string
		wantSecure bool
	}{
		{"plain http", "", false},
		{"forwarded https", "https", true},
		{"forwarded https mixed case", "HTTPS", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _, token, _ := newUIEnv(t)
			form := url.Values{"token": {token}}
			req, err := http.NewRequest("POST", ts.URL+"/ui/login", strings.NewReader(form.Encode()))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.header != "" {
				req.Header.Set("X-Forwarded-Proto", tc.header)
			}
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			cookies := resp.Cookies()
			if len(cookies) != 1 {
				t.Fatalf("cookies = %+v", cookies)
			}
			if cookies[0].Secure != tc.wantSecure {
				t.Errorf("Secure = %v, want %v", cookies[0].Secure, tc.wantSecure)
			}
			if !cookies[0].HttpOnly {
				t.Error("session cookie is not HttpOnly")
			}
		})
	}
}

func TestUIBoardAndIssue(t *testing.T) {
	ts, client, token, st := newUIEnv(t)
	if _, err := st.CreateProject(store.ProjectInput{
		Name:       "Site Rebuild",
		StartDate:  "2026-09-01",
		TargetDate: "2026-12-01",
	}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureLabel("agent-ready", ""); err != nil {
		t.Fatal(err)
	}
	issue, _, err := st.CreateIssue(store.IssueInput{
		Title:       "Fix <script>alert(1)</script> header",
		Description: "line one\n<b>not bold</b>",
		Status:      "Todo",
		Priority:    1,
		Project:     "site-rebuild",
		Assignee:    "engineer",
		Labels:      []string{"agent-ready"},
	}, "pm")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddComment(issue.Key, store.CommentInput{Body: "handoff <i>note</i>"}, "seo"); err != nil {
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
	for _, want := range []string{"TSK-1", "P1", "agent-ready", "site-rebuild", "Todo", "engineer"} {
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
	// version, assignee and the project's dates sit beside their neighbours.
	for _, want := range []string{"TSK-1", "P1 urgent", "handoff", "issue.created", "seo",
		"version", "engineer", "2026-09-01", "2026-12-01"} {
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

	// Filters narrow the board.
	code, body = fetch(t, client, ts.URL+"/?project=site-rebuild")
	if code != http.StatusOK || !strings.Contains(body, "TSK-1") {
		t.Errorf("filtered board = %d", code)
	}
	code, body = fetch(t, client, ts.URL+"/?label=agent-ready")
	if code != http.StatusOK || !strings.Contains(body, "TSK-1") {
		t.Errorf("label-filtered board = %d", code)
	}
	code, body = fetch(t, client, ts.URL+"/?assignee=engineer")
	if code != http.StatusOK || !strings.Contains(body, "TSK-1") {
		t.Errorf("assignee-filtered board = %d", code)
	}
	// A filter naming nothing can only come from a hand-edited URL, and it is
	// the caller's mistake, not the server's.
	code, _ = fetch(t, client, ts.URL+"/?project=no-such-project")
	if code != http.StatusBadRequest {
		t.Errorf("board with an unknown project = %d, want 400", code)
	}

	// The stylesheet is served without auth (it is static and harmless).
	code, body = fetch(t, client, ts.URL+"/ui/static/style.css")
	if code != http.StatusOK || !strings.Contains(body, "--accent") {
		t.Errorf("stylesheet = %d", code)
	}
}
