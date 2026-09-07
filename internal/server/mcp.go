package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mikedclarke/trackd/internal/store"
)

// mcpHandler serves MCP over streamable HTTP. It sits behind the same bearer
// auth as the REST API; the MCP server is built per request (stateless mode)
// so each tool handler closes over the authenticated token's actor and role.
func (s *Server) mcpHandler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		tokenActor, role := "", ""
		if t, ok := r.Context().Value(tokenKey).(*store.Token); ok {
			tokenActor, role = t.Name, t.Role
		}
		return s.newMCPServer(tokenActor, role)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
}

// Annotation values the SDK models as pointers because their spec default is
// true. Nothing trackd exposes destroys data or reaches outside the database,
// so both are false on every tool.
var (
	notDestructive = false
	closedWorld    = false
)

func readOnlyTool() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &notDestructive, OpenWorldHint: &closedWorld}
}

// writeTool marks a mutating tool. idempotent says whether calling it twice
// with the same arguments leaves the same state: true for the tools addressed
// by a key, slug or id, false for one that appends a new row each call.
func writeTool(idempotent bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		IdempotentHint:  idempotent,
		DestructiveHint: &notDestructive,
		OpenWorldHint:   &closedWorld,
	}
}

// enumSchema infers a tool's input schema and pins enum values onto the named
// properties. The jsonschema struct tag carries only a description, so
// amending the inferred schema is the only way to publish an enum; a bad
// property name here is a programming error and panics at server
// construction, which every MCP test exercises.
func enumSchema[In any](enums map[string][]any) *jsonschema.Schema {
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		panic(fmt.Sprintf("infer mcp input schema: %v", err))
	}
	for name, values := range enums {
		prop, ok := schema.Properties[name]
		if !ok {
			panic("enum on unknown property " + name)
		}
		prop.Enum = values
	}
	return schema
}

// mcpError prefixes the error class onto the message, so an agent reading a
// tool failure can tell a bad reference from a conflict without parsing prose.
func mcpError(err error) error {
	if err == nil {
		return nil
	}
	_, code := classify(err)
	return fmt.Errorf("[%s] %s", code, err.Error())
}

type mcpListIssuesIn struct {
	Statuses       []string `json:"statuses,omitempty" jsonschema:"status names to include, e.g. Todo or In Progress; any match"`
	StatusTypes    []string `json:"status_types,omitempty" jsonschema:"status types to include: triage, backlog, unstarted, started, completed, canceled; any match"`
	Project        string   `json:"project,omitempty" jsonschema:"project slug"`
	Labels         []string `json:"labels,omitempty" jsonschema:"label names the issue must all carry"`
	ExcludeLabels  []string `json:"exclude_labels,omitempty" jsonschema:"label names that exclude an issue"`
	Parent         string   `json:"parent,omitempty" jsonschema:"parent issue key"`
	Assignee       string   `json:"assignee,omitempty" jsonschema:"assignee name"`
	Milestone      string   `json:"milestone,omitempty" jsonschema:"milestone name"`
	Query          string   `json:"query,omitempty" jsonschema:"substring search over key, title, description and comment bodies"`
	UpdatedSince   string   `json:"updated_since,omitempty" jsonschema:"only issues updated at or after this RFC3339 timestamp"`
	CompletedSince string   `json:"completed_since,omitempty" jsonschema:"only issues completed at or after this RFC3339 timestamp"`
	Archived       string   `json:"archived,omitempty" jsonschema:"archived issues: empty excludes them, true includes them, only returns just them"`
	OrderBy        string   `json:"order_by,omitempty" jsonschema:"sort order: updated (newest first, the default), created (oldest first) or priority"`
	Limit          int      `json:"limit,omitempty" jsonschema:"maximum results, default 100, capped at 500"`
	Offset         int      `json:"offset,omitempty" jsonschema:"skip this many results"`
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
	Mode               string    `json:"mode" jsonschema:"create a new issue or update an existing one: create requires title and rejects key, update requires key"`
	Key                string    `json:"key,omitempty" jsonschema:"issue key to update; required with mode update, rejected with mode create"`
	Title              *string   `json:"title,omitempty" jsonschema:"issue title; required with mode create"`
	Description        *string   `json:"description,omitempty" jsonschema:"issue description in markdown; on update it needs replace_description, because descriptions are append-only"`
	AppendDescription  string    `json:"append_description,omitempty" jsonschema:"text to add to the end of the description, the safe way to add to another agent's notes"`
	ReplaceDescription bool      `json:"replace_description,omitempty" jsonschema:"allow description to overwrite an existing description instead of appending; admin tokens only"`
	Status             *string   `json:"status,omitempty" jsonschema:"status name: Triage, Backlog, Todo, In Progress, In Review, Done, Canceled or Duplicate"`
	Priority           *int      `json:"priority,omitempty" jsonschema:"priority 0-4: 0 none, 1 urgent, 2 high, 3 medium, 4 low"`
	Project            *string   `json:"project,omitempty" jsonschema:"project slug; empty string clears"`
	Parent             *string   `json:"parent,omitempty" jsonschema:"parent issue key; empty string clears"`
	Assignee           *string   `json:"assignee,omitempty" jsonschema:"assignee name (person or agent); empty string clears"`
	Milestone          *string   `json:"milestone,omitempty" jsonschema:"milestone name within the issue's project; empty string clears"`
	DueDate            *string   `json:"due_date,omitempty" jsonschema:"due date YYYY-MM-DD; empty string clears"`
	Labels             *[]string `json:"labels,omitempty" jsonschema:"replaces the whole label set; an empty array needs clear_labels, and every name must already exist"`
	ClearLabels        bool      `json:"clear_labels,omitempty" jsonschema:"confirm that an empty labels array really means remove every label"`
	AddLabels          []string  `json:"add_labels,omitempty" jsonschema:"label names to add, which cannot be combined with labels"`
	RemoveLabels       []string  `json:"remove_labels,omitempty" jsonschema:"label names to remove, which cannot be combined with labels"`
	ExpectedVersion    *int64    `json:"expected_version,omitempty" jsonschema:"fail with a version conflict unless the issue is still at this version"`
	IdempotencyKey     string    `json:"idempotency_key,omitempty" jsonschema:"on create, a repeat with the same key returns the original issue instead of a duplicate"`
	Archived           *bool     `json:"archived,omitempty" jsonschema:"archive or unarchive the issue"`
	Actor              string    `json:"actor,omitempty" jsonschema:"actor recorded on the audit trail; defaults to the API token's name"`
}

type mcpAddCommentIn struct {
	Key            string `json:"key" jsonschema:"issue key to comment on"`
	Body           string `json:"body" jsonschema:"comment body in markdown"`
	ParentID       int64  `json:"parent_id,omitempty" jsonschema:"id of a comment on the same issue, to reply in a thread"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"a repeat with the same key returns the original comment instead of a duplicate"`
	Actor          string `json:"actor,omitempty" jsonschema:"actor recorded on the comment; defaults to the API token's name"`
}

type mcpListProjectsIn struct {
	IncludeArchived bool `json:"include_archived,omitempty" jsonschema:"include archived projects"`
}

type mcpProjectsOut struct {
	Projects []store.Project `json:"projects"`
}

type mcpSaveProjectIn struct {
	Slug         string    `json:"slug,omitempty" jsonschema:"project slug to update; omit to create a new project"`
	Name         *string   `json:"name,omitempty" jsonschema:"project name; required when creating"`
	Description  *string   `json:"description,omitempty" jsonschema:"project description in markdown"`
	Status       *string   `json:"status,omitempty" jsonschema:"project status: backlog, planned, started, paused, completed or canceled"`
	StartDate    *string   `json:"start_date,omitempty" jsonschema:"start date YYYY-MM-DD; empty string clears"`
	TargetDate   *string   `json:"target_date,omitempty" jsonschema:"target date YYYY-MM-DD; empty string clears"`
	Labels       *[]string `json:"labels,omitempty" jsonschema:"replaces the whole label set; an empty array needs clear_labels, and every name must already exist"`
	ClearLabels  bool      `json:"clear_labels,omitempty" jsonschema:"confirm that an empty labels array really means remove every label"`
	AddLabels    []string  `json:"add_labels,omitempty" jsonschema:"label names to add, which cannot be combined with labels"`
	RemoveLabels []string  `json:"remove_labels,omitempty" jsonschema:"label names to remove, which cannot be combined with labels"`
	Archived     *bool     `json:"archived,omitempty" jsonschema:"archive or unarchive the project"`
	Actor        string    `json:"actor,omitempty" jsonschema:"actor recorded on the audit trail; defaults to the API token's name"`
}

type mcpLabelsOut struct {
	Labels []store.Label `json:"labels"`
}

type mcpStatusesOut struct {
	Statuses []store.Status `json:"statuses"`
}

type mcpListMilestonesIn struct {
	Project         string `json:"project,omitempty" jsonschema:"filter by project slug"`
	IncludeArchived bool   `json:"include_archived,omitempty" jsonschema:"include archived milestones"`
}

type mcpMilestonesOut struct {
	Milestones []store.Milestone `json:"milestones"`
}

type mcpSaveMilestoneIn struct {
	ID          int64   `json:"id,omitempty" jsonschema:"milestone id to update; omit to create a new milestone"`
	Project     string  `json:"project,omitempty" jsonschema:"project slug the milestone belongs to; required when creating"`
	Name        *string `json:"name,omitempty" jsonschema:"milestone name; required when creating"`
	Description *string `json:"description,omitempty" jsonschema:"milestone description"`
	TargetDate  *string `json:"target_date,omitempty" jsonschema:"target date YYYY-MM-DD; empty string clears"`
	Archived    *bool   `json:"archived,omitempty" jsonschema:"archive or unarchive the milestone"`
	Actor       string  `json:"actor,omitempty" jsonschema:"actor recorded on the audit trail; defaults to the API token's name"`
}

type mcpSaveRelationIn struct {
	Key     string `json:"key" jsonschema:"issue key the relation starts from"`
	Related string `json:"related" jsonschema:"issue key the relation points at"`
	Type    string `json:"type" jsonschema:"relation type: blocks, relates or duplicate"`
	Remove  bool   `json:"remove,omitempty" jsonschema:"remove this relation instead of adding it"`
	Actor   string `json:"actor,omitempty" jsonschema:"actor recorded on the audit trail; defaults to the API token's name"`
}

type mcpRelationsOut struct {
	Relations []store.Relation `json:"relations"`
	// Removed answers a removal only: false says there was no such relation,
	// which is still a success because removal is idempotent.
	Removed *bool `json:"removed,omitempty"`
}

type mcpListActivityIn struct {
	Since   string `json:"since,omitempty" jsonschema:"only events at or after this RFC3339 timestamp"`
	AfterID int64  `json:"after_id,omitempty" jsonschema:"only events with an id above this, the cursor for paging forward"`
	Entity  string `json:"entity,omitempty" jsonschema:"restrict to one entity kind: issue, project, milestone or token"`
	Limit   int    `json:"limit,omitempty" jsonschema:"maximum events, default 100, capped at 1000"`
}

type mcpActivityOut struct {
	Events []mcpEvent `json:"events"`
}

// mcpEvent restates store.Event with its before and after states as free-form
// JSON. The store carries them as json.RawMessage, whose Go type is a byte
// slice, and the schema inference would publish that as an array of numbers.
type mcpEvent struct {
	ID        int64  `json:"id"`
	Entity    string `json:"entity"`
	EntityID  int64  `json:"entity_id"`
	EntityKey string `json:"entity_key,omitempty"`
	Actor     string `json:"actor,omitempty"`
	Action    string `json:"action"`
	Before    any    `json:"before,omitempty"`
	After     any    `json:"after,omitempty"`
	CreatedAt string `json:"created_at"`
}

func toMCPEvents(events []store.Event) ([]mcpEvent, error) {
	out := make([]mcpEvent, 0, len(events))
	for _, e := range events {
		m := mcpEvent{
			ID: e.ID, Entity: e.Entity, EntityID: e.EntityID, EntityKey: e.EntityKey,
			Actor: e.Actor, Action: e.Action, CreatedAt: e.CreatedAt,
		}
		for _, conv := range []struct {
			raw json.RawMessage
			dst *any
		}{{e.Before, &m.Before}, {e.After, &m.After}} {
			if len(conv.raw) == 0 {
				continue
			}
			if err := json.Unmarshal(conv.raw, conv.dst); err != nil {
				return nil, err
			}
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *Server) newMCPServer(tokenActor, role string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "trackd", Title: "trackd", Version: s.version}, nil)
	resolve := func(explicit string) string {
		if explicit != "" {
			return explicit
		}
		return tokenActor
	}

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_issues",
		Annotations: readOnlyTool(),
		Description: "List issues filtered by any combination of status names, status types (triage, backlog, unstarted, started, completed, canceled), project, labels, parent, assignee, milestone, text query and timestamps, ordered by updated, created or priority.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpListIssuesIn) (*mcp.CallToolResult, mcpIssuesOut, error) {
		issues, err := s.store.ListIssues(store.IssueFilter{
			Statuses:       in.Statuses,
			StatusTypes:    in.StatusTypes,
			Project:        in.Project,
			Labels:         in.Labels,
			ExcludeLabels:  in.ExcludeLabels,
			Parent:         in.Parent,
			Assignee:       in.Assignee,
			Milestone:      in.Milestone,
			Query:          in.Query,
			UpdatedSince:   in.UpdatedSince,
			CompletedSince: in.CompletedSince,
			Archived:       in.Archived,
			OrderBy:        in.OrderBy,
			Limit:          in.Limit,
			Offset:         in.Offset,
		})
		if err != nil {
			return nil, mcpIssuesOut{}, mcpError(err)
		}
		return nil, mcpIssuesOut{Issues: issues}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_issue",
		Annotations: readOnlyTool(),
		Description: "Get one issue by key together with its comments and its relations.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpGetIssueIn) (*mcp.CallToolResult, mcpIssueDetailOut, error) {
		var out mcpIssueDetailOut
		issue, err := s.store.GetIssue(in.Key)
		if err != nil {
			return nil, out, mcpError(err)
		}
		comments, err := s.store.ListComments(in.Key)
		if err != nil {
			return nil, out, mcpError(err)
		}
		relations, err := s.store.ListRelations(in.Key)
		if err != nil {
			return nil, out, mcpError(err)
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
		Annotations: writeTool(true),
		Description: "Create an issue with mode create (title required, key rejected) or change one with mode update (key required); only the fields you pass change, descriptions are append-only unless replace_description is set (which needs an admin token), and every label name must already exist.",
		InputSchema: enumSchema[mcpSaveIssueIn](map[string][]any{"mode": {"create", "update"}}),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpSaveIssueIn) (*mcp.CallToolResult, store.Issue, error) {
		act := resolve(in.Actor)
		labels, err := mcpLabels(in.Labels, in.ClearLabels)
		if err != nil {
			return nil, store.Issue{}, mcpError(err)
		}
		switch in.Mode {
		case "create":
			if in.Key != "" {
				return nil, store.Issue{}, mcpError(errors.New("mode create does not take a key"))
			}
			if in.AppendDescription != "" {
				return nil, store.Issue{}, mcpError(errors.New("mode create takes description, not append_description"))
			}
			issue, _, err := s.store.CreateIssue(store.IssueInput{
				Title:          strOr(in.Title),
				Description:    strOr(in.Description),
				Status:         strOr(in.Status),
				Priority:       intOr(in.Priority),
				Project:        strOr(in.Project),
				Parent:         strOr(in.Parent),
				Assignee:       strOr(in.Assignee),
				Milestone:      strOr(in.Milestone),
				DueDate:        strOr(in.DueDate),
				Labels:         sliceOr(labels),
				IdempotencyKey: in.IdempotencyKey,
			}, act)
			if err != nil {
				return nil, store.Issue{}, mcpError(err)
			}
			return nil, *issue, nil
		case "update":
			if in.Key == "" {
				return nil, store.Issue{}, mcpError(errors.New("mode update needs a key"))
			}
			if in.ReplaceDescription && role != roleAdmin {
				return nil, store.Issue{}, mcpError(errAdminOnly)
			}
			var appended *store.Issue
			if in.AppendDescription != "" {
				if appended, err = s.store.AppendDescription(in.Key, in.AppendDescription, act); err != nil {
					return nil, store.Issue{}, mcpError(err)
				}
			}
			patch := store.IssuePatch{
				Title:              in.Title,
				Description:        in.Description,
				ReplaceDescription: in.ReplaceDescription,
				Status:             in.Status,
				Priority:           in.Priority,
				Project:            in.Project,
				Parent:             in.Parent,
				Assignee:           in.Assignee,
				Milestone:          in.Milestone,
				DueDate:            in.DueDate,
				Labels:             labels,
				AddLabels:          in.AddLabels,
				RemoveLabels:       in.RemoveLabels,
				ExpectedVersion:    in.ExpectedVersion,
				Archived:           in.Archived,
			}
			if mcpPatchEmpty(patch) {
				if appended != nil {
					return nil, *appended, nil
				}
				return nil, store.Issue{}, mcpError(errors.New("nothing to update: pass at least one field"))
			}
			// The append has already bumped the version, so an
			// expected_version sent alongside it would now be stale.
			if appended != nil {
				patch.ExpectedVersion = nil
			}
			updated, err := s.store.UpdateIssue(in.Key, patch, act)
			if err != nil {
				return nil, store.Issue{}, mcpError(err)
			}
			return nil, *updated, nil
		default:
			return nil, store.Issue{}, mcpError(fmt.Errorf("mode must be create or update, got %q", in.Mode))
		}
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "add_comment",
		// A repeat without an idempotency key adds a second comment, so this
		// one is honestly not idempotent.
		Annotations: writeTool(false),
		Description: "Add a comment to an issue, optionally as a reply to another comment on the same issue.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpAddCommentIn) (*mcp.CallToolResult, store.Comment, error) {
		comment, _, err := s.store.AddComment(in.Key, store.CommentInput{
			Body:           in.Body,
			ParentID:       in.ParentID,
			IdempotencyKey: in.IdempotencyKey,
		}, resolve(in.Actor))
		if err != nil {
			return nil, store.Comment{}, mcpError(err)
		}
		return nil, *comment, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_projects",
		Annotations: readOnlyTool(),
		Description: "List projects with their status, labels and dates, optionally including archived ones.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpListProjectsIn) (*mcp.CallToolResult, mcpProjectsOut, error) {
		projects, err := s.store.ListProjects(in.IncludeArchived)
		if err != nil {
			return nil, mcpProjectsOut{}, mcpError(err)
		}
		if projects == nil {
			projects = []store.Project{}
		}
		return nil, mcpProjectsOut{Projects: projects}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "save_project",
		Annotations: writeTool(true),
		Description: "Create a project (omit slug, pass name) or change one (pass slug), where status is one of backlog, planned, started, paused, completed or canceled and only the fields you pass change.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpSaveProjectIn) (*mcp.CallToolResult, store.Project, error) {
		act := resolve(in.Actor)
		labels, err := mcpLabels(in.Labels, in.ClearLabels)
		if err != nil {
			return nil, store.Project{}, mcpError(err)
		}
		if in.Slug == "" {
			project, err := s.store.CreateProject(store.ProjectInput{
				Name:        strOr(in.Name),
				Description: strOr(in.Description),
				Status:      strOr(in.Status),
				StartDate:   strOr(in.StartDate),
				TargetDate:  strOr(in.TargetDate),
				Labels:      sliceOr(labels),
			}, act)
			if err != nil {
				return nil, store.Project{}, mcpError(err)
			}
			return nil, *project, nil
		}
		project, err := s.store.UpdateProject(in.Slug, store.ProjectPatch{
			Name:         in.Name,
			Description:  in.Description,
			Status:       in.Status,
			StartDate:    in.StartDate,
			TargetDate:   in.TargetDate,
			Labels:       labels,
			AddLabels:    in.AddLabels,
			RemoveLabels: in.RemoveLabels,
			Archived:     in.Archived,
		}, act)
		if err != nil {
			return nil, store.Project{}, mcpError(err)
		}
		return nil, *project, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_milestones",
		Annotations: readOnlyTool(),
		Description: "List milestones with their target dates, optionally narrowed to one project or including archived ones.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpListMilestonesIn) (*mcp.CallToolResult, mcpMilestonesOut, error) {
		milestones, err := s.store.ListMilestones(in.Project, in.IncludeArchived)
		if err != nil {
			return nil, mcpMilestonesOut{}, mcpError(err)
		}
		if milestones == nil {
			milestones = []store.Milestone{}
		}
		return nil, mcpMilestonesOut{Milestones: milestones}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "save_milestone",
		Annotations: writeTool(true),
		Description: "Create a milestone (omit id, pass project and name) or change one (pass id), where only the fields you pass change.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpSaveMilestoneIn) (*mcp.CallToolResult, store.Milestone, error) {
		act := resolve(in.Actor)
		if in.ID == 0 {
			milestone, err := s.store.CreateMilestone(store.MilestoneInput{
				Project:     in.Project,
				Name:        strOr(in.Name),
				Description: strOr(in.Description),
				TargetDate:  strOr(in.TargetDate),
			}, act)
			if err != nil {
				return nil, store.Milestone{}, mcpError(err)
			}
			return nil, *milestone, nil
		}
		milestone, err := s.store.UpdateMilestone(in.ID, store.MilestonePatch{
			Name:        in.Name,
			Description: in.Description,
			TargetDate:  in.TargetDate,
			Archived:    in.Archived,
		}, act)
		if err != nil {
			return nil, store.Milestone{}, mcpError(err)
		}
		return nil, *milestone, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "save_relation",
		Annotations: writeTool(true),
		Description: "Link two issues with a relation of type blocks, relates or duplicate, or unlink them by passing remove. Unlinking is idempotent: removed reports whether there was a relation there.",
		InputSchema: enumSchema[mcpSaveRelationIn](map[string][]any{"type": {"blocks", "relates", "duplicate"}}),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpSaveRelationIn) (*mcp.CallToolResult, mcpRelationsOut, error) {
		act := resolve(in.Actor)
		var removed *bool
		var err error
		if in.Remove {
			var gone bool
			gone, err = s.store.RemoveRelation(in.Key, in.Related, in.Type, act)
			removed = &gone
		} else {
			err = s.store.AddRelation(in.Key, in.Related, in.Type, act)
		}
		if err != nil {
			return nil, mcpRelationsOut{}, mcpError(err)
		}
		relations, err := s.store.ListRelations(in.Key)
		if err != nil {
			return nil, mcpRelationsOut{}, mcpError(err)
		}
		if relations == nil {
			relations = []store.Relation{}
		}
		return nil, mcpRelationsOut{Relations: relations, Removed: removed}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_labels",
		Annotations: readOnlyTool(),
		Description: "List every label that exists, which is the set an issue or project write may draw on.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, mcpLabelsOut, error) {
		labels, err := s.store.ListLabels()
		if err != nil {
			return nil, mcpLabelsOut{}, mcpError(err)
		}
		if labels == nil {
			labels = []store.Label{}
		}
		return nil, mcpLabelsOut{Labels: labels}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_statuses",
		Annotations: readOnlyTool(),
		Description: "List the workflow statuses in board order with the type of each: triage, backlog, unstarted, started, completed or canceled.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, mcpStatusesOut, error) {
		statuses, err := s.store.ListStatuses()
		if err != nil {
			return nil, mcpStatusesOut{}, mcpError(err)
		}
		if statuses == nil {
			statuses = []store.Status{}
		}
		return nil, mcpStatusesOut{Statuses: statuses}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_activity",
		Annotations: readOnlyTool(),
		Description: "Read the audit feed oldest first, filtered by timestamp, entity kind (issue, project, milestone, token) or an after_id cursor, so a caller can page forward without missing an event.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpListActivityIn) (*mcp.CallToolResult, mcpActivityOut, error) {
		events, err := s.store.ListAllEvents(store.EventFilter{
			Since:   in.Since,
			AfterID: in.AfterID,
			Entity:  in.Entity,
			Limit:   in.Limit,
		})
		if err != nil {
			return nil, mcpActivityOut{}, mcpError(err)
		}
		out, err := toMCPEvents(events)
		if err != nil {
			return nil, mcpActivityOut{}, mcpError(err)
		}
		return nil, mcpActivityOut{Events: out}, nil
	})

	return srv
}

// mcpLabels guards the one destructive thing a label field can do. An omitted
// labels field leaves the set alone; an empty array would silently strip every
// label, so it only counts when the caller says it meant it.
func mcpLabels(labels *[]string, clear bool) (*[]string, error) {
	if labels == nil {
		return nil, nil
	}
	if len(*labels) == 0 && !clear {
		return nil, errors.New("labels: an empty array removes every label; pass clear_labels true to confirm")
	}
	return labels, nil
}

// mcpPatchEmpty reports whether an update would change nothing, which is worth
// saying out loud rather than recording an audit event for a no-op.
func mcpPatchEmpty(p store.IssuePatch) bool {
	return p.Title == nil && p.Description == nil && p.Status == nil && p.Priority == nil &&
		p.Project == nil && p.Parent == nil && p.Assignee == nil && p.Milestone == nil &&
		p.DueDate == nil && p.Labels == nil && p.Archived == nil &&
		len(p.AddLabels) == 0 && len(p.RemoveLabels) == 0
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

func sliceOr(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}
