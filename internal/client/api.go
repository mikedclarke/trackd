package client

import (
	"net/url"
	"strconv"

	"github.com/mikedclarke/trackd/internal/store"
)

// The typed calls below own two things the raw Do cannot: the exact JSON field
// names the API expects, and the envelope every list response is wrapped in.
// Callers get plain slices back.

func setIf(v url.Values, key, value string) {
	if value != "" {
		v.Set(key, value)
	}
}

// IssueQuery is the filter set of GET /api/v1/issues.
type IssueQuery struct {
	Statuses       []string
	StatusTypes    []string
	Labels         []string
	ExcludeLabels  []string
	Project        string
	Parent         string
	Assignee       string
	Milestone      string
	Query          string
	UpdatedSince   string
	CompletedSince string
	Archived       string // "", "true" (include), "only"
	OrderBy        string // "updated" (default), "created", "priority"
	Limit          int
	Offset         int
}

func (q IssueQuery) values() url.Values {
	v := url.Values{}
	for _, s := range q.Statuses {
		v.Add("status", s)
	}
	for _, s := range q.StatusTypes {
		v.Add("status_type", s)
	}
	for _, s := range q.Labels {
		v.Add("label", s)
	}
	for _, s := range q.ExcludeLabels {
		v.Add("exclude_label", s)
	}
	setIf(v, "project", q.Project)
	setIf(v, "parent", q.Parent)
	setIf(v, "assignee", q.Assignee)
	setIf(v, "milestone", q.Milestone)
	setIf(v, "q", q.Query)
	setIf(v, "updated_since", q.UpdatedSince)
	setIf(v, "completed_since", q.CompletedSince)
	setIf(v, "archived", q.Archived)
	setIf(v, "order_by", q.OrderBy)
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Offset > 0 {
		v.Set("offset", strconv.Itoa(q.Offset))
	}
	return v
}

// ListIssues returns one page of issues and the offset to ask for next, which
// is nil when this page was the last one.
func (c *Client) ListIssues(q IssueQuery) ([]store.Issue, *int, error) {
	var env struct {
		Issues     []store.Issue `json:"issues"`
		NextOffset *int          `json:"next_offset"`
	}
	if err := c.Do("GET", "/api/v1/issues", q.values(), nil, &env); err != nil {
		return nil, nil, err
	}
	return env.Issues, env.NextOffset, nil
}

func (c *Client) GetIssue(key string) (*store.Issue, error) {
	var issue store.Issue
	if err := c.Do("GET", "/api/v1/issues/"+url.PathEscape(key), nil, nil, &issue); err != nil {
		return nil, err
	}
	return &issue, nil
}

// IssueCreate is the body of POST /api/v1/issues. Empty fields are omitted, so
// the server applies its own defaults.
type IssueCreate struct {
	Title          string
	Description    string
	Status         string
	Priority       int
	Project        string
	Parent         string
	Assignee       string
	Milestone      string
	DueDate        string
	Labels         []string
	Actor          string
	IdempotencyKey string
}

func (in IssueCreate) body() map[string]any {
	body := map[string]any{"title": in.Title}
	for k, v := range map[string]string{
		"description": in.Description, "status": in.Status, "project": in.Project,
		"parent": in.Parent, "assignee": in.Assignee, "milestone": in.Milestone,
		"due_date": in.DueDate, "actor": in.Actor, "idempotency_key": in.IdempotencyKey,
	} {
		if v != "" {
			body[k] = v
		}
	}
	if in.Priority != 0 {
		body["priority"] = in.Priority
	}
	if len(in.Labels) > 0 {
		body["labels"] = in.Labels
	}
	return body
}

func (c *Client) CreateIssue(in IssueCreate) (*store.Issue, error) {
	var issue store.Issue
	if err := c.Do("POST", "/api/v1/issues", nil, in.body(), &issue); err != nil {
		return nil, err
	}
	return &issue, nil
}

// IssuePatch is the body of PATCH /api/v1/issues/{key}. A nil pointer leaves
// the field alone; a pointer to the empty string clears it.
type IssuePatch struct {
	Title              *string
	Description        *string
	ReplaceDescription bool
	Status             *string
	Priority           *int
	Project            *string
	Parent             *string
	Assignee           *string
	Milestone          *string
	DueDate            *string
	Labels             *[]string
	AddLabels          []string
	RemoveLabels       []string
	ExpectedVersion    *int64
	Archived           *bool
	Actor              string
}

func (p IssuePatch) body() map[string]any {
	body := map[string]any{}
	for k, v := range map[string]*string{
		"title": p.Title, "description": p.Description, "status": p.Status,
		"project": p.Project, "parent": p.Parent, "assignee": p.Assignee,
		"milestone": p.Milestone, "due_date": p.DueDate,
	} {
		if v != nil {
			body[k] = *v
		}
	}
	if p.ReplaceDescription {
		body["replace_description"] = true
	}
	if p.Priority != nil {
		body["priority"] = *p.Priority
	}
	if p.Labels != nil {
		body["labels"] = *p.Labels
	}
	if len(p.AddLabels) > 0 {
		body["add_labels"] = p.AddLabels
	}
	if len(p.RemoveLabels) > 0 {
		body["remove_labels"] = p.RemoveLabels
	}
	if p.ExpectedVersion != nil {
		body["expected_version"] = *p.ExpectedVersion
	}
	if p.Archived != nil {
		body["archived"] = *p.Archived
	}
	if p.Actor != "" {
		body["actor"] = p.Actor
	}
	return body
}

func (c *Client) UpdateIssue(key string, p IssuePatch) (*store.Issue, error) {
	var issue store.Issue
	if err := c.Do("PATCH", "/api/v1/issues/"+url.PathEscape(key), nil, p.body(), &issue); err != nil {
		return nil, err
	}
	return &issue, nil
}

// AppendDescription adds text to the end of an issue description. It is the
// only way to add to a description that is already written: a plain patch is
// refused by the server.
func (c *Client) AppendDescription(key, text, actor string) (*store.Issue, error) {
	body := map[string]any{"append": text}
	if actor != "" {
		body["actor"] = actor
	}
	var issue store.Issue
	if err := c.Do("POST", "/api/v1/issues/"+url.PathEscape(key)+"/description", nil, body, &issue); err != nil {
		return nil, err
	}
	return &issue, nil
}

func (c *Client) ListComments(key string) ([]store.Comment, error) {
	var env struct {
		Comments []store.Comment `json:"comments"`
	}
	if err := c.Do("GET", "/api/v1/issues/"+url.PathEscape(key)+"/comments", nil, nil, &env); err != nil {
		return nil, err
	}
	return env.Comments, nil
}

// CommentCreate is the body of POST /api/v1/issues/{key}/comments.
type CommentCreate struct {
	Body           string
	ParentID       int64
	Actor          string
	IdempotencyKey string
}

func (c *Client) AddComment(key string, in CommentCreate) (*store.Comment, error) {
	body := map[string]any{"body": in.Body}
	if in.ParentID != 0 {
		body["parent_id"] = in.ParentID
	}
	if in.Actor != "" {
		body["actor"] = in.Actor
	}
	if in.IdempotencyKey != "" {
		body["idempotency_key"] = in.IdempotencyKey
	}
	var comment store.Comment
	if err := c.Do("POST", "/api/v1/issues/"+url.PathEscape(key)+"/comments", nil, body, &comment); err != nil {
		return nil, err
	}
	return &comment, nil
}

// UpdateComment rewrites a comment body. The original is kept in the audit
// trail, as every mutation is.
func (c *Client) UpdateComment(id int64, commentBody, actor string) (*store.Comment, error) {
	body := map[string]any{"body": commentBody}
	if actor != "" {
		body["actor"] = actor
	}
	var comment store.Comment
	if err := c.Do("PATCH", "/api/v1/comments/"+strconv.FormatInt(id, 10), nil, body, &comment); err != nil {
		return nil, err
	}
	return &comment, nil
}

func (c *Client) ListRelations(key string) ([]store.Relation, error) {
	var env struct {
		Relations []store.Relation `json:"relations"`
	}
	if err := c.Do("GET", "/api/v1/issues/"+url.PathEscape(key)+"/relations", nil, nil, &env); err != nil {
		return nil, err
	}
	return env.Relations, nil
}

// SaveRelation adds or removes one relation and returns the issue's relations
// as they stand afterwards.
func (c *Client) SaveRelation(key, related, typ string, remove bool, actor string) ([]store.Relation, error) {
	body := map[string]any{"related": related, "type": typ, "remove": remove}
	if actor != "" {
		body["actor"] = actor
	}
	var env struct {
		Relations []store.Relation `json:"relations"`
	}
	if err := c.Do("POST", "/api/v1/issues/"+url.PathEscape(key)+"/relations", nil, body, &env); err != nil {
		return nil, err
	}
	return env.Relations, nil
}

// EventQuery is the filter set of the activity feed. AfterID is the cursor: it
// is the id of the last event already seen.
type EventQuery struct {
	Since   string
	AfterID int64
	Entity  string
	Limit   int
}

func (q EventQuery) values() url.Values {
	v := url.Values{}
	setIf(v, "since", q.Since)
	setIf(v, "entity", q.Entity)
	if q.AfterID > 0 {
		v.Set("after_id", strconv.FormatInt(q.AfterID, 10))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	return v
}

// ListEvents reads the global activity feed and returns the cursor to pass as
// AfterID next time, which is nil when the feed is exhausted.
func (c *Client) ListEvents(q EventQuery) ([]store.Event, *int64, error) {
	var env struct {
		Events      []store.Event `json:"events"`
		NextAfterID *int64        `json:"next_after_id"`
	}
	if err := c.Do("GET", "/api/v1/events", q.values(), nil, &env); err != nil {
		return nil, nil, err
	}
	return env.Events, env.NextAfterID, nil
}

// ListIssueEvents reads one issue's slice of the activity feed.
func (c *Client) ListIssueEvents(key string, limit int) ([]store.Event, error) {
	v := url.Values{}
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	var env struct {
		Events []store.Event `json:"events"`
	}
	if err := c.Do("GET", "/api/v1/issues/"+url.PathEscape(key)+"/events", v, nil, &env); err != nil {
		return nil, err
	}
	return env.Events, nil
}

func (c *Client) ListProjects(archived bool) ([]store.Project, error) {
	v := url.Values{}
	if archived {
		v.Set("archived", "true")
	}
	var env struct {
		Projects []store.Project `json:"projects"`
	}
	if err := c.Do("GET", "/api/v1/projects", v, nil, &env); err != nil {
		return nil, err
	}
	return env.Projects, nil
}

func (c *Client) GetProject(slug string) (*store.Project, error) {
	var project store.Project
	if err := c.Do("GET", "/api/v1/projects/"+url.PathEscape(slug), nil, nil, &project); err != nil {
		return nil, err
	}
	return &project, nil
}

// ProjectCreate is the body of POST /api/v1/projects.
type ProjectCreate struct {
	Name        string
	Slug        string
	Description string
	Status      string
	Labels      []string
	StartDate   string
	TargetDate  string
	Actor       string
}

func (c *Client) CreateProject(in ProjectCreate) (*store.Project, error) {
	body := map[string]any{"name": in.Name}
	for k, v := range map[string]string{
		"slug": in.Slug, "description": in.Description, "status": in.Status,
		"start_date": in.StartDate, "target_date": in.TargetDate, "actor": in.Actor,
	} {
		if v != "" {
			body[k] = v
		}
	}
	if len(in.Labels) > 0 {
		body["labels"] = in.Labels
	}
	var project store.Project
	if err := c.Do("POST", "/api/v1/projects", nil, body, &project); err != nil {
		return nil, err
	}
	return &project, nil
}

// ProjectPatch is the body of PATCH /api/v1/projects/{slug}.
type ProjectPatch struct {
	Name         *string
	Description  *string
	Status       *string
	Labels       *[]string
	AddLabels    []string
	RemoveLabels []string
	StartDate    *string
	TargetDate   *string
	Archived     *bool
	Actor        string
}

func (p ProjectPatch) body() map[string]any {
	body := map[string]any{}
	for k, v := range map[string]*string{
		"name": p.Name, "description": p.Description, "status": p.Status,
		"start_date": p.StartDate, "target_date": p.TargetDate,
	} {
		if v != nil {
			body[k] = *v
		}
	}
	if p.Labels != nil {
		body["labels"] = *p.Labels
	}
	if len(p.AddLabels) > 0 {
		body["add_labels"] = p.AddLabels
	}
	if len(p.RemoveLabels) > 0 {
		body["remove_labels"] = p.RemoveLabels
	}
	if p.Archived != nil {
		body["archived"] = *p.Archived
	}
	if p.Actor != "" {
		body["actor"] = p.Actor
	}
	return body
}

func (c *Client) UpdateProject(slug string, p ProjectPatch) (*store.Project, error) {
	var project store.Project
	if err := c.Do("PATCH", "/api/v1/projects/"+url.PathEscape(slug), nil, p.body(), &project); err != nil {
		return nil, err
	}
	return &project, nil
}

func (c *Client) ListMilestones(project string, archived bool) ([]store.Milestone, error) {
	v := url.Values{}
	setIf(v, "project", project)
	if archived {
		v.Set("archived", "true")
	}
	var env struct {
		Milestones []store.Milestone `json:"milestones"`
	}
	if err := c.Do("GET", "/api/v1/milestones", v, nil, &env); err != nil {
		return nil, err
	}
	return env.Milestones, nil
}

// MilestoneCreate is the body of POST /api/v1/milestones.
type MilestoneCreate struct {
	Project     string
	Name        string
	Description string
	TargetDate  string
	Actor       string
}

func (c *Client) CreateMilestone(in MilestoneCreate) (*store.Milestone, error) {
	body := map[string]any{"project": in.Project, "name": in.Name}
	for k, v := range map[string]string{
		"description": in.Description, "target_date": in.TargetDate, "actor": in.Actor,
	} {
		if v != "" {
			body[k] = v
		}
	}
	var milestone store.Milestone
	if err := c.Do("POST", "/api/v1/milestones", nil, body, &milestone); err != nil {
		return nil, err
	}
	return &milestone, nil
}

// MilestonePatch is the body of PATCH /api/v1/milestones/{id}.
type MilestonePatch struct {
	Name        *string
	Description *string
	TargetDate  *string
	Archived    *bool
	Actor       string
}

func (c *Client) UpdateMilestone(id string, p MilestonePatch) (*store.Milestone, error) {
	body := map[string]any{}
	for k, v := range map[string]*string{
		"name": p.Name, "description": p.Description, "target_date": p.TargetDate,
	} {
		if v != nil {
			body[k] = *v
		}
	}
	if p.Archived != nil {
		body["archived"] = *p.Archived
	}
	if p.Actor != "" {
		body["actor"] = p.Actor
	}
	var milestone store.Milestone
	if err := c.Do("PATCH", "/api/v1/milestones/"+url.PathEscape(id), nil, body, &milestone); err != nil {
		return nil, err
	}
	return &milestone, nil
}

func (c *Client) ListLabels() ([]store.Label, error) {
	var env struct {
		Labels []store.Label `json:"labels"`
	}
	if err := c.Do("GET", "/api/v1/labels", nil, nil, &env); err != nil {
		return nil, err
	}
	return env.Labels, nil
}

// CreateLabel is the only way a label comes into existence: issue and project
// writes reject a name that does not already exist.
func (c *Client) CreateLabel(name, color string) (*store.Label, error) {
	var label store.Label
	body := map[string]any{"name": name, "color": color}
	if err := c.Do("POST", "/api/v1/labels", nil, body, &label); err != nil {
		return nil, err
	}
	return &label, nil
}

func (c *Client) ListStatuses() ([]store.Status, error) {
	var env struct {
		Statuses []store.Status `json:"statuses"`
	}
	if err := c.Do("GET", "/api/v1/statuses", nil, nil, &env); err != nil {
		return nil, err
	}
	return env.Statuses, nil
}

// Health reads the unauthenticated health endpoint and returns the report with
// the HTTP status it came with. A degraded server answers 503 with the same
// body, so the report is what matters and the caller decides what the status
// means. The shape is deliberately loose: it is a report, not a record.
func (c *Client) Health() (map[string]any, int, error) {
	var health map[string]any
	status, err := c.getOnce("/healthz", &health)
	if err != nil {
		return nil, status, err
	}
	return health, status, nil
}
