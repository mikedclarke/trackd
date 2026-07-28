package server

import (
	"embed"
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

const uiCookie = "trackd_token"

// uiAuth gates the HTML pages on the same tokens as the API, carried in an
// HttpOnly cookie set by the login form.
func (s *Server) uiAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(uiCookie); err == nil {
			if _, err := s.store.VerifyToken(cookie.Value); err == nil {
				next(w, r)
				return
			}
		}
		if header, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			if _, err := s.store.VerifyToken(header); err == nil {
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
	token := r.PostFormValue("token")
	if _, err := s.store.VerifyToken(token); err != nil {
		time.Sleep(500 * time.Millisecond) // blunt the token form as a guessing surface
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, loginTemplate, map[string]any{"Title": "sign in", "Error": "That token was not accepted. Check it and try again."})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: uiCookie, Value: token, Path: "/",
		MaxAge:   30 * 24 * 60 * 60,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) uiLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: uiCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
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
	issues, err := s.store.ListIssues(store.IssueFilter{Project: project, Label: label, Assignee: assignee, Limit: 5000})
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
	s.render(w, issueTemplate, map[string]any{
		"Title":     issue.Key,
		"Issue":     issue,
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
