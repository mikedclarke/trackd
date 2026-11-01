package store

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
)

// The dump format is JSONL: one header line followed by one line per row,
// mirroring table columns exactly so that export -> import -> export is
// byte-identical. Records are ordered deterministically. From version 3 the
// last line is an end record holding the count of each record type and the
// sha256 of every byte before it, so a dump cut short on a line boundary is
// told apart from a complete one.

const dumpVersion = 3

// ErrUnverifiedDump is a dump written before the end record existed: its
// completeness cannot be checked, so importing it takes an explicit opt-in.
var ErrUnverifiedDump = errors.New("no end record, so its completeness cannot be checked")

type dumpHeader struct {
	Record  string `json:"record"`
	Version int    `json:"version"`
	Schema  int    `json:"schema,omitempty"`
}

type dumpEnd struct {
	Record string         `json:"record"`
	Counts map[string]int `json:"counts"`
	SHA256 string         `json:"sha256"`
}

type dumpSetting struct {
	Record string `json:"record"`
	Key    string `json:"key"`
	Value  string `json:"value"`
}

type dumpStatus struct {
	Record   string `json:"record"`
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Position int    `json:"position"`
}

type dumpProject struct {
	Record      string  `json:"record"`
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Slug        string  `json:"slug"`
	Description string  `json:"description"`
	Status      string  `json:"status"`
	StartDate   *string `json:"start_date,omitempty"`
	TargetDate  *string `json:"target_date,omitempty"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	CompletedAt *string `json:"completed_at,omitempty"`
	ArchivedAt  *string `json:"archived_at,omitempty"`
}

type dumpLabel struct {
	Record string `json:"record"`
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Color  string `json:"color"`
}

type dumpMilestone struct {
	Record      string  `json:"record"`
	ID          int64   `json:"id"`
	ProjectID   int64   `json:"project_id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	TargetDate  *string `json:"target_date,omitempty"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	ArchivedAt  *string `json:"archived_at,omitempty"`
}

type dumpIssue struct {
	Record         string  `json:"record"`
	ID             int64   `json:"id"`
	Key            string  `json:"key"`
	Title          string  `json:"title"`
	Description    string  `json:"description"`
	StatusID       int64   `json:"status_id"`
	Priority       int     `json:"priority"`
	ProjectID      *int64  `json:"project_id,omitempty"`
	ParentID       *int64  `json:"parent_id,omitempty"`
	Assignee       string  `json:"assignee,omitempty"`
	MilestoneID    *int64  `json:"milestone_id,omitempty"`
	DueDate        *string `json:"due_date,omitempty"`
	Version        int64   `json:"version"`
	IdempotencyKey *string `json:"idempotency_key,omitempty"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	StartedAt      *string `json:"started_at,omitempty"`
	CompletedAt    *string `json:"completed_at,omitempty"`
	CanceledAt     *string `json:"canceled_at,omitempty"`
	ArchivedAt     *string `json:"archived_at,omitempty"`
}

type dumpIssueLabel struct {
	Record  string `json:"record"`
	IssueID int64  `json:"issue_id"`
	LabelID int64  `json:"label_id"`
}

type dumpProjectLabel struct {
	Record    string `json:"record"`
	ProjectID int64  `json:"project_id"`
	LabelID   int64  `json:"label_id"`
}

type dumpComment struct {
	Record         string  `json:"record"`
	ID             int64   `json:"id"`
	IssueID        int64   `json:"issue_id"`
	Body           string  `json:"body"`
	Actor          string  `json:"actor"`
	ParentID       *int64  `json:"parent_id,omitempty"`
	IdempotencyKey *string `json:"idempotency_key,omitempty"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

type dumpRelation struct {
	Record    string `json:"record"`
	IssueID   int64  `json:"issue_id"`
	RelatedID int64  `json:"related_id"`
	Type      string `json:"type"`
}

type dumpToken struct {
	Record     string  `json:"record"`
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Hash       string  `json:"hash"`
	Role       string  `json:"role"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at,omitempty"`
	RevokedAt  *string `json:"revoked_at,omitempty"`
}

type dumpView struct {
	Record      string  `json:"record"`
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Filter      string  `json:"filter"`
	Quick       string  `json:"quick"`
	Owner       string  `json:"owner"`
	Shared      int     `json:"shared"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	ArchivedAt  *string `json:"archived_at,omitempty"`
}

type dumpEvent struct {
	Record    string          `json:"record"`
	ID        int64           `json:"id"`
	Entity    string          `json:"entity"`
	EntityID  int64           `json:"entity_id"`
	Actor     string          `json:"actor"`
	Action    string          `json:"action"`
	Before    json.RawMessage `json:"before,omitempty"`
	After     json.RawMessage `json:"after,omitempty"`
	CreatedAt string          `json:"created_at"`
}

// ExportDump writes the complete database as JSONL, ending with the end record.
func (s *Store) ExportDump(w io.Writer) error {
	bw := bufio.NewWriter(w)
	return s.tx(func(tx *sql.Tx) error {
		sum := sha256.New()
		out := io.MultiWriter(bw, sum)
		write := func(v any) error {
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			_, err = out.Write(append(b, '\n'))
			return err
		}
		counts := map[string]int{}
		rows := func(record, query string, scan func(rowScanner) (any, error)) error {
			n, err := exportRows(tx, query, scan, write)
			if n > 0 {
				counts[record] = n
			}
			return err
		}
		// The schema version travels with the dump so an older binary can
		// refuse a file it would silently mangle.
		var schema int
		if err := tx.QueryRow("PRAGMA user_version").Scan(&schema); err != nil {
			return err
		}
		if err := write(dumpHeader{Record: "trackd", Version: dumpVersion, Schema: schema}); err != nil {
			return err
		}
		if err := rows("setting", "SELECT key, value FROM settings ORDER BY key", func(scan rowScanner) (any, error) {
			r := dumpSetting{Record: "setting"}
			return r, scan.Scan(&r.Key, &r.Value)
		}); err != nil {
			return err
		}
		if err := rows("status", "SELECT id, name, type, position FROM statuses ORDER BY id", func(scan rowScanner) (any, error) {
			r := dumpStatus{Record: "status"}
			return r, scan.Scan(&r.ID, &r.Name, &r.Type, &r.Position)
		}); err != nil {
			return err
		}
		if err := rows("project", "SELECT id, name, slug, description, status, start_date, target_date, created_at, updated_at, completed_at, archived_at FROM projects ORDER BY id", func(scan rowScanner) (any, error) {
			r := dumpProject{Record: "project"}
			return r, scan.Scan(&r.ID, &r.Name, &r.Slug, &r.Description, &r.Status, &r.StartDate, &r.TargetDate, &r.CreatedAt, &r.UpdatedAt, &r.CompletedAt, &r.ArchivedAt)
		}); err != nil {
			return err
		}
		if err := rows("label", "SELECT id, name, color FROM labels ORDER BY id", func(scan rowScanner) (any, error) {
			r := dumpLabel{Record: "label"}
			return r, scan.Scan(&r.ID, &r.Name, &r.Color)
		}); err != nil {
			return err
		}
		if err := rows("milestone", "SELECT id, project_id, name, description, target_date, created_at, updated_at, archived_at FROM milestones ORDER BY id", func(scan rowScanner) (any, error) {
			r := dumpMilestone{Record: "milestone"}
			return r, scan.Scan(&r.ID, &r.ProjectID, &r.Name, &r.Description, &r.TargetDate, &r.CreatedAt, &r.UpdatedAt, &r.ArchivedAt)
		}); err != nil {
			return err
		}
		if err := rows("issue", "SELECT id, key, title, description, status_id, priority, project_id, parent_id, assignee, milestone_id, due_date, version, idempotency_key, created_at, updated_at, started_at, completed_at, canceled_at, archived_at FROM issues ORDER BY id", func(scan rowScanner) (any, error) {
			r := dumpIssue{Record: "issue"}
			return r, scan.Scan(&r.ID, &r.Key, &r.Title, &r.Description, &r.StatusID, &r.Priority, &r.ProjectID, &r.ParentID, &r.Assignee, &r.MilestoneID, &r.DueDate, &r.Version, &r.IdempotencyKey, &r.CreatedAt, &r.UpdatedAt, &r.StartedAt, &r.CompletedAt, &r.CanceledAt, &r.ArchivedAt)
		}); err != nil {
			return err
		}
		if err := rows("issue_label", "SELECT issue_id, label_id FROM issue_labels ORDER BY issue_id, label_id", func(scan rowScanner) (any, error) {
			r := dumpIssueLabel{Record: "issue_label"}
			return r, scan.Scan(&r.IssueID, &r.LabelID)
		}); err != nil {
			return err
		}
		if err := rows("project_label", "SELECT project_id, label_id FROM project_labels ORDER BY project_id, label_id", func(scan rowScanner) (any, error) {
			r := dumpProjectLabel{Record: "project_label"}
			return r, scan.Scan(&r.ProjectID, &r.LabelID)
		}); err != nil {
			return err
		}
		if err := rows("comment", "SELECT id, issue_id, body, actor, parent_id, idempotency_key, created_at, updated_at FROM comments ORDER BY id", func(scan rowScanner) (any, error) {
			r := dumpComment{Record: "comment"}
			return r, scan.Scan(&r.ID, &r.IssueID, &r.Body, &r.Actor, &r.ParentID, &r.IdempotencyKey, &r.CreatedAt, &r.UpdatedAt)
		}); err != nil {
			return err
		}
		if err := rows("relation", "SELECT issue_id, related_id, type FROM issue_relations ORDER BY issue_id, related_id, type", func(scan rowScanner) (any, error) {
			r := dumpRelation{Record: "relation"}
			return r, scan.Scan(&r.IssueID, &r.RelatedID, &r.Type)
		}); err != nil {
			return err
		}
		if err := rows("token", "SELECT id, name, hash, role, created_at, last_used_at, revoked_at FROM tokens ORDER BY id", func(scan rowScanner) (any, error) {
			r := dumpToken{Record: "token"}
			return r, scan.Scan(&r.ID, &r.Name, &r.Hash, &r.Role, &r.CreatedAt, &r.LastUsedAt, &r.RevokedAt)
		}); err != nil {
			return err
		}
		if err := rows("view", "SELECT id, name, description, filter_json, quick_json, owner, shared, created_at, updated_at, archived_at FROM views ORDER BY id", func(scan rowScanner) (any, error) {
			r := dumpView{Record: "view"}
			return r, scan.Scan(&r.ID, &r.Name, &r.Description, &r.Filter, &r.Quick, &r.Owner, &r.Shared, &r.CreatedAt, &r.UpdatedAt, &r.ArchivedAt)
		}); err != nil {
			return err
		}
		if err := rows("event", "SELECT id, entity, entity_id, actor, action, before_json, after_json, created_at FROM events ORDER BY id", func(scan rowScanner) (any, error) {
			r := dumpEvent{Record: "event"}
			var before, after *string
			if err := scan.Scan(&r.ID, &r.Entity, &r.EntityID, &r.Actor, &r.Action, &before, &after, &r.CreatedAt); err != nil {
				return nil, err
			}
			if before != nil {
				r.Before = json.RawMessage(*before)
			}
			if after != nil {
				r.After = json.RawMessage(*after)
			}
			return r, nil
		}); err != nil {
			return err
		}
		end, err := json.Marshal(dumpEnd{Record: "end", Counts: counts, SHA256: hex.EncodeToString(sum.Sum(nil))})
		if err != nil {
			return err
		}
		if _, err := bw.Write(append(end, '\n')); err != nil {
			return err
		}
		return bw.Flush()
	})
}

func exportRows(tx *sql.Tx, query string, scan func(rowScanner) (any, error), write func(any) error) (int, error) {
	rows, err := tx.Query(query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		rec, err := scan(rows)
		if err != nil {
			return n, err
		}
		if err := write(rec); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

// ImportDump loads a JSONL dump into a freshly created database. It refuses to
// run against a database that already holds data. A dump is checked against
// its end record before the import commits; one written before the end record
// existed (versions 1 and 2) is refused unless allowUnverified is set, and
// verified reports whether the check ran.
func (s *Store) ImportDump(r io.Reader, allowUnverified bool) (verified bool, err error) {
	empty, err := s.isEmpty()
	if err != nil {
		return false, err
	}
	if !empty {
		return false, fmt.Errorf("refusing to import into a non-empty database %s", s.path)
	}
	br := bufio.NewReaderSize(r, 1<<20)
	first, err := readLine(br)
	if err != nil {
		return false, fmt.Errorf("reading dump header: %w", err)
	}
	var header dumpHeader
	if err := json.Unmarshal(first, &header); err != nil || header.Record != "trackd" {
		return false, fmt.Errorf("not a trackd dump (bad header)")
	}
	if header.Version < 1 || header.Version > dumpVersion {
		return false, fmt.Errorf("unsupported dump version %d", header.Version)
	}
	verified = header.Version == dumpVersion
	if !verified && !allowUnverified {
		return false, fmt.Errorf("dump version %d has %w", header.Version, ErrUnverifiedDump)
	}
	// v1 dumps predate the header's schema field; they came from schema 2.
	schema := header.Schema
	if schema == 0 {
		schema = 2
	}
	migs, err := loadMigrations()
	if err != nil {
		return false, err
	}
	if schema > len(migs) {
		return false, fmt.Errorf("dump is at schema %d, this binary knows %d: %w", schema, len(migs), ErrSchemaNewer)
	}
	sum := sha256.New()
	sum.Write(first)
	counts := map[string]int{}
	err = s.tx(func(tx *sql.Tx) error {
		// Issues may reference parents with higher IDs; check FKs at commit.
		if _, err := tx.Exec("PRAGMA defer_foreign_keys=ON"); err != nil {
			return err
		}
		// The dump carries its own settings and statuses; drop the seeded ones.
		if _, err := tx.Exec("DELETE FROM settings"); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM statuses"); err != nil {
			return err
		}
		lineNo := 1
		ended := false
		for {
			line, err := readLine(br)
			if err == io.EOF {
				if verified && !ended {
					return errors.New("dump has no end record: it may be cut short")
				}
				return checkLabelGroups(tx)
			}
			if err != nil {
				return err
			}
			lineNo++
			if ended {
				return fmt.Errorf("line %d: data after the end record", lineNo)
			}
			var probe struct {
				Record string `json:"record"`
			}
			if err := json.Unmarshal(line, &probe); err != nil {
				return fmt.Errorf("line %d: %w", lineNo, err)
			}
			if verified && probe.Record == "end" {
				if err := checkEnd(line, counts, hex.EncodeToString(sum.Sum(nil))); err != nil {
					return fmt.Errorf("line %d: %w", lineNo, err)
				}
				ended = true
				continue
			}
			sum.Write(line)
			counts[probe.Record]++
			if err := importLine(tx, probe.Record, line, schema); err != nil {
				return fmt.Errorf("line %d: %w", lineNo, err)
			}
		}
	})
	if err != nil {
		return false, err
	}
	return verified, nil
}

// checkEnd compares the end record with what was read before it.
func checkEnd(line []byte, counts map[string]int, sum string) error {
	var end dumpEnd
	if err := decodeRecord(line, &end); err != nil {
		return err
	}
	if !maps.Equal(end.Counts, counts) {
		return fmt.Errorf("end record counts %v, the dump holds %v: it is incomplete or altered", end.Counts, counts)
	}
	if end.SHA256 != sum {
		return errors.New("dump checksum does not match its end record: it is altered or damaged")
	}
	return nil
}

func readLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadBytes('\n')
	if len(line) == 0 && err != nil {
		return nil, err
	}
	if err != nil && err != io.EOF {
		return nil, err
	}
	return line, nil
}

// decodeRecord is strict: a field the binary does not know about means the
// dump came from something this code cannot faithfully load, and silently
// dropping it would lose data.
func decodeRecord(line []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func importLine(tx *sql.Tx, record string, line []byte, schema int) error {
	switch record {
	case "setting":
		var r dumpSetting
		if err := decodeRecord(line, &r); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO settings (key, value) VALUES (?, ?)", r.Key, r.Value)
		return err
	case "status":
		var r dumpStatus
		if err := decodeRecord(line, &r); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO statuses (id, name, type, position) VALUES (?, ?, ?, ?)", r.ID, r.Name, r.Type, r.Position)
		return err
	case "project":
		var r dumpProject
		if err := decodeRecord(line, &r); err != nil {
			return err
		}
		// Schema 3 replaced the "active" project state with Linear's
		// vocabulary; an older dump still carries the old word.
		if schema < 3 && r.Status == "active" {
			r.Status = "started"
		}
		_, err := tx.Exec(
			"INSERT INTO projects (id, name, slug, description, status, start_date, target_date, created_at, updated_at, completed_at, archived_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			r.ID, r.Name, r.Slug, r.Description, r.Status, r.StartDate, r.TargetDate, r.CreatedAt, r.UpdatedAt, r.CompletedAt, r.ArchivedAt,
		)
		return err
	case "label":
		var r dumpLabel
		if err := decodeRecord(line, &r); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO labels (id, name, color) VALUES (?, ?, ?)", r.ID, r.Name, r.Color)
		return err
	case "milestone":
		var r dumpMilestone
		if err := decodeRecord(line, &r); err != nil {
			return err
		}
		_, err := tx.Exec(
			"INSERT INTO milestones (id, project_id, name, description, target_date, created_at, updated_at, archived_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			r.ID, r.ProjectID, r.Name, r.Description, r.TargetDate, r.CreatedAt, r.UpdatedAt, r.ArchivedAt,
		)
		return err
	case "issue":
		var r dumpIssue
		if err := decodeRecord(line, &r); err != nil {
			return err
		}
		_, err := tx.Exec(
			"INSERT INTO issues (id, key, title, description, status_id, priority, project_id, parent_id, assignee, milestone_id, due_date, version, idempotency_key, created_at, updated_at, started_at, completed_at, canceled_at, archived_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			r.ID, r.Key, r.Title, r.Description, r.StatusID, r.Priority, r.ProjectID, r.ParentID, r.Assignee, r.MilestoneID, r.DueDate, r.Version, r.IdempotencyKey, r.CreatedAt, r.UpdatedAt, r.StartedAt, r.CompletedAt, r.CanceledAt, r.ArchivedAt,
		)
		return err
	case "issue_label":
		var r dumpIssueLabel
		if err := decodeRecord(line, &r); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO issue_labels (issue_id, label_id) VALUES (?, ?)", r.IssueID, r.LabelID)
		return err
	case "project_label":
		var r dumpProjectLabel
		if err := decodeRecord(line, &r); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO project_labels (project_id, label_id) VALUES (?, ?)", r.ProjectID, r.LabelID)
		return err
	case "comment":
		var r dumpComment
		if err := decodeRecord(line, &r); err != nil {
			return err
		}
		_, err := tx.Exec(
			"INSERT INTO comments (id, issue_id, body, actor, parent_id, idempotency_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			r.ID, r.IssueID, r.Body, r.Actor, r.ParentID, r.IdempotencyKey, r.CreatedAt, r.UpdatedAt,
		)
		return err
	case "relation":
		var r dumpRelation
		if err := decodeRecord(line, &r); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO issue_relations (issue_id, related_id, type) VALUES (?, ?, ?)", r.IssueID, r.RelatedID, r.Type)
		return err
	case "token":
		var r dumpToken
		if err := decodeRecord(line, &r); err != nil {
			return err
		}
		_, err := tx.Exec(
			"INSERT INTO tokens (id, name, hash, role, created_at, last_used_at, revoked_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
			r.ID, r.Name, r.Hash, r.Role, r.CreatedAt, r.LastUsedAt, r.RevokedAt,
		)
		return err
	case "view":
		var r dumpView
		if err := decodeRecord(line, &r); err != nil {
			return err
		}
		_, err := tx.Exec(
			"INSERT INTO views (id, name, description, filter_json, quick_json, owner, shared, created_at, updated_at, archived_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			r.ID, r.Name, r.Description, r.Filter, r.Quick, r.Owner, r.Shared, r.CreatedAt, r.UpdatedAt, r.ArchivedAt,
		)
		return err
	case "event":
		var r dumpEvent
		if err := decodeRecord(line, &r); err != nil {
			return err
		}
		var before, after any
		if r.Before != nil {
			before = string(r.Before)
		}
		if r.After != nil {
			after = string(r.After)
		}
		_, err := tx.Exec(
			"INSERT INTO events (id, entity, entity_id, actor, action, before_json, after_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			r.ID, r.Entity, r.EntityID, r.Actor, r.Action, before, after, r.CreatedAt,
		)
		return err
	default:
		return fmt.Errorf("unknown record type %q", record)
	}
}

func (s *Store) isEmpty() (bool, error) {
	for _, table := range []string{"issues", "projects", "labels", "comments", "tokens", "events"} {
		var n int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return false, nil
		}
	}
	return true, nil
}
