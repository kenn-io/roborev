package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var commitColumns = []string{
	"id",
	"repo_id",
	"sha",
	"author",
	"subject",
	"timestamp",
	"created_at",
}

// GetOrCreateCommit finds or creates a commit record.
// Lookups are by (repo_id, sha) to handle the same SHA in different repos.
func (db *DB) GetOrCreateCommit(repoID int64, sha, author, subject string, timestamp time.Time) (*Commit, error) {
	ctx := context.Background()
	commit, err := db.selectCommit(ctx, "repo_id = ? AND sha = ?", repoID, sha)
	if err == nil {
		return commit, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	row := commitRow{
		RepoID:    repoID,
		SHA:       sha,
		Author:    author,
		Subject:   subject,
		Timestamp: dbTimeFromValue(timestamp),
	}
	if _, err := db.bun.NewInsert().
		Model(&row).
		Column("repo_id", "sha", "author", "subject", "timestamp").
		On("CONFLICT (repo_id, sha) DO NOTHING").
		Exec(ctx); err != nil {
		return nil, err
	}

	return db.selectCommit(ctx, "repo_id = ? AND sha = ?", repoID, sha)
}

// ErrAmbiguousCommit is returned when a SHA lookup matches multiple repos
var ErrAmbiguousCommit = sql.ErrNoRows // Use sql.ErrNoRows for API compatibility; callers can check message

// GetCommitBySHA returns a commit by its SHA.
// DEPRECATED: This is a legacy API that doesn't handle the same SHA in different repos.
// Returns sql.ErrNoRows if no commit found, or if multiple repos have this SHA (ambiguous).
// Prefer using GetCommitByRepoAndSHA or job-based lookups instead.
func (db *DB) GetCommitBySHA(sha string) (*Commit, error) {
	ctx := context.Background()
	var count int
	if err := db.bun.NewSelect().
		Table("commits").
		ColumnExpr("COUNT(DISTINCT repo_id)").
		Where("sha = ?", sha).
		Scan(ctx, &count); err != nil {
		return nil, err
	}
	if count > 1 {
		return nil, sql.ErrNoRows
	}

	return db.selectCommit(ctx, "sha = ?", sha)
}

// GetCommitByRepoAndSHA returns a commit by repo ID and SHA
func (db *DB) GetCommitByRepoAndSHA(repoID int64, sha string) (*Commit, error) {
	return db.selectCommit(context.Background(), "repo_id = ? AND sha = ?", repoID, sha)
}

// GetCommitByID returns a commit by its ID
func (db *DB) GetCommitByID(id int64) (*Commit, error) {
	return db.selectCommit(context.Background(), "id = ?", id)
}

func (db *DB) selectCommit(ctx context.Context, where string, args ...any) (*Commit, error) {
	var row commitRow
	if err := db.bun.NewSelect().
		Model(&row).
		Column(commitColumns...).
		Where(where, args...).
		Scan(ctx); err != nil {
		return nil, err
	}
	commit := row.toModel()
	return &commit, nil
}
