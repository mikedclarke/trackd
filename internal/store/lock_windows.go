//go:build windows

package store

// Windows has no flock. Single-writer safety there rests on SQLite's own
// locking, so the advisory lock is a no-op that always succeeds.
func lockFile(path string) (func(), error) {
	return func() {}, nil
}
