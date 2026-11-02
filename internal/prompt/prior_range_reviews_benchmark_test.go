package prompt

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func BenchmarkPriorRangeReviewViews(b *testing.B) {
	repoPath := b.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "base"},
	} {
		cmd := exec.CommandContext(b.Context(), "git", args...)
		cmd.Dir = repoPath
		out, err := cmd.CombinedOutput()
		require.NoError(b, err, "%s", out)
	}
	cmd := exec.CommandContext(b.Context(), "git", "rev-parse", "HEAD")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	require.NoError(b, err)
	base := strings.TrimSpace(string(out))

	db, err := storage.Open(filepath.Join(b.TempDir(), "reviews.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, db.Close()) })
	repo, err := db.GetOrCreateRepo(repoPath)
	require.NoError(b, err)
	_, err = db.ExecContext(b.Context(), `
		WITH RECURSIVE sequence(n) AS (
			SELECT 1 UNION ALL SELECT n + 1 FROM sequence WHERE n < 5000
		)
		INSERT INTO review_jobs (repo_id, git_ref, agent, status, job_type, uuid)
		SELECT ?, printf('start-%d..end-%d', n, n), 'test', 'done', 'range',
		       printf('00000000-0000-4000-8000-%012d', n)
		FROM sequence`, repo.ID)
	require.NoError(b, err)
	_, err = db.ExecContext(b.Context(), `
		INSERT INTO reviews (job_id, agent, output, uuid)
		SELECT id, agent, 'unrelated review', uuid FROM review_jobs`)
	require.NoError(b, err)
	builder := NewBuilder(db).WithContext(b.Context()).ForRepo(repoPath, repo.ID)

	for b.Loop() {
		views := builder.priorRangeReviewViews(base, base+"..HEAD", []string{base}, 3)
		require.Empty(b, views)
	}
}
