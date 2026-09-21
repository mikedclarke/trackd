package server

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mikedclarke/trackd/internal/store"
)

//go:embed ui
var uiFS embed.FS

// boardColumnCap bounds how many cards one column renders; the rest are
// summarized. Keeps a board with hundreds of Done issues fast and readable.
const boardColumnCap = 50

var uiFuncs = template.FuncMap{
	"shortTime": func(ts string) string {
		ts = strings.TrimSuffix(ts, "Z")
		return strings.Replace(ts, "T", " ", 1)
	},
	"has": func(list []string, want string) bool {
		for _, v := range list {
			if strings.EqualFold(v, want) {
				return true
			}
		}
		return false
	},
	"hasInt": func(list []int, want int) bool {
		for _, v := range list {
			if v == want {
				return true
			}
		}
		return false
	},
	"deref": func(p *int) int {
		if p == nil {
			return -1
		}
		return *p
	},
	// prioWords is the 0-4 priority scale in display order for a form.
	"prioWords": func() []string { return []string{"none", "P1 urgent", "P2 high", "P3 medium", "P4 low"} },
}

func parseUITemplate(page string) *template.Template {
	return template.Must(template.New("layout.html").Funcs(uiFuncs).ParseFS(uiFS, "ui/layout.html", "ui/"+page))
}

var (
	boardTemplate = parseUITemplate("board.html")
	issueTemplate = parseUITemplate("issue.html")
	loginTemplate = parseUITemplate("login.html")
)

const uiCookie = "trackd_session"

// uiSessionTTL is how long a web login lasts. Every authenticated page view
// after uiSessionRenewAfter pushes the expiry out again, so a board someone
// actually visits stays signed in indefinitely and an abandoned session dies.
const (
	uiSessionTTL        = 30 * 24 * time.Hour
	uiSessionRenewAfter = 24 * time.Hour
)

// newSessionID mints the opaque value the browser holds. The API token never
// travels in a cookie: a stolen cookie buys a session on a server that can
// revoke it, not a credential that works everywhere.
func newSessionID() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// startSession records a durable session for a verified token, so a server
// restart does not sign every browser out.
func (s *Server) startSession(token *store.Token) (string, error) {
	id, err := newSessionID()
	if err != nil {
		return "", err
	}
	if err := s.store.CreateUISession(id, token.ID, uiSessionTTL); err != nil {
		return "", err
	}
	return id, nil
}

// secureCookie reports whether the browser reached us over TLS, directly or
// through the reverse proxy that terminates it.
func secureCookie(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, id string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: uiCookie, Value: id, Path: "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// uiAuth gates the HTML pages on the same tokens as the API, held indirectly
// through a session cookie the login form sets.
func (s *Server) uiAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(uiCookie); err == nil {
			if session, err := s.store.UISession(cookie.Value); err == nil {
				// A visit a day or more into the session slides the expiry
				// out again; a renewal that fails only shortens the session,
				// so it is logged rather than failing the page.
				if time.Until(session.ExpiresAt) < uiSessionTTL-uiSessionRenewAfter {
					if err := s.store.RenewUISession(cookie.Value, uiSessionTTL); err != nil {
						log.Printf("renew ui session: %v", err)
					} else {
						s.setSessionCookie(w, r, cookie.Value, int(uiSessionTTL/time.Second))
					}
				}
				s.serveAs(next, w, r, session.TokenName, session.TokenRole)
				return
			}
		}
		// A bearer token still works directly, which is how a script or a
		// health probe reads a page without holding a session.
		if header, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			if token, err := s.store.VerifyToken(header); err == nil {
				s.serveAs(next, w, r, token.Name, token.Role)
				return
			}
		}
		http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
	}
}

// serveAs runs a UI handler with the signed-in token's name and role on the
// request: the name in requestInfo for the log line, and both in the context
// for handlers that attribute writes or decide what a token may see.
func (s *Server) serveAs(next http.HandlerFunc, w http.ResponseWriter, r *http.Request, name, role string) {
	if info, ok := r.Context().Value(requestKey).(*requestInfo); ok {
		info.actor = name
	}
	ctx := context.WithValue(r.Context(), uiActorKey, name)
	ctx = context.WithValue(ctx, uiRoleKey, role)
	next(w, r.WithContext(ctx))
}

// uiIdentity is the signed-in token's name and role, as serveAs stored them.
func uiIdentity(r *http.Request) (actor, role string) {
	actor, _ = r.Context().Value(uiActorKey).(string)
	role, _ = r.Context().Value(uiRoleKey).(string)
	return actor, role
}

func (s *Server) uiLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, loginTemplate, map[string]any{"Title": "sign in"})
}

func (s *Server) uiLoginSubmit(w http.ResponseWriter, r *http.Request) {
	token, err := s.store.VerifyToken(r.PostFormValue("token"))
	if err != nil {
		time.Sleep(500 * time.Millisecond) // blunt the token form as a guessing surface
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, loginTemplate, map[string]any{"Title": "sign in", "Error": "That token was not accepted. Check it and try again."})
		return
	}
	id, err := s.startSession(token)
	if err != nil {
		http.Error(w, "could not start a session", http.StatusInternalServerError)
		return
	}
	s.setSessionCookie(w, r, id, int(uiSessionTTL/time.Second))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) uiLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(uiCookie); err == nil {
		if err := s.store.DeleteUISession(cookie.Value); err != nil {
			log.Printf("delete ui session: %v", err)
		}
	}
	s.setSessionCookie(w, r, "", -1)
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

func (s *Server) uiStyle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	body, err := uiFS.ReadFile("ui/style.css")
	if err != nil {
		http.Error(w, "stylesheet missing", http.StatusInternalServerError)
		return
	}
	if _, err := w.Write(body); err != nil {
		log.Printf("write stylesheet: %v", err)
	}
}

type boardColumn struct {
	Status store.Status
	Issues []store.Issue
	More   int
	Total  int
}

func (s *Server) uiBoard(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	label := r.URL.Query().Get("label")
	assignee := r.URL.Query().Get("assignee")
	filter := store.IssueFilter{Project: project, Assignee: assignee, Limit: maxIssueLimit}
	if label != "" {
		filter.Labels = []string{label}
	}
	issues, err := s.store.ListIssues(filter)
	if errors.Is(err, store.ErrInvalidRef) {
		// A filter naming a project or label that does not exist can only come
		// from a hand-edited URL: the board's own controls offer real values.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	statuses, err := s.store.ListStatuses()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	projects, err := s.store.ListProjects(false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	labels, err := s.store.ListLabels()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Assignee filter options come from the full issue set, not the filtered
	// one, so picking an assignee doesn't empty the dropdown.
	assignees, err := s.store.ListAssignees()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	byStatus := map[string][]store.Issue{}
	for _, issue := range issues {
		byStatus[issue.Status] = append(byStatus[issue.Status], issue)
	}
	var columns []boardColumn
	for _, status := range statuses {
		group := byStatus[status.Name]
		// Always show the working columns; only show the rest when occupied.
		if len(group) == 0 && status.Type != "unstarted" && status.Type != "started" {
			continue
		}
		column := boardColumn{Status: status, Issues: group, Total: len(group)}
		if len(group) > boardColumnCap {
			column.Issues = group[:boardColumnCap]
			column.More = len(group) - boardColumnCap
		}
		columns = append(columns, column)
	}
	workspace, err := s.store.Setting("workspace_name")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if workspace == "trackd" {
		workspace = "" // the default would just repeat the wordmark
	}
	tabs, err := s.uiTabs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, boardTemplate, map[string]any{
		"Title":     "board",
		"Workspace": workspace,
		"Views":     tabs,
		"Current":   "",
		"Columns":   columns,
		"Projects":  projects,
		"Labels":    labels,
		"Assignees": assignees,
		"Project":   project,
		"Label":     label,
		"Assignee":  assignee,
		"Count":     len(issues),
	})
}

func (s *Server) uiIssue(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	issue, err := s.store.GetIssue(key)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	comments, err := s.store.ListComments(key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	relations, err := s.store.ListRelations(key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	events, err := s.store.ListEvents("issue", issue.ID, 100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The project's own dates belong next to the issue's, so a reader can see
	// whether an issue is late against its project without leaving the page.
	var project *store.Project
	if issue.Project != "" {
		if project, err = s.store.GetProject(issue.Project); err != nil && !errors.Is(err, store.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	statuses, err := s.store.ListStatuses()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	labels, err := s.store.ListLabels()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	actor, _ := uiIdentity(r)
	s.render(w, issueTemplate, map[string]any{
		"Title":     issue.Key,
		"Issue":     issue,
		"Project":   project,
		"Comments":  comments,
		"Relations": relations,
		"Events":    events,
		"Statuses":  statuses,
		"Labels":    labels,
		"Actor":     actor,
		"Notice":    noticeFrom(r.URL.Query()),
	})
}

// uiIssueComment accepts the issue page's comment form. The session cookie is
// SameSite=Lax, so a cross-site POST never carries it; checking Origin against
// the host we were addressed as is the second lock on the same door.
func (s *Server) uiIssueComment(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r) {
		http.Error(w, "cross-origin form post refused", http.StatusForbidden)
		return
	}
	key := r.PathValue("key")
	back := "/ui/issue/" + url.PathEscape(key)
	// Browsers form-post textarea lines as CRLF; store the comment with the
	// same line endings an API client would send.
	body := strings.ReplaceAll(r.PostFormValue("body"), "\r\n", "\n")
	if strings.TrimSpace(body) == "" {
		// The form marks the field required; an empty post can only come from
		// something odd, and an empty comment is never worth an error page.
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	actor, _ := r.Context().Value(uiActorKey).(string)
	if _, _, err := s.store.AddComment(key, store.CommentInput{Body: body}, actor); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, back+"#comments", http.StatusSeeOther)
}

// sameOrigin reports whether a browser-supplied Origin names this server: the
// host the request reached directly, or the public host a reverse proxy
// forwarded for.
func sameOrigin(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if forwarded, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ","); forwarded != "" {
		return strings.EqualFold(u.Host, strings.TrimSpace(forwarded))
	}
	return false
}

func (s *Server) render(w http.ResponseWriter, t *template.Template, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout.html", data); err != nil {
		log.Printf("render %s: %v", t.Name(), err)
	}
}
