package storage

import (
	"database/sql"
	"errors"
	"time"
)

// GetOrCreateCommit finds or creates a commit record.
// Lookups are by (repo_id, sha) to handle the same SHA in different repos.
func (db *DB) GetOrCreateCommit(repoID int64, sha, author, subject string, timestamp time.Time) (*Commit, error) {
	// Try to find existing by (repo_id, sha)
	commit, err := scanCommit(db.QueryRow(`SELECT id, repo_id, sha, author, subject, timestamp, created_at FROM commits WHERE repo_id = ? AND sha = ?`, repoID, sha))
	if err == nil {
		return commit, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// Create new
	result, err := db.Exec(`INSERT INTO commits (repo_id, sha, author, subject, timestamp) VALUES (?, ?, ?, ?, ?)`,
		repoID, sha, author, subject, timestamp.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}

	id, _ := result.LastInsertId()
	return &Commit{
		ID:        id,
		RepoID:    repoID,
		SHA:       sha,
		Author:    author,
		Subject:   subject,
		Timestamp: timestamp,
		CreatedAt: time.Now(),
	}, nil
}

// ErrAmbiguousCommit is returned when a SHA lookup matches multiple repos
var ErrAmbiguousCommit = sql.ErrNoRows // Use sql.ErrNoRows for API compatibility; callers can check message

// GetCommitBySHA returns a commit by its SHA.
// DEPRECATED: This is a legacy API that doesn't handle the same SHA in different repos.
// Returns sql.ErrNoRows if no commit found, or if multiple repos have this SHA (ambiguous).
// Prefer using GetCommitByRepoAndSHA or job-based lookups instead.
func (db *DB) GetCommitBySHA(sha string) (*Commit, error) {
	// Check for ambiguity first
	var count int
	if err := db.QueryRow(`SELECT COUNT(DISTINCT repo_id) FROM commits WHERE sha = ?`, sha).Scan(&count); err != nil {
		return nil, err
	}
	if count > 1 {
		return nil, sql.ErrNoRows // Ambiguous - multiple repos have this SHA
	}

	return scanCommit(db.QueryRow(`SELECT id, repo_id, sha, author, subject, timestamp, created_at FROM commits WHERE sha = ?`, sha))
}

// GetCommitByRepoAndSHA returns a commit by repo ID and SHA
func (db *DB) GetCommitByRepoAndSHA(repoID int64, sha string) (*Commit, error) {
	return scanCommit(db.QueryRow(`SELECT id, repo_id, sha, author, subject, timestamp, created_at FROM commits WHERE repo_id = ? AND sha = ?`, repoID, sha))
}

// GetCommitByID returns a commit by its ID
func (db *DB) GetCommitByID(id int64) (*Commit, error) {
	return scanCommit(db.QueryRow(`SELECT id, repo_id, sha, author, subject, timestamp, created_at FROM commits WHERE id = ?`, id))
}
