package store

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// snapshotName matches exactly what Backup writes, so pruning can never touch
// a file that happens to sit in the backup directory.
var snapshotName = regexp.MustCompile(`^trackd-\d{8}-\d{6}(-\d+)?\.db$`)

// partialName matches the hidden temp file a snapshot is written under until
// it has passed its integrity check, and nothing else.
var partialName = regexp.MustCompile(`^\.trackd-\d{8}-\d{6}(-\d+)?\.db\.partial$`)

// stalePartialAge is how long a temp file must have gone unwritten before
// pruning takes it for a killed run's leftover. A snapshot still being written
// touches its file far more often, and the scheduler gives up on one after ten
// minutes by default.
const stalePartialAge = time.Hour

// futureSkew is how far ahead of the clock a snapshot's modification time may
// be before it is taken for one written under a wrong clock. Such a file would
// otherwise read as brand new for as long as the clock stays behind it.
const futureSkew = 5 * time.Minute

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

// vacuumIntoContext writes the snapshot under a temp name that snapshotName
// cannot match and renames it to dest only once it has passed its integrity
// check, so a process killed mid-write never leaves a file that pruning or the
// startup age lookup would take for a snapshot. The temp file is created
// exclusively first (VACUUM INTO accepts an empty file), so a run that
// collides with another one's temp name fails up front, and only the run that
// created the file ever removes it.
func (s *Store) vacuumIntoContext(ctx context.Context, dest string) (string, error) {
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("snapshot target %s already exists", dest)
	}
	tmp := filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".partial")
	done := make(chan error, 1)
	go func() {
		done <- func() (err error) {
			f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				return err
			}
			renamed := false
			defer func() {
				if err != nil && !renamed {
					_ = os.Remove(tmp)
				}
			}()
			if err := f.Close(); err != nil {
				return err
			}
			db, err := sql.Open("sqlite", dataSourceName(s.path, s.readOnly))
			if err != nil {
				return err
			}
			defer db.Close()
			if _, err := db.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
				return err
			}
			if err := verifyIntegrity(tmp); err != nil {
				return fmt.Errorf("snapshot failed integrity check: %w", err)
			}
			// An abandoned attempt must not publish after the caller has
			// reported it failed.
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := os.Rename(tmp, dest); err != nil {
				return err
			}
			renamed = true
			return syncDir(filepath.Dir(dest))
		}()
	}()
	select {
	case err := <-done:
		if err != nil {
			return "", err
		}
		return dest, nil
	case <-ctx.Done():
		// The goroutine may be wedged in an uninterruptible filesystem call.
		// Whenever it returns, it finds ctx done before publishing and removes
		// its own partial snapshot.
		return "", ctx.Err()
	}
}

// syncDir makes a new directory entry durable.
func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
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
// it could take the last good copy's place in the count. A hidden temp file a
// killed snapshot left behind, unwritten for stalePartialAge, is removed too:
// it never passed its check, so it is no snapshot. A snapshot dated more than
// futureSkew ahead sorts as the oldest. Returns the removed paths.
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
	var stale []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		isSnap := snapshotName.MatchString(e.Name())
		if !isSnap && !partialName.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, e.Name())
		if !isSnap {
			if time.Since(info.ModTime()) > stalePartialAge {
				stale = append(stale, path)
			}
			continue
		}
		if err := verifyIntegrity(path); err != nil {
			continue
		}
		snaps = append(snaps, snap{path: path, mod: info.ModTime()})
	}
	cutoff := time.Now().Add(futureSkew)
	sort.Slice(snaps, func(i, j int) bool {
		if fi, fj := snaps[i].mod.After(cutoff), snaps[j].mod.After(cutoff); fi != fj {
			return fj
		}
		return snaps[i].mod.After(snaps[j].mod)
	})
	var removed []string
	for _, path := range stale {
		if err := os.Remove(path); err != nil {
			return removed, err
		}
		removed = append(removed, path)
	}
	for _, old := range snaps[min(keep, len(snaps)):] {
		if err := os.Remove(old.path); err != nil {
			return removed, err
		}
		removed = append(removed, old.path)
	}
	return removed, nil
}

// NewestBackupAge reports how long ago the newest snapshot in dir that passes
// its integrity check was written. The bool is false when no snapshot passes,
// which is what the server's startup rule needs to tell "too soon" from "never".
// A snapshot that fails its check is logged and left in place, as evidence,
// the same way PruneBackups treats it. So is one dated more than futureSkew
// ahead, which would otherwise count as a fresh backup until the clock caught
// up with it.
func NewestBackupAge(dir string) (time.Duration, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, false
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
			continue
		}
		snaps = append(snaps, snap{path: filepath.Join(dir, e.Name()), mod: info.ModTime()})
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].mod.After(snaps[j].mod) })
	cutoff := time.Now().Add(futureSkew)
	for _, sn := range snaps {
		if sn.mod.After(cutoff) {
			log.Printf("trackd: snapshot %s is dated %s, in the future, and does not count as a backup", sn.path, sn.mod.UTC().Format(time.RFC3339))
			continue
		}
		if err := verifyIntegrity(sn.path); err != nil {
			log.Printf("trackd: snapshot %s failed its integrity check and does not count as a backup: %v", sn.path, err)
			continue
		}
		age := time.Since(sn.mod)
		if age < 0 {
			age = 0
		}
		return age, true
	}
	return 0, false
}

// Restore copies a verified snapshot to dbPath. It refuses to overwrite an
// existing database, and it refuses when the write-ahead log or shared-memory
// sidecars are present: those belong to a database that is still open, and a
// restore under a running server would be silently undone. The copy goes to
// <dbPath>.restoring and is published only once synced and verified, so a run
// killed mid-copy leaves nothing at dbPath; a leftover temp file is refused.
// The caller holds the database's lock, which is what keeps a running server
// out, since that server may have had its file moved aside.
func Restore(snapshot, dbPath string) (err error) {
	if err := verifyIntegrity(snapshot); err != nil {
		return fmt.Errorf("refusing to restore: %w", err)
	}
	if _, err := os.Stat(dbPath); err == nil {
		return fmt.Errorf("refusing to overwrite existing database %s (%w); stop the server first, then move the database aside", dbPath, os.ErrExist)
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
	tmp := dbPath + ".restoring"
	dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("refusing to restore: %w; another restore may be running, or one was killed and left it behind", err)
	}
	defer func() {
		if rmErr := os.Remove(tmp); err == nil && rmErr != nil {
			err = rmErr
		}
	}()
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	if err := verifyIntegrity(tmp); err != nil {
		return fmt.Errorf("restored file failed its integrity check: %w", err)
	}
	// Link refuses an existing path, so a database that appeared during the
	// copy is never overwritten.
	if err := os.Link(tmp, dbPath); err != nil {
		return err
	}
	// The copy is durable only once the directory entry pointing at it is.
	return syncDir(filepath.Dir(dbPath))
}
