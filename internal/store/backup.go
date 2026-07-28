package store

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Backup writes a verified snapshot of the live database into dir and returns
// its path. Snapshots are full databases produced by VACUUM INTO and checked
// with PRAGMA integrity_check before being reported as successful.
func (s *Store) Backup(dir string) (string, error) {
	return s.BackupContext(context.Background(), dir)
}

// BackupContext is Backup bounded by ctx. The snapshot runs on its own
// database connection, never the store's serialized one, so a slow or hung
// backup cannot block other store operations. Directories that hang on file
// operations (network mounts, macOS privacy-protected folders) are detected
// by a writability probe before any database work starts; if ctx expires the
// attempt is abandoned and any partial snapshot is removed once the wedged
// operation eventually returns.
func (s *Store) BackupContext(ctx context.Context, dir string) (string, error) {
	if err := probeWritable(ctx, dir); err != nil {
		return "", fmt.Errorf("backup dir %s is not writable: %w (if this times out on macOS, grant trackd access to the folder in System Settings > Privacy & Security)", dir, err)
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	dest := filepath.Join(dir, fmt.Sprintf("trackd-%s.db", stamp))
	// Same-second backups get a numeric suffix rather than an error.
	for n := 2; ; n++ {
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			break
		}
		dest = filepath.Join(dir, fmt.Sprintf("trackd-%s-%d.db", stamp, n))
	}
	return s.vacuumIntoContext(ctx, dest)
}

// probeWritable proves dir accepts file creation before any snapshot work
// begins. The filesystem calls run in a goroutine because a blocked directory
// can hang them indefinitely rather than erroring; the caller gives up when
// ctx expires and the goroutine cleans up after itself if it ever completes.
func probeWritable(ctx context.Context, dir string) error {
	done := make(chan error, 1)
	go func() {
		err := os.MkdirAll(dir, 0o755)
		if err == nil {
			probe := filepath.Join(dir, ".trackd-probe")
			err = os.WriteFile(probe, []byte("probe"), 0o644)
			if err == nil {
				_ = os.Remove(probe)
			}
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) vacuumInto(dest string) (string, error) {
	return s.vacuumIntoContext(context.Background(), dest)
}

func (s *Store) vacuumIntoContext(ctx context.Context, dest string) (string, error) {
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("snapshot target %s already exists", dest)
	}
	done := make(chan error, 1)
	go func() {
		done <- func() error {
			db, err := sql.Open("sqlite", s.path)
			if err != nil {
				return err
			}
			defer db.Close()
			if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
				return err
			}
			if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
				return err
			}
			if err := verifyIntegrity(dest); err != nil {
				_ = os.Remove(dest)
				return fmt.Errorf("snapshot failed integrity check: %w", err)
			}
			return nil
		}()
	}()
	select {
	case err := <-done:
		if err != nil {
			return "", err
		}
		return dest, nil
	case <-ctx.Done():
		// The goroutine may be wedged in an uninterruptible filesystem call;
		// discard whatever it produces whenever it finally returns.
		go func() {
			if err := <-done; err == nil {
				_ = os.Remove(dest)
			}
		}()
		return "", ctx.Err()
	}
}

func verifyIntegrity(path string) error {
	// Opening a missing path would silently create an empty database, and an
	// empty database passes integrity_check; require a real file first.
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return fmt.Errorf("%s is empty", path)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("integrity_check: %s", result)
	}
	return nil
}

// PruneBackups removes the oldest trackd-*.db snapshots in dir beyond keep,
// ordered by modification time. It returns the removed paths.
func PruneBackups(dir string, keep int) ([]string, error) {
	if keep < 1 {
		return nil, fmt.Errorf("keep must be >= 1, got %d", keep)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type snap struct {
		path string
		mod  time.Time
	}
	var snaps []snap
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "trackd-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		snaps = append(snaps, snap{path: filepath.Join(dir, name), mod: info.ModTime()})
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].mod.After(snaps[j].mod) })
	var removed []string
	for _, old := range snaps[min(keep, len(snaps)):] {
		if err := os.Remove(old.path); err != nil {
			return removed, err
		}
		removed = append(removed, old.path)
	}
	return removed, nil
}

// Restore copies a verified snapshot to dbPath. It refuses to overwrite an
// existing database: moving the old file aside first is a deliberate step.
func Restore(snapshot, dbPath string) error {
	if err := verifyIntegrity(snapshot); err != nil {
		return fmt.Errorf("refusing to restore: %w", err)
	}
	if _, err := os.Stat(dbPath); err == nil {
		return fmt.Errorf("refusing to overwrite existing database %s; move it aside first", dbPath)
	}
	src, err := os.Open(snapshot)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(dbPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		_ = os.Remove(dbPath)
		return err
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}
