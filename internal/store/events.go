package store

import (
	"database/sql"
	"encoding/json"
)

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

func (s *Store) ListEvents(entity string, entityID int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(
		"SELECT id, entity, entity_id, actor, action, COALESCE(before_json, ''), COALESCE(after_json, ''), created_at "+
			"FROM events WHERE entity = ? AND entity_id = ? ORDER BY id DESC LIMIT ?",
		entity, entityID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var before, after string
		if err := rows.Scan(&e.ID, &e.Entity, &e.EntityID, &e.Actor, &e.Action, &before, &after, &e.CreatedAt); err != nil {
			return nil, err
		}
		if before != "" {
			e.Before = json.RawMessage(before)
		}
		if after != "" {
			e.After = json.RawMessage(after)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
