package storage

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkRangeReviewCandidates(b *testing.B) {
	db, err := Open(filepath.Join(b.TempDir(), "reviews.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, db.Close()) })
	repo, err := db.GetOrCreateRepo(b.TempDir())
	require.NoError(b, err)
	_, err = db.Exec(`
		WITH RECURSIVE sequence(n) AS (
			SELECT 1 UNION ALL SELECT n + 1 FROM sequence WHERE n < 5000
		)
		INSERT INTO review_jobs (repo_id, git_ref, agent, status, job_type, prompt, uuid)
		SELECT ?, 'base..head', 'test', 'done', 'range', ?,
		       printf('00000000-0000-4000-8000-%012d', n)
		FROM sequence`, repo.ID, strings.Repeat("prompt\n", 2048))
	require.NoError(b, err)
	_, err = db.Exec(`
		INSERT INTO reviews (job_id, agent, prompt, output, uuid)
		SELECT id, agent, prompt, ?, uuid FROM review_jobs`, strings.Repeat("review\n", 2048))
	require.NoError(b, err)

	for b.Loop() {
		candidates, err := db.GetRecentRangeReviewCandidates(b.Context(), repo.ID)
		require.NoError(b, err)
		require.Len(b, candidates, 5000)
	}
}
