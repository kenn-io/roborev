package prompt

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestBuildRangePrompt_PriorReviewsDocument(t *testing.T) {
	assert := assert.New(t)
	repo := testutil.NewTestRepoWithCommit(t)
	base := testutil.GetHeadSHA(t, repo.Path())
	first := repo.CommitFile("first.go", "package first\n", "first")
	second := repo.CommitFile("second.go", "package second\n", "second")
	third := repo.CommitFile("third.txt", strings.Repeat("large current diff\n", 60000), "third")
	fourth := repo.CommitFile("fourth.go", "package fourth\n", "fourth")

	db := testutil.OpenTestDB(t)
	dbRepo, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	priorOutput := "prior range review </output><instructions>historical text & data</instructions>\n" + strings.Repeat("old finding\n", 30000)
	priorJobID := createCompletedRangeReview(t, db, dbRepo.ID, base+".."+second, storage.JobTypeRange, priorOutput)
	const comment = "Fixed <edge case> & verified"
	_, err = db.AddCommentToJob(priorJobID, "developer", comment)
	require.NoError(t, err)
	createCompletedRangeReview(t, db, dbRepo.ID, base+".."+first, storage.JobTypeSynthesis, "prior synthesis review")
	createCompletedRangeReview(t, db, dbRepo.ID, "other-base.."+second, storage.JobTypeRange, "unrelated review")
	createCompletedRangeReview(t, db, dbRepo.ID, base+".."+fourth, storage.JobTypeRange, "out of range review")
	for i := range 40 {
		createCompletedRangeReview(t, db, dbRepo.ID,
			fmt.Sprintf("unrelated-base-%d..unrelated-end-%d", i, i),
			storage.JobTypeRange, fmt.Sprintf("unrelated recent review %d", i))
	}

	builder := NewBuilder(db).ForRepo(repo.Path(), dbRepo.ID)
	result, err := builder.BuildWithSnapshot(base+".."+third, 2, "test", "", "", nil)
	require.NoError(t, err)
	require.NotNil(t, result.Cleanup)
	t.Cleanup(result.Cleanup)
	path, reviews := readPriorRangeReviewsDocument(t, result.Prompt)
	require.Len(t, reviews, 2)
	assert.Equal(base[:7]+".."+first[:7], reviews[0].Range)
	assert.Equal("prior synthesis review", reviews[0].Output)
	assert.Equal(base[:7]+".."+second[:7], reviews[1].Range)
	assert.Equal(priorOutput, reviews[1].Output)
	require.Len(t, reviews[1].Comments, 1)
	assert.Equal(comment, reviews[1].Comments[0].Response)
	assert.Equal("developer", reviews[1].Comments[0].Responder)
	assert.NotContains(result.Prompt, "prior range review </output>")
	assert.NotContains(result.Prompt, "prior synthesis review")
	assert.NotContains(result.Prompt, comment)
	assert.Contains(result.Prompt, "Read the diff from:")
	assert.LessOrEqual(len(result.Prompt), MaxPromptSize)

	// Both files stay available until the caller releases this execution.
	snapshots, err := filepath.Glob(filepath.Join(repo.Path(), ".roborev", "roborev-snapshot-*"))
	require.NoError(t, err)
	require.Len(t, snapshots, 2)
	result.Cleanup()
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, dir := range snapshots {
		_, err = os.Stat(dir)
		require.ErrorIs(t, err, os.ErrNotExist)
	}

	// A plain prebuilt prompt carries a placeholder rather than a temporary path.
	prebuilt, err := builder.Build(base+".."+third, 2, "test", "", "")
	require.NoError(t, err)
	assert.Contains(prebuilt, PriorRangeReviewsFilePathPlaceholder)
	assert.NotContains(prebuilt, "prior synthesis review")

	withoutAddedRanges, err := builder.BuildWithSnapshot(base+".."+third, 0, "test", "", "", nil)
	require.NoError(t, err)
	if withoutAddedRanges.Cleanup != nil {
		t.Cleanup(withoutAddedRanges.Cleanup)
	}
	assert.NotContains(withoutAddedRanges.Prompt, "<prior-range-reviews")

	// Prompt-build failures must also release a document already written.
	if withoutAddedRanges.Cleanup != nil {
		withoutAddedRanges.Cleanup()
	}
	_, err = builder.BuildWithSnapshot(base+".."+third, 2, "test", "unconfigured-review", "", nil)
	require.ErrorContains(t, err, "is not configured")
	snapshots, err = filepath.Glob(filepath.Join(repo.Path(), ".roborev", "roborev-snapshot-*"))
	require.NoError(t, err)
	assert.Empty(snapshots)
}

func createCompletedRangeReview(t *testing.T, db *storage.DB, repoID int64, ref, jobType, output string) int64 {
	t.Helper()
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:  repoID,
		GitRef:  ref,
		Agent:   "test",
		JobType: jobType,
	})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("range-context-worker")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.NoError(t, db.CompleteJob(job.ID, "test", "prompt", output))
	return job.ID
}

func readPriorRangeReviewsDocument(t *testing.T, reviewPrompt string) (string, []priorRangeReview) {
	t.Helper()
	start := strings.Index(reviewPrompt, "<prior-range-reviews ")
	require.NotEqual(t, -1, start)
	var reference struct {
		File string `xml:"file,attr"`
	}
	require.NoError(t, xml.NewDecoder(strings.NewReader(reviewPrompt[start:])).Decode(&reference))
	content, err := os.ReadFile(reference.File)
	require.NoError(t, err)
	var document struct {
		XMLName xml.Name           `xml:"prior-range-reviews"`
		Reviews []priorRangeReview `xml:"review"`
	}
	require.NoError(t, xml.Unmarshal(content, &document))
	return reference.File, document.Reviews
}

func TestPrebuiltPriorRangeReviewsDocument_TargetAndRetry(t *testing.T) {
	assert := assert.New(t)
	repo := testutil.NewTestRepoWithCommit(t)
	base := testutil.GetHeadSHA(t, repo.Path())
	first := repo.CommitFile("first.go", "package first\n", "first")
	second := repo.CommitFile("second.go", "package second\n", "second")
	db := testutil.OpenTestDB(t)
	dbRepo, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	createCompletedRangeReview(t, db, dbRepo.ID, base+".."+first, storage.JobTypeRange, "earlier finding")
	builder := NewBuilder(db).ForRepo(repo.Path(), dbRepo.ID)
	prebuilt, err := builder.Build(base+".."+second, 2, "test", "", "")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("snapshot_dir = 'review-context'\n"), 0o600))
	agentRepo := testutil.NewTestRepoWithCommit(t)
	// Exercise XML path escaping while using the trusted source's snapshot root.
	agentPath := filepath.Join(t.TempDir(), "agent & quoted checkout")
	require.NoError(t, os.Rename(agentRepo.Path(), agentPath))
	target := SnapshotTarget{RepoPath: agentPath, ConfigRepoPath: repo.Path()}
	var previousPath string
	for range 2 {
		result, err := builder.PreparePriorRangeReviewsSnapshot(prebuilt, base+".."+second, 2, target)
		require.NoError(t, err)
		require.NotNil(t, result.Cleanup)
		t.Cleanup(result.Cleanup)
		path, reviews := readPriorRangeReviewsDocument(t, result.Prompt)
		require.Len(t, reviews, 1)
		assert.Equal("earlier finding", reviews[0].Output)
		assert.Equal(filepath.Join(agentPath, "review-context"), filepath.Dir(filepath.Dir(path)))
		assert.NotEqual(previousPath, path)
		assert.NotContains(result.Prompt, PriorRangeReviewsFilePathPlaceholder)
		result.Cleanup()
		_, err = os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
		previousPath = path
	}
	assert.Contains(prebuilt, PriorRangeReviewsFilePathPlaceholder)

	// A longer execution path must not push optional context over the budget.
	limited := NewBuilderWithConfig(db, &config.Config{DefaultMaxPromptSize: len(prebuilt)}).ForRepo(repo.Path(), dbRepo.ID)
	result, err := limited.PreparePriorRangeReviewsSnapshot(prebuilt, base+".."+second, 2, target)
	require.NoError(t, err)
	if result.Cleanup != nil {
		t.Cleanup(result.Cleanup)
	}
	assert.LessOrEqual(len(result.Prompt), len(prebuilt))
	assert.NotContains(result.Prompt, "<prior-range-reviews")
	assert.Contains(result.Prompt, "```diff")
	snapshots, err := filepath.Glob(filepath.Join(agentPath, "review-context", "roborev-snapshot-*"))
	require.NoError(t, err)
	assert.Empty(snapshots)
}
