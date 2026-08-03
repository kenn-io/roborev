package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AgentHookSnooze is a local agent-hook suppression window for one checkout
// and branch.
type AgentHookSnooze struct {
	RepoPath     string    `json:"repo_path"`
	WorktreePath string    `json:"worktree_path"`
	Branch       string    `json:"branch"`
	SnoozedUntil time.Time `json:"snoozed_until"`
}

// SetAgentHookSnooze creates or replaces the snooze for a tracked repository's
// current worktree and branch.
func (db *DB) SetAgentHookSnooze(
	repoPath, worktreePath, branch string, until time.Time,
) (*AgentHookSnooze, error) {
	repo, err := db.GetRepoByPath(repoPath)
	if err != nil {
		return nil, err
	}
	normalizedWorktree, err := normalizeRepoPath(worktreePath)
	if err != nil {
		return nil, fmt.Errorf("normalize worktree path: %w", err)
	}
	until = until.UTC()
	_, err = db.Exec(`
		INSERT INTO agent_hook_snoozes
			(repo_id, worktree_path, branch, snoozed_until, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(repo_id, worktree_path, branch) DO UPDATE SET
			snoozed_until = excluded.snoozed_until,
			updated_at = excluded.updated_at`,
		repo.ID, normalizedWorktree, branch,
		until.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("set agent hook snooze: %w", err)
	}
	return &AgentHookSnooze{
		RepoPath:     repo.RootPath,
		WorktreePath: normalizedWorktree,
		Branch:       branch,
		SnoozedUntil: until,
	}, nil
}

// ClearAgentHookSnooze removes the snooze for one checkout and branch. It is
// idempotent so `roborev snooze off` is safe when no snooze is active.
func (db *DB) ClearAgentHookSnooze(repoPath, worktreePath, branch string) error {
	repo, err := db.GetRepoByPath(repoPath)
	if err != nil {
		return err
	}
	normalizedWorktree, err := normalizeRepoPath(worktreePath)
	if err != nil {
		return fmt.Errorf("normalize worktree path: %w", err)
	}
	_, err = db.Exec(`
		DELETE FROM agent_hook_snoozes
		WHERE repo_id = ? AND worktree_path = ? AND branch = ?`,
		repo.ID, normalizedWorktree, branch,
	)
	if err != nil {
		return fmt.Errorf("clear agent hook snooze: %w", err)
	}
	return nil
}

// ActiveAgentHookSnooze returns the active snooze for one checkout and branch,
// or nil when none exists or its deadline has passed.
func (db *DB) ActiveAgentHookSnooze(
	repoPath, worktreePath, branch string, now time.Time,
) (*AgentHookSnooze, error) {
	repo, err := db.GetRepoByPath(repoPath)
	if err != nil {
		return nil, err
	}
	normalizedWorktree, err := normalizeRepoPath(worktreePath)
	if err != nil {
		return nil, fmt.Errorf("normalize worktree path: %w", err)
	}
	var untilRaw string
	err = db.QueryRow(`
		SELECT snoozed_until
		FROM agent_hook_snoozes
		WHERE repo_id = ? AND worktree_path = ? AND branch = ?`,
		repo.ID, normalizedWorktree, branch,
	).Scan(&untilRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get agent hook snooze: %w", err)
	}
	until, err := time.Parse(time.RFC3339Nano, untilRaw)
	if err != nil {
		return nil, fmt.Errorf("parse agent hook snooze deadline: %w", err)
	}
	if !until.After(now) {
		return nil, nil
	}
	return &AgentHookSnooze{
		RepoPath:     repo.RootPath,
		WorktreePath: normalizedWorktree,
		Branch:       branch,
		SnoozedUntil: until,
	}, nil
}
