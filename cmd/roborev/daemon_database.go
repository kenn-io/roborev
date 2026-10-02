package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

// lockDaemonDatabase excludes other daemon processes before SQLite is opened
// or migrated. The owner holds the lock until its database is closed.
func lockDaemonDatabase(dbPath string) (*flock.Flock, error) {
	path, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	// Resolve aliases so a symlink cannot give the same database a second owner.
	resolved, err := filepath.EvalSymlinks(path)
	if os.IsNotExist(err) {
		dir, dirErr := filepath.EvalSymlinks(filepath.Dir(path))
		if dirErr != nil {
			return nil, fmt.Errorf("resolve database directory: %w", dirErr)
		}
		resolved = filepath.Join(dir, filepath.Base(path))
	} else if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	lock := flock.New(resolved + ".daemon.lock")
	locked, err := lock.TryLock()
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("lock daemon database: %w", err)
	}
	if !locked {
		_ = lock.Close()
		return nil, fmt.Errorf("database is already owned by a daemon: %s", resolved)
	}
	return lock, nil
}
