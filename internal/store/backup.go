package store

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// snapshotName matches exactly what Backup writes, so pruning can never touch
// a file that happens to sit in the backup directory.
var snapshotName = regexp.MustCompile(`^trackd-\d{8}-\d{6}(-\d+)?\.db$`)

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
			db, err := sql.Open("sqlite", dataSourceName(s.path, s.readOnly))
			if err != nil {
				return err
			}
			defer db.Close()
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
		// whatever it leaves behind is a partial snapshot nobody may mistake
		// for a good one, so remove it whenever the call finally returns.
		go func() {
			<-done
			_ = os.Remove(dest)
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
	db, err := sql.Open("sqlite", dataSourceName(path, true))
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
// ordered by modification time. Files that fail their integrity check are
// neither counted nor removed: a corrupt snapshot is evidence, and deleting
// it could take the last good copy's place in the count. Returns the removed
// paths.
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
		if e.IsDir() || !snapshotName.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, e.Name())
		if err := verifyIntegrity(path); err != nil {
			continue
		}
		snaps = append(snaps, snap{path: path, mod: info.ModTime()})
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

// NewestBackupAge reports how long ago the newest snapshot in dir was written.
// The bool is false when the directory holds no snapshot at all, which is what
// the server's startup rule needs to tell "too soon" from "never".
func NewestBackupAge(dir string) (time.Duration, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, false
	}
	var newest time.Time
	for _, e := range entries {
		if e.IsDir() || !snapshotName.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if newest.IsZero() {
		return 0, false
	}
	age := time.Since(newest)
	if age < 0 {
		age = 0
	}
	return age, true
}

// Restore copies a verified snapshot to dbPath. It refuses to overwrite an
// existing database, and it refuses when the write-ahead log or shared-memory
// sidecars are present: those belong to a database that is still open, and a
// restore under a running server would be silently undone.
func Restore(snapshot, dbPath string) error {
	if err := verifyIntegrity(snapshot); err != nil {
		return fmt.Errorf("refusing to restore: %w", err)
	}
	if _, err := os.Stat(dbPath); err == nil {
		return fmt.Errorf("refusing to overwrite existing database %s; move it aside first", dbPath)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		sidecar := dbPath + suffix
		if _, err := os.Stat(sidecar); err == nil {
			return fmt.Errorf("refusing to restore: %s exists, so a trackd still has %s open; stop it and move the sidecars aside first", sidecar, dbPath)
		}
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
	if err := dst.Close(); err != nil {
		return err
	}
	if err := verifyIntegrity(dbPath); err != nil {
		_ = os.Remove(dbPath)
		return fmt.Errorf("restored file failed its integrity check: %w", err)
	}
	// The copy is durable only once the directory entry pointing at it is.
	dir, err := os.Open(filepath.Dir(dbPath))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
