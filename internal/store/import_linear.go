package store

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LinearImportStats reports what a Linear CSV import did (or would do, on a
// dry run).
type LinearImportStats struct {
	Issues          int            `json:"issues"`
	Projects        int            `json:"projects"`
	Labels          int            `json:"labels"`
	Milestones      int            `json:"milestones"`
	Assignees       int            `json:"assignees"`
	Relations       map[string]int `json:"relations"`
	StatusesCreated []string       `json:"statuses_created,omitempty"`
	Prefix          string         `json:"prefix"`
	Seq             int            `json:"seq"`
	Skipped         []string       `json:"skipped,omitempty"`
}

// linearStatusTypes maps Linear's default workflow names onto status types for
// statuses that are not already in the database.
var linearStatusTypes = map[string]string{
	"Triage":      "triage",
	"Backlog":     "backlog",
	"Todo":        "unstarted",
	"In Progress": "started",
	"In Review":   "started",
	"Done":        "completed",
	"Canceled":    "canceled",
	"Duplicate":   "canceled",
}

var linearPriorities = map[string]int{
	"No priority": 0,
	"Urgent":      1,
	"High":        2,
	"Medium":      3,
	"Low":         4,
}

type linearRow struct {
	key         string
	title       string
	description string
	status      string
	priority    int
	project     string
	assignee    string
	milestone   string
	labels      []string
	parent      string
	relatedTo   []string
	blockedBy   []string
	duplicateOf string
	dueDate     string
	createdAt   string
	updatedAt   string
	startedAt   string
	completedAt string
	canceledAt  string
	archivedAt  string
}

// ImportLinearCSV loads a Linear issue export (CSV) into an empty database.
// Comments are not part of Linear's CSV export and cannot be migrated here.
// With dryRun set, everything is parsed, validated, and counted, then rolled
// back.
func (s *Store) ImportLinearCSV(r io.Reader, actor string, dryRun bool) (*LinearImportStats, error) {
	empty, err := s.isEmpty()
	if err != nil {
		return nil, err
	}
	if !empty {
		return nil, fmt.Errorf("refusing to import into a non-empty database %s", s.path)
	}
	rows, err := parseLinearCSV(r)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no issues found in export")
	}

	stats := &LinearImportStats{Relations: map[string]int{}}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := s.importLinearRows(tx, rows, actor, stats); err != nil {
		return nil, err
	}
	if dryRun {
		return stats, tx.Rollback()
	}
	return stats, tx.Commit()
}

func (s *Store) importLinearRows(tx *sql.Tx, rows []linearRow, actor string, stats *LinearImportStats) error {
	// Statuses: reuse existing by name, create the rest.
	statusIDs := map[string]int64{}
	position := 0
	existing, err := tx.Query("SELECT id, name, position FROM statuses")
	if err != nil {
		return err
	}
	for existing.Next() {
		var id int64
		var name string
		var pos int
		if err := existing.Scan(&id, &name, &pos); err != nil {
			existing.Close()
			return err
		}
		statusIDs[name] = id
		if pos > position {
			position = pos
		}
	}
	existing.Close()
	if err := existing.Err(); err != nil {
		return err
	}
	for _, row := range rows {
		if _, ok := statusIDs[row.status]; ok {
			continue
		}
		typ := linearStatusTypes[row.status]
		if typ == "" {
			typ = "unstarted"
		}
		position++
		res, err := tx.Exec("INSERT INTO statuses (name, type, position) VALUES (?, ?, ?)", row.status, typ, position)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		statusIDs[row.status] = id
		stats.StatusesCreated = append(stats.StatusesCreated, row.status)
	}

	// Projects, deduped by name, slugs made unique.
	projectIDs := map[string]int64{}
	usedSlugs := map[string]bool{}
	for _, row := range rows {
		if row.project == "" {
			continue
		}
		if _, ok := projectIDs[row.project]; ok {
			continue
		}
		slug := slugify(row.project)
		if slug == "" {
			slug = "project"
		}
		base := slug
		for n := 2; usedSlugs[slug]; n++ {
			slug = fmt.Sprintf("%s-%d", base, n)
		}
		usedSlugs[slug] = true
		ts := now()
		res, err := tx.Exec(
			"INSERT INTO projects (name, slug, description, status, created_at, updated_at) VALUES (?, ?, '', 'planned', ?, ?)",
			row.project, slug, ts, ts,
		)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		projectIDs[row.project] = id
		stats.Projects++
	}

	// Milestones, deduped per project. Linear scopes them to a project; rows
	// with a milestone but no project can't keep it.
	milestoneIDs := map[string]int64{}
	assigneesSeen := map[string]bool{}
	for _, row := range rows {
		if row.milestone == "" || row.project == "" {
			continue
		}
		mkey := row.project + "\x00" + row.milestone
		if _, ok := milestoneIDs[mkey]; ok {
			continue
		}
		ts := now()
		res, err := tx.Exec(
			"INSERT INTO milestones (project_id, name, description, created_at, updated_at) VALUES (?, ?, '', ?, ?)",
			projectIDs[row.project], row.milestone, ts, ts,
		)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		milestoneIDs[mkey] = id
		stats.Milestones++
	}

	// First pass: issues and labels.
	issueIDs := map[string]int64{}
	labelSeen := map[string]bool{}
	for _, row := range rows {
		var projectID any
		if row.project != "" {
			projectID = projectIDs[row.project]
		}
		var milestoneID any
		if row.milestone != "" {
			if row.project == "" {
				stats.Skipped = append(stats.Skipped, fmt.Sprintf("%s: milestone %q dropped (issue has no project)", row.key, row.milestone))
			} else {
				milestoneID = milestoneIDs[row.project+"\x00"+row.milestone]
			}
		}
		if row.assignee != "" && !assigneesSeen[row.assignee] {
			assigneesSeen[row.assignee] = true
			stats.Assignees++
		}
		createdAt := row.createdAt
		if createdAt == "" {
			createdAt = now()
		}
		updatedAt := row.updatedAt
		if updatedAt == "" {
			updatedAt = createdAt
		}
		res, err := tx.Exec(`
			INSERT INTO issues (key, title, description, status_id, priority, project_id, assignee, milestone_id, due_date, created_at, updated_at, started_at, completed_at, canceled_at, archived_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.key, row.title, row.description, statusIDs[row.status], row.priority, projectID, row.assignee, milestoneID,
			nullable(row.dueDate), createdAt, updatedAt,
			nullable(row.startedAt), nullable(row.completedAt), nullable(row.canceledAt), nullable(row.archivedAt),
		)
		if err != nil {
			return fmt.Errorf("issue %s: %w", row.key, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		issueIDs[row.key] = id
		for _, name := range dedupeLabelNames(row.labels) {
			labelID, err := ensureLabel(tx, name)
			if err != nil {
				return err
			}
			if _, err := tx.Exec("INSERT INTO issue_labels (issue_id, label_id) VALUES (?, ?)", id, labelID); err != nil {
				return err
			}
			if !labelSeen[name] {
				labelSeen[name] = true
				stats.Labels++
			}
		}
		stats.Issues++
	}

	// Second pass: parents and relations, now that every key resolves.
	skip := func(format string, args ...any) {
		stats.Skipped = append(stats.Skipped, fmt.Sprintf(format, args...))
	}
	relatesSeen := map[string]bool{}
	for _, row := range rows {
		id := issueIDs[row.key]
		if row.parent != "" {
			parentID, ok := issueIDs[row.parent]
			if !ok {
				skip("%s: parent %s not in export", row.key, row.parent)
			} else if _, err := tx.Exec("UPDATE issues SET parent_id = ? WHERE id = ?", parentID, id); err != nil {
				return err
			}
		}
		addRelation := func(fromID, toID int64, typ string) error {
			res, err := tx.Exec("INSERT OR IGNORE INTO issue_relations (issue_id, related_id, type) VALUES (?, ?, ?)", fromID, toID, typ)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				stats.Relations[typ]++
			}
			return nil
		}
		for _, blocker := range row.blockedBy {
			blockerID, ok := issueIDs[blocker]
			if !ok {
				skip("%s: blocker %s not in export", row.key, blocker)
				continue
			}
			if err := addRelation(blockerID, id, "blocks"); err != nil {
				return err
			}
		}
		for _, related := range row.relatedTo {
			relatedID, ok := issueIDs[related]
			if !ok {
				skip("%s: related %s not in export", row.key, related)
				continue
			}
			// "Related to" appears on both sides of the pair; keep one row.
			a, b := row.key, related
			if a > b {
				a, b = b, a
			}
			pair := a + "\x00" + b
			if relatesSeen[pair] {
				continue
			}
			relatesSeen[pair] = true
			if err := addRelation(id, relatedID, "relates"); err != nil {
				return err
			}
		}
		if row.duplicateOf != "" {
			originalID, ok := issueIDs[row.duplicateOf]
			if !ok {
				skip("%s: duplicate-of %s not in export", row.key, row.duplicateOf)
			} else if err := addRelation(id, originalID, "duplicate"); err != nil {
				return err
			}
		}
		if err := recordEvent(tx, "issue", id, actor, "issue.imported", nil, map[string]string{"key": row.key, "source": "linear"}); err != nil {
			return err
		}
	}

	// Continue the key sequence of the dominant prefix.
	maxSeq := map[string]int{}
	count := map[string]int{}
	for _, row := range rows {
		prefix, numStr, ok := strings.Cut(row.key, "-")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(numStr)
		if err != nil {
			continue
		}
		count[prefix]++
		if n > maxSeq[prefix] {
			maxSeq[prefix] = n
		}
	}
	for prefix := range count {
		if stats.Prefix == "" || count[prefix] > count[stats.Prefix] {
			stats.Prefix = prefix
		}
	}
	stats.Seq = maxSeq[stats.Prefix]
	if _, err := tx.Exec("UPDATE settings SET value = ? WHERE key = 'issue_prefix'", stats.Prefix); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE settings SET value = ? WHERE key = 'issue_seq'", strconv.Itoa(stats.Seq)); err != nil {
		return err
	}
	sort.Strings(stats.Skipped)
	return nil
}

func parseLinearCSV(r io.Reader) ([]linearRow, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("reading CSV header: %w", err)
	}
	col := map[string]int{}
	for i, name := range header {
		col[strings.TrimSpace(name)] = i
	}
	for _, required := range []string{"ID", "Title", "Status", "Created", "Updated"} {
		if _, ok := col[required]; !ok {
			return nil, fmt.Errorf("not a Linear issue export: missing column %q", required)
		}
	}
	get := func(record []string, name string) string {
		i, ok := col[name]
		if !ok || i >= len(record) {
			return ""
		}
		return strings.TrimSpace(record[i])
	}
	var rows []linearRow
	for line := 2; ; line++ {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		row := linearRow{
			key:         get(record, "ID"),
			title:       get(record, "Title"),
			description: get(record, "Description"),
			status:      get(record, "Status"),
			project:     get(record, "Project"),
			assignee:    get(record, "Assignee"),
			milestone:   get(record, "Project Milestone"),
			labels:      splitKeys(get(record, "Labels")),
			parent:      get(record, "Parent issue"),
			relatedTo:   splitKeys(get(record, "Related to")),
			blockedBy:   splitKeys(get(record, "Blocked by")),
			duplicateOf: get(record, "Duplicate of"),
		}
		if row.milestone == "" {
			row.milestone = get(record, "Milestone")
		}
		if row.key == "" {
			return nil, fmt.Errorf("line %d: empty issue ID", line)
		}
		if row.title == "" {
			row.title = row.key
		}
		if p, ok := linearPriorities[get(record, "Priority")]; ok {
			row.priority = p
		} else if n, err := strconv.Atoi(get(record, "Priority")); err == nil && n >= 0 && n <= 4 {
			row.priority = n
		}
		for _, ts := range []struct {
			column string
			dest   *string
		}{
			{"Created", &row.createdAt},
			{"Updated", &row.updatedAt},
			{"Started", &row.startedAt},
			{"Completed", &row.completedAt},
			{"Canceled", &row.canceledAt},
			{"Archived", &row.archivedAt},
		} {
			parsed, err := parseLinearTime(get(record, ts.column))
			if err != nil {
				return nil, fmt.Errorf("line %d (%s): %s: %w", line, row.key, ts.column, err)
			}
			*ts.dest = parsed
		}
		due, err := parseLinearDueDate(get(record, "Due Date"))
		if err != nil {
			return nil, fmt.Errorf("line %d (%s): Due Date: %w", line, row.key, err)
		}
		row.dueDate = due
		rows = append(rows, row)
	}
	return rows, nil
}

func splitKeys(s string) []string {
	if s == "" {
		return nil
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

var linearTimeLayouts = []string{
	"Mon Jan 02 2006 15:04:05 GMT-0700",
	"Mon Jan 2 2006 15:04:05 GMT-0700",
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02",
}

func parseLinearTimeRaw(s string) (time.Time, error) {
	// Strip the trailing "(GMT+00:00)" style annotation.
	if i := strings.Index(s, " ("); i > 0 {
		s = s[:i]
	}
	for _, layout := range linearTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp %q", s)
}

func parseLinearTime(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	t, err := parseLinearTimeRaw(s)
	if err != nil {
		return "", err
	}
	return t.UTC().Format(time.RFC3339), nil
}

// parseLinearDueDate recovers the calendar date from Linear's export, which
// renders date-only fields as midnight in the exporting account's timezone.
// Rounding to the nearest UTC date boundary yields the intended date for any
// offset within (-12h, +12h].
func parseLinearDueDate(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	t, err := parseLinearTimeRaw(s)
	if err != nil {
		return "", err
	}
	return t.UTC().Add(12 * time.Hour).Format("2006-01-02"), nil
}
