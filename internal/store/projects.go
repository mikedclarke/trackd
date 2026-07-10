package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var projectStatuses = map[string]bool{"active": true, "paused": true, "completed": true, "canceled": true}

func (s *Store) CreateProject(in ProjectInput, actor string) (*Project, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, errors.New("project name is required")
	}
	slug := in.Slug
	if slug == "" {
		slug = slugify(in.Name)
	}
	if slug == "" {
		return nil, fmt.Errorf("cannot derive a slug from %q", in.Name)
	}
	status := in.Status
	if status == "" {
		status = "active"
	}
	if !projectStatuses[status] {
		return nil, fmt.Errorf("unknown project status %q", status)
	}
	var out *Project
	err := s.tx(func(tx *sql.Tx) error {
		ts := now()
		res, err := tx.Exec(
			"INSERT INTO projects (name, slug, description, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
			in.Name, slug, in.Description, status, ts, ts,
		)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
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
	if p.Status != nil && !projectStatuses[*p.Status] {
		return nil, fmt.Errorf("unknown project status %q", *p.Status)
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
			if strings.TrimSpace(*p.Name) == "" {
				return errors.New("project name is required")
			}
			sets, args = append(sets, "name = ?"), append(args, *p.Name)
		}
		if p.Description != nil {
			sets, args = append(sets, "description = ?"), append(args, *p.Description)
		}
		if p.Status != nil {
			sets, args = append(sets, "status = ?"), append(args, *p.Status)
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

func (s *Store) SetProjectLabels(slug string, names []string, actor string) (*Project, error) {
	var out *Project
	err := s.tx(func(tx *sql.Tx) error {
		before, err := loadProject(tx, slug)
		if err != nil {
			return err
		}
		if err := setProjectLabels(tx, before.ID, names); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE projects SET updated_at = ? WHERE id = ?", now(), before.ID); err != nil {
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

func (s *Store) ListProjects(includeArchived bool) ([]Project, error) {
	where := "WHERE archived_at IS NULL"
	if includeArchived {
		where = ""
	}
	var out []Project
	err := s.tx(func(tx *sql.Tx) error {
		rows, err := tx.Query(
			"SELECT id, name, slug, description, status, created_at, updated_at, COALESCE(archived_at, '') FROM projects " +
				where + " ORDER BY name",
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p Project
			if err := rows.Scan(&p.ID, &p.Name, &p.Slug, &p.Description, &p.Status, &p.CreatedAt, &p.UpdatedAt, &p.ArchivedAt); err != nil {
				return err
			}
			out = append(out, p)
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

func loadProject(tx *sql.Tx, slug string) (*Project, error) {
	var p Project
	err := tx.QueryRow(
		"SELECT id, name, slug, description, status, created_at, updated_at, COALESCE(archived_at, '') FROM projects WHERE slug = ?",
		slug,
	).Scan(&p.ID, &p.Name, &p.Slug, &p.Description, &p.Status, &p.CreatedAt, &p.UpdatedAt, &p.ArchivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("project %s: %w", slug, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if p.Labels, err = projectLabels(tx, p.ID); err != nil {
		return nil, err
	}
	return &p, nil
}

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(name string) string {
	slug := slugStrip.ReplaceAllString(strings.ToLower(name), "-")
	return strings.Trim(slug, "-")
}
