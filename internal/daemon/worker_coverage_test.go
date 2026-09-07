package daemon

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
)

func TestReviewFileCoverageForCommittedInputs(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := tc.GitRepo.CommitFiles(map[string]string{
		"included.txt": "included",
		"excluded.txt": "excluded",
	}, "coverage input")
	job := tc.createJob(t, sha)
	coverage := reviewFileCoverageForJob(context.Background(), tc.TmpDir, job, []string{"excluded.txt"})
	require.NotNil(t, coverage)
	assert.Equal(t, 1, *coverage.Reviewed)
	assert.Equal(t, 1, *coverage.Excluded)
	legacyJob := *job
	legacyJob.JobType = ""
	assert.Nil(t, reviewFileCoverageForJob(context.Background(), tc.TmpDir, &legacyJob, nil))
	dirtyJob := *job
	dirtyJob.JobType = storage.JobTypeDirty
	assert.Nil(t, reviewFileCoverageForJob(context.Background(), tc.TmpDir, &dirtyJob, nil))

	mainBranch := tc.GitRepo.Run("branch", "--show-current")
	tc.GitRepo.Run("checkout", "-b", "coverage-side")
	tc.GitRepo.CommitFile("side.txt", "side", "side")
	tc.GitRepo.Run("checkout", mainBranch)
	tc.GitRepo.CommitFile("main.txt", "main", "main")
	tc.GitRepo.Run("merge", "--no-ff", "coverage-side", "-m", "merge")
	mergeJob := tc.createJob(t, tc.GitRepo.HeadSHA())
	mergeCoverage := reviewFileCoverageForJob(context.Background(), tc.TmpDir, mergeJob, nil)
	require.NotNil(t, mergeCoverage)
	assert.Equal(t, 0, *mergeCoverage.Reviewed)
	assert.Equal(t, 0, *mergeCoverage.Excluded)

	invalidJob := tc.createJob(t, "missing-ref")
	assert.Nil(t, reviewFileCoverageForJob(context.Background(), tc.TmpDir, invalidJob, nil))
}

func TestProcessJobStoresCoverageWithoutChangingPrompt(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	baseline := &agent.FakeAgent{
		NameStr: "empty-review",
		ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
			return "", nil
		},
	}
	agent.Register(baseline)
	t.Cleanup(func() { agent.Unregister(baseline.NameStr) })

	sha := tc.GitRepo.CommitFile("coverage.go", "package coverage\n", "coverage review")
	job := tc.createJobWithAgent(t, sha, baseline.NameStr)
	_, err := tc.DB.Exec(`UPDATE review_jobs SET min_severity = ? WHERE id = ?`, "medium", job.ID)
	require.NoError(t, err)
	cfg := config.DefaultConfig()
	excludes := config.ResolveExcludePatterns(context.Background(), tc.TmpDir, cfg, job.ReviewType)
	minSeverity := "medium"
	builder := prompt.NewBuilderWithConfig(tc.DB, cfg).
		WithContext(context.Background()).
		ForRepo(tc.TmpDir, tc.Repo.ID).
		WithKataClient(kata.NewCLIClient(tc.TmpDir))
	independent, err := builder.BuildWithSnapshotTarget(
		sha, cfg.ReviewContextCount, "test", job.ReviewType, minSeverity, excludes, prompt.SnapshotTarget{},
	)
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	tc.Pool.processJob(testWorkerID, claimed)

	review, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	require.NotNil(t, review.FileCoverage)
	assert.Equal(t, 1, *review.FileCoverage.Reviewed)
	assert.Equal(t, 0, *review.FileCoverage.Excluded)
	assert.Nil(t, review.VerdictBool)
	assert.Equal(t, "medium", review.Job.MinSeverity)
	assert.Equal(t, independent.Prompt, review.Prompt)

	prebuilt, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: tc.Repo.ID, CommitID: *job.CommitID, GitRef: sha, Agent: "test",
		Prompt: "stored prompt", PromptPrebuilt: true, JobType: storage.JobTypeRange,
	})
	require.NoError(t, err)
	prebuiltClaimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	tc.Pool.processJob(testWorkerID, prebuiltClaimed)
	prebuiltReview, err := tc.DB.GetReviewByJobID(prebuilt.ID)
	require.NoError(t, err)
	assert.Nil(t, prebuiltReview.FileCoverage)
}

func TestReviewFileCoverageForUnsupportedCompletionStaysUnknown(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	task, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: tc.Repo.ID, Agent: "test", Prompt: "task prompt", JobType: storage.JobTypeTask,
	})
	require.NoError(t, err)
	_, err = tc.DB.Exec(`UPDATE review_jobs SET status = 'running' WHERE id = ?`, task.ID)
	require.NoError(t, err)
	require.NoError(t, tc.DB.CompleteJobResult(task.ID, "test", "task prompt", storage.ReviewCompletion{
		Output: "task output",
	}))
	taskReview, err := tc.DB.GetReviewByJobID(task.ID)
	require.NoError(t, err)
	assert.Nil(t, taskReview.FileCoverage)

	compact, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: tc.Repo.ID, Agent: "test", Prompt: "compact prompt", JobType: storage.JobTypeCompact,
	})
	require.NoError(t, err)
	_, err = tc.DB.Exec(`UPDATE review_jobs SET status = 'running' WHERE id = ?`, compact.ID)
	require.NoError(t, err)
	require.NoError(t, tc.DB.CompleteJobResult(compact.ID, "test", "compact prompt", storage.ReviewCompletion{
		Output: "compact output",
	}))
	compactReview, err := tc.DB.GetReviewByJobID(compact.ID)
	require.NoError(t, err)
	assert.Nil(t, compactReview.FileCoverage)

	fix, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: tc.Repo.ID, Agent: "test", GitRef: "fix", JobType: storage.JobTypeFix,
	})
	require.NoError(t, err)
	_, err = tc.DB.Exec(`UPDATE review_jobs SET status = 'running' WHERE id = ?`, fix.ID)
	require.NoError(t, err)
	require.NoError(t, tc.DB.CompleteFixJob(fix.ID, "test", "fix prompt", "fix output", ""))
	fixReview, err := tc.DB.GetReviewByJobID(fix.ID)
	require.NoError(t, err)
	assert.Nil(t, fixReview.FileCoverage)
}
