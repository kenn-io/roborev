package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/goalreview"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func registerGoalReviewPi(t *testing.T, cfg *config.Config) {
	t.Helper()
	original, err := agent.Get("pi")
	require.NoError(t, err)
	executable, err := os.Executable()
	require.NoError(t, err)
	agent.Register(agent.NewPiAgent(executable))
	t.Cleanup(func() { agent.Register(original) })
	cfg.PiCmd = executable
}

func goalReviewTestRunner(run func(context.Context, goalreview.Snapshot, prompt.SnapshotResult) ([]goalreview.Finding, error)) goalReviewRunner {
	return func(ctx context.Context, _ agent.Agent, _ string, snapshot goalreview.Snapshot, prepared prompt.SnapshotResult, _ io.Writer) ([]goalreview.Finding, error) {
		return run(ctx, snapshot, prepared)
	}
}

func goalArtifacts(t *testing.T, root string) {
	t.Helper()
	for name, text := range map[string]string{"docs/superpowers/specs/feature-design.md": "# Feature\n", "docs/superpowers/plans/feature.md": "**Spec:** `docs/superpowers/specs/feature-design.md`\n**Goal:** Feature\n### Task 1: Change\n"} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(text), 0o600))
	}
}

func TestJobEventTypeFallsBackWhenJobMetadataIsUnavailable(t *testing.T) {
	assert.Equal(t, "review.canceled", jobEventType(nil, "canceled"))
}

func TestGoalWorkerRejectsMissingOrChangedFrozenEvidence(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			c := newWorkerTestContext(t, 1)
			registerGoalReviewPi(t, c.Pool.cfgGetter.Config())
			reviewInvoked := false
			c.Pool.goalReviewRunner = goalReviewTestRunner(func(_ context.Context, _ goalreview.Snapshot, _ prompt.SnapshotResult) ([]goalreview.Finding, error) {
				reviewInvoked = true
				return nil, nil
			})
			snapshot := goalreview.Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: "# Frozen\n"}}}
			prompt := goalreview.BuildPrompt(snapshot)
			if missing {
				prompt = ""
			}
			job, err := c.DB.EnqueueJob(storage.EnqueueOpts{RepoID: c.Repo.ID, GitRef: "incorrect-digest", Agent: "pi", ReviewType: "goal", JobType: storage.JobTypeGoalReview, Prompt: prompt, PromptPrebuilt: true})
			require.NoError(t, err)
			for range maxRetries + 1 {
				claimed, err := c.DB.ClaimJob(testWorkerID)
				require.NoError(t, err)
				require.NotNil(t, claimed)
				c.Pool.processJob(testWorkerID, claimed)
			}
			failed, err := c.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, storage.JobStatusFailed, failed.Status)
			assert.Contains(t, failed.Error, "frozen")
			assert.False(t, reviewInvoked, "invalid frozen evidence cannot fall through to a schema review")
		})
	}
}

func TestGoalWorkerUpdateInterruptionGuardsSuccessfulCompletion(t *testing.T) {
	c := newWorkerTestContext(t, 1)
	registerGoalReviewPi(t, c.Pool.cfgGetter.Config())
	snapshot := goalreview.Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: "# Frozen\n"}}}
	job, err := c.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: c.Repo.ID, Agent: "pi", GitRef: snapshot.ID(), ReviewType: "goal",
		JobType: storage.JobTypeGoalReview, Prompt: goalreview.BuildPrompt(snapshot), PromptPrebuilt: true,
	})
	require.NoError(t, err)
	claimed, err := c.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)

	started := make(chan struct{})
	finish := make(chan struct{})
	c.Pool.goalReviewRunner = goalReviewTestRunner(func(context.Context, goalreview.Snapshot, prompt.SnapshotResult) ([]goalreview.Finding, error) {
		close(started)
		<-finish
		return nil, nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Pool.processJob(testWorkerID, claimed)
	}()
	require.True(t, waitForUpdateSignal(started, 5*time.Second), "goal review did not start")
	c.Pool.InterruptJobsForUpdate([]int64{job.ID})
	close(finish)
	require.True(t, waitForUpdateSignal(done, 5*time.Second), "goal review did not unwind")

	stored, err := c.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusQueued, stored.Status)
	assert.Zero(t, stored.RetryCount)
	_, err = c.DB.GetReviewByJobID(job.ID)
	assert.Error(t, err, "an update-interrupted result must not be persisted")
}

func TestGoalWorkerFallsBackToMainRepoWhenWorktreeWasRemoved(t *testing.T) {
	c := newWorkerTestContext(t, 1)
	registerGoalReviewPi(t, c.Pool.cfgGetter.Config())
	snapshot := goalreview.Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: "# Frozen\n"}}}
	removedWorktree := filepath.Join(t.TempDir(), "removed-worktree")
	require.NoError(t, os.Mkdir(removedWorktree, 0o700))
	require.NoError(t, os.RemoveAll(removedWorktree))
	job, err := c.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: c.Repo.ID, GitRef: snapshot.ID(), Agent: "pi", ReviewType: "goal",
		JobType: storage.JobTypeGoalReview, Prompt: goalreview.BuildPrompt(snapshot), PromptPrebuilt: true,
		WorktreePath: removedWorktree,
	})
	require.NoError(t, err)
	var runnerRepo string
	c.Pool.goalReviewRunner = func(_ context.Context, _ agent.Agent, repoPath string, _ goalreview.Snapshot, _ prompt.SnapshotResult, _ io.Writer) ([]goalreview.Finding, error) {
		runnerRepo = repoPath
		return nil, nil
	}
	claimed, err := c.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, job.ID, claimed.ID)
	c.Pool.processGoalReview(context.Background(), testWorkerID, claimed, c.Pool.cfgGetter.Config())

	completed, err := c.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusDone, completed.Status)
	assert.Equal(t, c.Repo.RootPath, runnerRepo)
}

func TestRunningGoalReviewCancellationBroadcastsOnce(t *testing.T) {
	server, db, repoPath := newTestServer(t)
	testutil.InitTestGitRepo(t, repoPath)
	registerGoalReviewPi(t, server.configWatcher.Config())

	repo, err := db.GetOrCreateRepo(repoPath)
	require.NoError(t, err)
	snapshot := goalreview.Snapshot{
		Source: "superpowers",
		Stage:  "spec",
		Artifacts: []goalreview.Artifact{{
			Kind: "spec", Path: "docs/superpowers/specs/feature-design.md", Content: "# Feature\n",
		}},
	}
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, Agent: "pi", GitRef: snapshot.ID(), ReviewType: config.ReviewTypeGoal,
		JobType: storage.JobTypeGoalReview, Prompt: goalreview.BuildPrompt(snapshot), PromptPrebuilt: true,
	})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("goal-review-cancel-worker")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, job.ID, claimed.ID)

	started := make(chan struct{})
	finished := make(chan struct{})
	server.workerPool.goalReviewRunner = goalReviewTestRunner(func(ctx context.Context, _ goalreview.Snapshot, _ prompt.SnapshotResult) ([]goalreview.Finding, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	go func() {
		defer close(finished)
		server.workerPool.processJob("goal-review-cancel-worker", claimed)
	}()
	t.Cleanup(func() {
		server.workerPool.CancelJob(job.ID)
		<-finished
	})

	// Wall-clock wait: the worker must complete its SQLite claim before running the gated agent.
	require.True(t, waitForUpdateSignal(started, 5*time.Second), "goal review did not start")
	_, events := server.broadcaster.Subscribe("")
	_, err = server.humaCancelJob(context.Background(), &CancelJobInput{
		Body: CancelJobRequest{JobID: job.ID},
	})
	require.NoError(t, err)
	// Wall-clock wait: the canceled worker must finish its SQLite transition before event count is checked.
	require.True(t, waitForUpdateSignal(finished, 5*time.Second), "canceled goal review did not finish")

	require.Len(t, events, 1)
	event := <-events
	assert.Equal(t, "goal_review.canceled", event.Type)
	assert.Equal(t, job.ID, event.JobID)
}

func TestGoalEnqueueFrozen(t *testing.T) {
	server, db, _ := newTestServer(t)
	registerGoalReviewPi(t, server.configWatcher.Config())
	repo := testutil.NewGitRepo(t)
	goalArtifacts(t, repo.Path())
	job := enqueueViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), ReviewType: config.ReviewTypeGoal, Agent: "pi"})
	assert.Equal(t, storage.JobTypeGoalReview, job.JobType)
	assert.Zero(t, job.CommitIDValue())
	snapshot, err := goalreview.ParseSnapshot(job.Prompt)
	require.NoError(t, err)
	assert.Equal(t, snapshot.ID(), job.GitRef)
	require.NoError(t, os.RemoveAll(filepath.Join(repo.Path(), "docs")))
	require.NoError(t, db.CancelJob(job.ID))
	require.NoError(t, db.ReenqueueJob(job.ID, storage.ReenqueueOpts{}))
	rerun, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, job.Prompt, rerun.Prompt)
}

func TestGoalEnqueueAcceptsAndPreservesOversizedArtifactPrompt(t *testing.T) {
	server, db, _ := newTestServer(t)
	registerGoalReviewPi(t, server.configWatcher.Config())
	repo := testutil.NewGitRepo(t)
	goalArtifacts(t, repo.Path())
	marker := "final line from the complete artifact"
	largeArtifact := strings.Repeat("complete intent evidence for review\n", 40000) + marker + "\n"
	specPath := filepath.Join(repo.Path(), "docs/superpowers/specs/feature-design.md")
	require.NoError(t, os.WriteFile(specPath, []byte(largeArtifact), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"),
		[]byte("review_agent = \"pi\"\ndefault_max_prompt_size = 204800\n"), 0o600))

	job := enqueueViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), ReviewType: "goal", Agent: "pi"})
	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	snapshot, err := goalreview.ParseSnapshot(stored.Prompt)
	require.NoError(t, err)
	assert.Contains(t, goalreview.BuildPrompt(snapshot), marker)
	assert.Greater(t, len(goalreview.BuildPrompt(snapshot)), config.DefaultMaxPromptSize)
}

func TestGoalWorkerAcceptsOversizedFrozenPrompt(t *testing.T) {
	c := newWorkerTestContext(t, 1)
	registerGoalReviewPi(t, c.Pool.cfgGetter.Config())
	c.Pool.cfgGetter.Config().DefaultMaxPromptSize = 1024
	largeContent := strings.Repeat("complete intent evidence\n", 50000) + "last captured requirement\n"
	snapshot := goalreview.Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: largeContent}}}
	fullPrompt := goalreview.BuildPrompt(snapshot)
	require.Greater(t, len(fullPrompt), 1024)
	job, err := c.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: c.Repo.ID, Agent: "pi", GitRef: snapshot.ID(), ReviewType: "goal",
		JobType: storage.JobTypeGoalReview, Prompt: fullPrompt, PromptPrebuilt: true,
	})
	require.NoError(t, err)
	c.Pool.goalReviewRunner = goalReviewTestRunner(func(_ context.Context, _ goalreview.Snapshot, prepared prompt.SnapshotResult) ([]goalreview.Finding, error) {
		complete := prepared.Prompt
		if prepared.FilePath != "" {
			data, readErr := os.ReadFile(prepared.FilePath)
			require.NoError(t, readErr)
			complete = string(data)
		}
		assert.Contains(t, complete, "last captured requirement")
		return nil, nil
	})
	claimed, err := c.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	c.Pool.processGoalReview(context.Background(), testWorkerID, claimed, c.Pool.cfgGetter.Config())

	completed, err := c.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusDone, completed.Status)
	review, err := c.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, fullPrompt, review.Prompt)
}

func TestGoalEnqueueRejectsUnsafeInputs(t *testing.T) {
	server, _, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	for _, req := range []EnqueueRequest{
		{RepoPath: repo.Path(), ReviewType: "goal", Agent: "test", CustomPrompt: "override"},
		{RepoPath: repo.Path(), ReviewType: "goal", Agent: "test", Agentic: true},
		{RepoPath: repo.Path(), ReviewType: "goal", Agent: "test", GitRef: "HEAD"},
		{RepoPath: repo.Path(), ReviewType: "goal", Agent: "test", Panel: "none"},
		{RepoPath: repo.Path(), Agent: "test", GitRef: "HEAD", SpecFile: new("spec.md")},
	} {
		w := enqueueRaw(t, server, req)
		assert.Equal(t, 400, w.Code, w.Body.String())
	}
}

func TestGoalWorkerFrozenEvidence(t *testing.T) {
	c := newWorkerTestContext(t, 1)
	registerGoalReviewPi(t, c.Pool.cfgGetter.Config())
	goalArtifacts(t, c.TmpDir)
	snapshot := goalreview.Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: "# Frozen design\n"}}}
	job, err := c.DB.EnqueueJob(storage.EnqueueOpts{RepoID: c.Repo.ID, GitRef: snapshot.ID(), Agent: "pi", ReviewType: "goal", JobType: storage.JobTypeGoalReview, Prompt: goalreview.BuildPrompt(snapshot), PromptPrebuilt: true})
	require.NoError(t, err)
	c.Pool.goalReviewRunner = goalReviewTestRunner(func(_ context.Context, _ goalreview.Snapshot, _ prompt.SnapshotResult) ([]goalreview.Finding, error) {
		return nil, nil
	})
	require.NoError(t, os.RemoveAll(filepath.Join(c.TmpDir, "docs")))
	subscriberID, events := c.Broadcaster.Subscribe("")
	defer c.Broadcaster.Unsubscribe(subscriberID)
	claimed, err := c.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	c.Pool.processJob(testWorkerID, claimed)
	completed, err := c.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusDone, completed.Status)
	review, err := c.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "No issues found.", review.Output)
	assert.Equal(t, job.Prompt, review.Prompt)
	for range 2 {
		var event Event
		select {
		case event = <-events:
		case <-time.After(time.Second):
		}
		require.Contains(t, event.Type, "goal_review.", "missing goal lifecycle event")
	}
}
