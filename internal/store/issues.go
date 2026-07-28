package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func (s *Store) CreateIssue(in IssueInput, actor string) (*Issue, error) {
	if strings.TrimSpace(in.Title) == "" {
		return nil, errors.New("issue title is required")
	}
	if in.Priority < 0 || in.Priority > 4 {
		return nil, fmt.Errorf("priority %d out of range 0-4", in.Priority)
	}
	statusName := in.Status
	if statusName == "" {
		statusName = "Triage"
	}
	var out *Issue
	err := s.tx(func(tx *sql.Tx) error {
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
		ts := now()
		res, err := tx.Exec(`
			INSERT INTO issues (key, title, description, status_id, priority, project_id, parent_id, assignee, milestone_id, due_date, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			key, in.Title, in.Description, st.ID, in.Priority, projectID, parentID, in.Assignee, milestoneID, nullable(in.DueDate), ts, ts,
		)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if err := setIssueLabels(tx, id, in.Labels); err != nil {
			return err
		}
		out, err = loadIssue(tx, key)
		if err != nil {
			return err
		}
		return recordEvent(tx, "issue", id, actor, "issue.created", nil, out)
	})
	return out, err
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
	var out *Issue
	err := s.tx(func(tx *sql.Tx) error {
		before, err := loadIssue(tx, key)
		if err != nil {
			return err
		}
		sets := []string{"updated_at = ?"}
		args := []any{now()}
		if p.Title != nil {
			if strings.TrimSpace(*p.Title) == "" {
				return errors.New("issue title is required")
			}
			sets, args = append(sets, "title = ?"), append(args, *p.Title)
		}
		if p.Description != nil {
			sets, args = append(sets, "description = ?"), append(args, *p.Description)
		}
		if p.Priority != nil {
			sets, args = append(sets, "priority = ?"), append(args, *p.Priority)
		}
		if p.Status != nil && *p.Status != before.Status {
			st, err := statusByName(tx, *p.Status)
			if err != nil {
				return err
			}
			sets, args = append(sets, "status_id = ?"), append(args, st.ID)
			// First transition into each phase stamps its timestamp; later
			// moves never overwrite it.
			ts := now()
			switch {
			case st.Type == "started" && before.StartedAt == "":
				sets, args = append(sets, "started_at = ?"), append(args, ts)
			case st.Type == "completed" && before.CompletedAt == "":
				sets, args = append(sets, "completed_at = ?"), append(args, ts)
			case st.Type == "canceled" && before.CanceledAt == "":
				sets, args = append(sets, "canceled_at = ?"), append(args, ts)
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
			if *p.Parent == key {
				return errors.New("issue cannot be its own parent")
			}
			parentID, err := optionalIssueID(tx, *p.Parent)
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
		args = append(args, before.ID)
		if _, err := tx.Exec("UPDATE issues SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
			return err
		}
		if p.Labels != nil {
			if err := setIssueLabels(tx, before.ID, *p.Labels); err != nil {
				return err
			}
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
	if !f.IncludeArchived {
		where = append(where, "i.archived_at IS NULL")
	}
	if f.Status != "" {
		where, args = append(where, "s.name = ?"), append(args, f.Status)
	}
	if f.StatusType != "" {
		where, args = append(where, "s.type = ?"), append(args, f.StatusType)
	}
	if f.Project != "" {
		where, args = append(where, "p.slug = ?"), append(args, f.Project)
	}
	if f.Parent != "" {
		where, args = append(where, "pi.key = ?"), append(args, f.Parent)
	}
	if f.Assignee != "" {
		where, args = append(where, "i.assignee = ?"), append(args, f.Assignee)
	}
	if f.Milestone != "" {
		where, args = append(where, "m.name = ?"), append(args, f.Milestone)
	}
	if f.Label != "" {
		where = append(where, "EXISTS (SELECT 1 FROM issue_labels il JOIN labels l ON l.id = il.label_id WHERE il.issue_id = i.id AND l.name = ?)")
		args = append(args, f.Label)
	}
	if f.Query != "" {
		where = append(where, "(i.title LIKE ? OR i.description LIKE ? OR i.key LIKE ?)")
		q := "%" + f.Query + "%"
		args = append(args, q, q, q)
	}
	if f.UpdatedSince != "" {
		where, args = append(where, "i.updated_at >= ?"), append(args, f.UpdatedSince)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	args = append(args, limit, f.Offset)

	var out []Issue
	err := s.tx(func(tx *sql.Tx) error {
		rows, err := tx.Query(issueSelect+" WHERE "+strings.Join(where, " AND ")+
			" ORDER BY i.updated_at DESC, i.id DESC LIMIT ? OFFSET ?", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			issue, err := scanIssue(rows)
			if err != nil {
				return err
			}
			out = append(out, *issue)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			if out[i].Labels, err = issueLabels(tx, out[i].ID); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func (s *Store) AddComment(issueKey, body, actor string) (*Comment, error) {
	if strings.TrimSpace(body) == "" {
		return nil, errors.New("comment body is required")
	}
	var out *Comment
	err := s.tx(func(tx *sql.Tx) error {
		issue, err := loadIssue(tx, issueKey)
		if err != nil {
			return err
		}
		ts := now()
		res, err := tx.Exec(
			"INSERT INTO comments (issue_id, body, actor, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
			issue.ID, body, actor, ts, ts,
		)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		out = &Comment{ID: id, IssueKey: issue.Key, Body: body, Actor: actor, CreatedAt: ts, UpdatedAt: ts}
		return recordEvent(tx, "issue", issue.ID, actor, "comment.created", nil, out)
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
			"SELECT id, body, actor, created_at, updated_at FROM comments WHERE issue_id = ? ORDER BY id",
			issue.ID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c := Comment{IssueKey: issue.Key}
			if err := rows.Scan(&c.ID, &c.Body, &c.Actor, &c.CreatedAt, &c.UpdatedAt); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
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
		rel := Relation{IssueKey: issue.Key, RelatedKey: related.Key, Type: typ}
		return recordEvent(tx, "issue", issue.ID, actor, "relation.added", nil, rel)
	})
}

func (s *Store) RemoveRelation(issueKey, relatedKey, typ, actor string) error {
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
			"DELETE FROM issue_relations WHERE issue_id = ? AND related_id = ? AND type = ?",
			issue.ID, related.ID, typ,
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("relation %s %s %s: %w", issueKey, typ, relatedKey, ErrNotFound)
		}
		rel := Relation{IssueKey: issue.Key, RelatedKey: related.Key, Type: typ}
		return recordEvent(tx, "issue", issue.ID, actor, "relation.removed", rel, nil)
	})
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
	       COALESCE(i.due_date, ''),
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
		&i.Project, &i.Parent, &i.Assignee, &i.Milestone, &i.DueDate,
		&i.CreatedAt, &i.UpdatedAt,
		&i.StartedAt, &i.CompletedAt, &i.CanceledAt, &i.ArchivedAt,
	)
	if err != nil {
		return nil, err
	}
	return &i, nil
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
	return issue, nil
}

func statusByName(tx *sql.Tx, name string) (*Status, error) {
	var st Status
	err := tx.QueryRow("SELECT id, name, type, position FROM statuses WHERE name = ?", name).
		Scan(&st.ID, &st.Name, &st.Type, &st.Position)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("status %q: %w", name, ErrNotFound)
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
	err := tx.QueryRow("SELECT id FROM projects WHERE slug = ?", slug).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("project %s: %w", slug, ErrNotFound)
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
	var id int64
	err := tx.QueryRow("SELECT id FROM issues WHERE key = ?", key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("issue %s: %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return id, nil
}
