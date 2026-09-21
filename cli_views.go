package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/mikedclarke/trackd/internal/client"
	"github.com/mikedclarke/trackd/internal/store"
)

// cmdView manages saved views: a named filter anyone can open by name with
// `issue list --view`, on the board, or over MCP. Deleting a view archives it
// (nothing in trackd is hard-deleted), which frees the name; `restore` brings
// it back.
func cmdView(args []string) error {
	if done, err := groupUsage(args, "usage: trackd view <list|show|create|update|delete|restore> [flags]"); done {
		return err
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return viewList(rest)
	case "show", "get":
		return viewShow(rest)
	case "create", "add":
		return viewCreate(rest)
	case "update", "edit":
		return viewUpdate(rest)
	case "delete", "archive", "remove":
		return viewArchive(rest, true)
	case "restore", "unarchive":
		return viewArchive(rest, false)
	default:
		return usagef("unknown view subcommand %q", sub)
	}
}

// viewFilterFlags are the filter fields a view stores, registered on create
// and update alike so the two commands read the same way.
type viewFilterFlags struct {
	statuses, types, labels, excludeLabels, priorities                             stringSlice
	project, assignee, milestone, updatedWithin, createdBy, query, search, orderBy *string
}

func addViewFilterFlags(fs *flag.FlagSet) *viewFilterFlags {
	f := &viewFilterFlags{}
	fs.Var(&f.statuses, "status", "status name to include (repeatable, matches any)")
	fs.Var(&f.types, "type", "status type to include: triage|backlog|unstarted|started|completed|canceled (repeatable)")
	fs.Var(&f.labels, "label", "label the issue must carry (repeatable, matches all)")
	fs.Var(&f.excludeLabels, "exclude-label", "label that excludes an issue (repeatable)")
	fs.Var(&f.priorities, "priority", "priority to include: 0-4 or a word (repeatable, matches any)")
	f.project = fs.String("project", "", "project slug")
	f.assignee = fs.String("assignee", "", "assignee name")
	f.milestone = fs.String("milestone", "", "milestone name")
	f.updatedWithin = fs.String("updated-within", "", "only issues updated within this window, e.g. 7d, 48h, 2w")
	f.createdBy = fs.String("created-by", "", "only issues created by this actor")
	f.query = fs.String("q", "", "substring search over key, title, description and comments")
	f.search = fs.String("search", "", "alias for -q")
	f.orderBy = fs.String("order-by", "", "sort order: updated (default), created, priority")
	return f
}

// apply writes the flags that were given onto a filter, leaving the rest as
// they are. On create the filter starts empty; on update it starts as stored.
func (f *viewFilterFlags) apply(set map[string]bool, out *store.ViewFilter) error {
	if len(f.statuses) > 0 {
		out.Statuses = f.statuses
	}
	if len(f.types) > 0 {
		out.StatusTypes = f.types
	}
	if len(f.labels) > 0 {
		out.Labels = f.labels
	}
	if len(f.excludeLabels) > 0 {
		out.ExcludeLabels = f.excludeLabels
	}
	if len(f.priorities) > 0 {
		out.Priorities = nil
		for _, raw := range f.priorities {
			p, err := parsePriority(raw)
			if err != nil {
				return err
			}
			out.Priorities = append(out.Priorities, p)
		}
	}
	query, err := oneOf("q", *f.query, "search", *f.search)
	if err != nil {
		return err
	}
	if query != "" {
		out.Query = query
	}
	for _, s := range []struct {
		name  string
		value *string
		dst   *string
	}{
		{"project", f.project, &out.Project},
		{"assignee", f.assignee, &out.Assignee},
		{"milestone", f.milestone, &out.Milestone},
		{"updated-within", f.updatedWithin, &out.UpdatedWithin},
		{"created-by", f.createdBy, &out.CreatedBy},
		{"order-by", f.orderBy, &out.OrderBy},
	} {
		if set[s.name] {
			*s.dst = *s.value
		}
	}
	return nil
}

// viewClearFlags are the update command's explicit ways to drop one filter
// field, because an empty string is never a value on this CLI.
type viewClearFlags struct {
	statuses, types, labels, excludeLabels, priorities, project, assignee, milestone, updatedWithin, createdBy, query, orderBy *bool
}

func addViewClearFlags(fs *flag.FlagSet) *viewClearFlags {
	return &viewClearFlags{
		statuses:      fs.Bool("clear-statuses", false, "drop the status filter"),
		types:         fs.Bool("clear-types", false, "drop the status type filter"),
		labels:        fs.Bool("clear-labels", false, "drop the label filter"),
		excludeLabels: fs.Bool("clear-exclude-labels", false, "drop the excluded labels"),
		priorities:    fs.Bool("clear-priorities", false, "drop the priority filter"),
		project:       clearFlag(fs, "project", "project filter"),
		assignee:      clearFlag(fs, "assignee", "assignee filter"),
		milestone:     clearFlag(fs, "milestone", "milestone filter"),
		updatedWithin: clearFlag(fs, "updated-within", "updated-within window"),
		createdBy:     clearFlag(fs, "created-by", "created-by filter"),
		query:         clearFlag(fs, "q", "text search"),
		orderBy:       clearFlag(fs, "order-by", "sort order"),
	}
}

func (c *viewClearFlags) apply(out *store.ViewFilter) {
	if *c.statuses {
		out.Statuses = nil
	}
	if *c.types {
		out.StatusTypes = nil
	}
	if *c.labels {
		out.Labels = nil
	}
	if *c.excludeLabels {
		out.ExcludeLabels = nil
	}
	if *c.priorities {
		out.Priorities = nil
	}
	if *c.project {
		out.Project = ""
	}
	if *c.assignee {
		out.Assignee = ""
	}
	if *c.milestone {
		out.Milestone = ""
	}
	if *c.updatedWithin {
		out.UpdatedWithin = ""
	}
	if *c.createdBy {
		out.CreatedBy = ""
	}
	if *c.query {
		out.Query = ""
	}
	if *c.orderBy {
		out.OrderBy = ""
	}
}

// parseQuickAction reads one --quick spec: a button name, a colon, then
// comma-separated key=value pairs from status, priority, add and remove, for
// example "Answered: remove=waiting" or "Ship: status=Done, add=released".
func parseQuickAction(spec string) (store.QuickAction, error) {
	name, rest, ok := strings.Cut(spec, ":")
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return store.QuickAction{}, usagef("--quick %q: want \"Name: status=..., priority=..., add=label, remove=label\"", spec)
	}
	q := store.QuickAction{Name: name}
	for _, pair := range strings.Split(rest, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, value, ok := strings.Cut(pair, "=")
		key, value = strings.TrimSpace(strings.ToLower(key)), strings.TrimSpace(value)
		if !ok || value == "" {
			return store.QuickAction{}, usagef("--quick %q: %q is not key=value", spec, pair)
		}
		switch key {
		case "status":
			q.Status = value
		case "priority":
			p, err := parsePriority(value)
			if err != nil {
				return store.QuickAction{}, err
			}
			q.Priority = &p
		case "add", "add-label", "add_label":
			q.AddLabels = append(q.AddLabels, value)
		case "remove", "remove-label", "remove_label":
			q.RemoveLabels = append(q.RemoveLabels, value)
		default:
			return store.QuickAction{}, usagef("--quick %q: unknown key %q; use status, priority, add or remove", spec, key)
		}
	}
	return q, nil
}

func parseQuickActions(specs []string) ([]store.QuickAction, error) {
	out := make([]store.QuickAction, 0, len(specs))
	for _, spec := range specs {
		q, err := parseQuickAction(spec)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, nil
}

// describeFilter renders a stored filter the way its flags were written, so a
// listing reads back as the command that would recreate it.
func describeFilter(f store.ViewFilter) string {
	var parts []string
	add := func(flag string, values []string) {
		for _, v := range values {
			parts = append(parts, "--"+flag+" "+quoteIfSpaced(v))
		}
	}
	add("status", f.Statuses)
	add("type", f.StatusTypes)
	if f.Project != "" {
		add("project", []string{f.Project})
	}
	add("label", f.Labels)
	add("exclude-label", f.ExcludeLabels)
	if f.Assignee != "" {
		add("assignee", []string{f.Assignee})
	}
	if f.Milestone != "" {
		add("milestone", []string{f.Milestone})
	}
	for _, p := range f.Priorities {
		parts = append(parts, "--priority "+priorityWords[p])
	}
	if f.UpdatedWithin != "" {
		add("updated-within", []string{f.UpdatedWithin})
	}
	if f.CreatedBy != "" {
		add("created-by", []string{f.CreatedBy})
	}
	if f.Query != "" {
		add("q", []string{f.Query})
	}
	if f.OrderBy != "" {
		add("order-by", []string{f.OrderBy})
	}
	return strings.Join(parts, " ")
}

func describeQuickAction(q store.QuickAction) string {
	var parts []string
	if q.Status != "" {
		parts = append(parts, "status="+q.Status)
	}
	if q.Priority != nil && *q.Priority >= 0 && *q.Priority < len(priorityWords) {
		parts = append(parts, "priority="+priorityWords[*q.Priority])
	}
	for _, l := range q.AddLabels {
		parts = append(parts, "add="+l)
	}
	for _, l := range q.RemoveLabels {
		parts = append(parts, "remove="+l)
	}
	return q.Name + ": " + strings.Join(parts, ", ")
}

func quoteIfSpaced(s string) string {
	if strings.ContainsAny(s, " \t") {
		return strconv.Quote(s)
	}
	return s
}

func viewList(args []string) error {
	fs := flag.NewFlagSet("view list", flag.ContinueOnError)
	common := addCommon(fs)
	archived := fs.Bool("archived", false, "include archived views")
	if err := parseFlags(fs, args, "trackd view list [flags]"); err != nil {
		return err
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	views, err := cl.ListViews(*archived)
	if err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(map[string]any{"views": views})
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tOWNER\tSHARED\tQUICK\tFILTER")
	for _, v := range views {
		shared := "yes"
		if !v.Shared {
			shared = "no"
		}
		if v.ArchivedAt != "" {
			shared += " (archived)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", v.Name, orDash(v.Owner), shared, len(v.QuickActions), describeFilter(v.Filter))
	}
	return w.Flush()
}

func viewShow(args []string) error {
	fs := flag.NewFlagSet("view show", flag.ContinueOnError)
	common := addCommon(fs)
	lead, err := parseArgs(fs, args, 1, "trackd view show <name> [flags]")
	if err != nil {
		return err
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	view, err := cl.GetView(lead[0])
	if err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(view)
	}
	printView(view)
	return nil
}

func printView(v *store.View) {
	shared := "shared"
	if !v.Shared {
		shared = "private"
	}
	fmt.Printf("%s  %s  owner %s\n", v.Name, shared, orDash(v.Owner))
	if v.Description != "" {
		fmt.Println(v.Description)
	}
	fmt.Printf("filter:  %s\n", orDash(describeFilter(v.Filter)))
	for _, q := range v.QuickActions {
		fmt.Printf("quick:   %s\n", describeQuickAction(q))
	}
	fmt.Printf("created: %s  updated: %s\n", v.CreatedAt, v.UpdatedAt)
	if v.ArchivedAt != "" {
		fmt.Printf("archived: %s\n", v.ArchivedAt)
	}
	fmt.Printf("open:    trackd issue list --view %s\n", quoteIfSpaced(v.Name))
}

func viewCreate(args []string) error {
	fs := flag.NewFlagSet("view create", flag.ContinueOnError)
	common := addCommon(fs)
	filterFlags := addViewFilterFlags(fs)
	description := fs.String("description", "", "what the view is for")
	private := fs.Bool("private", false, "only the creating token and admins see the view (default: shared with everyone)")
	var quick stringSlice
	fs.Var(&quick, "quick", "quick action button, \"Name: status=..., priority=..., add=label, remove=label\" (repeatable, up to 4)")
	actor := fs.String("actor", "", "actor recorded as the owner (default: token name)")
	lead, err := parseArgs(fs, args, 1, "trackd view create <name> [filter flags] [--quick ...]")
	if err != nil {
		return err
	}
	var filter store.ViewFilter
	if err := filterFlags.apply(setFlags(fs), &filter); err != nil {
		return err
	}
	if filter.Empty() {
		return usagef("a view needs at least one filter flag (--status, --label, --project, ...)")
	}
	actions, err := parseQuickActions(quick)
	if err != nil {
		return err
	}
	in := client.ViewCreate{Name: lead[0], Description: *description, Filter: filter, QuickActions: actions, Actor: *actor}
	if *private {
		shared := false
		in.Shared = &shared
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	view, err := cl.CreateView(in)
	if err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(view)
	}
	fmt.Printf("view %s created\n", quoteIfSpaced(view.Name))
	return nil
}

func viewUpdate(args []string) error {
	fs := flag.NewFlagSet("view update", flag.ContinueOnError)
	common := addCommon(fs)
	filterFlags := addViewFilterFlags(fs)
	clears := addViewClearFlags(fs)
	rename := fs.String("rename", "", "new name")
	description := fs.String("description", "", "new description")
	clearDescription := clearFlag(fs, "description", "description")
	shared := fs.Bool("shared", false, "share the view with every token")
	private := fs.Bool("private", false, "hide the view from tokens other than the owner and admins")
	var quick stringSlice
	fs.Var(&quick, "quick", "quick action, \"Name: status=..., add=label, remove=label\"; given once or more it replaces the whole set")
	clearQuick := fs.Bool("clear-quick", false, "remove every quick action")
	actor := fs.String("actor", "", "actor recorded on the audit trail (default: token name)")
	lead, err := parseArgs(fs, args, 1, "trackd view update <name> [flags]")
	if err != nil {
		return err
	}
	set := setFlags(fs)
	if *shared && *private {
		return usagef("--shared and --private contradict each other")
	}
	if set["description"] && *clearDescription {
		return usagef("--description and --clear-description do the same job; pick one")
	}
	if len(quick) > 0 && *clearQuick {
		return usagef("--quick and --clear-quick do the same job; pick one")
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	// The filter is replaced whole on the wire, so start from what is stored
	// and change only the fields the flags named.
	current, err := cl.GetView(lead[0])
	if err != nil {
		return err
	}
	patch := client.ViewPatch{Actor: *actor}
	filter := current.Filter
	if err := filterFlags.apply(set, &filter); err != nil {
		return err
	}
	clears.apply(&filter)
	if describeFilter(filter) != describeFilter(current.Filter) {
		patch.Filter = &filter
	}
	if set["rename"] {
		patch.Name = strp(*rename)
	}
	if set["description"] {
		patch.Description = strp(*description)
	}
	if *clearDescription {
		patch.Description = strp("")
	}
	if *shared {
		patch.Shared = shared
	}
	if *private {
		no := false
		patch.Shared = &no
	}
	if len(quick) > 0 {
		actions, err := parseQuickActions(quick)
		if err != nil {
			return err
		}
		patch.QuickActions = &actions
	}
	if *clearQuick {
		none := []store.QuickAction{}
		patch.QuickActions = &none
	}
	if patch.Filter == nil && patch.Name == nil && patch.Description == nil && patch.Shared == nil && patch.QuickActions == nil {
		return usagef("nothing to update: give at least one field flag")
	}
	view, err := cl.UpdateView(current.Name, patch)
	if err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(view)
	}
	fmt.Printf("view %s updated\n", quoteIfSpaced(view.Name))
	return nil
}

// viewArchive archives (delete) or restores a view. Nothing is destroyed:
// the row stays, its events stay, and only the name is freed.
func viewArchive(args []string, archive bool) error {
	verb := "delete"
	if !archive {
		verb = "restore"
	}
	fs := flag.NewFlagSet("view "+verb, flag.ContinueOnError)
	common := addCommon(fs)
	actor := fs.String("actor", "", "actor recorded on the audit trail (default: token name)")
	lead, err := parseArgs(fs, args, 1, "trackd view "+verb+" <name> [flags]")
	if err != nil {
		return err
	}
	cl, err := common.client()
	if err != nil {
		return err
	}
	view, err := cl.UpdateView(lead[0], client.ViewPatch{Archived: &archive, Actor: *actor})
	if err != nil {
		return err
	}
	if *common.jsonOut {
		return printJSON(view)
	}
	if archive {
		fmt.Printf("view %s archived (restore with: trackd view restore %s)\n", quoteIfSpaced(view.Name), quoteIfSpaced(view.Name))
	} else {
		fmt.Printf("view %s restored\n", quoteIfSpaced(view.Name))
	}
	return nil
}
