package server

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mikedclarke/trackd/internal/store"
)

// mcpHandler serves MCP over streamable HTTP. It sits behind the same bearer
// auth as the REST API; the MCP server is built per request (stateless mode)
// so each tool handler closes over the authenticated token's actor.
func (s *Server) mcpHandler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		tokenActor := ""
		if t, ok := r.Context().Value(tokenKey).(*store.Token); ok {
			tokenActor = t.Name
		}
		return s.newMCPServer(tokenActor)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
}

type mcpListIssuesIn struct {
	Status          string `json:"status,omitempty" jsonschema:"filter by status name, e.g. Todo or In Progress"`
	StatusType      string `json:"status_type,omitempty" jsonschema:"filter by status type: triage, backlog, unstarted, started, completed, or canceled"`
	Project         string `json:"project,omitempty" jsonschema:"filter by project slug"`
	Label           string `json:"label,omitempty" jsonschema:"filter by label name"`
	Parent          string `json:"parent,omitempty" jsonschema:"filter by parent issue key"`
	Query           string `json:"query,omitempty" jsonschema:"substring search over key, title, and description"`
	UpdatedSince    string `json:"updated_since,omitempty" jsonschema:"only issues updated at or after this RFC3339 timestamp"`
	IncludeArchived bool   `json:"include_archived,omitempty" jsonschema:"include archived issues"`
	Limit           int    `json:"limit,omitempty" jsonschema:"maximum results, default 100"`
	Offset          int    `json:"offset,omitempty" jsonschema:"skip this many results"`
}

type mcpIssuesOut struct {
	Issues []store.Issue `json:"issues"`
}

type mcpGetIssueIn struct {
	Key string `json:"key" jsonschema:"issue key, e.g. TSK-42"`
}

type mcpIssueDetailOut struct {
	Issue     store.Issue      `json:"issue"`
	Comments  []store.Comment  `json:"comments"`
	Relations []store.Relation `json:"relations"`
}

type mcpSaveIssueIn struct {
	Key         string   `json:"key,omitempty" jsonschema:"issue key to update; omit to create a new issue"`
	Title       *string  `json:"title,omitempty" jsonschema:"issue title (required when creating)"`
	Description *string  `json:"description,omitempty" jsonschema:"issue description in markdown"`
	Status      *string  `json:"status,omitempty" jsonschema:"status name, e.g. Todo, In Progress, Done"`
	Priority    *int     `json:"priority,omitempty" jsonschema:"priority 0-4: 0 none, 1 urgent, 2 high, 3 medium, 4 low"`
	Project     *string  `json:"project,omitempty" jsonschema:"project slug; empty string clears"`
	Parent      *string  `json:"parent,omitempty" jsonschema:"parent issue key; empty string clears"`
	DueDate     *string  `json:"due_date,omitempty" jsonschema:"due date YYYY-MM-DD; empty string clears"`
	Labels      []string `json:"labels,omitempty" jsonschema:"replaces the full label set; omit to leave unchanged"`
	Archived    *bool    `json:"archived,omitempty" jsonschema:"archive or unarchive the issue"`
	Actor       string   `json:"actor,omitempty" jsonschema:"actor recorded on the audit trail; defaults to the API token's name"`
}

type mcpAddCommentIn struct {
	Key   string `json:"key" jsonschema:"issue key to comment on"`
	Body  string `json:"body" jsonschema:"comment body in markdown"`
	Actor string `json:"actor,omitempty" jsonschema:"actor recorded on the comment; defaults to the API token's name"`
}

type mcpListProjectsIn struct {
	IncludeArchived bool `json:"include_archived,omitempty" jsonschema:"include archived projects"`
}

type mcpProjectsOut struct {
	Projects []store.Project `json:"projects"`
}

type mcpSaveProjectIn struct {
	Slug        string   `json:"slug,omitempty" jsonschema:"project slug to update; omit to create a new project"`
	Name        *string  `json:"name,omitempty" jsonschema:"project name (required when creating)"`
	Description *string  `json:"description,omitempty" jsonschema:"project description in markdown"`
	Status      *string  `json:"status,omitempty" jsonschema:"project status: active, paused, completed, or canceled"`
	Labels      []string `json:"labels,omitempty" jsonschema:"replaces the full label set; omit to leave unchanged"`
	Archived    *bool    `json:"archived,omitempty" jsonschema:"archive or unarchive the project"`
	Actor       string   `json:"actor,omitempty" jsonschema:"actor recorded on the audit trail; defaults to the API token's name"`
}

type mcpLabelsOut struct {
	Labels []store.Label `json:"labels"`
}

func (s *Server) newMCPServer(tokenActor string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "trackd", Title: "trackd", Version: s.version}, nil)
	resolve := func(explicit string) string {
		if explicit != "" {
			return explicit
		}
		return tokenActor
	}

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_issues",
		Description: "List issues, optionally filtered by status, project, label, parent, or a text query.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpListIssuesIn) (*mcp.CallToolResult, mcpIssuesOut, error) {
		issues, err := s.store.ListIssues(store.IssueFilter{
			Status:          in.Status,
			StatusType:      in.StatusType,
			Project:         in.Project,
			Label:           in.Label,
			Parent:          in.Parent,
			Query:           in.Query,
			UpdatedSince:    in.UpdatedSince,
			IncludeArchived: in.IncludeArchived,
			Limit:           in.Limit,
			Offset:          in.Offset,
		})
		if issues == nil {
			issues = []store.Issue{}
		}
		return nil, mcpIssuesOut{Issues: issues}, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_issue",
		Description: "Get one issue by key, with its comments and relations.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpGetIssueIn) (*mcp.CallToolResult, mcpIssueDetailOut, error) {
		var out mcpIssueDetailOut
		issue, err := s.store.GetIssue(in.Key)
		if err != nil {
			return nil, out, err
		}
		comments, err := s.store.ListComments(in.Key)
		if err != nil {
			return nil, out, err
		}
		relations, err := s.store.ListRelations(in.Key)
		if err != nil {
			return nil, out, err
		}
		if comments == nil {
			comments = []store.Comment{}
		}
		if relations == nil {
			relations = []store.Relation{}
		}
		return nil, mcpIssueDetailOut{Issue: *issue, Comments: comments, Relations: relations}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "save_issue",
		Description: "Create an issue (omit key) or update an existing one (pass key). Only provided fields change.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpSaveIssueIn) (*mcp.CallToolResult, store.Issue, error) {
		act := resolve(in.Actor)
		if in.Key == "" {
			issue, err := s.store.CreateIssue(store.IssueInput{
				Title:       strOr(in.Title),
				Description: strOr(in.Description),
				Status:      strOr(in.Status),
				Priority:    intOr(in.Priority),
				Project:     strOr(in.Project),
				Parent:      strOr(in.Parent),
				DueDate:     strOr(in.DueDate),
				Labels:      in.Labels,
			}, act)
			if err != nil {
				return nil, store.Issue{}, err
			}
			return nil, *issue, nil
		}
		patch := store.IssuePatch{
			Title:       in.Title,
			Description: in.Description,
			Status:      in.Status,
			Priority:    in.Priority,
			Project:     in.Project,
			Parent:      in.Parent,
			DueDate:     in.DueDate,
			Archived:    in.Archived,
		}
		if in.Labels != nil {
			patch.Labels = &in.Labels
		}
		issue, err := s.store.UpdateIssue(in.Key, patch, act)
		if err != nil {
			return nil, store.Issue{}, err
		}
		return nil, *issue, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "add_comment",
		Description: "Add a comment to an issue.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpAddCommentIn) (*mcp.CallToolResult, store.Comment, error) {
		comment, err := s.store.AddComment(in.Key, in.Body, resolve(in.Actor))
		if err != nil {
			return nil, store.Comment{}, err
		}
		return nil, *comment, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_projects",
		Description: "List projects.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpListProjectsIn) (*mcp.CallToolResult, mcpProjectsOut, error) {
		projects, err := s.store.ListProjects(in.IncludeArchived)
		if projects == nil {
			projects = []store.Project{}
		}
		return nil, mcpProjectsOut{Projects: projects}, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "save_project",
		Description: "Create a project (omit slug) or update an existing one (pass slug). Only provided fields change.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpSaveProjectIn) (*mcp.CallToolResult, store.Project, error) {
		act := resolve(in.Actor)
		if in.Slug == "" {
			project, err := s.store.CreateProject(store.ProjectInput{
				Name:        strOr(in.Name),
				Description: strOr(in.Description),
				Status:      strOr(in.Status),
			}, act)
			if err != nil {
				return nil, store.Project{}, err
			}
			if in.Labels != nil {
				if project, err = s.store.SetProjectLabels(project.Slug, in.Labels, act); err != nil {
					return nil, store.Project{}, err
				}
			}
			return nil, *project, nil
		}
		project, err := s.store.UpdateProject(in.Slug, store.ProjectPatch{
			Name:        in.Name,
			Description: in.Description,
			Status:      in.Status,
			Archived:    in.Archived,
		}, act)
		if err != nil {
			return nil, store.Project{}, err
		}
		if in.Labels != nil {
			if project, err = s.store.SetProjectLabels(in.Slug, in.Labels, act); err != nil {
				return nil, store.Project{}, err
			}
		}
		return nil, *project, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_labels",
		Description: "List all labels.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, mcpLabelsOut, error) {
		labels, err := s.store.ListLabels()
		if labels == nil {
			labels = []store.Label{}
		}
		return nil, mcpLabelsOut{Labels: labels}, err
	})

	return srv
}

func strOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func intOr(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
