package daemon

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// seedAgentHookReviewHistory puts failed reviews before newer passing reviews.
func seedAgentHookReviewHistory(t testing.TB, db *storage.DB, repoID int64, ref string, failed, passing int) {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(t.Context(), `
		WITH RECURSIVE sequence(n) AS (
			SELECT 1 UNION ALL SELECT n + 1 FROM sequence WHERE n < ?
		)
		INSERT INTO review_jobs (repo_id, git_ref, branch, agent, status, uuid)
		SELECT ?, ?, 'main', 'test', 'done', printf('00000000-0000-4000-8000-%012d', n)
		FROM sequence`, failed+passing, repoID, ref)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `
		INSERT INTO reviews (job_id, agent, output, closed, verdict_bool, structured_output)
		SELECT id, 'test', '', 0, CASE WHEN id <= ? THEN 0 ELSE 1 END,
			CASE WHEN id <= ? THEN ? ELSE ? END
		FROM review_jobs WHERE repo_id = ?`, failed, failed,
		string(testutil.ReviewFixtureJSON("[P2] Fix the unsafe query.")),
		string(testutil.ReviewFixtureJSON("No issues found.")), repoID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

// BenchmarkAgentHookOpenFailedReviews grows passing history with 20 failures.
func BenchmarkAgentHookOpenFailedReviews(b *testing.B) {
	for _, passing := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("passing-%d", passing), func(b *testing.B) {
			db := testutil.OpenTestDB(b)
			repo, err := db.GetOrCreateRepo(b.TempDir())
			require.NoError(b, err)
			seedAgentHookReviewHistory(b, db, repo.ID, "head", 20, passing)
			source := daemonAgentHookSource{db: db}
			b.ReportAllocs()
			for b.Loop() {
				jobs, ok := source.ListOpenFailedReviewJobs(b.Context(), repo.RootPath, "main")
				require.True(b, ok)
				require.Len(b, jobs, 20)
			}
		})
	}
}
