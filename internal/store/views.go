package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// maxViewNameLength keeps a view name short enough to sit on a tab and in a
// URL. Names are case-insensitively unique among unarchived views.
const maxViewNameLength = 60

// MaxQuickActions bounds the buttons a view puts on every row.
const MaxQuickActions = 4

// withinPattern is the relative window a view keeps instead of a timestamp:
// a whole number of minutes, hours, days or weeks.
var withinPattern = regexp.MustCompile(`^(\d+)([mhdw])$`)

// ParseWithin turns a relative window such as 7d, 48h, 30m or 2w into a
// duration. An empty string is zero, meaning no window.
func ParseWithin(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	m := withinPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("window %q must be a number followed by m, h, d or w, like 7d or 48h: %w", s, ErrInvalidRef)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("window %q must be a positive whole number: %w", s, ErrInvalidRef)
	}
	unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
	return time.Duration(n) * unit, nil
}

// Apply lays the view's filter under an explicit one: every field the caller
// left empty takes the view's value, so a request can narrow or re-sort a
// view without editing it. Labels and excluded labels are joined rather than
// replaced, since an issue must carry every label asked for, so an explicit
// label always narrows the view. The relative window becomes an absolute
// updated_since against now.
func (v ViewFilter) Apply(f IssueFilter, now time.Time) (IssueFilter, error) {
	f.Labels = union(v.Labels, f.Labels)
	f.ExcludeLabels = union(v.ExcludeLabels, f.ExcludeLabels)
	if len(f.Statuses) == 0 {
		f.Statuses = v.Statuses
	}
	if len(f.StatusTypes) == 0 {
		f.StatusTypes = v.StatusTypes
	}
	if f.Project == "" {
		f.Project = v.Project
	}
	if f.Assignee == "" {
		f.Assignee = v.Assignee
	}
	if f.Milestone == "" {
		f.Milestone = v.Milestone
	}
	if len(f.Priorities) == 0 {
		f.Priorities = v.Priorities
	}
	if f.CreatedBy == "" {
		f.CreatedBy = v.CreatedBy
	}
	if f.Query == "" {
		f.Query = v.Query
	}
	if f.OrderBy == "" {
		f.OrderBy = v.OrderBy
	}
	if f.UpdatedSince == "" && v.UpdatedWithin != "" {
		window, err := ParseWithin(v.UpdatedWithin)
		if err != nil {
			return f, err
		}
		f.UpdatedSince = now.UTC().Add(-window).Format(timestampFormat)
	}
	return f, nil
}

// union joins two label lists, keeping order and dropping repeats
// (case-insensitively, as label lookups are). Two empty lists give nil so an
// unfiltered request stays unfiltered.
func union(a, b []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range append(append([]string{}, a...), b...) {
		k := strings.ToLower(l)
		if l == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, l)
	}
	return out
}

// Empty reports a filter that names nothing, which as a view would just be
// the whole board under another name.
func (v ViewFilter) Empty() bool {
	return len(v.Statuses) == 0 && len(v.StatusTypes) == 0 && v.Project == "" &&
		len(v.Labels) == 0 && len(v.ExcludeLabels) == 0 && v.Assignee == "" &&
		v.Milestone == "" && len(v.Priorities) == 0 && v.UpdatedWithin == "" &&
		v.CreatedBy == "" && v.Query == ""
}

// Patch is the issue patch a quick action applies.
func (q QuickAction) Patch() IssuePatch {
	p := IssuePatch{AddLabels: q.AddLabels, RemoveLabels: q.RemoveLabels, Priority: q.Priority}
	if q.Status != "" {
		status := q.Status
		p.Status = &status
	}
	return p
}

func (q QuickAction) empty() bool {
	return q.Status == "" && q.Priority == nil && len(q.AddLabels) == 0 && len(q.RemoveLabels) == 0
}

func validViewName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("view name is required: %w", ErrInvalidRef)
	}
	if n := utf8.RuneCountInString(trimmed); n > maxViewNameLength {
		return "", fmt.Errorf("view name is %d characters, over the %d character limit: %w", n, maxViewNameLength, ErrInvalidRef)
	}
	if strings.ContainsAny(trimmed, "/?#") {
		return "", fmt.Errorf("view name %q cannot contain /, ? or #: %w", trimmed, ErrInvalidRef)
	}
	return trimmed, nil
}

// validateViewFilter resolves everything a stored filter names, the same way
// a list request is checked, so a view cannot be saved pointing at a label or
// a status that does not exist and quietly show nothing.
func validateViewFilter(tx *sql.Tx, v ViewFilter) error {
	if _, err := ParseWithin(v.UpdatedWithin); err != nil {
		return err
	}
	if _, err := issueOrder(v.OrderBy); err != nil {
		return fmt.Errorf("%v: %w", err, ErrInvalidRef)
	}
	f := IssueFilter{
		Statuses: v.Statuses, StatusTypes: v.StatusTypes, Project: v.Project,
		Labels: v.Labels, ExcludeLabels: v.ExcludeLabels, Milestone: v.Milestone,
		Priorities: v.Priorities,
	}
	return validateIssueFilter(tx, f)
}

func validateQuickActions(tx *sql.Tx, actions []QuickAction) error {
	if len(actions) > MaxQuickActions {
		return fmt.Errorf("a view holds at most %d quick actions: %w", MaxQuickActions, ErrInvalidRef)
	}
	seen := map[string]bool{}
	for i, q := range actions {
		name := strings.TrimSpace(q.Name)
		if name == "" {
			return fmt.Errorf("quick action %d needs a name: %w", i+1, ErrInvalidRef)
		}
		if seen[strings.ToLower(name)] {
			return fmt.Errorf("quick action %q is listed twice: %w", name, ErrInvalidRef)
		}
		seen[strings.ToLower(name)] = true
		if q.empty() {
			return fmt.Errorf("quick action %q changes nothing: give it a status, a priority or labels: %w", name, ErrInvalidRef)
		}
		if q.Status != "" {
			if _, err := statusByName(tx, q.Status); err != nil {
				return err
			}
		}
		if q.Priority != nil && (*q.Priority < 0 || *q.Priority > 4) {
			return fmt.Errorf("quick action %q: priority %d out of range 0-4: %w", name, *q.Priority, ErrInvalidRef)
		}
		if _, err := resolveLabels(tx, q.AddLabels); err != nil {
			return err
		}
		if _, err := resolveLabels(tx, q.RemoveLabels); err != nil {
			return err
		}
	}
	return nil
}

// normalizeQuickActions trims names and drops nil slices so the stored JSON is
// canonical whichever interface wrote it.
func normalizeQuickActions(actions []QuickAction) []QuickAction {
	out := make([]QuickAction, 0, len(actions))
	for _, q := range actions {
		q.Name = strings.TrimSpace(q.Name)
		q.AddLabels = dedupeLabelNames(q.AddLabels)
		q.RemoveLabels = dedupeLabelNames(q.RemoveLabels)
		out = append(out, q)
	}
	return out
}

// CreateView stores a view owned by actor. Shared defaults to true: a view is
// for a team unless its owner says otherwise.
func (s *Store) CreateView(in ViewInput, actor string) (*View, error) {
	name, err := validViewName(in.Name)
	if err != nil {
		return nil, err
	}
	if in.Filter.Empty() {
		return nil, fmt.Errorf("view %q filters nothing: give it at least one filter: %w", name, ErrInvalidRef)
	}
	shared := true
	if in.Shared != nil {
		shared = *in.Shared
	}
	quick := normalizeQuickActions(in.QuickActions)
	var out *View
	err = s.tx(func(tx *sql.Tx) error {
		if err := validateViewFilter(tx, in.Filter); err != nil {
			return err
		}
		if err := validateQuickActions(tx, quick); err != nil {
			return err
		}
		if err := viewNameFree(tx, name, 0); err != nil {
			return err
		}
		filterJSON, err := json.Marshal(in.Filter)
		if err != nil {
			return err
		}
		quickJSON, err := json.Marshal(quick)
		if err != nil {
			return err
		}
		ts := now()
		res, err := tx.Exec(
			"INSERT INTO views (name, description, filter_json, quick_json, owner, shared, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			name, strings.TrimSpace(in.Description), string(filterJSON), string(quickJSON), actor, boolInt(shared), ts, ts,
		)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if out, err = loadView(tx, id); err != nil {
			return err
		}
		return recordEvent(tx, "view", id, actor, "view.created", nil, out)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetView finds a view by name, case-insensitively, among the unarchived ones
// first; an archived view is found only when no live one carries the name.
func (s *Store) GetView(name string) (*View, error) {
	var out *View
	err := s.tx(func(tx *sql.Tx) error {
		var err error
		out, err = loadViewByName(tx, name)
		return err
	})
	return out, err
}

func (s *Store) UpdateView(name string, p ViewPatch, actor string) (*View, error) {
	var out *View
	err := s.tx(func(tx *sql.Tx) error {
		before, err := loadViewByName(tx, name)
		if err != nil {
			return err
		}
		sets := []string{"updated_at = ?"}
		args := []any{now()}
		if p.Name != nil {
			newName, err := validViewName(*p.Name)
			if err != nil {
				return err
			}
			if err := viewNameFree(tx, newName, before.ID); err != nil {
				return err
			}
			sets, args = append(sets, "name = ?"), append(args, newName)
		}
		if p.Description != nil {
			sets, args = append(sets, "description = ?"), append(args, strings.TrimSpace(*p.Description))
		}
		if p.Filter != nil {
			if p.Filter.Empty() {
				return fmt.Errorf("view %q would filter nothing: keep at least one filter: %w", before.Name, ErrInvalidRef)
			}
			if err := validateViewFilter(tx, *p.Filter); err != nil {
				return err
			}
			b, err := json.Marshal(*p.Filter)
			if err != nil {
				return err
			}
			sets, args = append(sets, "filter_json = ?"), append(args, string(b))
		}
		if p.QuickActions != nil {
			quick := normalizeQuickActions(*p.QuickActions)
			if err := validateQuickActions(tx, quick); err != nil {
				return err
			}
			b, err := json.Marshal(quick)
			if err != nil {
				return err
			}
			sets, args = append(sets, "quick_json = ?"), append(args, string(b))
		}
		if p.Shared != nil {
			sets, args = append(sets, "shared = ?"), append(args, boolInt(*p.Shared))
		}
		action := "view.updated"
		if p.Archived != nil {
			if *p.Archived {
				if before.ArchivedAt == "" {
					sets, args = append(sets, "archived_at = ?"), append(args, now())
					action = "view.archived"
				}
			} else if before.ArchivedAt != "" {
				// Unarchiving puts the name back in play, so it must be free.
				live := before.Name
				if p.Name != nil {
					live = *p.Name
				}
				if err := viewNameFree(tx, live, before.ID); err != nil {
					return err
				}
				sets = append(sets, "archived_at = NULL")
				action = "view.restored"
			}
		}
		args = append(args, before.ID)
		if _, err := tx.Exec("UPDATE views SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
			return err
		}
		if out, err = loadView(tx, before.ID); err != nil {
			return err
		}
		return recordEvent(tx, "view", before.ID, actor, action, before, out)
	})
	return out, err
}

// ListViews returns the views a viewer may see: the shared ones plus the
// viewer's own, or every view when all is set (an admin's read). Archived
// views are left out unless asked for.
func (s *Store) ListViews(viewer string, all, includeArchived bool) ([]View, error) {
	where := []string{"1=1"}
	var args []any
	if !includeArchived {
		where = append(where, "archived_at IS NULL")
	}
	if !all {
		where, args = append(where, "(shared = 1 OR owner = ?)"), append(args, viewer)
	}
	out := []View{}
	err := s.tx(func(tx *sql.Tx) error {
		rows, err := tx.Query(viewSelect+" WHERE "+strings.Join(where, " AND ")+" ORDER BY name COLLATE NOCASE, id", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scanView(rows)
			if err != nil {
				return err
			}
			out = append(out, *v)
		}
		return rows.Err()
	})
	return out, err
}

const viewSelect = `
	SELECT id, name, description, filter_json, quick_json, owner, shared,
	       created_at, updated_at, COALESCE(archived_at, '')
	FROM views`

func scanView(r rowScanner) (*View, error) {
	var v View
	var filterJSON, quickJSON string
	var shared int
	if err := r.Scan(&v.ID, &v.Name, &v.Description, &filterJSON, &quickJSON, &v.Owner, &shared,
		&v.CreatedAt, &v.UpdatedAt, &v.ArchivedAt); err != nil {
		return nil, err
	}
	v.Shared = shared != 0
	if err := json.Unmarshal([]byte(filterJSON), &v.Filter); err != nil {
		return nil, fmt.Errorf("view %s: stored filter: %w", v.Name, err)
	}
	if err := json.Unmarshal([]byte(quickJSON), &v.QuickActions); err != nil {
		return nil, fmt.Errorf("view %s: stored quick actions: %w", v.Name, err)
	}
	if v.QuickActions == nil {
		v.QuickActions = []QuickAction{}
	}
	return &v, nil
}

func loadView(tx *sql.Tx, id int64) (*View, error) {
	v, err := scanView(tx.QueryRow(viewSelect+" WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("view %d: %w", id, ErrNotFound)
	}
	return v, err
}

func loadViewByName(tx *sql.Tx, name string) (*View, error) {
	v, err := scanView(tx.QueryRow(
		viewSelect+" WHERE name = ? COLLATE NOCASE ORDER BY (archived_at IS NULL) DESC, id DESC LIMIT 1",
		strings.TrimSpace(name),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("view %q: %w", name, ErrNotFound)
	}
	return v, err
}

func viewNameFree(tx *sql.Tx, name string, exceptID int64) error {
	var id int64
	err := tx.QueryRow("SELECT id FROM views WHERE name = ? COLLATE NOCASE AND archived_at IS NULL AND id != ?", name, exceptID).Scan(&id)
	if err == nil {
		return fmt.Errorf("view name %q is already in use: %w", name, ErrConflict)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
