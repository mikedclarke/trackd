package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var priorityLabels = []string{"No priority", "Urgent", "High", "Medium", "Low"}

// CreateIssue inserts an issue and returns it. created is false when the input
// carried an idempotency key that has already been used: the original issue
// comes back untouched and no event is recorded.
func (s *Store) CreateIssue(in IssueInput, actor string) (*Issue, bool, error) {
	title, err := validTitle("issue title", in.Title)
	if err != nil {
		return nil, false, err
	}
	if in.Priority < 0 || in.Priority > 4 {
		return nil, false, fmt.Errorf("priority %d out of range 0-4", in.Priority)
	}
	if err := validDate(in.DueDate); err != nil {
		return nil, false, fmt.Errorf("due date: %w", err)
	}
	statusName := in.Status
	if statusName == "" {
		statusName = "Triage"
	}
	var out *Issue
	created := true
	err = s.tx(func(tx *sql.Tx) error {
		if in.IdempotencyKey != "" {
			var existing string
			err := tx.QueryRow("SELECT key FROM issues WHERE idempotency_key = ?", in.IdempotencyKey).Scan(&existing)
			if err == nil {
				created = false
				out, err = loadIssue(tx, existing)
				return err
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		st, err := statusByName(tx, statusName)
		if err != nil {
			return err
		}
		key, err := nextIssueKey(tx)
		if err != nil {
			return err
		}
		projectID, err := optionalProjectID(tx, in.Project)
		if err != nil {
			return err
		}
		parentID, err := optionalIssueID(tx, in.Parent)
		if err != nil {
			return err
		}
		milestoneID, err := optionalMilestoneID(tx, in.Project, in.Milestone)
		if err != nil {
			return err
		}
		labels, err := normalizeLabels(tx, nil, &in.Labels, nil, nil)
		if err != nil {
			return err
		}
		ts := now()
		// An issue born into a phase carries that phase's timestamp, the same
		// as one that reached it through an update.
		var startedAt, completedAt, canceledAt any
		switch st.Type {
		case "started":
			startedAt = ts
		case "completed":
			completedAt = ts
		case "canceled":
			canceledAt = ts
		}
		res, err := tx.Exec(`
			INSERT INTO issues (key, title, description, status_id, priority, project_id, parent_id, assignee, milestone_id, due_date, created_at, updated_at, started_at, completed_at, canceled_at, idempotency_key)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			key, title, in.Description, st.ID, in.Priority, projectID, parentID, in.Assignee, milestoneID, nullable(in.DueDate), ts, ts, startedAt, completedAt, canceledAt, nullable(in.IdempotencyKey),
		)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if err := applyIssueLabels(tx, id, labels); err != nil {
			return err
		}
		out, err = loadIssue(tx, key)
		if err != nil {
			return err
		}
		return recordEvent(tx, "issue", id, actor, "issue.created", nil, out)
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

func (s *Store) GetIssue(key string) (*Issue, error) {
	var out *Issue
	err := s.tx(func(tx *sql.Tx) error {
		var err error
		out, err = loadIssue(tx, key)
		return err
	})
	return out, err
}

func (s *Store) UpdateIssue(key string, p IssuePatch, actor string) (*Issue, error) {
	if p.Priority != nil && (*p.Priority < 0 || *p.Priority > 4) {
		return nil, fmt.Errorf("priority %d out of range 0-4", *p.Priority)
	}
	if p.Labels != nil && (len(p.AddLabels) > 0 || len(p.RemoveLabels) > 0) {
		return nil, errors.New("labels cannot be combined with add_labels or remove_labels")
	}
	if p.DueDate != nil {
		if err := validDate(*p.DueDate); err != nil {
			return nil, fmt.Errorf("due date: %w", err)
		}
	}
	var out *Issue
	err := s.tx(func(tx *sql.Tx) error {
		before, err := loadIssue(tx, key)
		if err != nil {
			return err
		}
		if p.ExpectedVersion != nil && *p.ExpectedVersion != before.Version {
			return fmt.Errorf("issue %s is at version %d, not %d: %w", before.Key, before.Version, *p.ExpectedVersion, ErrVersionConflict)
		}
		sets := []string{"updated_at = ?", "version = version + 1"}
		args := []any{now()}
		if p.Title != nil {
			title, err := validTitle("issue title", *p.Title)
			if err != nil {
				return err
			}
			sets, args = append(sets, "title = ?"), append(args, title)
		}
		if p.Description != nil {
			// Descriptions are append-only unless the caller says otherwise:
			// an agent overwriting another agent's handoff notes is the loss
			// this guards against.
			if before.Description != "" && !p.ReplaceDescription {
				return fmt.Errorf("issue %s: %w", before.Key, ErrDescriptionReplace)
			}
			sets, args = append(sets, "description = ?"), append(args, *p.Description)
		}
		if p.Priority != nil {
			sets, args = append(sets, "priority = ?"), append(args, *p.Priority)
		}
		if p.Status != nil && !strings.EqualFold(*p.Status, before.Status) {
			st, err := statusByName(tx, *p.Status)
			if err != nil {
				return err
			}
			sets, args = append(sets, "status_id = ?"), append(args, st.ID)
			ts := now()
			// started_at is sticky; completed_at and canceled_at follow the
			// status, so reopening an issue clears the one it left behind.
			if st.Type == "started" && before.StartedAt == "" {
				sets, args = append(sets, "started_at = ?"), append(args, ts)
			}
			switch {
			case st.Type == "completed" && before.CompletedAt == "":
				sets, args = append(sets, "completed_at = ?"), append(args, ts)
			case st.Type != "completed" && before.CompletedAt != "":
				sets = append(sets, "completed_at = NULL")
			}
			switch {
			case st.Type == "canceled" && before.CanceledAt == "":
				sets, args = append(sets, "canceled_at = ?"), append(args, ts)
			case st.Type != "canceled" && before.CanceledAt != "":
				sets = append(sets, "canceled_at = NULL")
			}
		}
		if p.Project != nil {
			projectID, err := optionalProjectID(tx, *p.Project)
			if err != nil {
				return err
			}
			sets, args = append(sets, "project_id = ?"), append(args, projectID)
			// Milestones belong to a project: moving the issue clears its
			// milestone unless the patch also sets one valid in the new project.
			if p.Milestone == nil && before.Milestone != "" {
				sets = append(sets, "milestone_id = NULL")
			}
		}
		if p.Assignee != nil {
			sets, args = append(sets, "assignee = ?"), append(args, *p.Assignee)
		}
		if p.Milestone != nil {
			project := before.Project
			if p.Project != nil {
				project = *p.Project
			}
			milestoneID, err := optionalMilestoneID(tx, project, *p.Milestone)
			if err != nil {
				return err
			}
			sets, args = append(sets, "milestone_id = ?"), append(args, milestoneID)
		}
		if p.Parent != nil {
			parentID, err := parentIssueID(tx, before, *p.Parent)
			if err != nil {
				return err
			}
			sets, args = append(sets, "parent_id = ?"), append(args, parentID)
		}
		if p.DueDate != nil {
			sets, args = append(sets, "due_date = ?"), append(args, nullable(*p.DueDate))
		}
		if p.Archived != nil {
			if *p.Archived {
				if before.ArchivedAt == "" {
					sets, args = append(sets, "archived_at = ?"), append(args, now())
				}
			} else {
				sets = append(sets, "archived_at = NULL")
			}
		}
		if p.Labels != nil || len(p.AddLabels) > 0 || len(p.RemoveLabels) > 0 {
			labels, err := normalizeLabels(tx, before.Labels, p.Labels, p.AddLabels, p.RemoveLabels)
			if err != nil {
				return err
			}
			if err := applyIssueLabels(tx, before.ID, labels); err != nil {
				return err
			}
		}
		args = append(args, before.ID)
		if _, err := tx.Exec("UPDATE issues SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
			return err
		}
		out, err = loadIssue(tx, key)
		if err != nil {
			return err
		}
		return recordEvent(tx, "issue", before.ID, actor, "issue.updated", before, out)
	})
	return out, err
}

// AppendDescription adds text to the end of an issue's description, the safe
// way for one agent to add to another's notes.
func (s *Store) AppendDescription(key, text, actor string) (*Issue, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("append text is required")
	}
	var out *Issue
	err := s.tx(func(tx *sql.Tx) error {
		before, err := loadIssue(tx, key)
		if err != nil {
			return err
		}
		description := text
		if before.Description != "" {
			description = before.Description + "\n\n" + text
		}
		if _, err := tx.Exec(
			"UPDATE issues SET description = ?, updated_at = ?, version = version + 1 WHERE id = ?",
			description, now(), before.ID,
		); err != nil {
			return err
		}
		out, err = loadIssue(tx, key)
		if err != nil {
			return err
		}
		return recordEvent(tx, "issue", before.ID, actor, "issue.updated", before, out)
	})
	return out, err
}

func (s *Store) ListIssues(f IssueFilter) ([]Issue, error) {
	where := []string{"1=1"}
	var args []any
	switch strings.ToLower(f.Archived) {
	case "", "false":
		where = append(where, "i.archived_at IS NULL")
	case "true":
	case "only":
		where = append(where, "i.archived_at IS NOT NULL")
	default:
		return nil, fmt.Errorf("unknown archived filter %q, want \"\", \"true\" or \"only\"", f.Archived)
	}
	if len(f.Statuses) > 0 {
		where = append(where, "s.name COLLATE NOCASE IN ("+placeholders(len(f.Statuses))+")")
		for _, v := range f.Statuses {
			args = append(args, v)
		}
	}
	if len(f.StatusTypes) > 0 {
		where = append(where, "s.type COLLATE NOCASE IN ("+placeholders(len(f.StatusTypes))+")")
		for _, v := range f.StatusTypes {
			args = append(args, v)
		}
	}
	if f.Project != "" {
		where, args = append(where, "p.slug = ? COLLATE NOCASE"), append(args, f.Project)
	}
	if f.Parent != "" {
		where, args = append(where, "pi.key = ? COLLATE NOCASE"), append(args, f.Parent)
	}
	if f.Assignee != "" {
		where, args = append(where, "i.assignee = ? COLLATE NOCASE"), append(args, f.Assignee)
	}
	if f.Milestone != "" {
		where, args = append(where, "m.name = ? COLLATE NOCASE"), append(args, f.Milestone)
	}
	for _, label := range f.Labels {
		where = append(where, "EXISTS (SELECT 1 FROM issue_labels il JOIN labels l ON l.id = il.label_id WHERE il.issue_id = i.id AND l.name = ?)")
		args = append(args, label)
	}
	for _, label := range f.ExcludeLabels {
		where = append(where, "NOT EXISTS (SELECT 1 FROM issue_labels il JOIN labels l ON l.id = il.label_id WHERE il.issue_id = i.id AND l.name = ?)")
		args = append(args, label)
	}
	if f.Query != "" {
		where = append(where, `(i.title LIKE ? ESCAPE '\' OR i.description LIKE ? ESCAPE '\' OR i.key LIKE ? ESCAPE '\'`+
			` OR EXISTS (SELECT 1 FROM comments c WHERE c.issue_id = i.id AND c.body LIKE ? ESCAPE '\'))`)
		q := "%" + escapeLike(f.Query) + "%"
		args = append(args, q, q, q, q)
	}
	if f.UpdatedSince != "" {
		where, args = append(where, "i.updated_at >= ?"), append(args, f.UpdatedSince)
	}
	if f.CompletedSince != "" {
		where, args = append(where, "i.completed_at >= ?"), append(args, f.CompletedSince)
	}
	order, err := issueOrder(f.OrderBy)
	if err != nil {
		return nil, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	args = append(args, limit, f.Offset)

	out := []Issue{}
	err = s.tx(func(tx *sql.Tx) error {
		if err := validateIssueFilter(tx, f); err != nil {
			return err
		}
		base, err := settingTx(tx, "base_url")
		if err != nil {
			return err
		}
		rows, err := tx.Query(issueSelect+" WHERE "+strings.Join(where, " AND ")+
			" ORDER BY "+order+" LIMIT ? OFFSET ?", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			issue, err := scanIssue(rows)
			if err != nil {
				return err
			}
			decorateIssue(issue, base)
			out = append(out, *issue)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		ids := make([]int64, len(out))
		for i := range out {
			ids[i] = out[i].ID
		}
		labels, err := issueLabelsFor(tx, ids)
		if err != nil {
			return err
		}
		for i := range out {
			if names, ok := labels[out[i].ID]; ok {
				out[i].Labels = names
			} else {
				out[i].Labels = []string{}
			}
		}
		return nil
	})
	return out, err
}

// statusTypes are the workflow phases a status may carry, matching the CHECK
// constraint on the statuses table.
var statusTypes = []string{"triage", "backlog", "unstarted", "started", "completed", "canceled"}

// validateIssueFilter resolves the filter values that name something before the
// query runs, using the same helpers a write does. A typo would otherwise come
// back as an empty page, which a caller reads as "no work" rather than "bad
// filter". Assignees are free-form names, so they stay lenient.
func validateIssueFilter(tx *sql.Tx, f IssueFilter) error {
	for _, name := range f.Statuses {
		if _, err := statusByName(tx, name); err != nil {
			return err
		}
	}
	for _, typ := range f.StatusTypes {
		if !containsFold(statusTypes, typ) {
			return fmt.Errorf("status type %q: %w", typ, ErrInvalidRef)
		}
	}
	if f.Project != "" {
		if _, err := optionalProjectID(tx, f.Project); err != nil {
			return err
		}
	}
	if _, err := resolveLabels(tx, f.Labels); err != nil {
		return err
	}
	if _, err := resolveLabels(tx, f.ExcludeLabels); err != nil {
		return err
	}
	// The two below are matched by the query itself rather than through the
	// write path's helpers: a milestone filter spans every project and includes
	// archived milestones, and a parent filter compares keys case-insensitively.
	if f.Milestone != "" {
		if err := refExists(tx, "SELECT 1 FROM milestones WHERE name = ? COLLATE NOCASE", "milestone", f.Milestone); err != nil {
			return err
		}
	}
	if f.Parent != "" {
		if err := refExists(tx, "SELECT 1 FROM issues WHERE key = ? COLLATE NOCASE", "parent", f.Parent); err != nil {
			return err
		}
	}
	return nil
}

// refExists reports a filter value that names nothing as an invalid reference.
func refExists(tx *sql.Tx, query, kind, value string) error {
	var one int
	err := tx.QueryRow(query, value).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s %q: %w", kind, value, ErrInvalidRef)
	}
	return err
}

func issueOrder(orderBy string) (string, error) {
	switch orderBy {
	case "", "updated":
		return "i.updated_at DESC, i.id DESC", nil
	case "created":
		return "i.created_at ASC, i.id ASC", nil
	case "priority":
		// Priority 0 means "no priority", which belongs last, not first.
		return "CASE i.priority WHEN 0 THEN 5 ELSE i.priority END ASC, i.created_at ASC", nil
	default:
		return "", fmt.Errorf("unknown order_by %q, want updated, created or priority", orderBy)
	}
}

// escapeLike neutralises the LIKE wildcards in a user's search string; the
// queries pair it with ESCAPE '\'.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// AddComment appends a comment. created is false when the idempotency key has
// already been used, in which case the original comment comes back.
func (s *Store) AddComment(issueKey string, in CommentInput, actor string) (*Comment, bool, error) {
	if strings.TrimSpace(in.Body) == "" {
		return nil, false, errors.New("comment body is required")
	}
	var out *Comment
	created := true
	err := s.tx(func(tx *sql.Tx) error {
		if in.IdempotencyKey != "" {
			var id int64
			err := tx.QueryRow("SELECT id FROM comments WHERE idempotency_key = ?", in.IdempotencyKey).Scan(&id)
			if err == nil {
				created = false
				out, err = loadComment(tx, id)
				return err
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		issue, err := loadIssue(tx, issueKey)
		if err != nil {
			return err
		}
		var parentID any
		if in.ParentID != 0 {
			var owner int64
			err := tx.QueryRow("SELECT issue_id FROM comments WHERE id = ?", in.ParentID).Scan(&owner)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != issue.ID) {
				return fmt.Errorf("comment %d is not on issue %s: %w", in.ParentID, issue.Key, ErrInvalidRef)
			}
			if err != nil {
				return err
			}
			parentID = in.ParentID
		}
		ts := now()
		res, err := tx.Exec(
			"INSERT INTO comments (issue_id, body, actor, parent_id, created_at, updated_at, idempotency_key) VALUES (?, ?, ?, ?, ?, ?, ?)",
			issue.ID, in.Body, actor, parentID, ts, ts, nullable(in.IdempotencyKey),
		)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		// A comment is activity on the issue even though it does not change
		// the issue row's own fields, so it moves updated_at but not version.
		if err := touchIssue(tx, issue.ID, ts); err != nil {
			return err
		}
		out, err = loadComment(tx, id)
		if err != nil {
			return err
		}
		return recordEvent(tx, "issue", issue.ID, actor, "comment.created", nil, out)
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

func (s *Store) UpdateComment(id int64, body, actor string) (*Comment, error) {
	if strings.TrimSpace(body) == "" {
		return nil, errors.New("comment body is required")
	}
	var out *Comment
	err := s.tx(func(tx *sql.Tx) error {
		before, err := loadComment(tx, id)
		if err != nil {
			return err
		}
		ts := now()
		if _, err := tx.Exec("UPDATE comments SET body = ?, updated_at = ? WHERE id = ?", body, ts, id); err != nil {
			return err
		}
		issue, err := loadIssue(tx, before.IssueKey)
		if err != nil {
			return err
		}
		if err := touchIssue(tx, issue.ID, ts); err != nil {
			return err
		}
		out, err = loadComment(tx, id)
		if err != nil {
			return err
		}
		return recordEvent(tx, "issue", issue.ID, actor, "comment.updated", before, out)
	})
	return out, err
}

func (s *Store) ListComments(issueKey string) ([]Comment, error) {
	var out []Comment
	err := s.tx(func(tx *sql.Tx) error {
		issue, err := loadIssue(tx, issueKey)
		if err != nil {
			return err
		}
		rows, err := tx.Query(
			"SELECT id, body, actor, COALESCE(parent_id, 0), created_at, updated_at FROM comments WHERE issue_id = ? ORDER BY id",
			issue.ID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c := Comment{IssueKey: issue.Key}
			if err := rows.Scan(&c.ID, &c.Body, &c.Actor, &c.ParentID, &c.CreatedAt, &c.UpdatedAt); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

func loadComment(tx *sql.Tx, id int64) (*Comment, error) {
	var c Comment
	err := tx.QueryRow(`
		SELECT c.id, i.key, c.body, c.actor, COALESCE(c.parent_id, 0), c.created_at, c.updated_at
		FROM comments c JOIN issues i ON i.id = c.issue_id
		WHERE c.id = ?`, id,
	).Scan(&c.ID, &c.IssueKey, &c.Body, &c.Actor, &c.ParentID, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("comment %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func touchIssue(tx *sql.Tx, issueID int64, ts string) error {
	_, err := tx.Exec("UPDATE issues SET updated_at = ? WHERE id = ?", ts, issueID)
	return err
}

var relationTypes = map[string]bool{"blocks": true, "relates": true, "duplicate": true}

func (s *Store) AddRelation(issueKey, relatedKey, typ, actor string) error {
	if !relationTypes[typ] {
		return fmt.Errorf("unknown relation type %q", typ)
	}
	if issueKey == relatedKey {
		return errors.New("issue cannot relate to itself")
	}
	return s.tx(func(tx *sql.Tx) error {
		issue, err := loadIssue(tx, issueKey)
		if err != nil {
			return err
		}
		related, err := loadIssue(tx, relatedKey)
		if err != nil {
			return err
		}
		res, err := tx.Exec(
			"INSERT OR IGNORE INTO issue_relations (issue_id, related_id, type) VALUES (?, ?, ?)",
			issue.ID, related.ID, typ,
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		ts := now()
		if err := touchIssue(tx, issue.ID, ts); err != nil {
			return err
		}
		if err := touchIssue(tx, related.ID, ts); err != nil {
			return err
		}
		rel := Relation{IssueKey: issue.Key, RelatedKey: related.Key, Type: typ}
		return recordEvent(tx, "issue", issue.ID, actor, "relation.added", nil, rel)
	})
}

// RemoveRelation unlinks two issues and reports whether there was a link to
// remove. Removal is idempotent: a relation that is already gone is the state
// the caller asked for, not a failure, so a retry after a lost connection
// answers the same as the call that got through. Both issues must still
// exist, and the type must still be a real one: those are typos, not states.
func (s *Store) RemoveRelation(issueKey, relatedKey, typ, actor string) (bool, error) {
	if !relationTypes[typ] {
		return false, fmt.Errorf("unknown relation type %q", typ)
	}
	removed := false
	err := s.tx(func(tx *sql.Tx) error {
		issue, err := loadIssue(tx, issueKey)
		if err != nil {
			return err
		}
		related, err := loadIssue(tx, relatedKey)
		if err != nil {
			return err
		}
		res, err := tx.Exec(
			"DELETE FROM issue_relations WHERE issue_id = ? AND related_id = ? AND type = ?",
			issue.ID, related.ID, typ,
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		removed = true
		ts := now()
		if err := touchIssue(tx, issue.ID, ts); err != nil {
			return err
		}
		if err := touchIssue(tx, related.ID, ts); err != nil {
			return err
		}
		rel := Relation{IssueKey: issue.Key, RelatedKey: related.Key, Type: typ}
		return recordEvent(tx, "issue", issue.ID, actor, "relation.removed", rel, nil)
	})
	if err != nil {
		return false, err
	}
	return removed, nil
}

func (s *Store) ListRelations(issueKey string) ([]Relation, error) {
	var out []Relation
	err := s.tx(func(tx *sql.Tx) error {
		issue, err := loadIssue(tx, issueKey)
		if err != nil {
			return err
		}
		rows, err := tx.Query(`
			SELECT a.key, b.key, r.type
			FROM issue_relations r
			JOIN issues a ON a.id = r.issue_id
			JOIN issues b ON b.id = r.related_id
			WHERE r.issue_id = ? OR r.related_id = ?
			ORDER BY a.key, b.key, r.type`,
			issue.ID, issue.ID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Relation
			if err := rows.Scan(&r.IssueKey, &r.RelatedKey, &r.Type); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// ListAssignees returns the distinct assignees on unarchived issues.
func (s *Store) ListAssignees() ([]string, error) {
	rows, err := s.db.Query("SELECT DISTINCT assignee FROM issues WHERE assignee != '' AND archived_at IS NULL ORDER BY assignee")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ListStatuses() ([]Status, error) {
	rows, err := s.db.Query("SELECT id, name, type, position FROM statuses ORDER BY position")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Status
	for rows.Next() {
		var st Status
		if err := rows.Scan(&st.ID, &st.Name, &st.Type, &st.Position); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

const issueSelect = `
	SELECT i.id, i.key, i.title, i.description, s.name, s.type, i.priority,
	       COALESCE(p.slug, ''), COALESCE(pi.key, ''), i.assignee, COALESCE(m.name, ''),
	       COALESCE(i.due_date, ''), i.version,
	       i.created_at, i.updated_at,
	       COALESCE(i.started_at, ''), COALESCE(i.completed_at, ''),
	       COALESCE(i.canceled_at, ''), COALESCE(i.archived_at, '')
	FROM issues i
	JOIN statuses s ON s.id = i.status_id
	LEFT JOIN projects p ON p.id = i.project_id
	LEFT JOIN issues pi ON pi.id = i.parent_id
	LEFT JOIN milestones m ON m.id = i.milestone_id`

type rowScanner interface{ Scan(dest ...any) error }

func scanIssue(r rowScanner) (*Issue, error) {
	var i Issue
	err := r.Scan(
		&i.ID, &i.Key, &i.Title, &i.Description, &i.Status, &i.StatusType, &i.Priority,
		&i.Project, &i.Parent, &i.Assignee, &i.Milestone, &i.DueDate, &i.Version,
		&i.CreatedAt, &i.UpdatedAt,
		&i.StartedAt, &i.CompletedAt, &i.CanceledAt, &i.ArchivedAt,
	)
	if err != nil {
		return nil, err
	}
	return &i, nil
}

// decorateIssue fills the fields derived at read time rather than stored.
func decorateIssue(i *Issue, baseURL string) {
	if i.Priority >= 0 && i.Priority < len(priorityLabels) {
		i.PriorityLabel = priorityLabels[i.Priority]
	}
	if baseURL != "" {
		i.URL = strings.TrimRight(baseURL, "/") + "/ui/issue/" + i.Key
	}
}

func loadIssue(tx *sql.Tx, key string) (*Issue, error) {
	issue, err := scanIssue(tx.QueryRow(issueSelect+" WHERE i.key = ?", key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("issue %s: %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if issue.Labels, err = issueLabels(tx, issue.ID); err != nil {
		return nil, err
	}
	base, err := settingTx(tx, "base_url")
	if err != nil {
		return nil, err
	}
	decorateIssue(issue, base)
	return issue, nil
}

func statusByName(tx *sql.Tx, name string) (*Status, error) {
	var st Status
	err := tx.QueryRow("SELECT id, name, type, position FROM statuses WHERE name = ? COLLATE NOCASE", name).
		Scan(&st.ID, &st.Name, &st.Type, &st.Position)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("status %q: %w", name, ErrInvalidRef)
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

func nextIssueKey(tx *sql.Tx) (string, error) {
	prefix, err := settingTx(tx, "issue_prefix")
	if err != nil {
		return "", err
	}
	if prefix == "" {
		prefix = "TSK"
	}
	raw, err := settingTx(tx, "issue_seq")
	if err != nil {
		return "", err
	}
	seq, _ := strconv.Atoi(raw)
	seq++
	if _, err := tx.Exec("UPDATE settings SET value = ? WHERE key = 'issue_seq'", strconv.Itoa(seq)); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%d", prefix, seq), nil
}

func optionalProjectID(tx *sql.Tx, slug string) (any, error) {
	if slug == "" {
		return nil, nil
	}
	var id int64
	err := tx.QueryRow("SELECT id FROM projects WHERE slug = ? COLLATE NOCASE", slug).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("project %s: %w", slug, ErrInvalidRef)
	}
	if err != nil {
		return nil, err
	}
	return id, nil
}

func optionalIssueID(tx *sql.Tx, key string) (any, error) {
	if key == "" {
		return nil, nil
	}
	id, err := issueIDByKey(tx, key)
	if err != nil {
		return nil, err
	}
	return id, nil
}

func issueIDByKey(tx *sql.Tx, key string) (int64, error) {
	var id int64
	err := tx.QueryRow("SELECT id FROM issues WHERE key = ?", key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("issue %s: %w", key, ErrInvalidRef)
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

// parentIssueID resolves a patch's parent key and refuses anything that would
// make the hierarchy a loop.
func parentIssueID(tx *sql.Tx, issue *Issue, parentKey string) (any, error) {
	if parentKey == "" {
		return nil, nil
	}
	if parentKey == issue.Key {
		return nil, fmt.Errorf("issue %s cannot be its own parent: %w", issue.Key, ErrInvalidRef)
	}
	parentID, err := issueIDByKey(tx, parentKey)
	if err != nil {
		return nil, err
	}
	// Walk up from the proposed parent: reaching this issue means the edge
	// would close a cycle. The depth cap stops a pre-existing loop spinning.
	id := parentID
	for depth := 0; depth < 1000; depth++ {
		if id == issue.ID {
			return nil, fmt.Errorf("issue %s is already above %s: %w", issue.Key, parentKey, ErrInvalidRef)
		}
		var next sql.NullInt64
		if err := tx.QueryRow("SELECT parent_id FROM issues WHERE id = ?", id).Scan(&next); err != nil {
			return nil, err
		}
		if !next.Valid {
			return parentID, nil
		}
		id = next.Int64
	}
	return nil, fmt.Errorf("parent chain above %s is too deep: %w", parentKey, ErrInvalidRef)
}
