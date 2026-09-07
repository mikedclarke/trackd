// Package store implements trackd's SQLite storage layer. All reads and writes
// go through this package. There are no hard-delete operations by design:
// issues and projects are archived or canceled, tokens are revoked, and every
// mutation records an audit event with before/after state.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

var (
	ErrNotFound    = errors.New("not found")
	ErrInvalidRef  = errors.New("invalid reference")
	ErrConflict    = errors.New("conflict")
	ErrBusy        = errors.New("database is busy")
	ErrLocked      = errors.New("database is locked by another trackd")
	ErrSchemaNewer = errors.New("database schema is newer than this binary")
	ErrIntegrity   = errors.New("database failed its integrity check")

	// Both of these satisfy errors.Is(err, ErrConflict) as well, so a caller
	// mapping every conflict to one status code can still tell them apart by
	// testing the specific sentinel first.
	ErrVersionConflict    error = conflictError("version conflict")
	ErrDescriptionReplace error = conflictError("description replace refused: use append or replace_description")
)

type conflictError string

func (e conflictError) Error() string { return string(e) }

func (e conflictError) Is(target error) bool { return target == ErrConflict }

type migration struct {
	name string
	sql  string
}

// Options configure a store handle. The zero value is a read-write store that
// snapshots pre-migration copies beside the database file.
type Options struct {
	ReadOnly    bool
	SnapshotDir string
}

type Store struct {
	db          *sql.DB
	path        string
	readOnly    bool
	snapshotDir string
}

// Open opens (creating if needed) the database at path and applies any pending
// migrations. Databases with existing data are snapshotted before migrating.
func Open(path string) (*Store, error) { return OpenWith(path, Options{}) }

// OpenReadOnly opens an existing database without migrating it. Commands that
// only read (export, backup, token list) use it so they can never write to a
// file another process owns.
func OpenReadOnly(path string) (*Store, error) { return OpenWith(path, Options{ReadOnly: true}) }

func OpenWith(path string, opt Options) (*Store, error) {
	db, err := sql.Open("sqlite", dataSourceName(path, opt.ReadOnly))
	if err != nil {
		return nil, err
	}
	// A single connection serializes all access: one writer, no busy-retry
	// logic, and connection pragmas that stay put for the process lifetime.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path, readOnly: opt.ReadOnly, snapshotDir: opt.SnapshotDir}
	// sql.Open is lazy; force the connection so a missing file, a bad DSN or
	// a rejected pragma is reported here rather than on first use.
	if err := s.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, wrapBusy(err))
	}
	migs, err := loadMigrations()
	if err != nil {
		db.Close()
		return nil, err
	}
	// A read-write open is about to migrate: check the file is sound and that
	// this binary is not older than the schema before touching anything.
	if !opt.ReadOnly {
		if err := s.QuickCheck(); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := s.checkSchemaVersion(len(migs)); err != nil {
		db.Close()
		return nil, err
	}
	if opt.ReadOnly {
		return s, nil
	}
	if err := s.applyMigrations(migs); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// dataSourceName builds the driver DSN. The pragmas travel in the DSN so they
// apply to every connection the driver makes, including the ones the backup
// path opens. Read-only handles leave journal_mode and _txlock off: setting a
// journal mode writes to the file, and BEGIN IMMEDIATE fails outright on a
// read-only database.
func dataSourceName(path string, readOnly bool) string {
	params := []string{}
	if readOnly {
		params = append(params, "mode=ro")
	} else {
		params = append(params, "_pragma=journal_mode(WAL)")
	}
	params = append(params,
		"_pragma=synchronous(FULL)",
		"_pragma=foreign_keys(ON)",
		"_pragma=busy_timeout(5000)",
	)
	if !readOnly {
		params = append(params, "_txlock=immediate")
	}
	return "file:" + uriPath(path) + "?" + strings.Join(params, "&")
}

// uriPath percent-encodes the three characters SQLite's URI filename parser
// treats specially, so a path containing them still opens.
var uriPath = strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

func (s *Store) ReadOnly() bool { return s.readOnly }

func (s *Store) Ping() error {
	var n int
	return s.db.QueryRow("SELECT 1").Scan(&n)
}

// QuickCheck runs PRAGMA quick_check, the cheap structural half of
// integrity_check. Anything but a single "ok" row is ErrIntegrity.
func (s *Store) QuickCheck() error {
	rows, err := s.db.Query("PRAGMA quick_check")
	if err != nil {
		return wrapIntegrity(s.path, err)
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return wrapIntegrity(s.path, err)
		}
		results = append(results, line)
	}
	if err := rows.Err(); err != nil {
		return wrapIntegrity(s.path, err)
	}
	if len(results) == 1 && results[0] == "ok" {
		return nil
	}
	return fmt.Errorf("%s: quick_check reported %s: %w", s.path, strings.Join(results, "; "), ErrIntegrity)
}

// wrapIntegrity reports the codes SQLite raises when it cannot read the file
// at all as an integrity failure: badly damaged databases fail the check by
// erroring rather than by returning a row that says so.
func wrapIntegrity(path string, err error) error {
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff {
		case 11, 26: // SQLITE_CORRUPT, SQLITE_NOTADB
			return fmt.Errorf("%s: %s: %w", path, se.Error(), ErrIntegrity)
		}
	}
	return err
}

// LockExclusive takes an advisory lock on <path>.lock so a second server, or a
// CLI command that would migrate the file, cannot run against a live database.
// The lock file is not the database and is safe to leave behind.
func (s *Store) LockExclusive() (func(), error) { return lockFile(s.path + ".lock") }

func (s *Store) checkSchemaVersion(migrations int) error {
	var current int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return err
	}
	if current > migrations {
		return fmt.Errorf("%s is at schema %d, this binary knows %d: %w", s.path, current, migrations, ErrSchemaNewer)
	}
	return nil
}

func loadMigrations() ([]migration, error) {
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	migs := make([]migration, 0, len(names))
	for _, name := range names {
		b, err := migrationsFS.ReadFile(name)
		if err != nil {
			return nil, err
		}
		migs = append(migs, migration{name: filepath.Base(name), sql: string(b)})
	}
	return migs, nil
}

func (s *Store) applyMigrations(migs []migration) error {
	var current int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return err
	}
	if current >= len(migs) {
		return nil
	}
	// A database that has already been migrated once holds real data; snapshot
	// it before touching the schema.
	if current > 0 {
		if _, err := s.snapshotBeforeMigration(current); err != nil {
			return fmt.Errorf("pre-migration snapshot: %w", err)
		}
	}
	for i := current; i < len(migs); i++ {
		if err := s.applyMigration(i+1, migs[i]); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration runs one migration on a single pinned connection. Migrations
// that rebuild a table need foreign_keys off, and that pragma is a silent
// no-op inside a transaction, so it has to be toggled on the same connection
// the transaction will use.
func (s *Store) applyMigration(version int, m migration) (err error) {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	defer func() {
		if _, cerr := conn.ExecContext(ctx, "PRAGMA foreign_keys=ON"); cerr != nil && err == nil {
			err = fmt.Errorf("migration %s: restoring foreign_keys: %w", m.name, cerr)
		}
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(m.sql); err != nil {
		tx.Rollback()
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	if err := foreignKeyCheck(tx); err != nil {
		tx.Rollback()
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
		tx.Rollback()
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	return tx.Commit()
}

func foreignKeyCheck(tx *sql.Tx) error {
	rows, err := tx.Query("PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	var broken []string
	for rows.Next() {
		var table, rowid, parent, fkid sql.NullString
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return err
		}
		broken = append(broken, fmt.Sprintf("%s row %s -> %s", table.String, rowid.String, parent.String))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(broken) > 0 {
		return fmt.Errorf("foreign_key_check: %s", strings.Join(broken, "; "))
	}
	return nil
}

func (s *Store) snapshotBeforeMigration(fromVersion int) (string, error) {
	stamp := time.Now().UTC().Format("20060102-150405")
	base := fmt.Sprintf("%s.pre-migrate-v%d-%s.db", filepath.Base(s.path), fromVersion, stamp)
	dir := s.snapshotDir
	if dir == "" {
		dir = filepath.Dir(s.path)
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return s.vacuumInto(filepath.Join(dir, base))
}

func (s *Store) tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return wrapBusy(err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return wrapBusy(err)
	}
	return wrapBusy(tx.Commit())
}

// wrapBusy turns SQLITE_BUSY and SQLITE_LOCKED into ErrBusy so callers can
// retry them instead of treating a contended write as a server fault. The
// driver reports extended result codes, whose low byte is the primary code.
func wrapBusy(err error) error {
	if err == nil {
		return nil
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff {
		case 5, 6: // SQLITE_BUSY, SQLITE_LOCKED
			return fmt.Errorf("%s: %w", se.Error(), ErrBusy)
		}
	}
	return err
}

const timestampFormat = "2006-01-02T15:04:05.000Z"

func now() string { return time.Now().UTC().Format(timestampFormat) }

// ParseTimestamp canonicalises a caller-supplied timestamp to the format the
// database stores. It accepts RFC3339 with any offset (with or without
// fractional seconds) and a bare YYYY-MM-DD, which is read as UTC midnight.
// An empty string passes through so optional filters can be forwarded as-is.
func ParseTimestamp(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(timestampFormat), nil
		}
	}
	return "", fmt.Errorf("timestamp %q must be RFC3339 (2006-01-02T15:04:05Z) or YYYY-MM-DD", s)
}

// validDate checks the YYYY-MM-DD form used by due dates, start dates and
// target dates. Callers wrap the error with the field name.
func validDate(s string) error {
	if s == "" {
		return nil
	}
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return fmt.Errorf("date %q must be YYYY-MM-DD", s)
	}
	return nil
}

// maxTitleLength caps an issue title, a project name and a milestone name.
// Long enough for any real headline, short enough that a whole document pasted
// into the field is refused instead of stored where nothing will read it.
const maxTitleLength = 500

// validTitle checks the one-line naming fields and returns the trimmed value
// the caller should store. field is the caller's own word for it, so the
// message reads in the terms the request used.
func validTitle(field, s string) (string, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "", fmt.Errorf("%s is required: %w", field, ErrInvalidRef)
	}
	if n := utf8.RuneCountInString(trimmed); n > maxTitleLength {
		return "", fmt.Errorf("%s is %d characters, over the %d character limit: %w", field, n, maxTitleLength, ErrInvalidRef)
	}
	return trimmed, nil
}

// nullable maps "" to NULL for optional text columns.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value",
		key, value,
	)
	return wrapBusy(err)
}

func settingTx(tx *sql.Tx, key string) (string, error) {
	var v string
	err := tx.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}
