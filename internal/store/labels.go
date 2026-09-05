package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// EnsureLabel is the only way a label comes into existence: issue and project
// writes reject names that are not already here.
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

// normalizeLabels resolves a label edit against the labels that exist. current
// is the issue's or project's stored set; replace (when non-nil) supersedes
// it; add and remove are then applied on top. Names resolve case-insensitively
// to the stored spelling, unknown names are rejected, and the exclusive groups
// in the label_groups setting are enforced: adding a member drops its
// siblings, and a replacement set holding two members of one group is refused.
func normalizeLabels(tx *sql.Tx, current []string, replace *[]string, add, remove []string) ([]string, error) {
	base := current
	if replace != nil {
		base = *replace
	}
	set, err := resolveLabels(tx, base)
	if err != nil {
		return nil, err
	}
	groups, err := labelGroups(tx)
	if err != nil {
		return nil, err
	}
	if replace != nil {
		for _, group := range groups {
			var hit []string
			for _, name := range set {
				if groupHas(group, name) {
					hit = append(hit, name)
				}
			}
			if len(hit) > 1 {
				return nil, fmt.Errorf("labels %s are mutually exclusive: %w", strings.Join(hit, ", "), ErrConflict)
			}
		}
	}
	added, err := resolveLabels(tx, add)
	if err != nil {
		return nil, err
	}
	for _, name := range added {
		for _, group := range groups {
			if groupHas(group, name) {
				set = dropGroup(set, group)
			}
		}
		if !containsFold(set, name) {
			set = append(set, name)
		}
	}
	removed, err := resolveLabels(tx, remove)
	if err != nil {
		return nil, err
	}
	for _, name := range removed {
		set = dropLabel(set, name)
	}
	sort.Strings(set)
	return set, nil
}

// resolveLabels maps caller-supplied names onto the stored spellings, dropping
// blanks and duplicates. An unknown name is ErrInvalidRef: labels are created
// through the label endpoint, never as a side effect of an issue write.
func resolveLabels(tx *sql.Tx, names []string) ([]string, error) {
	var out []string
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		var canonical string
		err := tx.QueryRow("SELECT name FROM labels WHERE name = ?", name).Scan(&canonical)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("label %q: %w", name, ErrInvalidRef)
		}
		if err != nil {
			return nil, err
		}
		if !containsFold(out, canonical) {
			out = append(out, canonical)
		}
	}
	return out, nil
}

// labelGroups reads the exclusive label groups: a JSON array of arrays, each
// inner array a set of labels of which an issue or project may hold one.
func labelGroups(tx *sql.Tx) ([][]string, error) {
	raw, err := settingTx(tx, "label_groups")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var groups [][]string
	if err := json.Unmarshal([]byte(raw), &groups); err != nil {
		return nil, fmt.Errorf("label_groups setting: %w", err)
	}
	return groups, nil
}

// dedupeLabelNames trims, drops blanks, dedupes and sorts raw label names. The
// CSV importer uses it because it creates the labels it finds; every other
// caller goes through normalizeLabels, which refuses unknown names.
func dedupeLabelNames(names []string) []string {
	var out []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || containsFold(out, n) {
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func groupHas(group []string, name string) bool { return containsFold(group, name) }

func containsFold(names []string, want string) bool {
	for _, n := range names {
		if strings.EqualFold(n, want) {
			return true
		}
	}
	return false
}

func dropGroup(set, group []string) []string {
	out := set[:0]
	for _, name := range set {
		if !containsFold(group, name) {
			out = append(out, name)
		}
	}
	return out
}

func dropLabel(set []string, name string) []string {
	out := set[:0]
	for _, have := range set {
		if !strings.EqualFold(have, name) {
			out = append(out, have)
		}
	}
	return out
}

func applyIssueLabels(tx *sql.Tx, issueID int64, names []string) error {
	return applyLabels(tx, "issue_labels", "issue_id", issueID, names)
}

func applyProjectLabels(tx *sql.Tx, projectID int64, names []string) error {
	return applyLabels(tx, "project_labels", "project_id", projectID, names)
}

// applyLabels rewrites one owner's label rows. names must already be canonical
// (normalizeLabels output), so every lookup here is expected to hit.
func applyLabels(tx *sql.Tx, table, column string, ownerID int64, names []string) error {
	if _, err := tx.Exec("DELETE FROM "+table+" WHERE "+column+" = ?", ownerID); err != nil {
		return err
	}
	for _, name := range names {
		var labelID int64
		err := tx.QueryRow("SELECT id FROM labels WHERE name = ?", name).Scan(&labelID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("label %q: %w", name, ErrInvalidRef)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO "+table+" ("+column+", label_id) VALUES (?, ?)", ownerID, labelID); err != nil {
			return err
		}
	}
	return nil
}

func issueLabels(tx *sql.Tx, issueID int64) ([]string, error) {
	return ownerLabels(tx, "issue_labels", "issue_id", issueID)
}

func projectLabels(tx *sql.Tx, projectID int64) ([]string, error) {
	return ownerLabels(tx, "project_labels", "project_id", projectID)
}

func ownerLabels(tx *sql.Tx, table, column string, ownerID int64) ([]string, error) {
	rows, err := tx.Query(
		"SELECT l.name FROM "+table+" x JOIN labels l ON l.id = x.label_id WHERE x."+column+" = ? ORDER BY l.name",
		ownerID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// issueLabelsFor loads the labels of a whole page of issues in one query
// rather than one per issue.
func issueLabelsFor(tx *sql.Tx, ids []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := tx.Query(
		"SELECT il.issue_id, l.name FROM issue_labels il JOIN labels l ON l.id = il.label_id "+
			"WHERE il.issue_id IN ("+placeholders(len(ids))+") ORDER BY il.issue_id, l.name",
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = append(out[id], name)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
