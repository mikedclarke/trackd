package server

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/mikedclarke/trackd/internal/store"
)

// The store's own paging defaults, repeated here because next_offset can only
// be computed against the limit the store actually used.
const (
	defaultIssueLimit = 100
	maxIssueLimit     = 500
	defaultEventLimit = 100
	maxEventLimit     = 1000
)

// checkParams rejects a query parameter the handler does not know. A
// misspelled filter that is silently ignored returns the wrong issues, which
// is worse than an error.
func checkParams(q url.Values, allowed []string) error {
	for name := range q {
		if !slices.Contains(allowed, name) {
			return fmt.Errorf("unknown query parameter %q", name)
		}
	}
	return nil
}

// intParam parses a whole-number parameter. Absent is zero; anything that is
// not a non-negative integer is the caller's mistake, not a reason to fall
// back to a default.
func intParam(q url.Values, name string) (int, error) {
	raw := q.Get(name)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", name, raw)
	}
	return n, nil
}

func int64Param(q url.Values, name string) (int64, error) {
	raw := q.Get(name)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", name, raw)
	}
	return n, nil
}

// timeParam canonicalises a timestamp filter through the store's parser, so
// the API and the CLI agree on what a timestamp is.
func timeParam(q url.Values, name string) (string, error) {
	ts, err := store.ParseTimestamp(q.Get(name))
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return ts, nil
}

func clamp(limit, fallback, max int) int {
	if limit <= 0 {
		return fallback
	}
	if limit > max {
		return max
	}
	return limit
}

type issueListResponse struct {
	Issues []store.Issue `json:"issues"`
	// NextOffset is null on a short page: the caller has reached the end and
	// does not need another request to find out.
	NextOffset *int `json:"next_offset"`
}

var issueListParams = []string{
	"status", "status_type", "project", "label", "exclude_label", "parent",
	"assignee", "milestone", "q", "updated_since", "completed_since",
	"archived", "order_by", "limit", "offset",
}

func (s *Server) handleListIssues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := checkParams(q, issueListParams); err != nil {
		writeValidation(w, err.Error())
		return
	}
	limit, err := intParam(q, "limit")
	if err != nil {
		writeValidation(w, err.Error())
		return
	}
	offset, err := intParam(q, "offset")
	if err != nil {
		writeValidation(w, err.Error())
		return
	}
	updatedSince, err := timeParam(q, "updated_since")
	if err != nil {
		writeValidation(w, err.Error())
		return
	}
	completedSince, err := timeParam(q, "completed_since")
	if err != nil {
		writeValidation(w, err.Error())
		return
	}
	issues, err := s.store.ListIssues(store.IssueFilter{
		Statuses:       q["status"],
		StatusTypes:    q["status_type"],
		Project:        q.Get("project"),
		Labels:         q["label"],
		ExcludeLabels:  q["exclude_label"],
		Parent:         q.Get("parent"),
		Assignee:       q.Get("assignee"),
		Milestone:      q.Get("milestone"),
		Query:          q.Get("q"),
		UpdatedSince:   updatedSince,
		CompletedSince: completedSince,
		Archived:       q.Get("archived"),
		OrderBy:        q.Get("order_by"),
		Limit:          limit,
		Offset:         offset,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	resp := issueListResponse{Issues: issues}
	if len(issues) == clamp(limit, defaultIssueLimit, maxIssueLimit) {
		next := offset + len(issues)
		resp.NextOffset = &next
	}
	writeJSON(w, http.StatusOK, resp)
}

type issueCreateReq struct {
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	Status         string   `json:"status"`
	Priority       int      `json:"priority"`
	Project        string   `json:"project"`
	Parent         string   `json:"parent"`
	Assignee       string   `json:"assignee"`
	Milestone      string   `json:"milestone"`
	DueDate        string   `json:"due_date"`
	Labels         []string `json:"labels"`
	IdempotencyKey string   `json:"idempotency_key"`
	Actor          string   `json:"actor"`
}

func (s *Server) handleCreateIssue(w http.ResponseWriter, r *http.Request) {
	var req issueCreateReq
	if err := decodeBody(w, r, &req); err != nil {
		writeValidation(w, err.Error())
		return
	}
	issue, created, err := s.store.CreateIssue(store.IssueInput{
		Title:          req.Title,
		Description:    req.Description,
		Status:         req.Status,
		Priority:       req.Priority,
		Project:        req.Project,
		Parent:         req.Parent,
		Assignee:       req.Assignee,
		Milestone:      req.Milestone,
		DueDate:        req.DueDate,
		Labels:         req.Labels,
		IdempotencyKey: req.IdempotencyKey,
	}, actor(r, req.Actor))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	// A replay of an idempotency key is a successful no-op, not a creation.
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, issue)
}

func (s *Server) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	issue, err := s.store.GetIssue(r.PathValue("key"))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, issue)
}

type issuePatchReq struct {
	Title              *string   `json:"title"`
	Description        *string   `json:"description"`
	ReplaceDescription bool      `json:"replace_description"`
	Status             *string   `json:"status"`
	Priority           *int      `json:"priority"`
	Project            *string   `json:"project"`
	Parent             *string   `json:"parent"`
	Assignee           *string   `json:"assignee"`
	Milestone          *string   `json:"milestone"`
	DueDate            *string   `json:"due_date"`
	Labels             *[]string `json:"labels"`
	AddLabels          []string  `json:"add_labels"`
	RemoveLabels       []string  `json:"remove_labels"`
	ExpectedVersion    *int64    `json:"expected_version"`
	Archived           *bool     `json:"archived"`
	Actor              string    `json:"actor"`
}

func (p issuePatchReq) empty() bool {
	return p.Title == nil && p.Description == nil && p.Status == nil && p.Priority == nil &&
		p.Project == nil && p.Parent == nil && p.Assignee == nil && p.Milestone == nil &&
		p.DueDate == nil && p.Labels == nil && p.Archived == nil &&
		len(p.AddLabels) == 0 && len(p.RemoveLabels) == 0
}

func (s *Server) handlePatchIssue(w http.ResponseWriter, r *http.Request) {
	var req issuePatchReq
	if err := decodeBody(w, r, &req); err != nil {
		writeValidation(w, err.Error())
		return
	}
	// The CLI's --clear-description is this same field with an empty
	// description beside it, so one check covers both ways of losing the text.
	if req.ReplaceDescription && tokenRole(r) != roleAdmin {
		writeStoreError(w, r, errAdminOnly)
		return
	}
	if req.empty() {
		writeValidation(w, "empty patch: no fields to update")
		return
	}
	issue, err := s.store.UpdateIssue(r.PathValue("key"), store.IssuePatch{
		Title:              req.Title,
		Description:        req.Description,
		ReplaceDescription: req.ReplaceDescription,
		Status:             req.Status,
		Priority:           req.Priority,
		Project:            req.Project,
		Parent:             req.Parent,
		Assignee:           req.Assignee,
		Milestone:          req.Milestone,
		DueDate:            req.DueDate,
		Labels:             req.Labels,
		AddLabels:          req.AddLabels,
		RemoveLabels:       req.RemoveLabels,
		ExpectedVersion:    req.ExpectedVersion,
		Archived:           req.Archived,
	}, actor(r, req.Actor))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, issue)
}

type descriptionAppendReq struct {
	Append string `json:"append"`
	Actor  string `json:"actor"`
}

// handleAppendDescription is the safe way to add to a description: it never
// needs to read the old text first, so two agents appending at once cannot
// overwrite each other.
func (s *Server) handleAppendDescription(w http.ResponseWriter, r *http.Request) {
	var req descriptionAppendReq
	if err := decodeBody(w, r, &req); err != nil {
		writeValidation(w, err.Error())
		return
	}
	issue, err := s.store.AppendDescription(r.PathValue("key"), req.Append, actor(r, req.Actor))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, issue)
}

type commentListResponse struct {
	Comments []store.Comment `json:"comments"`
}

func (s *Server) handleListComments(w http.ResponseWriter, r *http.Request) {
	comments, err := s.store.ListComments(r.PathValue("key"))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if comments == nil {
		comments = []store.Comment{}
	}
	writeJSON(w, http.StatusOK, commentListResponse{Comments: comments})
}

type commentCreateReq struct {
	Body           string `json:"body"`
	ParentID       int64  `json:"parent_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Actor          string `json:"actor"`
}

func (s *Server) handleAddComment(w http.ResponseWriter, r *http.Request) {
	var req commentCreateReq
	if err := decodeBody(w, r, &req); err != nil {
		writeValidation(w, err.Error())
		return
	}
	comment, created, err := s.store.AddComment(r.PathValue("key"), store.CommentInput{
		Body:           req.Body,
		ParentID:       req.ParentID,
		IdempotencyKey: req.IdempotencyKey,
	}, actor(r, req.Actor))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, comment)
}

type commentPatchReq struct {
	Body  string `json:"body"`
	Actor string `json:"actor"`
}

func (s *Server) handlePatchComment(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeValidation(w, "comment id must be a number")
		return
	}
	var req commentPatchReq
	if err := decodeBody(w, r, &req); err != nil {
		writeValidation(w, err.Error())
		return
	}
	comment, err := s.store.UpdateComment(id, req.Body, actor(r, req.Actor))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, comment)
}

type relationListResponse struct {
	Relations []store.Relation `json:"relations"`
	// Removed answers a removal only, and says whether there was a relation
	// there to remove. Removal is idempotent, so false is still a 200.
	Removed *bool `json:"removed,omitempty"`
}

func (s *Server) handleListRelations(w http.ResponseWriter, r *http.Request) {
	relations, err := s.store.ListRelations(r.PathValue("key"))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	s.writeRelations(w, r, relations, nil)
}

type relationReq struct {
	Related string `json:"related"`
	Type    string `json:"type"`
	Remove  bool   `json:"remove"`
	Actor   string `json:"actor"`
}

// handleSetRelation adds or removes a relation. Removal rides on POST with
// {"remove": true}: the API keeps its no-DELETE-endpoints guarantee. A removal
// that found nothing to remove is a 200 with "removed": false, because the
// caller's goal is the state, and that state is already reached.
func (s *Server) handleSetRelation(w http.ResponseWriter, r *http.Request) {
	var req relationReq
	if err := decodeBody(w, r, &req); err != nil {
		writeValidation(w, err.Error())
		return
	}
	key := r.PathValue("key")
	act := actor(r, req.Actor)
	var removed *bool
	var err error
	if req.Remove {
		var gone bool
		gone, err = s.store.RemoveRelation(key, req.Related, req.Type, act)
		removed = &gone
	} else {
		err = s.store.AddRelation(key, req.Related, req.Type, act)
	}
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	relations, err := s.store.ListRelations(key)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	s.writeRelations(w, r, relations, removed)
}

func (s *Server) writeRelations(w http.ResponseWriter, _ *http.Request, relations []store.Relation, removed *bool) {
	if relations == nil {
		relations = []store.Relation{}
	}
	writeJSON(w, http.StatusOK, relationListResponse{Relations: relations, Removed: removed})
}

type eventListResponse struct {
	Events []store.Event `json:"events"`
	// NextAfterID is the cursor for the next page of the global feed, and null
	// on a short page or on a per-entity feed, which has no cursor.
	NextAfterID *int64 `json:"next_after_id"`
}

var eventListParams = []string{"since", "after_id", "entity", "limit"}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := checkParams(q, eventListParams); err != nil {
		writeValidation(w, err.Error())
		return
	}
	since, err := timeParam(q, "since")
	if err != nil {
		writeValidation(w, err.Error())
		return
	}
	afterID, err := int64Param(q, "after_id")
	if err != nil {
		writeValidation(w, err.Error())
		return
	}
	limit, err := intParam(q, "limit")
	if err != nil {
		writeValidation(w, err.Error())
		return
	}
	events, err := s.store.ListAllEvents(store.EventFilter{
		Since:   since,
		AfterID: afterID,
		Entity:  q.Get("entity"),
		Limit:   limit,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	resp := eventListResponse{Events: events}
	if len(events) == clamp(limit, defaultEventLimit, maxEventLimit) {
		next := events[len(events)-1].ID
		resp.NextAfterID = &next
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListIssueEvents(w http.ResponseWriter, r *http.Request) {
	issue, err := s.store.GetIssue(r.PathValue("key"))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	s.writeEntityEvents(w, r, "issue", issue.ID)
}

func (s *Server) handleListProjectEvents(w http.ResponseWriter, r *http.Request) {
	project, err := s.store.GetProject(r.PathValue("slug"))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	s.writeEntityEvents(w, r, "project", project.ID)
}

func (s *Server) handleListMilestoneEvents(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeValidation(w, "milestone id must be a number")
		return
	}
	milestone, err := s.store.GetMilestone(id)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	s.writeEntityEvents(w, r, "milestone", milestone.ID)
}

func (s *Server) writeEntityEvents(w http.ResponseWriter, r *http.Request, entity string, id int64) {
	q := r.URL.Query()
	if err := checkParams(q, []string{"limit"}); err != nil {
		writeValidation(w, err.Error())
		return
	}
	limit, err := intParam(q, "limit")
	if err != nil {
		writeValidation(w, err.Error())
		return
	}
	events, err := s.store.ListEvents(entity, id, limit)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if events == nil {
		events = []store.Event{}
	}
	writeJSON(w, http.StatusOK, eventListResponse{Events: events})
}

type projectListResponse struct {
	Projects []store.Project `json:"projects"`
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := checkParams(q, []string{"archived"}); err != nil {
		writeValidation(w, err.Error())
		return
	}
	projects, err := s.store.ListProjects(q.Get("archived") == "true")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if projects == nil {
		projects = []store.Project{}
	}
	writeJSON(w, http.StatusOK, projectListResponse{Projects: projects})
}

type projectCreateReq struct {
	Name        string   `json:"name"`
	Slug        string   `json:"slug"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Labels      []string `json:"labels"`
	StartDate   string   `json:"start_date"`
	TargetDate  string   `json:"target_date"`
	Actor       string   `json:"actor"`
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req projectCreateReq
	if err := decodeBody(w, r, &req); err != nil {
		writeValidation(w, err.Error())
		return
	}
	project, err := s.store.CreateProject(store.ProjectInput{
		Name:        req.Name,
		Slug:        req.Slug,
		Description: req.Description,
		Status:      req.Status,
		Labels:      req.Labels,
		StartDate:   req.StartDate,
		TargetDate:  req.TargetDate,
	}, actor(r, req.Actor))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, project)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	project, err := s.store.GetProject(r.PathValue("slug"))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

type projectPatchReq struct {
	Name         *string   `json:"name"`
	Description  *string   `json:"description"`
	Status       *string   `json:"status"`
	Archived     *bool     `json:"archived"`
	Labels       *[]string `json:"labels"`
	AddLabels    []string  `json:"add_labels"`
	RemoveLabels []string  `json:"remove_labels"`
	StartDate    *string   `json:"start_date"`
	TargetDate   *string   `json:"target_date"`
	Actor        string    `json:"actor"`
}

func (p projectPatchReq) empty() bool {
	return p.Name == nil && p.Description == nil && p.Status == nil && p.Archived == nil &&
		p.Labels == nil && p.StartDate == nil && p.TargetDate == nil &&
		len(p.AddLabels) == 0 && len(p.RemoveLabels) == 0
}

func (s *Server) handlePatchProject(w http.ResponseWriter, r *http.Request) {
	var req projectPatchReq
	if err := decodeBody(w, r, &req); err != nil {
		writeValidation(w, err.Error())
		return
	}
	if req.empty() {
		writeValidation(w, "empty patch: no fields to update")
		return
	}
	project, err := s.store.UpdateProject(r.PathValue("slug"), store.ProjectPatch{
		Name:         req.Name,
		Description:  req.Description,
		Status:       req.Status,
		Archived:     req.Archived,
		Labels:       req.Labels,
		AddLabels:    req.AddLabels,
		RemoveLabels: req.RemoveLabels,
		StartDate:    req.StartDate,
		TargetDate:   req.TargetDate,
	}, actor(r, req.Actor))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

type milestoneListResponse struct {
	Milestones []store.Milestone `json:"milestones"`
}

func (s *Server) handleListMilestones(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := checkParams(q, []string{"project", "archived"}); err != nil {
		writeValidation(w, err.Error())
		return
	}
	milestones, err := s.store.ListMilestones(q.Get("project"), q.Get("archived") == "true")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if milestones == nil {
		milestones = []store.Milestone{}
	}
	writeJSON(w, http.StatusOK, milestoneListResponse{Milestones: milestones})
}

type milestoneCreateReq struct {
	Project     string `json:"project"`
	Name        string `json:"name"`
	Description string `json:"description"`
	TargetDate  string `json:"target_date"`
	Actor       string `json:"actor"`
}

func (s *Server) handleCreateMilestone(w http.ResponseWriter, r *http.Request) {
	var req milestoneCreateReq
	if err := decodeBody(w, r, &req); err != nil {
		writeValidation(w, err.Error())
		return
	}
	milestone, err := s.store.CreateMilestone(store.MilestoneInput{
		Project:     req.Project,
		Name:        req.Name,
		Description: req.Description,
		TargetDate:  req.TargetDate,
	}, actor(r, req.Actor))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, milestone)
}

func (s *Server) handleGetMilestone(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeValidation(w, "milestone id must be a number")
		return
	}
	milestone, err := s.store.GetMilestone(id)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, milestone)
}

type milestonePatchReq struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	TargetDate  *string `json:"target_date"`
	Archived    *bool   `json:"archived"`
	Actor       string  `json:"actor"`
}

func (s *Server) handlePatchMilestone(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeValidation(w, "milestone id must be a number")
		return
	}
	var req milestonePatchReq
	if err := decodeBody(w, r, &req); err != nil {
		writeValidation(w, err.Error())
		return
	}
	if req.Name == nil && req.Description == nil && req.TargetDate == nil && req.Archived == nil {
		writeValidation(w, "empty patch: no fields to update")
		return
	}
	milestone, err := s.store.UpdateMilestone(id, store.MilestonePatch{
		Name:        req.Name,
		Description: req.Description,
		TargetDate:  req.TargetDate,
		Archived:    req.Archived,
	}, actor(r, req.Actor))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, milestone)
}

type labelListResponse struct {
	Labels []store.Label `json:"labels"`
}

func (s *Server) handleListLabels(w http.ResponseWriter, r *http.Request) {
	labels, err := s.store.ListLabels()
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if labels == nil {
		labels = []store.Label{}
	}
	writeJSON(w, http.StatusOK, labelListResponse{Labels: labels})
}

type labelReq struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

func (s *Server) handleEnsureLabel(w http.ResponseWriter, r *http.Request) {
	var req labelReq
	if err := decodeBody(w, r, &req); err != nil {
		writeValidation(w, err.Error())
		return
	}
	label, err := s.store.EnsureLabel(req.Name, req.Color)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, label)
}

type statusListResponse struct {
	Statuses []store.Status `json:"statuses"`
}

func (s *Server) handleListStatuses(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.store.ListStatuses()
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if statuses == nil {
		statuses = []store.Status{}
	}
	writeJSON(w, http.StatusOK, statusListResponse{Statuses: statuses})
}
