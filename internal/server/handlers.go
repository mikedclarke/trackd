package server

import (
	"net/http"
	"strconv"

	"github.com/mikedclarke/trackd/internal/store"
)

func (s *Server) handleListIssues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	issues, err := s.store.ListIssues(store.IssueFilter{
		Status:          q.Get("status"),
		StatusType:      q.Get("status_type"),
		Project:         q.Get("project"),
		Label:           q.Get("label"),
		Parent:          q.Get("parent"),
		Query:           q.Get("q"),
		UpdatedSince:    q.Get("updated_since"),
		IncludeArchived: q.Get("archived") == "true",
		Limit:           limit,
		Offset:          offset,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if issues == nil {
		issues = []store.Issue{}
	}
	writeJSON(w, http.StatusOK, issues)
}

type issueCreateReq struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Priority    int      `json:"priority"`
	Project     string   `json:"project"`
	Parent      string   `json:"parent"`
	DueDate     string   `json:"due_date"`
	Labels      []string `json:"labels"`
	Actor       string   `json:"actor"`
}

func (s *Server) handleCreateIssue(w http.ResponseWriter, r *http.Request) {
	var req issueCreateReq
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	issue, err := s.store.CreateIssue(store.IssueInput{
		Title:       req.Title,
		Description: req.Description,
		Status:      req.Status,
		Priority:    req.Priority,
		Project:     req.Project,
		Parent:      req.Parent,
		DueDate:     req.DueDate,
		Labels:      req.Labels,
	}, actor(r, req.Actor))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, issue)
}

func (s *Server) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	issue, err := s.store.GetIssue(r.PathValue("key"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, issue)
}

type issuePatchReq struct {
	Title       *string   `json:"title"`
	Description *string   `json:"description"`
	Status      *string   `json:"status"`
	Priority    *int      `json:"priority"`
	Project     *string   `json:"project"`
	Parent      *string   `json:"parent"`
	DueDate     *string   `json:"due_date"`
	Labels      *[]string `json:"labels"`
	Archived    *bool     `json:"archived"`
	Actor       string    `json:"actor"`
}

func (p issuePatchReq) empty() bool {
	return p.Title == nil && p.Description == nil && p.Status == nil && p.Priority == nil &&
		p.Project == nil && p.Parent == nil && p.DueDate == nil && p.Labels == nil && p.Archived == nil
}

func (s *Server) handlePatchIssue(w http.ResponseWriter, r *http.Request) {
	var req issuePatchReq
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.empty() {
		writeError(w, http.StatusBadRequest, "empty patch: no fields to update")
		return
	}
	issue, err := s.store.UpdateIssue(r.PathValue("key"), store.IssuePatch{
		Title:       req.Title,
		Description: req.Description,
		Status:      req.Status,
		Priority:    req.Priority,
		Project:     req.Project,
		Parent:      req.Parent,
		DueDate:     req.DueDate,
		Labels:      req.Labels,
		Archived:    req.Archived,
	}, actor(r, req.Actor))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, issue)
}

func (s *Server) handleListComments(w http.ResponseWriter, r *http.Request) {
	comments, err := s.store.ListComments(r.PathValue("key"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if comments == nil {
		comments = []store.Comment{}
	}
	writeJSON(w, http.StatusOK, comments)
}

type commentReq struct {
	Body  string `json:"body"`
	Actor string `json:"actor"`
}

func (s *Server) handleAddComment(w http.ResponseWriter, r *http.Request) {
	var req commentReq
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	comment, err := s.store.AddComment(r.PathValue("key"), req.Body, actor(r, req.Actor))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, comment)
}

func (s *Server) handleListRelations(w http.ResponseWriter, r *http.Request) {
	relations, err := s.store.ListRelations(r.PathValue("key"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if relations == nil {
		relations = []store.Relation{}
	}
	writeJSON(w, http.StatusOK, relations)
}

type relationReq struct {
	Related string `json:"related"`
	Type    string `json:"type"`
	Remove  bool   `json:"remove"`
	Actor   string `json:"actor"`
}

// handleSetRelation adds or removes a relation. Removal rides on POST with
// {"remove": true}: the API keeps its no-DELETE-endpoints guarantee.
func (s *Server) handleSetRelation(w http.ResponseWriter, r *http.Request) {
	var req relationReq
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	key := r.PathValue("key")
	act := actor(r, req.Actor)
	var err error
	if req.Remove {
		err = s.store.RemoveRelation(key, req.Related, req.Type, act)
	} else {
		err = s.store.AddRelation(key, req.Related, req.Type, act)
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	relations, err := s.store.ListRelations(key)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if relations == nil {
		relations = []store.Relation{}
	}
	writeJSON(w, http.StatusOK, relations)
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	issue, err := s.store.GetIssue(r.PathValue("key"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.store.ListEvents("issue", issue.ID, limit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if events == nil {
		events = []store.Event{}
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects(r.URL.Query().Get("archived") == "true")
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if projects == nil {
		projects = []store.Project{}
	}
	writeJSON(w, http.StatusOK, projects)
}

type projectCreateReq struct {
	Name        string   `json:"name"`
	Slug        string   `json:"slug"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Labels      []string `json:"labels"`
	Actor       string   `json:"actor"`
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req projectCreateReq
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	act := actor(r, req.Actor)
	project, err := s.store.CreateProject(store.ProjectInput{
		Name:        req.Name,
		Slug:        req.Slug,
		Description: req.Description,
		Status:      req.Status,
	}, act)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if len(req.Labels) > 0 {
		if project, err = s.store.SetProjectLabels(project.Slug, req.Labels, act); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, project)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	project, err := s.store.GetProject(r.PathValue("slug"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

type projectPatchReq struct {
	Name        *string   `json:"name"`
	Description *string   `json:"description"`
	Status      *string   `json:"status"`
	Archived    *bool     `json:"archived"`
	Labels      *[]string `json:"labels"`
	Actor       string    `json:"actor"`
}

func (s *Server) handlePatchProject(w http.ResponseWriter, r *http.Request) {
	var req projectPatchReq
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == nil && req.Description == nil && req.Status == nil && req.Archived == nil && req.Labels == nil {
		writeError(w, http.StatusBadRequest, "empty patch: no fields to update")
		return
	}
	slug := r.PathValue("slug")
	act := actor(r, req.Actor)
	project, err := s.store.GetProject(slug)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if req.Name != nil || req.Description != nil || req.Status != nil || req.Archived != nil {
		project, err = s.store.UpdateProject(slug, store.ProjectPatch{
			Name:        req.Name,
			Description: req.Description,
			Status:      req.Status,
			Archived:    req.Archived,
		}, act)
		if err != nil {
			writeStoreError(w, err)
			return
		}
	}
	if req.Labels != nil {
		if project, err = s.store.SetProjectLabels(slug, *req.Labels, act); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) handleListLabels(w http.ResponseWriter, r *http.Request) {
	labels, err := s.store.ListLabels()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if labels == nil {
		labels = []store.Label{}
	}
	writeJSON(w, http.StatusOK, labels)
}

type labelReq struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

func (s *Server) handleEnsureLabel(w http.ResponseWriter, r *http.Request) {
	var req labelReq
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	label, err := s.store.EnsureLabel(req.Name, req.Color)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, label)
}

func (s *Server) handleListStatuses(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.store.ListStatuses()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, statuses)
}
