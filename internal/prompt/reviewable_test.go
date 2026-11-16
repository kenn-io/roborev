package prompt

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
)

// A clean merge commit has an empty combined diff, but binary evidence compares
// it with its first parent. With evidence on, a binary file the merge brings in
// is content to review. With evidence off, nothing reaches the reviewer.
func TestReviewableCountsBinaryEvidenceForCleanMerge(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	trunk := repo.Run("branch", "--show-current")
	repo.Run("checkout", "-b", "side")
	repo.CommitFile("image.png", "PNG\x00binary", "add image")
	repo.Run("checkout", trunk)
	repo.CommitFile("trunk.txt", "trunk\n", "trunk change")
	repo.Run("merge", "--no-ff", "side", "-m", "merge side")
	merge := repo.HeadSHA()

	census, err := TakeReviewPathCensus(t.Context(), repo.Root, merge, nil)
	require.NoError(t, err)
	require.Empty(t, census.Reviewed, "a clean merge has an empty combined diff")

	require.NoError(t, census.Reviewable(t.Context(), true))
	var empty *NoReviewableChangesError
	require.ErrorAs(t, census.Reviewable(t.Context(), false), &empty)

	excluded, err := TakeReviewPathCensus(t.Context(), repo.Root, merge, []string{"*.png"})
	require.NoError(t, err)
	require.ErrorAs(t, excluded.Reviewable(t.Context(), true), &empty,
		"excluded binary files are not evidence")
}
