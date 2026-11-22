//go:build unix

package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// LockExclusive takes the advisory lock on <path>.lock so a second server, or a
// CLI command that would migrate the file, cannot run against a live database.
// The lock file is not the database and is safe to leave behind.
func LockExclusive(path string) (func(), error) {
	f, _, err := lockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	return func() { unlockFile(f) }, nil
}

// LockFile takes the same non-blocking advisory lock on path itself, creating
// the file when it is missing, and returns the locked file; closing it
// releases the lock, as does a crash. ErrLocked means another process holds
// it. ErrLockUnsupported means the filesystem has no advisory locks, and a
// file this call created is removed again.
func LockFile(path string) (*os.File, error) {
	f, created, err := lockFile(path)
	if errors.Is(err, ErrLockUnsupported) && created {
		_ = os.Remove(path)
	}
	return f, err
}

// lockFile takes a non-blocking exclusive flock. The lock lives on the open
// file description, so it is released by closing the file and by process
// exit, including a crash. created reports whether this call made the file.
func lockFile(path string) (f *os.File, created bool, err error) {
	f, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	created = err == nil
	if errors.Is(err, os.ErrExist) {
		f, err = os.OpenFile(path, os.O_RDWR, 0)
	}
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		switch {
		case errors.Is(err, syscall.EWOULDBLOCK):
			return nil, created, fmt.Errorf("%s: %w", path, ErrLocked)
		case errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.ENOLCK):
			return nil, created, fmt.Errorf("locking %s: %v: %w", path, err, ErrLockUnsupported)
		}
		return nil, created, fmt.Errorf("locking %s: %w", path, err)
	}
	return f, created, nil
}

func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}
