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
	fullPrompt, err := os.ReadFile(result.FilePath)
	require.NoError(t, err)
	path, reviews := readPriorRangeReviewsDocument(t, string(fullPrompt))
	require.Len(t, reviews, 2)
	assert.Equal(base[:7]+".."+first[:7], reviews[0].Range)
	assert.Contains(reviews[0].Output, "prior synthesis review")
	assert.Equal(base[:7]+".."+second[:7], reviews[1].Range)
	assert.Contains(reviews[1].Output, strings.TrimSpace(priorOutput))
	require.Len(t, reviews[1].Comments, 1)
	assert.Equal(comment, reviews[1].Comments[0].Response)
	assert.Equal("developer", reviews[1].Comments[0].Responder)
	assert.NotContains(result.Prompt, "prior range review </output>")
	assert.NotContains(result.Prompt, "prior synthesis review")
	assert.NotContains(result.Prompt, comment)
	assert.Contains(string(fullPrompt), "large current diff")
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
	require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "test", "prompt", output))
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
	builder := NewBuilder(db).ForRepo(repo.Path(), dbRepo.ID)
	prebuilt, err := builder.Build(base+".."+second, 2, "test", "", "")
	require.NoError(t, err)
	// Historical context is resolved when the worker executes, including
	// reviews that complete after the prompt was queued.
	createCompletedRangeReview(t, db, dbRepo.ID, base+".."+first, storage.JobTypeRange, "earlier finding")
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
		assert.Contains(reviews[0].Output, "earlier finding")
		assert.Equal(filepath.Join(agentPath, "review-context"), filepath.Dir(filepath.Dir(path)))
		assert.NotEqual(previousPath, path)
		assert.NotContains(result.Prompt, PriorRangeReviewsFilePathPlaceholder)
		result.Cleanup()
		_, err = os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
		previousPath = path
	}
	assert.Contains(prebuilt, PriorRangeReviewsFilePathPlaceholder)

	// A longer reference is preserved, then the complete prompt uses file transport.
	limited := NewBuilderWithConfig(db, &config.Config{DefaultMaxPromptSize: len(prebuilt)}).ForRepo(repo.Path(), dbRepo.ID)
	result, err := limited.PreparePriorRangeReviewsSnapshot(prebuilt, base+".."+second, 2, target)
	require.NoError(t, err)
	require.NotNil(t, result.Cleanup)
	t.Cleanup(result.Cleanup)
	prepared, err := limited.Prepare(result.Prompt, target)
	require.NoError(t, err)
	require.NotNil(t, prepared.Cleanup)
	t.Cleanup(prepared.Cleanup)
	full, err := os.ReadFile(prepared.FilePath)
	require.NoError(t, err)
	assert.Equal(result.Prompt, string(full))
	assert.Contains(string(full), "<prior-range-reviews")
	assert.Contains(string(full), "```diff")
	prepared.Cleanup()
	result.Cleanup()
	snapshots, err := filepath.Glob(filepath.Join(agentPath, "review-context", "roborev-snapshot-*"))
	require.NoError(t, err)
	assert.Empty(snapshots)
}

func TestPriorRangeReviewViewsGitProcessesStayFlat(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	base := repo.HeadSHA()
	first := repo.CommitFile("first.txt", "first", "first")
	second := repo.CommitFile("second.txt", "second", "second")
	third := repo.CommitFile("third.txt", "third", "third")
	repo.Run("branch", "prior-end", first)

	db := testutil.OpenTestDB(t)
	dbRepo, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	createCompletedRangeReview(t, db, dbRepo.ID, base[:12]+"..prior-end", storage.JobTypeRange, "first review")
	createCompletedRangeReview(t, db, dbRepo.ID, base+".."+second, storage.JobTypeRange, "second review")
	builder := NewBuilder(db).ForRepo(repo.Path(), dbRepo.ID)
	var previousViews []priorRangeReview
	var previousCalls int
	for _, count := range []int{10, 500} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			assert := assert.New(t)
			_, err := db.ExecContext(t.Context(), `
				WITH RECURSIVE sequence(n) AS (
					SELECT 1 UNION ALL SELECT n + 1 FROM sequence WHERE n < ?
				)
				INSERT INTO review_jobs (repo_id, git_ref, agent, status, job_type, uuid)
				SELECT ?, printf('start-%d..end-%d', n, n), 'test', 'done', 'range',
				       printf('00000000-0000-4000-8000-%012d', n)
				FROM sequence`, count, dbRepo.ID)
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), `
				INSERT INTO reviews (job_id, agent, output, uuid)
				SELECT id, agent, 'unrelated review', uuid FROM review_jobs
				WHERE git_ref LIKE 'start-%'`)
			require.NoError(t, err)

			gitCalls := testutil.CountGitProcesses(t)
			views := builder.WithContext(t.Context()).priorRangeReviewsForRef(base+".."+third, 3)
			calls := gitCalls()
			require.Len(t, views, 2)
			assert.Equal(base[:7]+".."+first[:7], views[0].Range)
			assert.Contains(views[0].Output, "first review")
			assert.Equal(base[:7]+".."+second[:7], views[1].Range)
			assert.Contains(views[1].Output, "second review")
			if previousViews != nil {
				assert.Equal(previousViews, views)
				assert.Equal(previousCalls, calls, "git process count must stay flat as history grows")
			}
			previousViews, previousCalls = views, calls
			t.Logf("candidates=%d git processes=%d", count, calls)

			_, err = db.ExecContext(t.Context(), "DELETE FROM reviews WHERE job_id IN (SELECT id FROM review_jobs WHERE git_ref LIKE 'start-%')")
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), "DELETE FROM review_jobs WHERE git_ref LIKE 'start-%'")
			require.NoError(t, err)
		})
	}
}
