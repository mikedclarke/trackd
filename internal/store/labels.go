package store

import (
	"database/sql"
	"errors"
	"sort"
	"strings"
)

func (s *Store) ListLabels() ([]Label, error) {
	rows, err := s.db.Query("SELECT id, name, color FROM labels ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Label
	for rows.Next() {
		var l Label
		if err := rows.Scan(&l.ID, &l.Name, &l.Color); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) EnsureLabel(name, color string) (*Label, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("label name is required")
	}
	var out Label
	err := s.tx(func(tx *sql.Tx) error {
		id, err := ensureLabel(tx, name)
		if err != nil {
			return err
		}
		if color != "" {
			if _, err := tx.Exec("UPDATE labels SET color = ? WHERE id = ?", color, id); err != nil {
				return err
			}
		}
		return tx.QueryRow("SELECT id, name, color FROM labels WHERE id = ?", id).
			Scan(&out.ID, &out.Name, &out.Color)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func ensureLabel(tx *sql.Tx, name string) (int64, error) {
	if _, err := tx.Exec("INSERT INTO labels (name) VALUES (?) ON CONFLICT (name) DO NOTHING", name); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRow("SELECT id FROM labels WHERE name = ?", name).Scan(&id)
	return id, err
}

// normalizeLabels trims, drops empties, dedupes, and sorts label names.
func normalizeLabels(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func setIssueLabels(tx *sql.Tx, issueID int64, names []string) error {
	if _, err := tx.Exec("DELETE FROM issue_labels WHERE issue_id = ?", issueID); err != nil {
		return err
	}
	for _, name := range normalizeLabels(names) {
		labelID, err := ensureLabel(tx, name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO issue_labels (issue_id, label_id) VALUES (?, ?)", issueID, labelID); err != nil {
			return err
		}
	}
	return nil
}

func issueLabels(tx *sql.Tx, issueID int64) ([]string, error) {
	rows, err := tx.Query(
		"SELECT l.name FROM issue_labels il JOIN labels l ON l.id = il.label_id WHERE il.issue_id = ? ORDER BY l.name",
		issueID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func setProjectLabels(tx *sql.Tx, projectID int64, names []string) error {
	if _, err := tx.Exec("DELETE FROM project_labels WHERE project_id = ?", projectID); err != nil {
		return err
	}
	for _, name := range normalizeLabels(names) {
		labelID, err := ensureLabel(tx, name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO project_labels (project_id, label_id) VALUES (?, ?)", projectID, labelID); err != nil {
			return err
		}
	}
	return nil
}

func projectLabels(tx *sql.Tx, projectID int64) ([]string, error) {
	rows, err := tx.Query(
		"SELECT l.name FROM project_labels pl JOIN labels l ON l.id = pl.label_id WHERE pl.project_id = ? ORDER BY l.name",
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
