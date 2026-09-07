package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

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

func (c *commonFlags) client() (*client.Client, error) {
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
	if token == "" {
		// Catch the common "forgot to authenticate" case here rather than
		// after a wasted round-trip. Reusing APIError keeps the exit code (4)
		// and the "unauthorized" machine code identical to a server 401.
		return nil, &client.APIError{
			Status:  http.StatusUnauthorized,
			Code:    "unauthorized",
			Message: "no API token set (set $TRACKD_TOKEN or pass --token)",
		}
	}
	return client.New(base, token), nil
}

// usageError is a mistake in the command line rather than a failure at the
// server. It exits 2, the same code the API's validation errors get.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// issueKeyRe matches an issue key like GDL-742. It is prefix-agnostic so it
// works whatever issue_prefix a server is configured with.
var issueKeyRe = regexp.MustCompile(`^[A-Z]+-\d+$`)

// oneOf returns the non-empty value of two flags that mean the same thing (a
// primary name and an alias), or a usage error if both are set. Empty is a
// valid result: the caller still enforces "required".
func oneOf(n1, v1, n2, v2 string) (string, error) {
	if v1 != "" && v2 != "" {
		return "", usagef("--%s and --%s are the same thing; pass one", n1, n2)
	}
	if v1 != "" {
		return v1, nil
	}
	return v2, nil
}

// groupUsage handles a command group (issue, project, token, ...) called with
// no subcommand or with a help request. Help prints the usage line and
// succeeds; no subcommand at all is a usage error. Each subcommand answers
// --help itself with its flag list.
func groupUsage(args []string, line string) (handled bool, err error) {
	if len(args) == 0 {
		return true, usagef("%s", line)
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Println(line)
		return true, nil
	}
	return false, nil
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

func (s *stringSlice) hasEmpty() bool {
	for _, v := range *s {
		if v == "" {
			return true
		}
	}
	return false
}

// readValue returns v, or all of stdin when v is "-", so long markdown bodies
// can be piped in. Empty stdin is a mistake, never an instruction to write an
// empty description: a pipeline that produced nothing would otherwise wipe the
// field it was meant to fill.
func readValue(flagName, v string) (string, error) {
	if v != "-" {
		return v, nil
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	text := strings.TrimRight(string(b), "\n")
	if strings.TrimSpace(text) == "" {
		return "", usagef("--%s -: stdin was empty", flagName)
	}
	return text, nil
}

// parseSet parses one flag set and turns a bad flag into a usage error, so a
// typo exits 2 like every other command-line mistake rather than landing in the
// exit 1 bucket a script reads as a trackd bug. The flag package writes its own
// message and a full usage dump on the way out; both are noise once the error
// carries the message, so the output is captured and replayed only for an
// explicit help request.
func parseSet(fs *flag.FlagSet, args []string) error {
	var out bytes.Buffer
	fs.SetOutput(&out)
	err := fs.Parse(args)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(os.Stderr, out.String())
		return err
	default:
		return usagef("%s", err)
	}
}

// parseArgs parses flags and pulls out n positional arguments, which may sit
// anywhere among the flags: "issue show --json TSK-1" and "issue show TSK-1
// --json" are the same command.
func parseArgs(fs *flag.FlagSet, args []string, n int, usage string) ([]string, error) {
	var positional []string
	for {
		if err := parseSet(fs, args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
	if len(positional) != n {
		return nil, usagef("usage: %s", usage)
	}
	if err := checkEmptyFlags(fs); err != nil {
		return nil, err
	}
	return positional, nil
}

// parseFlags is parseArgs for a command that takes no positional arguments.
func parseFlags(fs *flag.FlagSet, args []string, usage string) error {
	_, err := parseArgs(fs, args, 0, usage)
	return err
}

// checkEmptyFlags rejects a flag given a bare empty string. Clearing a field is
// explicit (--clear-project and friends), so an empty value is almost always a
// shell variable that did not expand.
func checkEmptyFlags(fs *flag.FlagSet) error {
	var bad string
	fs.Visit(func(f *flag.Flag) {
		if bad != "" {
			return
		}
		if s, ok := f.Value.(*stringSlice); ok {
			if s.hasEmpty() {
				bad = f.Name
			}
			return
		}
		if f.Value.String() == "" {
			bad = f.Name
		}
	})
	if bad == "" {
		return nil
	}
	if fs.Lookup("clear-"+bad) != nil {
		return usagef("--%s was given an empty value; use --clear-%s to clear the field", bad, bad)
	}
	return usagef("--%s was given an empty value", bad)
}

// setFlags reports which flags were given on the command line, so a patch can
// send only the fields the user actually named.
func setFlags(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// clearFlag registers a --clear-<name> boolean. Clearing is its own flag so
// that no empty string can ever silently erase a field.
func clearFlag(fs *flag.FlagSet, name, what string) *bool {
	return fs.Bool("clear-"+name, false, "clear the "+what)
}

// strp is the pointer form used by every patch type: a set field, an unset one
// (nil), or an explicit clear (a pointer to "").
func strp(s string) *string { return &s }

func cmdIssue(args []string) error {
	if done, err := groupUsage(args, "usage: trackd issue <list|show|create|update|append|comment|relate|events> [flags]"); done {
		return err
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
	case "append":
		return issueAppend(rest)
	case "comment":
		return issueComment(rest)
	case "relate":
		return issueRelate(rest)
	case "events":
		return issueEvents(rest)
	default:
		return usagef("unknown issue subcommand %q", sub)
	}
}

func issueList(args []string) error {
	fs := flag.NewFlagSet("issue list", flag.ContinueOnError)
	common := addCommon(fs)
	var statuses, types, labels, excludeLabels stringSlice
	fs.Var(&statuses, "status", "filter by status name (repeatable, matches any)")
	fs.Var(&types, "type", "filter by status type: triage|backlog|unstarted|started|completed|canceled (repeatable)")
	fs.Var(&labels, "label", "filter by label (repeatable, matches all)")
	fs.Var(&excludeLabels, "exclude-label", "exclude issues carrying this label (repeatable)")
	project := fs.String("project", "", "filter by project slug")
	parent := fs.String("parent", "", "filter by parent issue key")
	assignee := fs.String("assignee", "", "filter by assignee name")
	milestone := fs.String("milestone", "", "filter by milestone name")
	query := fs.String("q", "", "substring search over key, title, description and comments")
	updatedSince := fs.String("updated-since", "", "only issues updated at or after this RFC3339 time")
	completedSince := fs.String("completed-since", "", "only issues completed at or after this RFC3339 time")
	archived := fs.Bool("archived", false, "include archived issues")
	archivedOnly := fs.Bool("archived-only", false, "only archived issues")
	orderBy := fs.String("order-by", "", "sort order: updated (default), created, priority")
	limit := fs.Int("limit", 0, "maximum results (default 100, max 500)")
	offset := fs.Int("offset", 0, "skip this many results")
	if err := parseFlags(fs, args, "trackd issue list [flags]"); err != nil {
		return err
	}
	q := client.IssueQuery{
		Statuses: statuses, StatusTypes: types, Labels: labels, ExcludeLabels: excludeLabels,
		Project: *project, Parent: *parent, Assignee: *assignee, Milestone: *milestone,
		Query: *query, UpdatedSince: *updatedSince, CompletedSince: *completedSince,
		OrderBy: *orderBy, Limit: *limit, Offset: *offset,
	}
	switch {
	case *archivedOnly:
		q.Archived = "only"
	case *archived:
		q.Archived = "true"
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	issues, next, err := cl.ListIssues(q)
	if err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(map[string]any{"issues": issues, "next_offset": next})
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\tSTATUS\tPRI\tPROJECT\tASSIGNEE\tLABELS\tTITLE")
	for _, i := range issues {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			i.Key, i.Status, i.Priority, i.Project, i.Assignee, strings.Join(i.Labels, ","), truncate(i.Title, 70))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if next != nil {
		fmt.Printf("more results: --offset %d\n", *next)
	}
	return nil
}

func issueShow(args []string) error {
	fs := flag.NewFlagSet("issue show", flag.ContinueOnError)
	common := addCommon(fs)
	lead, err := parseArgs(fs, args, 1, "trackd issue show <key> [flags]")
	if err != nil {
		return err
	}
	key := lead[0]
	c, err := common.client()
	if err != nil {
		return err
	}
	issue, err := c.GetIssue(key)
	if err != nil {
		return err
	}
	comments, err := c.ListComments(key)
	if err != nil {
		return err
	}
	relations, err := c.ListRelations(key)
	if err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(map[string]any{"issue": issue, "comments": comments, "relations": relations})
	}
	fmt.Printf("%s  %s (%s)  priority %d  version %d\n", issue.Key, issue.Status, issue.StatusType, issue.Priority, issue.Version)
	fmt.Println(issue.Title)
	fmt.Println()
	fmt.Printf("project: %s  parent: %s  due: %s\n", orDash(issue.Project), orDash(issue.Parent), orDash(issue.DueDate))
	fmt.Printf("assignee: %s  milestone: %s\n", orDash(issue.Assignee), orDash(issue.Milestone))
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
			reply := ""
			if c.ParentID != 0 {
				reply = fmt.Sprintf(" reply to %d", c.ParentID)
			}
			fmt.Printf("  [%d %s %s%s] %s\n", c.ID, c.CreatedAt, who, reply, c.Body)
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
	assignee := fs.String("assignee", "", "assignee name (person or agent)")
	milestone := fs.String("milestone", "", "milestone name within the project")
	due := fs.String("due", "", "due date (YYYY-MM-DD)")
	var labels stringSlice
	fs.Var(&labels, "label", "label to apply (repeatable; the label must already exist)")
	idempotencyKey := fs.String("idempotency-key", "", "repeat-safe key: a second create with the same key returns the first issue")
	actor := fs.String("actor", "", "actor recorded on the audit trail (default: token name)")
	if err := parseFlags(fs, args, "trackd issue create --title <title> [flags]"); err != nil {
		return err
	}
	desc, err := readValue("description", *description)
	if err != nil {
		return err
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	issue, err := cl.CreateIssue(client.IssueCreate{
		Title: *title, Description: desc, Status: *status, Priority: *priority,
		Project: *project, Parent: *parent, Assignee: *assignee, Milestone: *milestone,
		DueDate: *due, Labels: labels, Actor: *actor, IdempotencyKey: *idempotencyKey,
	})
	if err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(issue)
	}
	fmt.Printf("%s created (%s)\n", issue.Key, issue.Status)
	return nil
}

func issueUpdate(args []string) error {
	fs := flag.NewFlagSet("issue update", flag.ContinueOnError)
	common := addCommon(fs)
	title := fs.String("title", "", "new title")
	description := fs.String("description", "", "replacement description; use - to read stdin (needs --replace-description when one is already written)")
	replaceDescription := fs.Bool("replace-description", false, "allow --description to overwrite an existing description")
	appendText := fs.String("append", "", "text to add to the end of the description; use - to read stdin")
	status := fs.String("status", "", "new status")
	priority := fs.Int("priority", 0, "new priority 0-4")
	project := fs.String("project", "", "project slug")
	parent := fs.String("parent", "", "parent issue key")
	assignee := fs.String("assignee", "", "assignee name")
	milestone := fs.String("milestone", "", "milestone name within the project")
	due := fs.String("due", "", "due date (YYYY-MM-DD)")
	labelsCSV := fs.String("labels", "", "comma-separated labels, replacing the current set")
	var addLabels, removeLabels stringSlice
	fs.Var(&addLabels, "add-label", "label to add, keeping the others (repeatable)")
	fs.Var(&removeLabels, "remove-label", "label to remove (repeatable)")
	expectedVersion := fs.Int64("expected-version", -1, "fail with a conflict unless the issue is still at this version")
	clearDescription := clearFlag(fs, "description", "description (implies --replace-description)")
	clearLabels := clearFlag(fs, "labels", "whole label set")
	clearProject := clearFlag(fs, "project", "project")
	clearParent := clearFlag(fs, "parent", "parent")
	clearAssignee := clearFlag(fs, "assignee", "assignee")
	clearMilestone := clearFlag(fs, "milestone", "milestone")
	clearDue := clearFlag(fs, "due", "due date")
	archive := fs.Bool("archive", false, "archive the issue")
	unarchive := fs.Bool("unarchive", false, "unarchive the issue")
	actor := fs.String("actor", "", "actor recorded on the audit trail (default: token name)")
	lead, err := parseArgs(fs, args, 1, "trackd issue update <key> [flags]")
	if err != nil {
		return err
	}
	set := setFlags(fs)
	key := lead[0]
	c, err := common.client()
	if err != nil {
		return err
	}

	patch := client.IssuePatch{Actor: *actor, ReplaceDescription: *replaceDescription}
	if set["title"] {
		patch.Title = strp(*title)
	}
	if set["description"] && *clearDescription {
		return usagef("--description and --clear-description do the same job; pick one")
	}
	if set["description"] {
		desc, err := readValue("description", *description)
		if err != nil {
			return err
		}
		patch.Description = strp(desc)
	}
	if *clearDescription {
		patch.Description = strp("")
		patch.ReplaceDescription = true
	}
	if set["status"] {
		patch.Status = strp(*status)
	}
	if set["priority"] {
		patch.Priority = priority
	}
	for _, f := range []struct {
		name  string
		value *string
		clear *bool
		dst   **string
	}{
		{"project", project, clearProject, &patch.Project},
		{"parent", parent, clearParent, &patch.Parent},
		{"assignee", assignee, clearAssignee, &patch.Assignee},
		{"milestone", milestone, clearMilestone, &patch.Milestone},
		{"due", due, clearDue, &patch.DueDate},
	} {
		if set[f.name] && *f.clear {
			return usagef("--%s and --clear-%s do the same job; pick one", f.name, f.name)
		}
		switch {
		case set[f.name]:
			*f.dst = strp(*f.value)
		case *f.clear:
			*f.dst = strp("")
		}
	}
	if (set["labels"] || *clearLabels) && (len(addLabels) > 0 || len(removeLabels) > 0) {
		return usagef("--labels replaces the whole set; it cannot be combined with --add-label or --remove-label")
	}
	if set["labels"] && *clearLabels {
		return usagef("--labels and --clear-labels do the same job; pick one")
	}
	if set["labels"] {
		replacement := splitCSV(*labelsCSV)
		patch.Labels = &replacement
	}
	if *clearLabels {
		empty := []string{}
		patch.Labels = &empty
	}
	patch.AddLabels = addLabels
	patch.RemoveLabels = removeLabels
	if set["expected-version"] {
		patch.ExpectedVersion = expectedVersion
	}
	if *archive && *unarchive {
		return usagef("--archive and --unarchive contradict each other")
	}
	if *archive {
		patch.Archived = archive
	}
	if *unarchive {
		no := false
		patch.Archived = &no
	}

	// An append is its own request: the server refuses to overwrite a written
	// description, so appending and patching cannot share one call.
	if set["append"] {
		text, err := readValue("append", *appendText)
		if err != nil {
			return err
		}
		issue, err := c.AppendDescription(key, text, *actor)
		if err != nil {
			return err
		}
		if isEmptyPatch(patch) {
			return reportIssue(issue, *common.jsonOut, "updated")
		}
	}
	if isEmptyPatch(patch) && !set["append"] {
		return usagef("nothing to update: give at least one field flag")
	}
	issue, err := c.UpdateIssue(key, patch)
	if err != nil {
		return err
	}
	return reportIssue(issue, *common.jsonOut, "updated")
}

// isEmptyPatch reports whether a patch would send no field at all, which is a
// mistyped command rather than a request worth making.
func isEmptyPatch(p client.IssuePatch) bool {
	return p.Title == nil && p.Description == nil && p.Status == nil && p.Priority == nil &&
		p.Project == nil && p.Parent == nil && p.Assignee == nil && p.Milestone == nil &&
		p.DueDate == nil && p.Labels == nil && len(p.AddLabels) == 0 && len(p.RemoveLabels) == 0 &&
		p.Archived == nil
}

func reportIssue(issue *store.Issue, jsonOut bool, verb string) error {
	if jsonOut {
		return printJSON(issue)
	}
	fmt.Printf("%s %s (%s, version %d)\n", issue.Key, verb, issue.Status, issue.Version)
	return nil
}

func issueAppend(args []string) error {
	fs := flag.NewFlagSet("issue append", flag.ContinueOnError)
	common := addCommon(fs)
	text := fs.String("text", "", "text to add to the end of the description; use - to read stdin")
	bodyAlias := fs.String("body", "", "alias for --text")
	actor := fs.String("actor", "", "actor recorded on the audit trail (default: token name)")
	lead, err := parseArgs(fs, args, 1, "trackd issue append <key> --text <text>")
	if err != nil {
		return err
	}
	raw, err := oneOf("text", *text, "body", *bodyAlias)
	if err != nil {
		return err
	}
	if raw == "" {
		return usagef("usage: trackd issue append <key> --text <text>")
	}
	body, err := readValue("text", raw)
	if err != nil {
		return err
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	issue, err := cl.AppendDescription(lead[0], body, *actor)
	if err != nil {
		return err
	}
	return reportIssue(issue, *common.jsonOut, "appended")
}

func issueComment(args []string) error {
	fs := flag.NewFlagSet("issue comment", flag.ContinueOnError)
	common := addCommon(fs)
	bodyFlag := fs.String("body", "", "comment body (required); use - to read stdin")
	textAlias := fs.String("text", "", "alias for --body")
	parent := fs.Int64("parent", 0, "reply to this comment id")
	idempotencyKey := fs.String("idempotency-key", "", "repeat-safe key: a second comment with the same key returns the first")
	actor := fs.String("actor", "", "actor recorded on the comment (default: token name)")
	lead, err := parseArgs(fs, args, 1, "trackd issue comment <key> --body <text> [flags]")
	if err != nil {
		return err
	}
	raw, err := oneOf("body", *bodyFlag, "text", *textAlias)
	if err != nil {
		return err
	}
	if raw == "" {
		return usagef("usage: trackd issue comment <key> --body <text> [flags]")
	}
	text, err := readValue("body", raw)
	if err != nil {
		return err
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	comment, err := cl.AddComment(lead[0], client.CommentCreate{
		Body: text, ParentID: *parent, Actor: *actor, IdempotencyKey: *idempotencyKey,
	})
	if err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(comment)
	}
	fmt.Printf("comment %d added to %s as %s\n", comment.ID, comment.IssueKey, orDash(comment.Actor))
	return nil
}

func cmdComment(args []string) error {
	if done, err := groupUsage(args, "usage: trackd comment edit <id> --body <text> [flags]"); done {
		return err
	}
	sub, rest := args[0], args[1:]
	if sub != "edit" {
		// A bare issue key here is almost always someone reaching for the
		// nested command; point them at it instead of a blank "unknown
		// subcommand".
		if issueKeyRe.MatchString(sub) {
			return usagef("to comment on an issue use: trackd issue comment %s --body <text>", sub)
		}
		return usagef("unknown comment subcommand %q", sub)
	}
	fs := flag.NewFlagSet("comment edit", flag.ContinueOnError)
	common := addCommon(fs)
	bodyFlag := fs.String("body", "", "replacement body (required); use - to read stdin")
	textAlias := fs.String("text", "", "alias for --body")
	actor := fs.String("actor", "", "actor recorded on the audit trail (default: token name)")
	lead, err := parseArgs(fs, rest, 1, "trackd comment edit <id> --body <text> [flags]")
	if err != nil {
		return err
	}
	id, err := strconv.ParseInt(lead[0], 10, 64)
	if err != nil {
		return usagef("comment id must be a number, got %q", lead[0])
	}
	raw, err := oneOf("body", *bodyFlag, "text", *textAlias)
	if err != nil {
		return err
	}
	if raw == "" {
		return usagef("usage: trackd comment edit <id> --body <text> [flags]")
	}
	text, err := readValue("body", raw)
	if err != nil {
		return err
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	comment, err := cl.UpdateComment(id, text, *actor)
	if err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(comment)
	}
	fmt.Printf("comment %d edited on %s\n", comment.ID, comment.IssueKey)
	return nil
}

func issueRelate(args []string) error {
	fs := flag.NewFlagSet("issue relate", flag.ContinueOnError)
	common := addCommon(fs)
	typ := fs.String("type", "relates", "relation type: blocks, relates, or duplicate")
	remove := fs.Bool("remove", false, "remove the relation instead of adding it")
	actor := fs.String("actor", "", "actor recorded on the audit trail (default: token name)")
	lead, err := parseArgs(fs, args, 2, "trackd issue relate <key> <related-key> --type <blocks|relates|duplicate> [--remove]")
	if err != nil {
		return err
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	relations, err := cl.SaveRelation(lead[0], lead[1], *typ, *remove, *actor)
	if err != nil {
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
	fs := flag.NewFlagSet("issue events", flag.ContinueOnError)
	common := addCommon(fs)
	limit := fs.Int("limit", 0, "maximum events (default 100)")
	lead, err := parseArgs(fs, args, 1, "trackd issue events <key> [flags]")
	if err != nil {
		return err
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	events, err := cl.ListIssueEvents(lead[0], *limit)
	if err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(events)
	}
	return printEvents(events, false)
}

// cmdEvents is the global activity feed: everything that happened, in the order
// it happened, with a cursor to pick up from next time.
func cmdEvents(args []string) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	common := addCommon(fs)
	since := fs.String("since", "", "only events at or after this RFC3339 time")
	afterID := fs.Int64("after-id", 0, "only events after this event id (the cursor from a previous run)")
	entity := fs.String("entity", "", "filter by entity type: issue, project, milestone, token or setting (a comment or a relation is recorded against its issue)")
	limit := fs.Int("limit", 0, "maximum events (default 100, max 1000)")
	if err := parseFlags(fs, args, "trackd events [flags]"); err != nil {
		return err
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	events, next, err := cl.ListEvents(client.EventQuery{
		Since: *since, AfterID: *afterID, Entity: *entity, Limit: *limit,
	})
	if err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(map[string]any{"events": events, "next_after_id": next})
	}
	if err := printEvents(events, true); err != nil {
		return err
	}
	if next != nil {
		fmt.Printf("more events: --after-id %d\n", *next)
	}
	return nil
}

func printEvents(events []store.Event, withEntity bool) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if withEntity {
		fmt.Fprintln(w, "ID\tTIME\tENTITY\tACTION\tACTOR")
		for _, e := range events {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", e.ID, e.CreatedAt, orDash(e.EntityKey), e.Action, orDash(e.Actor))
		}
	} else {
		fmt.Fprintln(w, "TIME\tACTION\tACTOR")
		for _, e := range events {
			fmt.Fprintf(w, "%s\t%s\t%s\n", e.CreatedAt, e.Action, orDash(e.Actor))
		}
	}
	return w.Flush()
}

func cmdProject(args []string) error {
	if done, err := groupUsage(args, "usage: trackd project <list|show|create|update> [flags]"); done {
		return err
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
		return usagef("unknown project subcommand %q", sub)
	}
}

func projectList(args []string) error {
	fs := flag.NewFlagSet("project list", flag.ContinueOnError)
	common := addCommon(fs)
	archived := fs.Bool("archived", false, "include archived projects")
	if err := parseFlags(fs, args, "trackd project list [flags]"); err != nil {
		return err
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	projects, err := cl.ListProjects(*archived)
	if err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(projects)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SLUG\tSTATUS\tSTART\tTARGET\tLABELS\tNAME")
	for _, p := range projects {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			p.Slug, p.Status, orDash(p.StartDate), orDash(p.TargetDate), strings.Join(p.Labels, ","), p.Name)
	}
	return w.Flush()
}

func projectShow(args []string) error {
	fs := flag.NewFlagSet("project show", flag.ContinueOnError)
	common := addCommon(fs)
	lead, err := parseArgs(fs, args, 1, "trackd project show <slug> [flags]")
	if err != nil {
		return err
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	project, err := cl.GetProject(lead[0])
	if err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(project)
	}
	fmt.Printf("%s  %s\n", project.Slug, project.Status)
	fmt.Println(project.Name)
	fmt.Printf("labels: %s\n", orDash(strings.Join(project.Labels, ",")))
	fmt.Printf("start: %s  target: %s  completed: %s\n",
		orDash(project.StartDate), orDash(project.TargetDate), orDash(project.CompletedAt))
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
	status := fs.String("status", "", "project status: backlog (default), planned, started, paused, completed, canceled")
	start := fs.String("start", "", "start date (YYYY-MM-DD)")
	target := fs.String("target", "", "target date (YYYY-MM-DD)")
	var labels stringSlice
	fs.Var(&labels, "label", "label to apply (repeatable; the label must already exist)")
	actor := fs.String("actor", "", "actor recorded on the audit trail (default: token name)")
	if err := parseFlags(fs, args, "trackd project create --name <name> [flags]"); err != nil {
		return err
	}
	desc, err := readValue("description", *description)
	if err != nil {
		return err
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	project, err := cl.CreateProject(client.ProjectCreate{
		Name: *name, Slug: *slug, Description: desc, Status: *status,
		Labels: labels, StartDate: *start, TargetDate: *target, Actor: *actor,
	})
	if err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(project)
	}
	fmt.Printf("%s created\n", project.Slug)
	return nil
}

func projectUpdate(args []string) error {
	fs := flag.NewFlagSet("project update", flag.ContinueOnError)
	common := addCommon(fs)
	name := fs.String("name", "", "new name")
	description := fs.String("description", "", "new description; use - to read stdin")
	status := fs.String("status", "", "new status: backlog, planned, started, paused, completed, canceled")
	labelsCSV := fs.String("labels", "", "comma-separated labels, replacing the current set")
	var addLabels, removeLabels stringSlice
	fs.Var(&addLabels, "add-label", "label to add, keeping the others (repeatable)")
	fs.Var(&removeLabels, "remove-label", "label to remove (repeatable)")
	start := fs.String("start", "", "start date (YYYY-MM-DD)")
	target := fs.String("target", "", "target date (YYYY-MM-DD)")
	clearDescription := clearFlag(fs, "description", "description")
	clearLabels := clearFlag(fs, "labels", "whole label set")
	clearStart := clearFlag(fs, "start", "start date")
	clearTarget := clearFlag(fs, "target", "target date")
	archive := fs.Bool("archive", false, "archive the project")
	unarchive := fs.Bool("unarchive", false, "unarchive the project")
	actor := fs.String("actor", "", "actor recorded on the audit trail (default: token name)")
	lead, err := parseArgs(fs, args, 1, "trackd project update <slug> [flags]")
	if err != nil {
		return err
	}
	set := setFlags(fs)
	patch := client.ProjectPatch{Actor: *actor, AddLabels: addLabels, RemoveLabels: removeLabels}
	if set["name"] {
		patch.Name = strp(*name)
	}
	if set["status"] {
		patch.Status = strp(*status)
	}
	for _, f := range []struct {
		name  string
		value *string
		clear *bool
		dst   **string
	}{
		{"description", description, clearDescription, &patch.Description},
		{"start", start, clearStart, &patch.StartDate},
		{"target", target, clearTarget, &patch.TargetDate},
	} {
		if set[f.name] && *f.clear {
			return usagef("--%s and --clear-%s do the same job; pick one", f.name, f.name)
		}
		switch {
		case set[f.name]:
			value := *f.value
			if f.name == "description" {
				if value, err = readValue("description", value); err != nil {
					return err
				}
			}
			*f.dst = strp(value)
		case *f.clear:
			*f.dst = strp("")
		}
	}
	if (set["labels"] || *clearLabels) && (len(addLabels) > 0 || len(removeLabels) > 0) {
		return usagef("--labels replaces the whole set; it cannot be combined with --add-label or --remove-label")
	}
	if set["labels"] && *clearLabels {
		return usagef("--labels and --clear-labels do the same job; pick one")
	}
	if set["labels"] {
		replacement := splitCSV(*labelsCSV)
		patch.Labels = &replacement
	}
	if *clearLabels {
		empty := []string{}
		patch.Labels = &empty
	}
	if *archive && *unarchive {
		return usagef("--archive and --unarchive contradict each other")
	}
	if *archive {
		patch.Archived = archive
	}
	if *unarchive {
		no := false
		patch.Archived = &no
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	project, err := cl.UpdateProject(lead[0], patch)
	if err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(project)
	}
	fmt.Printf("%s updated (%s)\n", project.Slug, project.Status)
	return nil
}

func cmdLabel(args []string) error {
	if done, err := groupUsage(args, "usage: trackd label <list|add> [flags]"); done {
		return err
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		fs := flag.NewFlagSet("label list", flag.ContinueOnError)
		common := addCommon(fs)
		if err := parseFlags(fs, rest, "trackd label list [flags]"); err != nil {
			return err
		}
		cl, err := common.client()
		if err != nil {
			return err
		}
		labels, err := cl.ListLabels()
		if err != nil {
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
		fs := flag.NewFlagSet("label add", flag.ContinueOnError)
		common := addCommon(fs)
		color := fs.String("color", "", "label color, e.g. #ff0000")
		lead, err := parseArgs(fs, rest, 1, "trackd label add <name> [--color <hex>]")
		if err != nil {
			return err
		}
		cl, err := common.client()
		if err != nil {
			return err
		}
		label, err := cl.CreateLabel(lead[0], *color)
		if err != nil {
			return err
		}
		if *common.jsonOut {
			return printJSON(label)
		}
		fmt.Printf("label %s\n", label.Name)
		return nil
	default:
		return usagef("unknown label subcommand %q", sub)
	}
}

func cmdMilestone(args []string) error {
	if done, err := groupUsage(args, "usage: trackd milestone <list|create|update> [flags]"); done {
		return err
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		fs := flag.NewFlagSet("milestone list", flag.ContinueOnError)
		common := addCommon(fs)
		project := fs.String("project", "", "filter by project slug")
		archived := fs.Bool("archived", false, "include archived milestones")
		if err := parseFlags(fs, rest, "trackd milestone list [flags]"); err != nil {
			return err
		}
		cl, err := common.client()
		if err != nil {
			return err
		}
		milestones, err := cl.ListMilestones(*project, *archived)
		if err != nil {
			return err
		}
		if *common.jsonOut {
			return printJSON(milestones)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tPROJECT\tNAME\tTARGET\tARCHIVED")
		for _, m := range milestones {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", m.ID, m.Project, m.Name, orDash(m.TargetDate), m.ArchivedAt)
		}
		return w.Flush()
	case "create":
		fs := flag.NewFlagSet("milestone create", flag.ContinueOnError)
		common := addCommon(fs)
		project := fs.String("project", "", "project slug (required)")
		name := fs.String("name", "", "milestone name (required)")
		description := fs.String("description", "", "milestone description")
		target := fs.String("target", "", "target date (YYYY-MM-DD)")
		actor := fs.String("actor", "", "actor recorded on the audit trail (default: token name)")
		if err := parseFlags(fs, rest, "trackd milestone create --project <slug> --name <name> [flags]"); err != nil {
			return err
		}
		cl, err := common.client()
		if err != nil {
			return err
		}
		milestone, err := cl.CreateMilestone(client.MilestoneCreate{
			Project: *project, Name: *name, Description: *description, TargetDate: *target, Actor: *actor,
		})
		if err != nil {
			return err
		}
		if *common.jsonOut {
			return printJSON(milestone)
		}
		fmt.Printf("milestone %d: %s (%s)\n", milestone.ID, milestone.Name, milestone.Project)
		return nil
	case "update":
		fs := flag.NewFlagSet("milestone update", flag.ContinueOnError)
		common := addCommon(fs)
		name := fs.String("name", "", "new name")
		description := fs.String("description", "", "new description")
		target := fs.String("target", "", "target date (YYYY-MM-DD)")
		clearDescription := clearFlag(fs, "description", "description")
		clearTarget := clearFlag(fs, "target", "target date")
		archive := fs.Bool("archive", false, "archive the milestone")
		unarchive := fs.Bool("unarchive", false, "unarchive the milestone")
		actor := fs.String("actor", "", "actor recorded on the audit trail (default: token name)")
		lead, err := parseArgs(fs, rest, 1, "trackd milestone update <id> [flags]")
		if err != nil {
			return err
		}
		set := setFlags(fs)
		patch := client.MilestonePatch{Actor: *actor}
		if set["name"] {
			patch.Name = strp(*name)
		}
		for _, f := range []struct {
			name  string
			value *string
			clear *bool
			dst   **string
		}{
			{"description", description, clearDescription, &patch.Description},
			{"target", target, clearTarget, &patch.TargetDate},
		} {
			if set[f.name] && *f.clear {
				return usagef("--%s and --clear-%s do the same job; pick one", f.name, f.name)
			}
			switch {
			case set[f.name]:
				*f.dst = strp(*f.value)
			case *f.clear:
				*f.dst = strp("")
			}
		}
		if *archive && *unarchive {
			return usagef("--archive and --unarchive contradict each other")
		}
		if *archive {
			patch.Archived = archive
		}
		if *unarchive {
			no := false
			patch.Archived = &no
		}
		cl, err := common.client()
		if err != nil {
			return err
		}
		milestone, err := cl.UpdateMilestone(lead[0], patch)
		if err != nil {
			return err
		}
		if *common.jsonOut {
			return printJSON(milestone)
		}
		fmt.Printf("milestone %d updated\n", milestone.ID)
		return nil
	default:
		return usagef("unknown milestone subcommand %q", sub)
	}
}

func cmdStatuses(args []string) error {
	fs := flag.NewFlagSet("statuses", flag.ContinueOnError)
	common := addCommon(fs)
	if err := parseFlags(fs, args, "trackd statuses [flags]"); err != nil {
		return err
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	statuses, err := cl.ListStatuses()
	if err != nil {
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
	if err := parseFlags(fs, args, "trackd health [flags]"); err != nil {
		return err
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	health, status, err := cl.Health()
	if err != nil {
		return err
	}
	if *common.jsonOut {
		if err := printJSON(health); err != nil {
			return err
		}
	} else if err := printHealth(health); err != nil {
		return err
	}
	// A degraded server answers 503. The report is already printed; the exit
	// code is what a monitor reads.
	if status != 200 {
		return &client.APIError{Status: status, Code: "degraded", Message: "server reports degraded health"}
	}
	return nil
}

// printHealth is the human form of the report, one row per fact, in the order
// an operator asks the questions. The report is a loose map, so every lookup
// tolerates a field the server did not send.
func printHealth(health map[string]any) error {
	backup := healthSection(health, "backup")
	integrity := healthSection(health, "integrity")
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "FIELD\tVALUE")
	fmt.Fprintf(w, "status\t%s\n", healthField(health, "status"))
	fmt.Fprintf(w, "version\t%s\n", healthField(health, "version"))
	fmt.Fprintf(w, "schema\t%s\n", healthField(health, "schema"))
	fmt.Fprintf(w, "backup\t%s\n", healthBackup(backup))
	fmt.Fprintf(w, "integrity\t%s\n", healthIntegrity(integrity))
	if msg := healthField(backup, "error"); msg != "-" {
		fmt.Fprintf(w, "backup error\t%s\n", msg)
	}
	return w.Flush()
}

func healthSection(health map[string]any, name string) map[string]any {
	section, _ := health[name].(map[string]any)
	return section
}

// healthField renders one value of the report. JSON numbers arrive as floats
// and every field is optional, so a missing one reads as a dash rather than an
// invented zero.
func healthField(section map[string]any, name string) string {
	value, ok := section[name]
	if !ok || value == nil || value == "" {
		return "-"
	}
	if f, ok := value.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprintf("%v", value)
}

func healthBackup(backup map[string]any) string {
	at := healthField(backup, "last_at")
	if at == "-" {
		return "no snapshot recorded"
	}
	age, ok := backup["age_seconds"].(float64)
	if !ok {
		return at
	}
	return fmt.Sprintf("%s ago (%s)", (time.Duration(age) * time.Second).Round(time.Second), at)
}

func healthIntegrity(integrity map[string]any) string {
	state := "failed"
	if ok, _ := integrity["ok"].(bool); ok {
		state = "ok"
	}
	at := healthField(integrity, "checked_at")
	if at == "-" {
		return state + " (never checked)"
	}
	return fmt.Sprintf("%s (checked %s)", state, at)
}

// exitCode maps an error to the process exit status. The classes are stable:
// scripts branch on them, so a not-found stays 3 whatever the message says.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var usage *usageError
	if errors.As(err, &usage) || errors.Is(err, flag.ErrHelp) {
		return 2
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Status == 400 || apiErr.Status == 422:
			return 2
		case apiErr.Status == 404:
			return 3
		case apiErr.Status == 401 || apiErr.Status == 403:
			return 4
		case apiErr.Status == 409:
			return 5
		case apiErr.Status >= 500:
			return 6
		}
		return 1
	}
	var transport *client.TransportError
	if errors.As(err, &transport) {
		return 6
	}
	return 1
}

// errorObject is what --json prints on failure, so a caller parsing stdout gets
// the same machine-readable shape whether the command worked or not.
func errorObject(err error) map[string]any {
	obj := map[string]any{"error": err.Error(), "code": "internal", "exit": exitCode(err)}
	var usage *usageError
	var apiErr *client.APIError
	var transport *client.TransportError
	switch {
	case errors.As(err, &apiErr):
		obj["error"] = apiErr.Message
		obj["code"] = apiErr.Code
		obj["status"] = apiErr.Status
	case errors.As(err, &usage) || errors.Is(err, flag.ErrHelp):
		obj["code"] = "usage"
	case errors.As(err, &transport):
		obj["code"] = "unreachable"
	}
	return obj
}

// wantsJSON scans the raw arguments for --json. The flag lives on each
// subcommand's flag set, but an error can happen before that set is parsed and
// the output shape has to be decided either way.
func wantsJSON(args []string) bool {
	for _, a := range args {
		switch a {
		case "-json", "--json", "-json=true", "--json=true":
			return true
		case "--":
			return false
		}
	}
	return false
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
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
