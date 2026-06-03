package storage

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReviewAttemptsTableExists(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, err := db.Exec(`INSERT INTO ci_pr_review_attempts
		(github_repo, pr_number, head_sha, attempt, first_attempt_at, next_attempt_at,
		 last_error_class, consecutive_genuine_attempts, last_error_excerpt,
		 last_panel_run_uuid, state, updated_at)
		VALUES ('o/r', 1, 'sha', 1, datetime('now'), NULL, '', 0, '', '', 'pending', datetime('now'))`)
	require.NoError(t, err)
}
