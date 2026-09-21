package store

import "encoding/json"

type Status struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Position int    `json:"position"`
}

type Project struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Slug        string   `json:"slug"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Labels      []string `json:"labels"`
	StartDate   string   `json:"start_date,omitempty"`
	TargetDate  string   `json:"target_date,omitempty"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	CompletedAt string   `json:"completed_at,omitempty"`
	ArchivedAt  string   `json:"archived_at,omitempty"`
}

type Issue struct {
	ID            int64    `json:"id"`
	Key           string   `json:"key"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	Status        string   `json:"status"`
	StatusType    string   `json:"status_type"`
	Priority      int      `json:"priority"`
	PriorityLabel string   `json:"priority_label"`
	Project       string   `json:"project,omitempty"`
	Parent        string   `json:"parent,omitempty"`
	Assignee      string   `json:"assignee,omitempty"`
	Milestone     string   `json:"milestone,omitempty"`
	DueDate       string   `json:"due_date,omitempty"`
	Labels        []string `json:"labels"`
	Version       int64    `json:"version"`
	URL           string   `json:"url,omitempty"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
	StartedAt     string   `json:"started_at,omitempty"`
	CompletedAt   string   `json:"completed_at,omitempty"`
	CanceledAt    string   `json:"canceled_at,omitempty"`
	ArchivedAt    string   `json:"archived_at,omitempty"`
}

type Comment struct {
	ID        int64  `json:"id"`
	IssueKey  string `json:"issue_key"`
	Body      string `json:"body"`
	Actor     string `json:"actor,omitempty"`
	ParentID  int64  `json:"parent_id,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type Relation struct {
	IssueKey   string `json:"issue_key"`
	RelatedKey string `json:"related_key"`
	Type       string `json:"type"`
}

type Label struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
}

type Token struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Role       string `json:"role"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	RevokedAt  string `json:"revoked_at,omitempty"`
}

type Event struct {
	ID        int64           `json:"id"`
	Entity    string          `json:"entity"`
	EntityID  int64           `json:"entity_id"`
	EntityKey string          `json:"entity_key,omitempty"`
	Actor     string          `json:"actor,omitempty"`
	Action    string          `json:"action"`
	Before    json.RawMessage `json:"before,omitempty"`
	After     json.RawMessage `json:"after,omitempty"`
	CreatedAt string          `json:"created_at"`
}

type IssueInput struct {
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
	IdempotencyKey string
}

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
}

type IssueFilter struct {
	Statuses       []string
	StatusTypes    []string
	Project        string
	Labels         []string
	ExcludeLabels  []string
	Parent         string
	Assignee       string
	Milestone      string
	Query          string
	Priorities     []int // any match; 0 is "no priority"
	CreatedBy      string
	UpdatedSince   string
	CompletedSince string
	Archived       string // "", "true" (include), "only"
	OrderBy        string // "updated" (default), "created", "priority"
	Limit          int
	Offset         int
}

type CommentInput struct {
	Body           string
	ParentID       int64
	IdempotencyKey string
}

type EventFilter struct {
	Since   string
	AfterID int64
	Entity  string
	Limit   int
}

type Milestone struct {
	ID          int64  `json:"id"`
	Project     string `json:"project"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	TargetDate  string `json:"target_date,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	ArchivedAt  string `json:"archived_at,omitempty"`
}

type MilestoneInput struct {
	Project     string
	Name        string
	Description string
	TargetDate  string
}

type MilestonePatch struct {
	Name        *string
	Description *string
	TargetDate  *string
	Archived    *bool
}

type ProjectInput struct {
	Name        string
	Slug        string
	Description string
	Status      string
	Labels      []string
	StartDate   string
	TargetDate  string
}

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
}

// View is a saved issue filter with a name, so a queue can be opened by name
// from the board, the API, the CLI or MCP. A shared view is visible to every
// token; a private one only to its owner and to admins.
type View struct {
	ID           int64         `json:"id"`
	Name         string        `json:"name"`
	Description  string        `json:"description,omitempty"`
	Filter       ViewFilter    `json:"filter"`
	QuickActions []QuickAction `json:"quick_actions"`
	Owner        string        `json:"owner"`
	Shared       bool          `json:"shared"`
	CreatedAt    string        `json:"created_at"`
	UpdatedAt    string        `json:"updated_at"`
	ArchivedAt   string        `json:"archived_at,omitempty"`
}

// ViewFilter is the stored half of an IssueFilter: the fields that make sense
// to keep. Paging and the archived switch are per request, and a saved
// updated_since would go stale, so a view keeps a relative window instead
// (updated_within, e.g. 7d or 48h) and resolves it when it is applied.
type ViewFilter struct {
	Statuses      []string `json:"statuses,omitempty"`
	StatusTypes   []string `json:"status_types,omitempty"`
	Project       string   `json:"project,omitempty"`
	Labels        []string `json:"labels,omitempty"`
	ExcludeLabels []string `json:"exclude_labels,omitempty"`
	Assignee      string   `json:"assignee,omitempty"`
	Milestone     string   `json:"milestone,omitempty"`
	Priorities    []int    `json:"priorities,omitempty"`
	UpdatedWithin string   `json:"updated_within,omitempty"`
	CreatedBy     string   `json:"created_by,omitempty"`
	Query         string   `json:"query,omitempty"`
	OrderBy       string   `json:"order_by,omitempty"`
}

// QuickAction is a one-click patch a view offers on each of its rows: a name
// on the button and the fields it changes. It goes through UpdateIssue like
// any other write, so it carries the same audit event and version check.
type QuickAction struct {
	Name         string   `json:"name"`
	Status       string   `json:"status,omitempty"`
	Priority     *int     `json:"priority,omitempty"`
	AddLabels    []string `json:"add_labels,omitempty"`
	RemoveLabels []string `json:"remove_labels,omitempty"`
}

type ViewInput struct {
	Name         string
	Description  string
	Filter       ViewFilter
	QuickActions []QuickAction
	Shared       *bool // nil means shared
}

// ViewPatch changes only the fields it carries. Filter and QuickActions are
// replaced whole: a view is small enough to read, amend and write back.
type ViewPatch struct {
	Name         *string
	Description  *string
	Filter       *ViewFilter
	QuickActions *[]QuickAction
	Shared       *bool
	Archived     *bool
}
