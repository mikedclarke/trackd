//go:build windows

package store

import "os"

// Windows has no flock. Single-writer safety there rests on SQLite's own
// locking, so the database lock is a no-op that always succeeds.
func LockExclusive(path string) (func(), error) {
	return func() {}, nil
}

// LockFile has no lock to take on Windows, so it reports ErrLockUnsupported
// and creates nothing; the caller falls back to an exclusive create.
func LockFile(path string) (*os.File, error) {
	return nil, ErrLockUnsupported
}
