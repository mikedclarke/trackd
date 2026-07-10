// Package store implements trackd's SQLite storage layer. All reads and writes
// go through this package. There are no hard-delete operations by design:
// issues and projects are archived or canceled, tokens are revoked, and every
// mutation records an audit event with before/after state.
package store

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

var ErrNotFound = errors.New("not found")

type migration struct {
	name string
	sql  string
}

type Store struct {
	db   *sql.DB
	path string
}

// Open opens (creating if needed) the database at path and applies any pending
// migrations. Databases with existing data are snapshotted before migrating.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// A single connection serializes all access: one writer, no busy-retry logic.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	migs, err := loadMigrations()
	if err != nil {
		db.Close()
		return nil, err
	}
	if err := s.applyMigrations(migs); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

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
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migs[i].sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", migs[i].name, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) snapshotBeforeMigration(fromVersion int) (string, error) {
	stamp := time.Now().UTC().Format("20060102-150405")
	base := fmt.Sprintf("%s.pre-migrate-v%d-%s.db", filepath.Base(s.path), fromVersion, stamp)
	dest := filepath.Join(filepath.Dir(s.path), base)
	return s.vacuumInto(dest)
}

func (s *Store) tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

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
	return err
}

func settingTx(tx *sql.Tx, key string) (string, error) {
	var v string
	err := tx.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}
