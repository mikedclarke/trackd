package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// eventEntities are the entity kinds the audit trail records. A comment and a
// relation are recorded against the issue they belong to, so neither is a kind
// of its own and neither is a filter value.
var eventEntities = []string{"issue", "project", "milestone", "token"}

func recordEvent(tx *sql.Tx, entity string, entityID int64, actor, action string, before, after any) error {
	var beforeJSON, afterJSON any
	if before != nil {
		b, err := json.Marshal(before)
		if err != nil {
			return err
		}
		beforeJSON = string(b)
	}
	if after != nil {
		b, err := json.Marshal(after)
		if err != nil {
			return err
		}
		afterJSON = string(b)
	}
	_, err := tx.Exec(
		"INSERT INTO events (entity, entity_id, actor, action, before_json, after_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		entity, entityID, actor, action, beforeJSON, afterJSON, now(),
	)
	return err
}

// entity_key is resolved at read time rather than stored, so an event still
// names its subject after a rename.
const eventSelect = `
	SELECT e.id, e.entity, e.entity_id, e.actor, e.action,
	       COALESCE(e.before_json, ''), COALESCE(e.after_json, ''), e.created_at,
	       COALESCE(CASE e.entity
	           WHEN 'issue' THEN (SELECT i.key FROM issues i WHERE i.id = e.entity_id)
	           WHEN 'project' THEN (SELECT p.slug FROM projects p WHERE p.id = e.entity_id)
	           WHEN 'milestone' THEN (SELECT p.slug || '/' || m.name FROM milestones m
	                                  JOIN projects p ON p.id = m.project_id WHERE m.id = e.entity_id)
	       END, '')
	FROM events e`

func scanEvent(r rowScanner) (*Event, error) {
	var e Event
	var before, after string
	if err := r.Scan(&e.ID, &e.Entity, &e.EntityID, &e.Actor, &e.Action, &before, &after, &e.CreatedAt, &e.EntityKey); err != nil {
		return nil, err
	}
	if before != "" {
		e.Before = json.RawMessage(before)
	}
	if after != "" {
		e.After = json.RawMessage(after)
	}
	return &e, nil
}

// ListEvents is one entity's feed, newest first.
func (s *Store) ListEvents(entity string, entityID int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(
		eventSelect+" WHERE e.entity = ? AND e.entity_id = ? ORDER BY e.id DESC LIMIT ?",
		entity, entityID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// ListAllEvents is the workspace-wide activity feed. It runs oldest first so a
// consumer can page forward with after_id and never miss a row.
func (s *Store) ListAllEvents(f EventFilter) ([]Event, error) {
	where := []string{"1=1"}
	var args []any
	if f.Since != "" {
		since, err := ParseTimestamp(f.Since)
		if err != nil {
			return nil, err
		}
		where, args = append(where, "e.created_at >= ?"), append(args, since)
	}
	if f.AfterID > 0 {
		where, args = append(where, "e.id > ?"), append(args, f.AfterID)
	}
	if f.Entity != "" {
		if !containsFold(eventEntities, f.Entity) {
			return nil, fmt.Errorf("unknown entity %q, want issue, project, milestone or token: %w", f.Entity, ErrNotFound)
		}
		where, args = append(where, "e.entity = ?"), append(args, f.Entity)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	args = append(args, limit)
	rows, err := s.db.Query(
		eventSelect+" WHERE "+strings.Join(where, " AND ")+" ORDER BY e.id ASC LIMIT ?",
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}
