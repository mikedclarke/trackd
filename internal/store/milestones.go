package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

func (s *Store) CreateMilestone(in MilestoneInput, actor string) (*Milestone, error) {
	name, err := validTitle("milestone name", in.Name)
	if err != nil {
		return nil, err
	}
	if in.Project == "" {
		return nil, errors.New("milestone project is required")
	}
	if err := validDate(in.TargetDate); err != nil {
		return nil, fmt.Errorf("target date: %w", err)
	}
	var out *Milestone
	err = s.tx(func(tx *sql.Tx) error {
		projectID, err := optionalProjectID(tx, in.Project)
		if err != nil {
			return err
		}
		ts := now()
		res, err := tx.Exec(`
			INSERT INTO milestones (project_id, name, description, target_date, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			projectID, name, in.Description, nullable(in.TargetDate), ts, ts,
		)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return fmt.Errorf("milestone %q already exists in project %s: %w", name, in.Project, ErrConflict)
			}
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		out, err = loadMilestone(tx, id)
		if err != nil {
			return err
		}
		return recordEvent(tx, "milestone", id, actor, "milestone.created", nil, out)
	})
	return out, err
}

func (s *Store) GetMilestone(id int64) (*Milestone, error) {
	var out *Milestone
	err := s.tx(func(tx *sql.Tx) error {
		var err error
		out, err = loadMilestone(tx, id)
		return err
	})
	return out, err
}

func (s *Store) UpdateMilestone(id int64, p MilestonePatch, actor string) (*Milestone, error) {
	if p.TargetDate != nil {
		if err := validDate(*p.TargetDate); err != nil {
			return nil, fmt.Errorf("target date: %w", err)
		}
	}
	var out *Milestone
	err := s.tx(func(tx *sql.Tx) error {
		before, err := loadMilestone(tx, id)
		if err != nil {
			return err
		}
		sets := []string{"updated_at = ?"}
		args := []any{now()}
		if p.Name != nil {
			name, err := validTitle("milestone name", *p.Name)
			if err != nil {
				return err
			}
			sets, args = append(sets, "name = ?"), append(args, name)
		}
		if p.Description != nil {
			sets, args = append(sets, "description = ?"), append(args, *p.Description)
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
		args = append(args, id)
		if _, err := tx.Exec("UPDATE milestones SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				// The name in play is the patched one when the patch sets it,
				// otherwise the stored one being un-archived back into a clash.
				name := before.Name
				if p.Name != nil {
					name = *p.Name
				}
				return fmt.Errorf("milestone %q already exists in project %s: %w", name, before.Project, ErrConflict)
			}
			return err
		}
		out, err = loadMilestone(tx, id)
		if err != nil {
			return err
		}
		return recordEvent(tx, "milestone", id, actor, "milestone.updated", before, out)
	})
	return out, err
}

func (s *Store) ListMilestones(project string, includeArchived bool) ([]Milestone, error) {
	where := []string{"1=1"}
	var args []any
	if project != "" {
		where, args = append(where, "p.slug = ? COLLATE NOCASE"), append(args, project)
	}
	if !includeArchived {
		where = append(where, "m.archived_at IS NULL")
	}
	var out []Milestone
	err := s.tx(func(tx *sql.Tx) error {
		rows, err := tx.Query(milestoneSelect+" WHERE "+strings.Join(where, " AND ")+
			" ORDER BY p.slug, COALESCE(m.target_date, '9999'), m.id", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMilestone(rows)
			if err != nil {
				return err
			}
			out = append(out, *m)
		}
		return rows.Err()
	})
	return out, err
}

const milestoneSelect = `
	SELECT m.id, p.slug, m.name, m.description, COALESCE(m.target_date, ''),
	       m.created_at, m.updated_at, COALESCE(m.archived_at, '')
	FROM milestones m
	JOIN projects p ON p.id = m.project_id`

func scanMilestone(r rowScanner) (*Milestone, error) {
	var m Milestone
	err := r.Scan(
		&m.ID, &m.Project, &m.Name, &m.Description, &m.TargetDate,
		&m.CreatedAt, &m.UpdatedAt, &m.ArchivedAt,
	)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func loadMilestone(tx *sql.Tx, id int64) (*Milestone, error) {
	m, err := scanMilestone(tx.QueryRow(milestoneSelect+" WHERE m.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("milestone %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// optionalMilestoneID resolves a milestone by name within a project. Archived
// milestones cannot be assigned to issues.
func optionalMilestoneID(tx *sql.Tx, projectSlug, name string) (any, error) {
	if name == "" {
		return nil, nil
	}
	if projectSlug == "" {
		return nil, errors.New("milestones belong to a project; set the issue's project first")
	}
	var id int64
	err := tx.QueryRow(`
		SELECT m.id FROM milestones m
		JOIN projects p ON p.id = m.project_id
		WHERE p.slug = ? COLLATE NOCASE AND m.name = ? COLLATE NOCASE AND m.archived_at IS NULL`,
		projectSlug, name,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("milestone %q in project %s: %w", name, projectSlug, ErrInvalidRef)
	}
	if err != nil {
		return nil, err
	}
	return id, nil
}
