package server

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"errors"
	"html/template"
	"log"
	"net/http"
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

// sessions live in memory only, so a restart signs everyone out. That is the
// intended lifetime for a read-only board behind a private network.
func (s *Server) startSession(tokenName string) (string, error) {
	id, err := newSessionID()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.sessions[id] = tokenName
	s.mu.Unlock()
	return id, nil
}

func (s *Server) session(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok := s.sessions[id]
	return name, ok
}

func (s *Server) endSession(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
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
			if name, ok := s.session(cookie.Value); ok {
				if info, ok := r.Context().Value(requestKey).(*requestInfo); ok {
					info.actor = name
				}
				next(w, r)
				return
			}
		}
		// A bearer token still works directly, which is how a script or a
		// health probe reads a page without holding a session.
		if header, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			if token, err := s.store.VerifyToken(header); err == nil {
				if info, ok := r.Context().Value(requestKey).(*requestInfo); ok {
					info.actor = token.Name
				}
				next(w, r)
				return
			}
		}
		http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
	}
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
	id, err := s.startSession(token.Name)
	if err != nil {
		http.Error(w, "could not start a session", http.StatusInternalServerError)
		return
	}
	s.setSessionCookie(w, r, id, 30*24*60*60)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) uiLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(uiCookie); err == nil {
		s.endSession(cookie.Value)
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
	s.render(w, boardTemplate, map[string]any{
		"Title":     "board",
		"Workspace": workspace,
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
	s.render(w, issueTemplate, map[string]any{
		"Title":     issue.Key,
		"Issue":     issue,
		"Project":   project,
		"Comments":  comments,
		"Relations": relations,
		"Events":    events,
	})
}

func (s *Server) render(w http.ResponseWriter, t *template.Template, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout.html", data); err != nil {
		log.Printf("render %s: %v", t.Name(), err)
	}
}
