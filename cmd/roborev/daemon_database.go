package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/pathresolve"
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
	var resolved string
	for {
		resolved, err = pathresolve.EvalSymlinks(path)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("resolve database path: %w", err)
		}
		dir, err := pathresolve.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			return nil, fmt.Errorf("resolve database directory: %w", err)
		}
		path = filepath.Join(dir, filepath.Base(path))
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			resolved = path
			break
		}
		if err != nil {
			return nil, fmt.Errorf("inspect database path: %w", err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved = path
			break
		}
		target, err := os.Readlink(path)
		if err != nil {
			return nil, fmt.Errorf("read database symlink: %w", err)
		}
		path = target
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
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
