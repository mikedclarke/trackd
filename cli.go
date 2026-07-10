package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/mikedclarke/trackd/internal/client"
	"github.com/mikedclarke/trackd/internal/store"
)

// commonFlags are shared by every client command. The server address and
// token come from flags first, then TRACKD_URL / TRACKD_TOKEN.
type commonFlags struct {
	url     *string
	token   *string
	jsonOut *bool
}

func addCommon(fs *flag.FlagSet) *commonFlags {
	return &commonFlags{
		url:     fs.String("url", "", "server URL (default $TRACKD_URL, then http://127.0.0.1:8484)"),
		token:   fs.String("token", "", "API token (default $TRACKD_TOKEN)"),
		jsonOut: fs.Bool("json", false, "output JSON instead of a table"),
	}
}

func (c *commonFlags) client() *client.Client {
	base := *c.url
	if base == "" {
		base = os.Getenv("TRACKD_URL")
	}
	if base == "" {
		base = "http://127.0.0.1:8484"
	}
	token := *c.token
	if token == "" {
		token = os.Getenv("TRACKD_TOKEN")
	}
	return client.New(base, token)
}

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// stringSlice implements a repeatable string flag.
type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

// readValue returns v, or all of stdin when v is "-", so long markdown bodies
// can be piped in.
func readValue(v string) (string, error) {
	if v != "-" {
		return v, nil
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\n"), nil
}

// leadingArgs splits n positional arguments that must come before any flags,
// e.g. "trackd issue update TSK-1 --status Done".
func leadingArgs(args []string, n int, usage string) ([]string, []string, error) {
	if len(args) < n {
		return nil, nil, fmt.Errorf("usage: %s", usage)
	}
	for _, a := range args[:n] {
		if strings.HasPrefix(a, "-") {
			return nil, nil, fmt.Errorf("usage: %s", usage)
		}
	}
	return args[:n], args[n:], nil
}

func cmdIssue(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: trackd issue <list|show|create|update|comment|relate|events> [flags]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return issueList(rest)
	case "show":
		return issueShow(rest)
	case "create":
		return issueCreate(rest)
	case "update":
		return issueUpdate(rest)
	case "comment":
		return issueComment(rest)
	case "relate":
		return issueRelate(rest)
	case "events":
		return issueEvents(rest)
	default:
		return fmt.Errorf("unknown issue subcommand %q", sub)
	}
}

func issueList(args []string) error {
	fs := flag.NewFlagSet("issue list", flag.ContinueOnError)
	common := addCommon(fs)
	status := fs.String("status", "", "filter by status name")
	statusType := fs.String("type", "", "filter by status type (triage|backlog|unstarted|started|completed|canceled)")
	project := fs.String("project", "", "filter by project slug")
	label := fs.String("label", "", "filter by label")
	parent := fs.String("parent", "", "filter by parent issue key")
	query := fs.String("q", "", "substring search over key, title, and description")
	updatedSince := fs.String("updated-since", "", "only issues updated at or after this RFC3339 time")
	archived := fs.Bool("archived", false, "include archived issues")
	limit := fs.Int("limit", 0, "maximum results (default 100)")
	offset := fs.Int("offset", 0, "skip this many results")
	if err := fs.Parse(args); err != nil {
		return err
	}
	q := url.Values{}
	for k, v := range map[string]string{
		"status": *status, "status_type": *statusType, "project": *project,
		"label": *label, "parent": *parent, "q": *query, "updated_since": *updatedSince,
	} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if *archived {
		q.Set("archived", "true")
	}
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	if *offset > 0 {
		q.Set("offset", strconv.Itoa(*offset))
	}
	var issues []store.Issue
	if err := common.client().Do("GET", "/api/v1/issues", q, nil, &issues); err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(issues)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\tSTATUS\tPRI\tPROJECT\tLABELS\tTITLE")
	for _, i := range issues {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n",
			i.Key, i.Status, i.Priority, i.Project, strings.Join(i.Labels, ","), truncate(i.Title, 70))
	}
	return w.Flush()
}

func issueShow(args []string) error {
	lead, rest, err := leadingArgs(args, 1, "trackd issue show <key> [flags]")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("issue show", flag.ContinueOnError)
	common := addCommon(fs)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	key := lead[0]
	c := common.client()
	var issue store.Issue
	if err := c.Do("GET", "/api/v1/issues/"+key, nil, nil, &issue); err != nil {
		return err
	}
	var comments []store.Comment
	if err := c.Do("GET", "/api/v1/issues/"+key+"/comments", nil, nil, &comments); err != nil {
		return err
	}
	var relations []store.Relation
	if err := c.Do("GET", "/api/v1/issues/"+key+"/relations", nil, nil, &relations); err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(map[string]any{"issue": issue, "comments": comments, "relations": relations})
	}
	fmt.Printf("%s  %s (%s)  priority %d\n", issue.Key, issue.Status, issue.StatusType, issue.Priority)
	fmt.Println(issue.Title)
	fmt.Println()
	fmt.Printf("project: %s  parent: %s  due: %s\n", orDash(issue.Project), orDash(issue.Parent), orDash(issue.DueDate))
	fmt.Printf("labels:  %s\n", orDash(strings.Join(issue.Labels, ",")))
	fmt.Printf("created: %s  updated: %s\n", issue.CreatedAt, issue.UpdatedAt)
	if issue.ArchivedAt != "" {
		fmt.Printf("archived: %s\n", issue.ArchivedAt)
	}
	if issue.Description != "" {
		fmt.Println()
		fmt.Println(issue.Description)
	}
	if len(relations) > 0 {
		fmt.Println()
		fmt.Println("Relations:")
		for _, r := range relations {
			fmt.Printf("  %s %s %s\n", r.IssueKey, r.Type, r.RelatedKey)
		}
	}
	if len(comments) > 0 {
		fmt.Println()
		fmt.Printf("Comments (%d):\n", len(comments))
		for _, c := range comments {
			who := c.Actor
			if who == "" {
				who = "unknown"
			}
			fmt.Printf("  [%s %s] %s\n", c.CreatedAt, who, c.Body)
		}
	}
	return nil
}

func issueCreate(args []string) error {
	fs := flag.NewFlagSet("issue create", flag.ContinueOnError)
	common := addCommon(fs)
	title := fs.String("title", "", "issue title (required)")
	description := fs.String("description", "", "issue description; use - to read stdin")
	status := fs.String("status", "", "initial status (default Triage)")
	priority := fs.Int("priority", 0, "priority 0-4 (0 none, 1 urgent, 4 low)")
	project := fs.String("project", "", "project slug")
	parent := fs.String("parent", "", "parent issue key")
	due := fs.String("due", "", "due date (YYYY-MM-DD)")
	var labels stringSlice
	fs.Var(&labels, "label", "label to apply (repeatable)")
	actor := fs.String("actor", "", "actor recorded on the audit trail (default: token name)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	desc, err := readValue(*description)
	if err != nil {
		return err
	}
	body := map[string]any{"title": *title}
	if desc != "" {
		body["description"] = desc
	}
	if *status != "" {
		body["status"] = *status
	}
	if *priority != 0 {
		body["priority"] = *priority
	}
	if *project != "" {
		body["project"] = *project
	}
	if *parent != "" {
		body["parent"] = *parent
	}
	if *due != "" {
		body["due_date"] = *due
	}
	if len(labels) > 0 {
		body["labels"] = []string(labels)
	}
	if *actor != "" {
		body["actor"] = *actor
	}
	var issue store.Issue
	if err := common.client().Do("POST", "/api/v1/issues", nil, body, &issue); err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(issue)
	}
	fmt.Printf("%s created (%s)\n", issue.Key, issue.Status)
	return nil
}

func issueUpdate(args []string) error {
	lead, rest, err := leadingArgs(args, 1, "trackd issue update <key> [flags]")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("issue update", flag.ContinueOnError)
	common := addCommon(fs)
	title := fs.String("title", "", "new title")
	description := fs.String("description", "", "new description; use - to read stdin")
	status := fs.String("status", "", "new status")
	priority := fs.Int("priority", 0, "new priority 0-4")
	project := fs.String("project", "", "project slug; empty string clears")
	parent := fs.String("parent", "", "parent issue key; empty string clears")
	due := fs.String("due", "", "due date; empty string clears")
	labelsCSV := fs.String("labels", "", "comma-separated labels, replacing the current set; empty string clears")
	archive := fs.Bool("archive", false, "archive the issue")
	unarchive := fs.Bool("unarchive", false, "unarchive the issue")
	actor := fs.String("actor", "", "actor recorded on the audit trail (default: token name)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	body := map[string]any{}
	if set["title"] {
		body["title"] = *title
	}
	if set["description"] {
		desc, err := readValue(*description)
		if err != nil {
			return err
		}
		body["description"] = desc
	}
	if set["status"] {
		body["status"] = *status
	}
	if set["priority"] {
		body["priority"] = *priority
	}
	if set["project"] {
		body["project"] = *project
	}
	if set["parent"] {
		body["parent"] = *parent
	}
	if set["due"] {
		body["due_date"] = *due
	}
	if set["labels"] {
		body["labels"] = splitCSV(*labelsCSV)
	}
	if *archive {
		body["archived"] = true
	}
	if *unarchive {
		body["archived"] = false
	}
	if *actor != "" {
		body["actor"] = *actor
	}
	var issue store.Issue
	if err := common.client().Do("PATCH", "/api/v1/issues/"+lead[0], nil, body, &issue); err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(issue)
	}
	fmt.Printf("%s updated (%s)\n", issue.Key, issue.Status)
	return nil
}

func issueComment(args []string) error {
	lead, rest, err := leadingArgs(args, 1, "trackd issue comment <key> --body <text> [flags]")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("issue comment", flag.ContinueOnError)
	common := addCommon(fs)
	bodyFlag := fs.String("body", "", "comment body (required); use - to read stdin")
	actor := fs.String("actor", "", "actor recorded on the comment (default: token name)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	text, err := readValue(*bodyFlag)
	if err != nil {
		return err
	}
	body := map[string]any{"body": text}
	if *actor != "" {
		body["actor"] = *actor
	}
	var comment store.Comment
	if err := common.client().Do("POST", "/api/v1/issues/"+lead[0]+"/comments", nil, body, &comment); err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(comment)
	}
	fmt.Printf("comment added to %s as %s\n", comment.IssueKey, comment.Actor)
	return nil
}

func issueRelate(args []string) error {
	lead, rest, err := leadingArgs(args, 2, "trackd issue relate <key> <related-key> --type <blocks|relates|duplicate> [--remove]")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("issue relate", flag.ContinueOnError)
	common := addCommon(fs)
	typ := fs.String("type", "relates", "relation type: blocks, relates, or duplicate")
	remove := fs.Bool("remove", false, "remove the relation instead of adding it")
	actor := fs.String("actor", "", "actor recorded on the audit trail (default: token name)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	body := map[string]any{"related": lead[1], "type": *typ, "remove": *remove}
	if *actor != "" {
		body["actor"] = *actor
	}
	var relations []store.Relation
	if err := common.client().Do("POST", "/api/v1/issues/"+lead[0]+"/relations", nil, body, &relations); err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(relations)
	}
	for _, r := range relations {
		fmt.Printf("%s %s %s\n", r.IssueKey, r.Type, r.RelatedKey)
	}
	if len(relations) == 0 {
		fmt.Println("no relations")
	}
	return nil
}

func issueEvents(args []string) error {
	lead, rest, err := leadingArgs(args, 1, "trackd issue events <key> [flags]")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("issue events", flag.ContinueOnError)
	common := addCommon(fs)
	limit := fs.Int("limit", 0, "maximum events (default 100)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	q := url.Values{}
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	var events []store.Event
	if err := common.client().Do("GET", "/api/v1/issues/"+lead[0]+"/events", q, nil, &events); err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(events)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tACTION\tACTOR")
	for _, e := range events {
		fmt.Fprintf(w, "%s\t%s\t%s\n", e.CreatedAt, e.Action, orDash(e.Actor))
	}
	return w.Flush()
}

func cmdProject(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: trackd project <list|show|create|update> [flags]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return projectList(rest)
	case "show":
		return projectShow(rest)
	case "create":
		return projectCreate(rest)
	case "update":
		return projectUpdate(rest)
	default:
		return fmt.Errorf("unknown project subcommand %q", sub)
	}
}

func projectList(args []string) error {
	fs := flag.NewFlagSet("project list", flag.ContinueOnError)
	common := addCommon(fs)
	archived := fs.Bool("archived", false, "include archived projects")
	if err := fs.Parse(args); err != nil {
		return err
	}
	q := url.Values{}
	if *archived {
		q.Set("archived", "true")
	}
	var projects []store.Project
	if err := common.client().Do("GET", "/api/v1/projects", q, nil, &projects); err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(projects)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SLUG\tSTATUS\tLABELS\tNAME")
	for _, p := range projects {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Slug, p.Status, strings.Join(p.Labels, ","), p.Name)
	}
	return w.Flush()
}

func projectShow(args []string) error {
	lead, rest, err := leadingArgs(args, 1, "trackd project show <slug> [flags]")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("project show", flag.ContinueOnError)
	common := addCommon(fs)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	var project store.Project
	if err := common.client().Do("GET", "/api/v1/projects/"+lead[0], nil, nil, &project); err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(project)
	}
	fmt.Printf("%s  %s\n", project.Slug, project.Status)
	fmt.Println(project.Name)
	fmt.Printf("labels: %s\n", orDash(strings.Join(project.Labels, ",")))
	fmt.Printf("created: %s  updated: %s\n", project.CreatedAt, project.UpdatedAt)
	if project.Description != "" {
		fmt.Println()
		fmt.Println(project.Description)
	}
	return nil
}

func projectCreate(args []string) error {
	fs := flag.NewFlagSet("project create", flag.ContinueOnError)
	common := addCommon(fs)
	name := fs.String("name", "", "project name (required)")
	slug := fs.String("slug", "", "project slug (default: derived from name)")
	description := fs.String("description", "", "project description; use - to read stdin")
	status := fs.String("status", "", "project status (default active)")
	var labels stringSlice
	fs.Var(&labels, "label", "label to apply (repeatable)")
	actor := fs.String("actor", "", "actor recorded on the audit trail (default: token name)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	desc, err := readValue(*description)
	if err != nil {
		return err
	}
	body := map[string]any{"name": *name}
	if *slug != "" {
		body["slug"] = *slug
	}
	if desc != "" {
		body["description"] = desc
	}
	if *status != "" {
		body["status"] = *status
	}
	if len(labels) > 0 {
		body["labels"] = []string(labels)
	}
	if *actor != "" {
		body["actor"] = *actor
	}
	var project store.Project
	if err := common.client().Do("POST", "/api/v1/projects", nil, body, &project); err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(project)
	}
	fmt.Printf("%s created\n", project.Slug)
	return nil
}

func projectUpdate(args []string) error {
	lead, rest, err := leadingArgs(args, 1, "trackd project update <slug> [flags]")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("project update", flag.ContinueOnError)
	common := addCommon(fs)
	name := fs.String("name", "", "new name")
	description := fs.String("description", "", "new description; use - to read stdin")
	status := fs.String("status", "", "new status (active|paused|completed|canceled)")
	labelsCSV := fs.String("labels", "", "comma-separated labels, replacing the current set; empty string clears")
	archive := fs.Bool("archive", false, "archive the project")
	unarchive := fs.Bool("unarchive", false, "unarchive the project")
	actor := fs.String("actor", "", "actor recorded on the audit trail (default: token name)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	body := map[string]any{}
	if set["name"] {
		body["name"] = *name
	}
	if set["description"] {
		desc, err := readValue(*description)
		if err != nil {
			return err
		}
		body["description"] = desc
	}
	if set["status"] {
		body["status"] = *status
	}
	if set["labels"] {
		body["labels"] = splitCSV(*labelsCSV)
	}
	if *archive {
		body["archived"] = true
	}
	if *unarchive {
		body["archived"] = false
	}
	if *actor != "" {
		body["actor"] = *actor
	}
	var project store.Project
	if err := common.client().Do("PATCH", "/api/v1/projects/"+lead[0], nil, body, &project); err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(project)
	}
	fmt.Printf("%s updated (%s)\n", project.Slug, project.Status)
	return nil
}

func cmdLabel(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: trackd label <list|add> [flags]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		fs := flag.NewFlagSet("label list", flag.ContinueOnError)
		common := addCommon(fs)
		if err := fs.Parse(rest); err != nil {
			return err
		}
		var labels []store.Label
		if err := common.client().Do("GET", "/api/v1/labels", nil, nil, &labels); err != nil {
			return err
		}
		if *common.jsonOut {
			return printJSON(labels)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tCOLOR")
		for _, l := range labels {
			fmt.Fprintf(w, "%s\t%s\n", l.Name, l.Color)
		}
		return w.Flush()
	case "add":
		lead, rest, err := leadingArgs(rest, 1, "trackd label add <name> [--color <hex>]")
		if err != nil {
			return err
		}
		fs := flag.NewFlagSet("label add", flag.ContinueOnError)
		common := addCommon(fs)
		color := fs.String("color", "", "label color, e.g. #ff0000")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		var label store.Label
		body := map[string]any{"name": lead[0], "color": *color}
		if err := common.client().Do("POST", "/api/v1/labels", nil, body, &label); err != nil {
			return err
		}
		if *common.jsonOut {
			return printJSON(label)
		}
		fmt.Printf("label %s\n", label.Name)
		return nil
	default:
		return fmt.Errorf("unknown label subcommand %q", sub)
	}
}

func cmdStatuses(args []string) error {
	fs := flag.NewFlagSet("statuses", flag.ContinueOnError)
	common := addCommon(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	var statuses []store.Status
	if err := common.client().Do("GET", "/api/v1/statuses", nil, nil, &statuses); err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(statuses)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE")
	for _, s := range statuses {
		fmt.Fprintf(w, "%s\t%s\n", s.Name, s.Type)
	}
	return w.Flush()
}

func cmdHealth(args []string) error {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	common := addCommon(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	var health map[string]any
	if err := common.client().Do("GET", "/healthz", nil, nil, &health); err != nil {
		return err
	}
	return printJSON(health)
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
