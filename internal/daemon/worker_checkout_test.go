package daemon

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestPrepareJobCheckoutIsolationPolicy(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile("file.txt", "committed\n", "initial")
	source := filepath.Join(t.TempDir(), "source")
	repo.Run("worktree", "add", "--detach", source, head)
	for _, tt := range []struct {
		name     string
		global   bool
		local    string
		jobType  string
		dirty    bool
		isolated bool
	}{
		{name: "default", jobType: storage.JobTypeReview},
		{name: "global enabled", global: true, jobType: storage.JobTypeReview, isolated: true},
		{name: "repo enabled", local: "isolate_reviews = true", jobType: storage.JobTypeRange, isolated: true},
		{name: "repo disabled", global: true, local: "isolate_reviews = false", jobType: storage.JobTypeReview},
		{name: "synthesis", global: true, jobType: storage.JobTypeSynthesis, isolated: true},
		{name: "dirty review", global: true, jobType: storage.JobTypeDirty, dirty: true},
		{name: "dirty synthesis", global: true, jobType: storage.JobTypeSynthesis, dirty: true},
		{name: "task", global: true, jobType: storage.JobTypeTask},
		{name: "compact", global: true, jobType: storage.JobTypeCompact},
		{name: "fix", global: true, jobType: storage.JobTypeFix},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			cfg := config.DefaultConfig()
			require.NoError(t, config.SetConfigValue(cfg, "isolate_reviews", strconv.FormatBool(tt.global)))
			require.NoError(t, os.WriteFile(filepath.Join(source, ".roborev.toml"), []byte(tt.local), 0o600))
			pool := NewWorkerPool(nil, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil)
			job := &storage.ReviewJob{RepoPath: repo.Path(), WorktreePath: source, GitRef: head, JobType: tt.jobType}
			if tt.dirty {
				job.GitRef = "dirty"
			}
			checkout, err := pool.prepareJobCheckout(context.Background(), testWorkerID, job, cfg)
			require.NoError(t, err)
			if checkout.cleanup != nil {
				t.Cleanup(checkout.cleanup)
			}
			assert.Equal(source, checkout.eventWorktreePath)
			assert.Equal(source, checkout.promptRepoPath)
			if tt.isolated {
				assert.NotEqual(source, checkout.agentRepoPath)
				assert.Equal(head, strings.TrimSpace(repo.Run("-C", checkout.agentRepoPath, "rev-parse", "HEAD")))
				assert.Equal(checkout.agentRepoPath, checkout.snapshotTarget.RepoPath)
			} else {
				assert.Equal(source, checkout.agentRepoPath)
			}
		})
	}
}

func TestIsolatedCheckoutFailureDoesNotUseSource(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	tc := newWorkerTestContext(t, 1)
	cfg := config.DefaultConfig()
	cfg.IsolateReviews = true
	checkout, err := tc.Pool.prepareJobCheckout(context.Background(), testWorkerID, &storage.ReviewJob{
		RepoPath: tc.TmpDir, GitRef: "missing-ref", JobType: storage.JobTypeReview,
	}, cfg)
	require.Error(t, err)
	assert.Empty(t, checkout.agentRepoPath)
}

func TestBareBackedIsolatedCheckoutCleanup(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := testutil.NewTestRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "repo.git")
	repo.Run("clone", "--bare", repo.Path(), bare)
	for _, stale := range []bool{false, true} {
		t.Run(strconv.FormatBool(stale), func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "source")
			repo.Run("-C", bare, "worktree", "add", "--detach", source, "HEAD")
			cfg := config.DefaultConfig()
			cfg.IsolateReviews = true
			pool := NewWorkerPool(nil, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil)
			// Enqueue registers the checkout itself for a bare-backed worktree.
			checkout, err := pool.prepareJobCheckout(context.Background(), testWorkerID, &storage.ReviewJob{
				RepoPath: source, GitRef: repo.HeadSHA(), JobType: storage.JobTypeReview,
			}, cfg)
			require.NoError(t, err)
			require.NotNil(t, checkout.cleanup)
			repo.Run("-C", bare, "worktree", "remove", source)
			if stale {
				require.NoError(t, cleanupStaleCIWorktrees(context.Background()))
			} else {
				checkout.cleanup()
			}
			assert.NoDirExists(t, checkout.agentRepoPath)
			list := repo.Run("-C", bare, "worktree", "list", "--porcelain")
			assert.NotContains(t, normalizeGitListOutput(list), normalizeGitListPath(t, checkout.agentRepoPath),
				"cleanup must also remove the detached checkout's Git registration")
		})
	}
}

func TestWorkerIsolatedReviewSurvivesWorktreeRemoval(t *testing.T) {
	for _, outcome := range []string{"commit", "range", "retry", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			assert := assert.New(t)
			tc := newWorkerTestContext(t, 1)
			base := tc.GitRepo.CommitFile(".roborev.toml", "isolate_reviews = true\nmax_prompt_size = 4096\nsnapshot_dir = '.review-inputs'\n", "review settings")
			content := strings.Repeat("reviewed content\n", 600)
			head := tc.GitRepo.CommitFile("file.txt", content, "review target")
			source := filepath.Join(t.TempDir(), "source")
			tc.GitRepo.Run("worktree", "add", "--detach", source, head)
			tc.GitRepo.CommitFile("file.txt", "newer checkout\n", "advance main checkout")
			require.NoError(t, os.WriteFile(filepath.Join(source, "file.txt"), []byte("local edits\n"), 0o600))
			gitRef := head
			if outcome == "range" {
				gitRef = base + ".." + head
			}
			var job *storage.ReviewJob
			var paths []string
			const agentName = "isolated-review-reader"
			agent.RegisterForTest(t, &agent.FakeAgent{
				NameStr: agentName,
				ReviewFn: func(ctx context.Context, repoPath, ref, reviewPrompt string, _ io.Writer) (string, error) {
					paths = append(paths, repoPath)
					require.NotEqual(t, source, repoPath, "review must release the caller's checkout")
					require.NotEqual(t, tc.TmpDir, repoPath, "review must use an exact detached checkout")
					assert.Equal(gitRef, ref)
					if len(paths) == 1 {
						tc.GitRepo.Run("worktree", "remove", "--force", source)
					}
					cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
					cmd.Dir = repoPath
					out, err := cmd.Output()
					require.NoError(t, err, "agent cwd must remain usable after source removal")
					assert.Equal(head, strings.TrimSpace(string(out)))
					data, err := os.ReadFile(filepath.Join(repoPath, "file.txt"))
					require.NoError(t, err)
					assert.Equal(content, string(data))
					assert.Contains(reviewPrompt, "Read the complete task prompt")
					snapshots, err := filepath.Glob(filepath.Join(repoPath, ".review-inputs", "*", "prompt.md"))
					require.NoError(t, err)
					require.Len(t, snapshots, 1)
					data, err = os.ReadFile(snapshots[0])
					require.NoError(t, err)
					assert.Contains(string(data), "+reviewed content")
					if outcome == "cancel" {
						require.NoError(t, tc.DB.CancelJob(job.ID))
						require.True(t, tc.Pool.CancelJob(job.ID))
						return "", ctx.Err()
					}
					if outcome == "retry" && len(paths) == 1 {
						return "", errors.New("connection reset by peer")
					}
					return string(testutil.ReviewFixtureJSON("No issues found.")), nil
				},
			})
			jobType := storage.JobTypeReview
			if outcome == "range" {
				jobType = storage.JobTypeRange
			}
			_, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
				RepoID: tc.Repo.ID, GitRef: gitRef, Agent: agentName,
				JobType: jobType, WorktreePath: source,
			})
			require.NoError(t, err)
			job, err = tc.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, job)
			_, events := tc.Broadcaster.Subscribe("")
			tc.Pool.processJob(testWorkerID, job)
			require.Len(t, paths, 1)
			assert.NoDirExists(paths[0])
			require.NotEmpty(t, events, "expected review.started event")
			started := <-events
			assert.Equal("review.started", started.Type)
			assert.Equal(source, started.WorktreePath)
			if outcome == "retry" {
				tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
				job, err = tc.DB.ClaimJob(testWorkerID)
				require.NoError(t, err)
				require.NotNil(t, job)
				tc.Pool.processJob(testWorkerID, job)
				require.Len(t, paths, 2)
				assert.NotEqual(paths[0], paths[1])
				assert.NoDirExists(paths[1])
			}
			stored, err := tc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(source, stored.WorktreePath, "stored identity must remain the source worktree")
			if outcome == "cancel" {
				assert.Equal(storage.JobStatusCanceled, stored.Status)
			} else {
				assert.Equal(storage.JobStatusDone, stored.Status)
			}
		})
	}
}
