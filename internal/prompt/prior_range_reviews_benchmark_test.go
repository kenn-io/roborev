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
	seedUnrelatedRangeReviews(b, db, repo.ID, 5000)
	builder := NewBuilder(db).WithContext(b.Context()).ForRepo(repoPath, repo.ID)

	for b.Loop() {
		views := builder.priorRangeReviewViews(base, base+"..HEAD", []string{base}, 3)
		require.Empty(b, views)
	}
}
