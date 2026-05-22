package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	gitpkg "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"

	// workerTestContext encapsulates the common setup for worker pool tests.
	"github.com/stretchr/testify/require"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testWorkerID = "test-worker"

type workerTestContext struct {
	DB          *storage.DB
	TmpDir      string
	Repo        *storage.Repo
	Pool        *WorkerPool
	Broadcaster Broadcaster
}

// newWorkerTestContext creates a DB, repo, broadcaster, and worker pool with
// the given number of workers. Pass 0 to use the config default.
func newWorkerTestContext(t *testing.T, workers int) *workerTestContext {
	t.Helper()
	db, tmpDir := testutil.OpenTestDBWithDir(t)
	testutil.InitTestGitRepo(t, tmpDir)

	cfg := config.DefaultConfig()
	if workers > 0 {
		cfg.MaxWorkers = workers
	}

	repo, err := db.GetOrCreateRepo(tmpDir)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}

	b := NewBroadcaster()
	pool := NewWorkerPool(db, NewStaticConfig(cfg), cfg.MaxWorkers, b, nil, nil)

	return &workerTestContext{
		DB:          db,
		TmpDir:      tmpDir,
		Repo:        repo,
		Pool:        pool,
		Broadcaster: b,
	}
}

// createJobWithAgent enqueues a job for the given SHA and agent and returns it.
func (c *workerTestContext) createJobWithAgent(t *testing.T, sha, agent string) *storage.ReviewJob {
	t.Helper()
	commit, err := c.DB.GetOrCreateCommit(c.Repo.ID, sha, "Author", "Subject", time.Now())
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateCommit failed: %v", err)
	}
	job, err := c.DB.EnqueueJob(storage.EnqueueOpts{RepoID: c.Repo.ID, CommitID: commit.ID, GitRef: sha, Agent: agent})
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "EnqueueJob failed: %v", err)
	}
	return job
}

// createAndClaimJobWithAgent enqueues and claims a job with a specific agent.
func (c *workerTestContext) createAndClaimJobWithAgent(t *testing.T, sha, workerID, agent string) *storage.ReviewJob {
	t.Helper()
	job := c.createJobWithAgent(t, sha, agent)
	claimed, err := c.DB.ClaimJob(workerID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "ClaimJob failed: %v", err)
	}
	if claimed.ID != job.ID {
		require.Condition(t, func() bool {
			return false
		}, "Expected to claim job %d, got %d", job.ID, claimed.ID)
	}
	return claimed
}

// createJob enqueues a job for the given SHA and returns it.
func (c *workerTestContext) createJob(t *testing.T, sha string) *storage.ReviewJob {
	t.Helper()
	return c.createJobWithAgent(t, sha, "test")
}

// createAndClaimJob enqueues and claims a job, returning both.
func (c *workerTestContext) createAndClaimJob(t *testing.T, sha, workerID string) *storage.ReviewJob {
	t.Helper()
	return c.createAndClaimJobWithAgent(t, sha, workerID, "test")
}

// exhaustRetries exhausts retries for a job to simulate failure loop
func (c *workerTestContext) exhaustRetries(t *testing.T, job *storage.ReviewJob, workerID, agent string) *storage.ReviewJob {
	t.Helper()
	for i := range maxRetries {
		c.Pool.failOrRetryInner(workerID, job, agent, "connection reset", true)
		reclaimed, err := c.DB.ClaimJob(workerID)
		if err != nil || reclaimed == nil {
			require.Condition(t, func() bool {
				return false
			}, "re-claim after retry %d: %v", i, err)
		}
		job = reclaimed
	}
	return job
}

func (c *workerTestContext) assertJobPendingCancel(t *testing.T, jobID int64, expected bool) {
	t.Helper()
	if got := c.Pool.IsJobPendingCancel(jobID); got != expected {
		assert.Condition(t, func() bool {
			return false
		}, "IsJobPendingCancel(%d) = %v, want %v", jobID, got, expected)
	}
}

// assertJobStatus fetches the job by ID and asserts its status matches want.
// Returns the fetched job for further assertions on other fields.
func (c *workerTestContext) assertJobStatus(t *testing.T, jobID int64, want storage.JobStatus) *storage.ReviewJob {
	t.Helper()
	job, err := c.DB.GetJobByID(jobID)
	require.NoError(t, err, "GetJobByID(%d)", jobID)
	require.Equal(t, want, job.Status, "job %d status", jobID)
	return job
}

// startPool starts the worker pool and returns it for chaining.
func (c *workerTestContext) startPool() {
	c.Pool.Start()
}

// reconfigurePool replaces the pool with a new one using the given config
// and 1 worker, preserving the existing DB and broadcaster.
func (c *workerTestContext) reconfigurePool(cfg *config.Config) {
	c.Pool = NewWorkerPool(c.DB, NewStaticConfig(cfg), 1, c.Broadcaster, nil, nil)
}

func TestWorkerPoolConcurrency(t *testing.T) {
	t.Parallel()
	tc := newWorkerTestContext(t, 4)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)

	for range 5 {
		tc.createJob(t, sha)
	}

	tc.startPool()

	// Poll until workers are active or timeout
	var activeWorkers int
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		activeWorkers = tc.Pool.ActiveWorkers()
		if activeWorkers > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if activeWorkers == 0 {
		require.Condition(t, func() bool {
			return false
		}, "expected active worker within timeout")
	}

	tc.Pool.Stop()

	t.Logf("Peak active workers: %d", activeWorkers)
}

func TestWorkerPoolPendingCancellation(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "pending-cancel", testWorkerID)

	// Don't start the pool - test pending cancellation manually
	if !tc.Pool.CancelJob(job.ID) {
		assert.Condition(t, func() bool {
			return false
		}, "CancelJob should return true for valid running job")
	}

	tc.assertJobPendingCancel(t, job.ID, true)

	canceled := false
	tc.Pool.registerRunningJob(job.ID, func() { canceled = true })

	if !canceled {
		assert.Condition(t, func() bool {
			return false
		}, "Job should have been canceled immediately on registration")
	}

	tc.assertJobPendingCancel(t, job.ID, false)
}

func TestWorkerPoolPendingCancellationAfterDBCancel(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "api-cancel-race", testWorkerID)

	// Simulate the API path: db.CancelJob first
	if err := tc.DB.CancelJob(job.ID); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "db.CancelJob failed: %v", err)
	}

	jobAfterDBCancel := tc.assertJobStatus(t, job.ID, storage.JobStatusCanceled)
	if jobAfterDBCancel.WorkerID == "" {
		require.Condition(t, func() bool {
			return false
		}, "Expected WorkerID to be set after claim")
	}

	if !tc.Pool.CancelJob(job.ID) {
		assert.Condition(t, func() bool {
			return false
		}, "CancelJob should return true for canceled-but-claimed job")
	}

	tc.assertJobPendingCancel(t, job.ID, true)

	canceled := false
	tc.Pool.registerRunningJob(job.ID, func() { canceled = true })

	if !canceled {
		assert.Condition(t, func() bool {
			return false
		}, "Job should have been canceled immediately on registration")
	}
}

func TestWorkerPoolCancelInvalidJob(t *testing.T) {
	db := testutil.OpenTestDB(t)

	cfg := config.DefaultConfig()
	broadcaster := NewBroadcaster()
	pool := NewWorkerPool(db, NewStaticConfig(cfg), 1, broadcaster, nil, nil)

	if pool.CancelJob(99999) {
		assert.Condition(t, func() bool {
			return false
		}, "CancelJob should return false for non-existent job")
	}

	if pool.IsJobPendingCancel(99999) {
		assert.Condition(t, func() bool {
			return false
		}, "Non-existent job should not be added to pendingCancels")
	}
}

func TestWorkerPoolCancelJobFinishedDuringWindow(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "finish-window", testWorkerID)

	if err := tc.DB.CompleteJob(job.ID, "test", "prompt", "output"); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "CompleteJob failed: %v", err)
	}

	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)

	if tc.Pool.CancelJob(job.ID) {
		assert.Condition(t, func() bool {
			return false
		}, "CancelJob should return false for completed job")
	}

	if tc.Pool.IsJobPendingCancel(job.ID) {
		assert.Condition(t, func() bool {
			return false
		}, "Completed job should not be added to pendingCancels")
	}
}

func TestWorkerPoolCancelJobRegisteredDuringCheck(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "register-during", testWorkerID)

	canceled := false
	tc.Pool.registerRunningJob(job.ID, func() { canceled = true })

	if !tc.Pool.CancelJob(job.ID) {
		assert.Condition(t, func() bool {
			return false
		}, "CancelJob should return true for registered job")
	}

	if !canceled {
		assert.Condition(t, func() bool {
			return false
		}, "Job should have been canceled")
	}

	tc.assertJobPendingCancel(t, job.ID, false)
}

func TestWorkerPoolCancelJobConcurrentRegister(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "concurrent-register", testWorkerID)

	var canceled atomic.Int32
	cancelFunc := func() { canceled.Add(1) }

	tc.Pool.testHookAfterSecondCheck = func() {
		tc.Pool.registerRunningJob(job.ID, cancelFunc)
	}

	result := tc.Pool.CancelJob(job.ID)

	if !result {
		assert.Condition(t, func() bool {
			return false
		}, "CancelJob should return true")
	}
	if canceled.Load() != 1 {
		assert.Condition(t, func() bool {
			return false
		}, "Job should have been canceled exactly once")
	}

	tc.Pool.unregisterRunningJob(job.ID)
}

type sessionStreamingTestAgent struct {
	name       string
	streamLine string
}

func (a *sessionStreamingTestAgent) Name() string { return a.name }

func (a *sessionStreamingTestAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	if output != nil {
		if _, err := io.WriteString(output, a.streamLine+"\n"); err != nil {
			return "", err
		}
	}
	return "No issues found.", nil
}

func (a *sessionStreamingTestAgent) WithReasoning(level agent.ReasoningLevel) agent.Agent {
	return a
}

func (a *sessionStreamingTestAgent) WithAgentic(agentic bool) agent.Agent {
	return a
}

func (a *sessionStreamingTestAgent) WithModel(model string) agent.Agent {
	return a
}

func (a *sessionStreamingTestAgent) CommandLine() string { return a.name }

func TestProcessJob_CapturesSessionID(t *testing.T) {
	tests := []struct {
		name       string
		streamLine string
		want       string
	}{
		{
			name:       "claude session_id",
			streamLine: `{"type":"system","subtype":"init","session_id":"claude-session-123"}`,
			want:       "claude-session-123",
		},
		{
			name:       "codex thread_id",
			streamLine: `{"type":"thread.started","thread_id":"codex-thread-456"}`,
			want:       "codex-thread-456",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tcxt := newWorkerTestContext(t, 1)
			sha := testutil.GetHeadSHA(t, tcxt.TmpDir)
			agentName := fmt.Sprintf("session-stream-%s", strings.ReplaceAll(tc.name, " ", "-"))
			agent.Register(&sessionStreamingTestAgent{name: agentName, streamLine: tc.streamLine})
			t.Cleanup(func() { agent.Unregister(agentName) })

			job := tcxt.createAndClaimJobWithAgent(t, sha, testWorkerID, agentName)
			tcxt.Pool.processJob(testWorkerID, job)

			updated := tcxt.assertJobStatus(t, job.ID, storage.JobStatusDone)
			if updated.SessionID != tc.want {
				require.Condition(t, func() bool {
					return false
				}, "session_id=%q, want %q", updated.SessionID, tc.want)
			}

			review, err := tcxt.DB.GetReviewByJobID(job.ID)
			if err != nil {
				require.Condition(t, func() bool {
					return false
				}, "GetReviewByJobID: %v", err)
			}
			if review.Job == nil {
				require.Condition(t, func() bool {
					return false
				}, "expected joined job on review")
			}
			if review.Job.SessionID != tc.want {
				require.Condition(t, func() bool {
					return false
				}, "review job session_id=%q, want %q", review.Job.SessionID, tc.want)
			}
		})
	}
}

func TestProcessJob_UsesStoredReviewPromptOverride(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)

	commit, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, sha, "Author", "Subject", time.Now())
	require.NoError(t, err)

	var capturedPrompt string
	agentName := "stored-review-prompt-capture"
	agent.Register(&agent.FakeAgent{
		NameStr: agentName,
		ReviewFn: func(ctx context.Context, repoPath, commitSHA, reviewPrompt string, output io.Writer) (string, error) {
			capturedPrompt = reviewPrompt
			return "No issues found.", nil
		},
	})
	t.Cleanup(func() { agent.Unregister(agentName) })

	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID:         tc.Repo.ID,
		CommitID:       commit.ID,
		GitRef:         sha,
		Agent:          agentName,
		Prompt:         "review body\n<untrusted-pr-discussion>\n<comment>latest</comment>\n</untrusted-pr-discussion>\n",
		PromptPrebuilt: true,
		JobType:        storage.JobTypeRange,
	})
	require.NoError(t, err)

	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)

	tc.Pool.processJob(testWorkerID, claimed)

	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	assert.Equal(t, job.Prompt, capturedPrompt)
	assert.Equal(t, job.Prompt, updated.Prompt)
}

func TestApplyCodexReviewSettingsOnlyForReviewJobs(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agent.Codex.DisableReviewSkills = true
	cfg.Agent.Codex.IgnoreReviewUserConfig = true

	tests := []struct {
		name string
		job  storage.ReviewJob
		want bool
	}{
		{
			name: "single commit review",
			job:  storage.ReviewJob{JobType: storage.JobTypeReview},
			want: true,
		},
		{
			name: "range review",
			job:  storage.ReviewJob{JobType: storage.JobTypeRange},
			want: true,
		},
		{
			name: "dirty review",
			job:  storage.ReviewJob{JobType: storage.JobTypeDirty},
			want: true,
		},
		{
			name: "task job",
			job:  storage.ReviewJob{JobType: storage.JobTypeTask},
			want: false,
		},
		{
			name: "classify job",
			job:  storage.ReviewJob{JobType: storage.JobTypeClassify},
			want: false,
		},
		{
			name: "fix job",
			job:  storage.ReviewJob{JobType: storage.JobTypeFix},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := applyCodexReviewSettings(agent.NewCodexAgent("codex"), &tt.job, cfg)
			codexAgent, ok := got.(*agent.CodexAgent)
			require.True(t, ok)
			assert.Equal(t, tt.want, codexAgent.SuppressSkillInstructions)
			assert.Equal(t, tt.want, codexAgent.IgnoreUserConfig)
		})
	}
}

func TestProcessJob_RebuildsAndPersistsFreshPromptForReviewRetry(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)

	commit, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, sha, "Author", "Subject", time.Now())
	require.NoError(t, err)

	var capturedPrompt string
	agentName := "review-retry-prompt-capture"
	agent.Register(&agent.FakeAgent{
		NameStr: agentName,
		ReviewFn: func(ctx context.Context, repoPath, commitSHA, reviewPrompt string, output io.Writer) (string, error) {
			capturedPrompt = reviewPrompt
			return "No issues found.", nil
		},
	})
	t.Cleanup(func() { agent.Unregister(agentName) })

	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID:   tc.Repo.ID,
		CommitID: commit.ID,
		GitRef:   sha,
		Agent:    agentName,
	})
	require.NoError(t, err)
	require.NoError(t, tc.DB.SaveJobPrompt(job.ID, "stale prompt from prior attempt"))

	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.Equal(t, "stale prompt from prior attempt", claimed.Prompt)

	tc.Pool.processJob(testWorkerID, claimed)

	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	require.NotEmpty(t, capturedPrompt)
	assert.NotEqual(t, "stale prompt from prior attempt", capturedPrompt)
	assert.Equal(t, capturedPrompt, updated.Prompt)
}

func TestWriteDiffSnapshot_WritesExternalReadableTempFile(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	worktreeDir := filepath.Join(t.TempDir(), "feature-worktree")

	cmd := exec.Command("git", "-C", repo.Root, "worktree", "add", "-b", "feature/worktree", worktreeDir, "HEAD")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git worktree add failed: %s", out)
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repo.Root, "worktree", "remove", "--force", worktreeDir).Run()
	})

	require.NoError(t, os.WriteFile(filepath.Join(worktreeDir, "feature.txt"), []byte("feature change\n"), 0o644))
	cmd = exec.Command("git", "-C", worktreeDir, "add", "feature.txt")
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, "git add failed: %s", out)
	cmd = exec.Command("git", "-C", worktreeDir, "commit", "-m", "worktree change")
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, "git commit failed: %s", out)

	shaBytes, err := exec.Command("git", "-C", worktreeDir, "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	sha := strings.TrimSpace(string(shaBytes))

	diffFile, cleanup, err := prompt.NewBuilder(nil).ForRepo(worktreeDir, 0).WriteDiffSnapshot(sha, nil)
	require.NoError(t, err)
	require.NotEmpty(t, diffFile, "expected diff file for worktree-backed review")
	require.NotNil(t, cleanup)

	gitDirBytes, err := exec.Command("git", "-C", worktreeDir, "rev-parse", "--git-dir").Output()
	require.NoError(t, err)
	gitDir := strings.TrimSpace(string(gitDirBytes))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktreeDir, gitDir)
	}

	assert.False(t,
		strings.HasPrefix(filepath.Clean(diffFile), filepath.Clean(gitDir)),
		"snapshot should not live in git dir: got %s, git dir %s", diffFile, gitDir)
	assert.True(t,
		strings.HasPrefix(filepath.Base(filepath.Dir(diffFile)), "roborev-snapshot-"),
		"snapshot should live in a private roborev temp dir, got %s", diffFile)
	data, err := os.ReadFile(diffFile)
	require.NoError(t, err)
	assert.Contains(t, string(data), "diff --git")
	assert.Contains(t, string(data), "feature.txt")

	cleanup()
	_, err = os.Stat(diffFile)
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err), "expected cleanup to remove diff file, got %v", err)
}

func TestPreparePrebuiltPrompt_ReplacesDiffFilePlaceholder(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := &storage.ReviewJob{ID: 73, Agent: "codex", GitRef: sha}

	reviewPrompt, cleanup, err := preparePrebuiltPrompt(
		tc.TmpDir,
		job,
		"## Pull Request Discussion\n\n### Diff\n\nRead the diff from: `"+prompt.DiffFilePathPlaceholder+"`\n",
		nil,
	)
	require.NoError(t, err)
	require.NotNil(t, cleanup)

	assert.NotContains(t, reviewPrompt, prompt.DiffFilePathPlaceholder)
	assert.Contains(t, reviewPrompt, "roborev-snapshot-")

	cleanup()
}

func TestPreparePrebuiltPrompt_RequotesDiffPathWithSingleQuote(t *testing.T) {
	baseDir := t.TempDir()
	repoPath := filepath.Join(baseDir, "repo's")
	require.NoError(t, os.MkdirAll(repoPath, 0o755))
	testutil.InitTestGitRepo(t, repoPath)

	sha := testutil.GetHeadSHA(t, repoPath)
	job := &storage.ReviewJob{ID: 74, Agent: "codex", GitRef: sha}

	reviewPrompt, cleanup, err := preparePrebuiltPrompt(
		repoPath,
		job,
		"### Diff\n\nRead the diff from: `"+prompt.DiffFilePathPlaceholder+"`\n",
		nil,
	)
	require.NoError(t, err)
	require.NotNil(t, cleanup)

	assert.Contains(t, reviewPrompt, "roborev-snapshot-")
	assert.Contains(t, reviewPrompt, ".diff")
	assert.NotContains(t, reviewPrompt, prompt.DiffFilePathPlaceholder)

	cleanup()
}

func TestPreparePrebuiltPrompt_AllowsUnsafeModeByStillWritingDiffFile(t *testing.T) {
	prev := agent.AllowUnsafeAgents()
	agent.SetAllowUnsafeAgents(true)
	t.Cleanup(func() { agent.SetAllowUnsafeAgents(prev) })

	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := &storage.ReviewJob{ID: 75, Agent: "codex", GitRef: sha}

	reviewPrompt, cleanup, err := preparePrebuiltPrompt(
		tc.TmpDir,
		job,
		"### Diff\n\nRead the diff from: `"+prompt.DiffFilePathPlaceholder+"`\n",
		nil,
	)
	require.NoError(t, err)
	require.NotNil(t, cleanup)
	assert.NotContains(t, reviewPrompt, prompt.DiffFilePathPlaceholder)
	assert.Contains(t, reviewPrompt, "roborev-snapshot-")

	cleanup()
}

func TestProcessJob_SmallDiffSucceedsWhenGitDirReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not restrict writes on Windows")
	}

	originalCodex, err := agent.Get("codex")
	require.NoError(t, err)

	agentCalled := false
	agent.Register(&agent.FakeAgent{
		NameStr: "codex",
		ReviewFn: func(ctx context.Context, repoPath, commitSHA, p string, output io.Writer) (string, error) {
			agentCalled = true
			return "No issues found.", nil
		},
	})
	t.Cleanup(func() { agent.Register(originalCodex) })

	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	commit, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, sha, "Author", "test", time.Now())
	require.NoError(t, err)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID:   tc.Repo.ID,
		CommitID: commit.ID,
		GitRef:   sha,
		Agent:    "codex",
	})
	require.NoError(t, err)

	// Make git dir read-only. Small diffs should not need a snapshot.
	gitDir, err := gitpkg.ResolveGitDir(tc.TmpDir)
	require.NoError(t, err)
	info, err := os.Stat(gitDir)
	require.NoError(t, err)
	origMode := info.Mode().Perm()
	require.NoError(t, os.Chmod(gitDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(gitDir, origMode) })

	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)

	tc.Pool.processJob(testWorkerID, claimed)

	// Small diff fits inline.
	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	assert.True(t, agentCalled, "agent should run for small diffs even when .git is read-only")
}

func TestProcessJob_LargeDiffUsesExternalSnapshotWhenGitDirReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not restrict writes on Windows")
	}

	originalCodex, err := agent.Get("codex")
	require.NoError(t, err)

	agentCalled := false
	agent.Register(&agent.FakeAgent{
		NameStr: "codex",
		ReviewFn: func(ctx context.Context, repoPath, commitSHA, p string, output io.Writer) (string, error) {
			agentCalled = true
			return "No issues found.", nil
		},
	})
	t.Cleanup(func() { agent.Register(originalCodex) })

	tc := newWorkerTestContext(t, 1)
	var content strings.Builder
	for range 20000 {
		content.WriteString("line ")
		content.WriteString(strings.Repeat("x", 20))
		content.WriteString(" ")
		content.WriteString(strings.Repeat("y", 20))
		content.WriteString("\n")
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(tc.TmpDir, "large.txt"),
		[]byte(content.String()), 0o644,
	))
	cmd := exec.Command("git", "-C", tc.TmpDir, "add", "large.txt")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git add failed: %s", out)
	cmd = exec.Command("git", "-C", tc.TmpDir, "commit", "-m", "large diff")
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, "git commit failed: %s", out)

	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	commit, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, sha, "Author", "large", time.Now())
	require.NoError(t, err)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID:   tc.Repo.ID,
		CommitID: commit.ID,
		GitRef:   sha,
		Agent:    "codex",
	})
	require.NoError(t, err)

	// Make git dir read-only. Large-diff snapshots should still work
	// because they are written outside .git.
	gitDir, err := gitpkg.ResolveGitDir(tc.TmpDir)
	require.NoError(t, err)
	info, err := os.Stat(gitDir)
	require.NoError(t, err)
	origMode := info.Mode().Perm()
	require.NoError(t, os.Chmod(gitDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(gitDir, origMode) })

	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)

	tc.Pool.processJob(testWorkerID, claimed)

	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	assert.True(t, agentCalled, "agent should run because snapshots no longer require writing to .git")
}

func TestProcessJob_LargeDiffUsesExternalSnapshotWithoutOversizedPrompt(t *testing.T) {
	originalTest, err := agent.Get("test")
	require.NoError(t, err)

	agentCalled := false
	var capturedPrompt string
	snapshotRE := regexp.MustCompile("`([^`]+roborev-snapshot-[^`]+\\.diff)`")
	agent.Register(&agent.FakeAgent{
		NameStr: "test",
		ReviewFn: func(ctx context.Context, repoPath, commitSHA, p string, output io.Writer) (string, error) {
			agentCalled = true
			capturedPrompt = p
			require.LessOrEqual(t, len(p), 4096, "submitted prompt must stay within configured cap")
			match := snapshotRE.FindStringSubmatch(p)
			require.NotNil(t, match, "large diff prompt should reference a snapshot file")
			snapshotPath := match[1]
			assert.NotContains(t, snapshotPath, string(filepath.Separator)+".git"+string(filepath.Separator))
			data, readErr := os.ReadFile(snapshotPath)
			require.NoError(t, readErr)
			assert.Contains(t, string(data), "large-agent.txt")
			return "No issues found.", nil
		},
	})
	t.Cleanup(func() { agent.Register(originalTest) })

	tc := newWorkerTestContext(t, 1)
	cfg := config.DefaultConfig()
	cfg.DefaultMaxPromptSize = 4096
	tc.reconfigurePool(cfg)

	var content strings.Builder
	for range 300 {
		content.WriteString("+")
		content.WriteString(strings.Repeat("large diff content ", 8))
		content.WriteString("\n")
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(tc.TmpDir, "large-agent.txt"),
		[]byte(content.String()), 0o644,
	))
	cmd := exec.Command("git", "-C", tc.TmpDir, "add", "large-agent.txt")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git add failed: %s", out)
	cmd = exec.Command("git", "-C", tc.TmpDir, "commit", "-m", "large agent diff")
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, "git commit failed: %s", out)

	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	commit, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, sha, "Author", "large", time.Now())
	require.NoError(t, err)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID:   tc.Repo.ID,
		CommitID: commit.ID,
		GitRef:   sha,
		Agent:    "test",
	})
	require.NoError(t, err)

	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)

	tc.Pool.processJob(testWorkerID, claimed)

	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	assert.True(t, agentCalled, "agent should run with an external snapshot prompt")
	assert.Equal(t, 0, updated.RetryCount)
	assert.NotEmpty(t, capturedPrompt)
	assert.NotContains(t, capturedPrompt, "```diff")
}

func TestProcessJob_OversizedFinalPromptFailsBeforeAnyAgent(t *testing.T) {
	originalTest, err := agent.Get("test")
	require.NoError(t, err)

	agentCalled := false
	agent.Register(&agent.FakeAgent{
		NameStr: "test",
		ReviewFn: func(ctx context.Context, repoPath, commitSHA, p string, output io.Writer) (string, error) {
			agentCalled = true
			return "No issues found.", nil
		},
	})
	t.Cleanup(func() { agent.Register(originalTest) })

	tc := newWorkerTestContext(t, 1)
	cfg := config.DefaultConfig()
	cfg.DefaultMaxPromptSize = 1024
	tc.reconfigurePool(cfg)

	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID:  tc.Repo.ID,
		GitRef:  "run:large-prompt",
		Agent:   "test",
		Prompt:  strings.Repeat("x", 2048),
		JobType: storage.JobTypeTask,
	})
	require.NoError(t, err)

	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)

	tc.Pool.processJob(testWorkerID, claimed)

	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.False(t, agentCalled, "oversized final prompt must not be submitted")
	assert.Equal(t, 0, updated.RetryCount)
	assert.Contains(t, updated.Error, "prompt exceeds size limit before agent submission")
}

func TestFailOrRetryAgent_ContextWindowErrorFailsWithoutRetry(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJobWithAgent(t, "context-limit-test", testWorkerID, "codex")

	tc.Pool.failOrRetryAgent(testWorkerID, job, "codex", "agent: codex failed: Codex ran out of room in the model's context window")

	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Equal(t, 0, updated.RetryCount)
	assert.Contains(t, updated.Error, "context window")
}

func TestWorkerPoolCancelJobFinalCheckDeadlockSafe(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "deadlock-test", testWorkerID)

	canceled := false
	cancelFunc := func() {
		canceled = true
		tc.Pool.unregisterRunningJob(job.ID)
	}

	tc.Pool.testHookAfterSecondCheck = func() {
		tc.Pool.registerRunningJob(job.ID, cancelFunc)
	}

	done := make(chan bool)
	go func() {
		done <- tc.Pool.CancelJob(job.ID)
	}()

	select {
	case result := <-done:
		if !result {
			assert.Condition(t, func() bool {
				return false
			}, "CancelJob should return true")
		}
	case <-time.After(2 * time.Second):
		require.Condition(t, func() bool {
			return false
		}, "CancelJob deadlocked - cancel() called while holding lock")
	}

	if !canceled {
		assert.Condition(t, func() bool {
			return false
		}, "Job should have been canceled via final check path")
	}
}

func TestAgentCooldown(t *testing.T) {
	cfg := config.DefaultConfig()
	pool := NewWorkerPool(nil, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil)

	// Not cooling down initially
	if pool.isAgentCoolingDown("gemini") {
		assert.Condition(t, func() bool {
			return false
		}, "expected gemini not in cooldown initially")
	}

	// Set cooldown
	pool.cooldownAgent("gemini", time.Now().Add(1*time.Hour))
	if !pool.isAgentCoolingDown("gemini") {
		assert.Condition(t, func() bool {
			return false
		}, "expected gemini in cooldown after set")
	}

	// Different agent not affected
	if pool.isAgentCoolingDown("codex") {
		assert.Condition(t, func() bool {
			return false
		}, "expected codex not in cooldown")
	}

	// Expired cooldown returns false
	pool.cooldownAgent("codex", time.Now().Add(-1*time.Second))
	if pool.isAgentCoolingDown("codex") {
		assert.Condition(t, func() bool {
			return false
		}, "expected expired cooldown to return false")
	}

	// cooldownAgent never shortens
	pool.cooldownAgent("gemini", time.Now().Add(1*time.Minute))
	if !pool.isAgentCoolingDown("gemini") {
		assert.Condition(t, func() bool {
			return false
		}, "cooldown should not have been shortened")
	}
}

func TestAgentCooldown_ExpiredEntryDeleted(t *testing.T) {
	cfg := config.DefaultConfig()
	pool := NewWorkerPool(
		nil, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil,
	)

	// Set an already-expired cooldown
	pool.cooldownAgent("gemini", time.Now().Add(-1*time.Second))

	// Should return false and clean up the entry
	if pool.isAgentCoolingDown("gemini") {
		assert.Condition(t, func() bool {
			return false
		}, "expected expired cooldown to return false")
	}

	// Entry should be deleted from the map
	pool.agentCooldownsMu.RLock()
	_, exists := pool.agentCooldowns["gemini"]
	pool.agentCooldownsMu.RUnlock()
	if exists {
		assert.Condition(t, func() bool {
			return false
		}, "expected expired entry to be deleted from map")
	}
}

func TestAgentCooldown_RefreshDuringUpgrade(t *testing.T) {
	cfg := config.DefaultConfig()
	pool := NewWorkerPool(
		nil, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil,
	)

	// Set an already-expired cooldown so RLock path enters upgrade
	pool.cooldownAgent("gemini", time.Now().Add(-1*time.Second))

	// Use the test hook to refresh the cooldown in the window
	// between RUnlock and Lock, simulating a concurrent goroutine.
	pool.testHookCooldownLockUpgrade = func() {
		pool.agentCooldownsMu.Lock()
		pool.agentCooldowns["gemini"] = time.Now().Add(1 * time.Hour)
		pool.agentCooldownsMu.Unlock()
	}

	// The read-lock path sees expired, upgrades, recheck sees
	// refreshed entry — should return true.
	if !pool.isAgentCoolingDown("gemini") {
		assert.Condition(t, func() bool {
			return false
		}, "expected refreshed cooldown to return true")
	}
}

func TestProcessJob_CooldownResolvesAlias(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)

	// Enqueue a job with the alias "claude" (canonical: "claude-code")
	claimed := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "claude")
	job := claimed

	// Cool down "claude-code" (canonical name)
	tc.Pool.cooldownAgent(
		"claude-code", time.Now().Add(1*time.Hour),
	)

	// processJob should detect cooldown via alias resolution
	tc.Pool.processJob(testWorkerID, claimed)

	tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
}

func TestResolveBackupAgent_AliasMatchesPrimary(t *testing.T) {
	// "claude" is an alias for "claude-code". If job.Agent is "claude"
	// and backup resolves to "claude-code", they are the same agent.
	cfg := config.DefaultConfig()
	cfg.DefaultBackupAgent = "claude-code"
	pool := NewWorkerPool(
		nil, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil,
	)
	job := &storage.ReviewJob{
		Agent:    "claude",
		RepoPath: t.TempDir(),
	}
	got := pool.resolveBackupAgent(job)
	// Should return "" because claude == claude-code after alias
	// resolution. (May also return "" if claude-code binary is not
	// installed, which is fine — both reasons are correct.)
	if got != "" {
		assert.Condition(t, func() bool {
			return false
		}, "resolveBackupAgent() = %q, want empty (alias match)",
			got)

	}
}

func TestResolveBackupIgnoresMalformedRepoConfig(t *testing.T) {
	repoPath := t.TempDir()
	err := os.WriteFile(
		filepath.Join(repoPath, ".roborev.toml"),
		[]byte("review_backup_agent = ["),
		0o644,
	)
	require.NoError(t, err)

	cfg := config.DefaultConfig()
	cfg.DefaultBackupAgent = "test"
	cfg.DefaultBackupModel = "backup-model"

	pool := NewWorkerPool(
		nil, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil,
	)
	job := &storage.ReviewJob{
		Agent:    "codex",
		RepoPath: repoPath,
	}

	assert.Equal(t, "test", pool.resolveBackupAgent(job))
	assert.Equal(t, "backup-model", pool.resolveBackupModel(job))
}

func TestFailOrRetryInner_QuotaSkipsRetries(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJob(t, sha, testWorkerID)

	// Subscribe to events to verify broadcast
	_, eventCh := tc.Broadcaster.Subscribe("")

	quotaErr := "resource exhausted: reset after 1h"
	tc.Pool.failOrRetryInner(testWorkerID, job, "gemini", quotaErr, true)

	// Job should be failed (not retried) with quota prefix
	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	if !strings.HasPrefix(updated.Error, review.QuotaErrorPrefix) {
		assert.Condition(t, func() bool {
			return false
		}, "error=%q, want prefix %q", updated.Error, review.QuotaErrorPrefix)
	}

	// Retry count should be 0 — no retries attempted
	retryCount, err := tc.DB.GetJobRetryCount(job.ID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetJobRetryCount: %v", err)
	}
	if retryCount != 0 {
		assert.Condition(t, func() bool {
			return false
		}, "retry_count=%d, want 0 (quota should skip retries)", retryCount)
	}

	// Agent should be in cooldown
	if !tc.Pool.isAgentCoolingDown("gemini") {
		assert.Condition(t, func() bool {
			return false
		}, "expected gemini in cooldown after quota error")
	}

	// Broadcast should have fired
	select {
	case ev := <-eventCh:
		if ev.Type != "review.failed" {
			assert.Condition(t, func() bool {
				return false
			}, "event type=%q, want review.failed", ev.Type)
		}
		if !strings.HasPrefix(ev.Error, review.QuotaErrorPrefix) {
			assert.Condition(t, func() bool {
				return false
			}, "event error=%q, want prefix %q", ev.Error, review.QuotaErrorPrefix)
		}
	case <-time.After(time.Second):
		assert.Condition(t, func() bool {
			return false
		}, "no broadcast event received")
	}
}

func TestFailOrRetryInner_QuotaExhaustedVariant(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJob(t, sha, testWorkerID)

	// "quota exhausted" (not "quota exceeded") must also trigger quota-skip
	tc.Pool.failOrRetryInner(testWorkerID, job, "gemini", "quota exhausted, reset after 2h", true)

	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	if !strings.HasPrefix(updated.Error, review.QuotaErrorPrefix) {
		assert.Condition(t, func() bool {
			return false
		}, "error=%q, want prefix %q", updated.Error, review.QuotaErrorPrefix)
	}
	retryCount, _ := tc.DB.GetJobRetryCount(job.ID)
	if retryCount != 0 {
		assert.Condition(t, func() bool {
			return false
		}, "retry_count=%d, want 0", retryCount)
	}
}

func TestFailOrRetryInner_NonQuotaStillRetries(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJob(t, sha, testWorkerID)

	// A non-quota agent error should follow the normal retry path
	tc.Pool.failOrRetryInner(testWorkerID, job, "gemini", "connection reset", true)

	// Should be queued for retry, not failed
	tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)

	retryCount, err := tc.DB.GetJobRetryCount(job.ID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetJobRetryCount: %v", err)
	}
	if retryCount != 1 {
		assert.Condition(t, func() bool {
			return false
		}, "retry_count=%d, want 1", retryCount)
	}

	// Agent should NOT be in cooldown
	if tc.Pool.isAgentCoolingDown("gemini") {
		assert.Condition(t, func() bool {
			return false
		}, "expected gemini NOT in cooldown for non-quota error")
	}
}

func TestFailOrRetryInner_SessionLimitCoolsDownAndSkipsRetries(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJob(t, sha, testWorkerID)

	// Stub classifier: any error message containing the marker yields
	// KindSession with a 1-hour CooldownFor. Tests the seam without
	// depending on real Claude wording.
	tc.Pool.classify = func(agentName, msg string) agent.LimitClassification {
		if strings.Contains(msg, "MARKER-SESSION-LIMIT") {
			return agent.LimitClassification{
				Kind:        agent.LimitKindSession,
				Agent:       agentName,
				CooldownFor: 1 * time.Hour,
				Message:     msg,
			}
		}
		return agent.LimitClassification{Kind: agent.LimitKindNone, Agent: agentName, Message: msg}
	}

	tc.Pool.failOrRetryAgent(testWorkerID, job, "test", "boom MARKER-SESSION-LIMIT")

	assert := assert.New(t)
	assert.True(tc.Pool.isAgentCoolingDown("test"), "agent should be in cooldown")

	// retry_count must NOT have advanced — quota/session errors skip
	// retries entirely (matches the original isQuotaError semantics).
	got, err := tc.DB.GetJobRetryCount(job.ID)
	require.NoError(t, err)
	assert.Equal(0, got, "session-limit error must not consume a retry slot")
}

func TestFailOrRetryInner_UnmatchedAgentErrorLogsWarn(t *testing.T) {
	tc := newWorkerTestContext(t, 1)

	// Capture log output by swapping the standard logger's writer.
	var buf bytes.Buffer
	origOutput := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(origOutput) })

	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJob(t, sha, testWorkerID)

	tc.Pool.classify = func(agentName, msg string) agent.LimitClassification {
		return agent.LimitClassification{Kind: agent.LimitKindNone, Agent: agentName, Message: msg}
	}

	tc.Pool.failOrRetryAgent(testWorkerID, job, "test", "some brand new error wording from a future agent")

	logged := buf.String()
	assert := assert.New(t)
	assert.Contains(logged, "unclassified agent error", "expected WARN line for unmatched error")
	assert.Contains(logged, "from test:", "log line should include agent name as 'from <agent>:'")
	assert.Contains(logged, "some brand new error wording", "log line should include error preview")
}

func TestFailoverOrFail_FailsOverToBackup(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)

	// Configure backup agent
	cfg := config.DefaultConfig()
	cfg.DefaultBackupAgent = "test"
	tc.reconfigurePool(cfg)

	// Enqueue with agent "codex" (backup is "test")
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "codex")
	// Fill in RepoPath so resolveBackupAgent can work
	job.RepoPath = tc.TmpDir

	tc.Pool.failoverOrFail(testWorkerID, job, "codex", "quota exhausted")

	// Should be queued for failover, agent changed to "test"
	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	if updated.Agent != "test" {
		assert.Condition(t, func() bool {
			return false
		}, "agent=%q, want test (failover)", updated.Agent)
	}
}

func TestFailoverOrFail_PassesBackupModel(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)

	// Configure backup agent AND backup model
	cfg := config.DefaultConfig()
	cfg.DefaultBackupAgent = "test"
	cfg.DefaultBackupModel = "claude-sonnet"
	tc.reconfigurePool(cfg)

	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "codex")
	job.RepoPath = tc.TmpDir

	tc.Pool.failoverOrFail(testWorkerID, job, "codex", "quota exhausted")

	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	if updated.Agent != "test" {
		assert.Condition(t, func() bool {
			return false
		}, "agent=%q, want test (failover)", updated.Agent)
	}
	if updated.Model != "claude-sonnet" {
		assert.Condition(t, func() bool {
			return false
		}, "model=%q, want claude-sonnet (backup model)", updated.Model)
	}
}

func TestFailoverOrFail_NoBackupFailsWithQuotaPrefix(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJob(t, sha, testWorkerID)

	// No backup configured — should fail with quota prefix
	tc.Pool.failoverOrFail(testWorkerID, job, "test", "quota exhausted")

	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	if !strings.HasPrefix(updated.Error, review.QuotaErrorPrefix) {
		assert.Condition(t, func() bool {
			return false
		}, "error=%q, want prefix %q", updated.Error, review.QuotaErrorPrefix)
	}
}

func TestFailOrRetryInner_RetryExhaustedBackupInCooldown(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)

	// Configure backup agent
	cfg := config.DefaultConfig()
	cfg.DefaultBackupAgent = "test"
	tc.reconfigurePool(cfg)

	// Enqueue with agent "codex"
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "codex")
	job.RepoPath = tc.TmpDir

	// Exhaust retries
	job = tc.exhaustRetries(t, job, testWorkerID, "codex")

	// Put the backup agent in cooldown
	tc.Pool.cooldownAgent(
		"test", time.Now().Add(30*time.Minute),
	)

	// Final failure — retries exhausted, backup in cooldown
	tc.Pool.failOrRetryInner(
		testWorkerID, job, "codex",
		"connection reset", true,
	)

	// Should be failed, NOT queued for failover to cooled-down agent
	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	// Agent should still be codex (not failed over)
	if updated.Agent != "codex" {
		assert.Condition(t, func() bool {
			return false
		}, "agent=%q, want codex (no failover)", updated.Agent)
	}
}

func TestFailOrRetryInner_RetryExhaustedFailsOverToBackup(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)

	// Configure backup agent
	cfg := config.DefaultConfig()
	cfg.DefaultBackupAgent = "test"
	tc.reconfigurePool(cfg)

	// Enqueue with agent "codex"
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "codex")
	job.RepoPath = tc.TmpDir

	// Exhaust retries
	job = tc.exhaustRetries(t, job, testWorkerID, "codex")

	// Final failure — retries exhausted, backup available
	tc.Pool.failOrRetryInner(
		testWorkerID, job, "codex",
		"connection reset", true,
	)

	// Should be queued for failover, agent changed to "test"
	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	if updated.Agent != "test" {
		assert.Condition(t, func() bool {
			return false
		}, "agent=%q, want test (failover)", updated.Agent)
	}
}

func TestResolveBackupAgent(t *testing.T) {
	tests := []struct {
		name       string
		jobAgent   string
		reviewType string
		config     config.Config
		want       string
	}{
		{
			name:     "no backup configured",
			jobAgent: "test",
			config:   config.Config{},
			want:     "",
		},
		{
			name:     "unknown backup agent",
			jobAgent: "test",
			config: config.Config{
				DefaultBackupAgent: "nonexistent-agent-xyz",
			},
			want: "",
		},
		{
			name:     "backup same as primary",
			jobAgent: "test",
			config: config.Config{
				DefaultBackupAgent: "test",
			},
			want: "",
		},
		{
			name:     "default review type uses review workflow",
			jobAgent: "codex",
			config: config.Config{
				ReviewBackupAgent: "test",
			},
			want: "test",
		},
		{
			name:       "security review type uses security workflow",
			jobAgent:   "codex",
			reviewType: "security",
			config: config.Config{
				SecurityBackupAgent: "test",
			},
			want: "test",
		},
		{
			name:       "design review type uses design workflow",
			jobAgent:   "codex",
			reviewType: "design",
			config: config.Config{
				DesignBackupAgent: "test",
			},
			want: "test",
		},
		{
			name:       "workflow mismatch returns empty",
			jobAgent:   "codex",
			reviewType: "security",
			config: config.Config{
				ReviewBackupAgent: "test",
			},
			want: "",
		},
		{
			name:     "default_backup_agent fallback",
			jobAgent: "codex",
			config: config.Config{
				DefaultBackupAgent: "test",
			},
			want: "test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()

			// Merge test config with defaults
			cfg.DefaultBackupAgent = tt.config.DefaultBackupAgent
			cfg.ReviewBackupAgent = tt.config.ReviewBackupAgent
			cfg.SecurityBackupAgent = tt.config.SecurityBackupAgent
			cfg.DesignBackupAgent = tt.config.DesignBackupAgent

			pool := NewWorkerPool(nil, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil)
			job := &storage.ReviewJob{
				Agent:      tt.jobAgent,
				RepoPath:   t.TempDir(),
				ReviewType: tt.reviewType,
			}

			got := pool.resolveBackupAgent(job)
			if got != tt.want {
				assert.Condition(t, func() bool {
					return false
				}, "resolveBackupAgent() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFailoverWorkflow_FixJob(t *testing.T) {
	// Fix jobs should use "fix" workflow, not "review"
	cfg := config.DefaultConfig()
	cfg.FixBackupAgent = "test"
	cfg.FixBackupModel = "fix-model"

	pool := NewWorkerPool(nil, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil)
	job := &storage.ReviewJob{
		Agent:    "codex",
		RepoPath: t.TempDir(),
		JobType:  storage.JobTypeFix,
		// ReviewType is empty for fix jobs
	}

	gotAgent := pool.resolveBackupAgent(job)
	if gotAgent != "test" {
		assert.Condition(t, func() bool {
			return false
		}, "resolveBackupAgent(fix job) = %q, want %q", gotAgent, "test")
	}

	gotModel := pool.resolveBackupModel(job)
	if gotModel != "fix-model" {
		assert.Condition(t, func() bool {
			return false
		}, "resolveBackupModel(fix job) = %q, want %q", gotModel, "fix-model")
	}
}

func TestFailoverWorkflow_FixJobDoesNotUseReviewBackup(t *testing.T) {
	// A fix job should NOT pick up review_backup_agent/model
	cfg := config.DefaultConfig()
	cfg.ReviewBackupAgent = "test"
	cfg.ReviewBackupModel = "review-model"

	pool := NewWorkerPool(nil, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil)
	job := &storage.ReviewJob{
		Agent:    "codex",
		RepoPath: t.TempDir(),
		JobType:  storage.JobTypeFix,
	}

	gotAgent := pool.resolveBackupAgent(job)
	if gotAgent != "" {
		assert.Condition(t, func() bool {
			return false
		}, "resolveBackupAgent(fix job) = %q, want empty (no fix-specific backup)", gotAgent)
	}

	gotModel := pool.resolveBackupModel(job)
	if gotModel != "" {
		assert.Condition(t, func() bool {
			return false
		}, "resolveBackupModel(fix job) = %q, want empty", gotModel)
	}
}

func TestFailOrRetryInner_RetryExhaustedPassesBackupModel(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)

	cfg := config.DefaultConfig()
	cfg.DefaultBackupAgent = "test"
	cfg.DefaultBackupModel = "backup-model"
	tc.reconfigurePool(cfg)

	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "codex")
	job.RepoPath = tc.TmpDir

	// Exhaust retries
	job = tc.exhaustRetries(t, job, testWorkerID, "codex")

	// Final failure — retries exhausted, backup available
	tc.Pool.failOrRetryInner(
		testWorkerID, job, "codex",
		"connection reset", true,
	)

	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	if updated.Agent != "test" {
		assert.Condition(t, func() bool {
			return false
		}, "agent=%q, want test (failover)", updated.Agent)
	}
	if updated.Model != "backup-model" {
		assert.Condition(t, func() bool {
			return false
		}, "model=%q, want backup-model", updated.Model)
	}
}

func TestAutoClosePassingReviews(t *testing.T) {
	// Not parallel at the outer level: Register/Unregister modify the
	// global agent registry which isn't synchronized. Running this test
	// sequentially ensures no other test reads the registry concurrently.
	// Subtests below are still parallel with each other.
	const passAgentName = "auto-close-pass-agent"
	agent.Register(&agent.FakeAgent{
		NameStr: passAgentName,
		ReviewFn: func(_ context.Context, _, _, _ string, w io.Writer) (string, error) {
			out := "No issues found."
			_, _ = w.Write([]byte(out))
			return out, nil
		},
	})
	t.Cleanup(func() { agent.Unregister(passAgentName) })

	tests := []struct {
		name       string
		enabled    bool
		wantClosed bool
	}{
		{"enabled", true, true},
		{"disabled", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tc := newWorkerTestContext(t, 1)
			cfg := config.DefaultConfig()
			cfg.AutoClosePassingReviews = tt.enabled
			tc.reconfigurePool(cfg)

			sha := testutil.GetHeadSHA(t, tc.TmpDir)
			job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, passAgentName)

			tc.Pool.processJob(testWorkerID, job)

			tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
			review, err := tc.DB.GetReviewByJobID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, tt.wantClosed, review.Closed)
		})
	}

	// Non-review job types must never be auto-closed, even with the setting enabled.
	t.Run("skips_non_review_jobs", func(t *testing.T) {
		t.Parallel()
		tc := newWorkerTestContext(t, 1)
		cfg := config.DefaultConfig()
		cfg.AutoClosePassingReviews = true
		tc.reconfigurePool(cfg)

		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		commit, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, sha, "Author", "Subject", time.Now())
		require.NoError(t, err)
		job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
			RepoID:   tc.Repo.ID,
			CommitID: commit.ID,
			GitRef:   sha,
			Agent:    passAgentName,
			JobType:  "task",
			Prompt:   "test prompt",
		})
		require.NoError(t, err)
		claimed, err := tc.DB.ClaimJob(testWorkerID)
		require.NoError(t, err)
		require.Equal(t, job.ID, claimed.ID)

		tc.Pool.processJob(testWorkerID, claimed)

		tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
		review, err := tc.DB.GetReviewByJobID(job.ID)
		require.NoError(t, err)
		assert.False(t, review.Closed, "task job should not be auto-closed")
	})
}

func TestProcessJob_MinSeverityCascade(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)

	var capturedPrompt string
	agentName := "min-sev-cascade-capture"
	agent.Register(&agent.FakeAgent{
		NameStr: agentName,
		ReviewFn: func(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
			capturedPrompt = prompt
			return "No issues found.", nil
		},
	})
	t.Cleanup(func() { agent.Unregister(agentName) })

	// Set global config with ReviewMinSeverity
	cfg := config.DefaultConfig()
	cfg.ReviewMinSeverity = "medium"
	tc.Pool = NewWorkerPool(tc.DB, NewStaticConfig(cfg), 1, tc.Broadcaster, nil, nil)

	job := tc.createJobWithAgent(t, sha, agentName)
	claimed, err := tc.DB.ClaimJob("test-worker")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)

	tc.Pool.processJob("test-worker", claimed)

	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	assert.Contains(t, capturedPrompt, "Severity filter:")
	assert.Contains(t, capturedPrompt, "SEVERITY_THRESHOLD_MET")
}

func TestProcessJob_MinSeverityJobOverrideWins(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)

	var capturedPrompt string
	agentName := "min-sev-override-capture"
	agent.Register(&agent.FakeAgent{
		NameStr: agentName,
		ReviewFn: func(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
			capturedPrompt = prompt
			return "No issues found.", nil
		},
	})
	t.Cleanup(func() { agent.Unregister(agentName) })

	// Global says "medium" but job says "critical"
	cfg := config.DefaultConfig()
	cfg.ReviewMinSeverity = "medium"
	tc.Pool = NewWorkerPool(tc.DB, NewStaticConfig(cfg), 1, tc.Broadcaster, nil, nil)

	commit, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, sha, "Author", "Subject", time.Now())
	require.NoError(t, err)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID:      tc.Repo.ID,
		CommitID:    commit.ID,
		GitRef:      sha,
		Agent:       agentName,
		MinSeverity: "critical",
	})
	require.NoError(t, err)

	claimed, err := tc.DB.ClaimJob("test-worker")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)

	tc.Pool.processJob("test-worker", claimed)

	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	assert.Contains(t, capturedPrompt, "Critical")
}

// createAndClaimClassifyJob enqueues a classify job and claims it with testWorkerID.
func (c *workerTestContext) createAndClaimClassifyJob(
	t *testing.T, sha, subject, diff string,
) *storage.ReviewJob {
	t.Helper()
	commit, err := c.DB.GetOrCreateCommit(c.Repo.ID, sha, "Author", subject, time.Now())
	require.NoError(t, err)
	job, err := c.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID:      c.Repo.ID,
		CommitID:    commit.ID,
		GitRef:      sha,
		Agent:       "test",
		JobType:     storage.JobTypeClassify,
		ReviewType:  "design",
		DiffContent: diff,
		Prompt:      subject,
	})
	require.NoError(t, err)
	// Mark the job as auto_design source so it participates in dedup.
	_, err = c.DB.Exec(`UPDATE review_jobs SET source = 'auto_design' WHERE id = ?`, job.ID)
	require.NoError(t, err)
	claimed, err := c.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	return claimed
}

func TestWorker_ClassifyJob_Yes_PromotesToDesignReview(t *testing.T) {
	tc := newWorkerTestContext(t, 0)

	job := tc.createAndClaimClassifyJob(t, "feedcafe", "feat: new package", "+lots of new code\n")

	SetTestClassifierVerdict(true, "new package detected")
	t.Cleanup(func() { SetTestClassifierVerdict(false, "") })

	tc.Pool.processJob(testWorkerID, job)

	after, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.Equal("review", after.JobType)
	assert.Equal(storage.JobStatusQueued, after.Status)
	assert.Equal("design", after.ReviewType)
	assert.Equal("auto_design", after.Source, "source preserved across promotion")
	assert.Empty(after.WorkerID, "worker_id cleared so a new worker can claim")
	assert.Nil(after.StartedAt, "started_at cleared")

	var n int
	require.NoError(t, tc.DB.QueryRow(
		`SELECT COUNT(*) FROM review_jobs rj JOIN commits c ON rj.commit_id = c.id
		 WHERE rj.repo_id = ? AND c.sha = ? AND rj.source = 'auto_design'`,
		tc.Repo.ID, "feedcafe").Scan(&n))
	assert.Equal(1, n, "exactly one auto_design row must exist (no second INSERT)")
}

func TestWorker_ClassifyJob_No_MarksSkipped(t *testing.T) {
	tc := newWorkerTestContext(t, 0)

	job := tc.createAndClaimClassifyJob(t, "beefc0de", "fix: local rename", "+x\n")

	SetTestClassifierVerdict(false, "local rename only")
	t.Cleanup(func() { SetTestClassifierVerdict(false, "") })

	tc.Pool.processJob(testWorkerID, job)

	after, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.Equal("review", after.JobType, "job_type flipped from classify to review")
	assert.Equal(storage.JobStatusSkipped, after.Status)
	assert.Equal("design", after.ReviewType)
	assert.Equal("auto_design", after.Source)
	assert.Equal("local rename only", after.SkipReason)

	var n int
	require.NoError(t, tc.DB.QueryRow(
		`SELECT COUNT(*) FROM review_jobs rj JOIN commits c ON rj.commit_id = c.id
		 WHERE rj.repo_id = ? AND c.sha = ? AND rj.source = 'auto_design'`,
		tc.Repo.ID, "beefc0de").Scan(&n))
	assert.Equal(1, n, "exactly one auto_design row must exist (no second INSERT)")
}
