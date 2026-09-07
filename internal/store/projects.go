package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Linear's project state vocabulary, so an imported project keeps its state.
var projectStatuses = map[string]bool{
	"backlog":   true,
	"planned":   true,
	"started":   true,
	"paused":    true,
	"completed": true,
	"canceled":  true,
}

func (s *Store) CreateProject(in ProjectInput, actor string) (*Project, error) {
	name, err := validTitle("project name", in.Name)
	if err != nil {
		return nil, err
	}
	slug := in.Slug
	if slug == "" {
		slug = slugify(name)
	}
	if slug == "" {
		return nil, fmt.Errorf("cannot derive a slug from %q", name)
	}
	status := in.Status
	if status == "" {
		status = "backlog"
	}
	status = strings.ToLower(status)
	if !projectStatuses[status] {
		return nil, fmt.Errorf("unknown project status %q", status)
	}
	if err := validDate(in.StartDate); err != nil {
		return nil, fmt.Errorf("start date: %w", err)
	}
	if err := validDate(in.TargetDate); err != nil {
		return nil, fmt.Errorf("target date: %w", err)
	}
	var out *Project
	err = s.tx(func(tx *sql.Tx) error {
		if err := projectNameFree(tx, name, 0); err != nil {
			return err
		}
		var taken int64
		err := tx.QueryRow("SELECT id FROM projects WHERE slug = ? COLLATE NOCASE", slug).Scan(&taken)
		if err == nil {
			return fmt.Errorf("project slug %q is already taken: %w", slug, ErrConflict)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		labels, err := normalizeLabels(tx, nil, &in.Labels, nil, nil)
		if err != nil {
			return err
		}
		ts := now()
		var completedAt any
		if status == "completed" {
			completedAt = ts
		}
		res, err := tx.Exec(
			"INSERT INTO projects (name, slug, description, status, start_date, target_date, created_at, updated_at, completed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			name, slug, in.Description, status, nullable(in.StartDate), nullable(in.TargetDate), ts, ts, completedAt,
		)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if err := applyProjectLabels(tx, id, labels); err != nil {
			return err
		}
		out, err = loadProject(tx, slug)
		if err != nil {
			return err
		}
		return recordEvent(tx, "project", id, actor, "project.created", nil, out)
	})
	return out, err
}

func (s *Store) GetProject(slug string) (*Project, error) {
	var out *Project
	err := s.tx(func(tx *sql.Tx) error {
		var err error
		out, err = loadProject(tx, slug)
		return err
	})
	return out, err
}

func (s *Store) UpdateProject(slug string, p ProjectPatch, actor string) (*Project, error) {
	if p.Status != nil && !projectStatuses[strings.ToLower(*p.Status)] {
		return nil, fmt.Errorf("unknown project status %q", *p.Status)
	}
	if p.Labels != nil && (len(p.AddLabels) > 0 || len(p.RemoveLabels) > 0) {
		return nil, errors.New("labels cannot be combined with add_labels or remove_labels")
	}
	for _, d := range []struct {
		field string
		value *string
	}{{"start date", p.StartDate}, {"target date", p.TargetDate}} {
		if d.value != nil {
			if err := validDate(*d.value); err != nil {
				return nil, fmt.Errorf("%s: %w", d.field, err)
			}
		}
	}
	var out *Project
	err := s.tx(func(tx *sql.Tx) error {
		before, err := loadProject(tx, slug)
		if err != nil {
			return err
		}
		sets := []string{"updated_at = ?"}
		args := []any{now()}
		if p.Name != nil {
			name, err := validTitle("project name", *p.Name)
			if err != nil {
				return err
			}
			if err := projectNameFree(tx, name, before.ID); err != nil {
				return err
			}
			sets, args = append(sets, "name = ?"), append(args, name)
		}
		if p.Description != nil {
			sets, args = append(sets, "description = ?"), append(args, *p.Description)
		}
		if p.Status != nil {
			status := strings.ToLower(*p.Status)
			sets, args = append(sets, "status = ?"), append(args, status)
			switch {
			case status == "completed" && before.CompletedAt == "":
				sets, args = append(sets, "completed_at = ?"), append(args, now())
			case status != "completed" && before.CompletedAt != "":
				sets = append(sets, "completed_at = NULL")
			}
		}
		if p.StartDate != nil {
			sets, args = append(sets, "start_date = ?"), append(args, nullable(*p.StartDate))
		}
		if p.TargetDate != nil {
			sets, args = append(sets, "target_date = ?"), append(args, nullable(*p.TargetDate))
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
			if err := applyProjectLabels(tx, before.ID, labels); err != nil {
				return err
			}
		}
		args = append(args, before.ID)
		if _, err := tx.Exec("UPDATE projects SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
			return err
		}
		out, err = loadProject(tx, slug)
		if err != nil {
			return err
		}
		return recordEvent(tx, "project", before.ID, actor, "project.updated", before, out)
	})
	return out, err
}

// projectNameFree rejects a name another project already holds, compared
// without case so "Site Rebuild" and "site rebuild" cannot both exist.
func projectNameFree(tx *sql.Tx, name string, exceptID int64) error {
	var id int64
	err := tx.QueryRow("SELECT id FROM projects WHERE name = ? COLLATE NOCASE AND id != ?", name, exceptID).Scan(&id)
	if err == nil {
		return fmt.Errorf("project %q already exists: %w", name, ErrConflict)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func (s *Store) ListProjects(includeArchived bool) ([]Project, error) {
	where := "WHERE archived_at IS NULL"
	if includeArchived {
		where = ""
	}
	var out []Project
	err := s.tx(func(tx *sql.Tx) error {
		rows, err := tx.Query(projectSelect + " " + where + " ORDER BY name")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanProject(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			if out[i].Labels, err = projectLabels(tx, out[i].ID); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

const projectSelect = `
	SELECT id, name, slug, description, status,
	       COALESCE(start_date, ''), COALESCE(target_date, ''),
	       created_at, updated_at, COALESCE(completed_at, ''), COALESCE(archived_at, '')
	FROM projects`

func scanProject(r rowScanner) (*Project, error) {
	var p Project
	err := r.Scan(
		&p.ID, &p.Name, &p.Slug, &p.Description, &p.Status,
		&p.StartDate, &p.TargetDate,
		&p.CreatedAt, &p.UpdatedAt, &p.CompletedAt, &p.ArchivedAt,
	)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func loadProject(tx *sql.Tx, slug string) (*Project, error) {
	p, err := scanProject(tx.QueryRow(projectSelect+" WHERE slug = ? COLLATE NOCASE", slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("project %s: %w", slug, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if p.Labels, err = projectLabels(tx, p.ID); err != nil {
		return nil, err
	}
	return p, nil
}

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(name string) string {
	slug := slugStrip.ReplaceAllString(strings.ToLower(name), "-")
	return strings.Trim(slug, "-")
}
