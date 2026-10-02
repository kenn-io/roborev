package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	promptpkg "go.kenn.io/roborev/internal/prompt"
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

func TestPrepareLocalReviewCheckout(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := testutil.NewGitRepo(t)
	base := repo.CommitFile("file.txt", "base\n", "base")
	head := repo.CommitFile("file.txt", "reviewed\n", "head")
	repo.Checkout("--detach", base)
	caller := filepath.Join(t.TempDir(), "caller")
	repo.Run("worktree", "add", "--detach", caller, head)
	commitID := int64(1)
	for _, tc := range []struct {
		name, kind, ref, repoConfig string
		global, isolate             bool
		legacy                      bool
	}{
		{name: "default", kind: storage.JobTypeReview, ref: head},
		{name: "global enabled", kind: storage.JobTypeReview, ref: head, global: true, isolate: true},
		{name: "repo enabled", kind: storage.JobTypeReview, ref: head, repoConfig: "review_in_worktree=true", isolate: true},
		{name: "repo disabled", kind: storage.JobTypeReview, ref: head, global: true, repoConfig: "review_in_worktree=false"},
		{name: "range", kind: storage.JobTypeRange, ref: base + ".." + head, global: true, isolate: true},
		{name: "legacy commit", ref: head, global: true, isolate: true, legacy: true},
		{name: "committed synthesis", kind: storage.JobTypeSynthesis, ref: head, global: true, isolate: true, legacy: true},
		{name: "range synthesis", kind: storage.JobTypeSynthesis, ref: base + ".." + head, global: true, isolate: true},
		{name: "dirty synthesis", kind: storage.JobTypeSynthesis, ref: "dirty", global: true},
		{name: "prompt synthesis", kind: storage.JobTypeSynthesis, ref: "analysis", global: true},
		{name: "dirty", kind: storage.JobTypeDirty, ref: "dirty", global: true},
		{name: "task", kind: storage.JobTypeTask, ref: "analysis", global: true},
		{name: "compact", kind: storage.JobTypeCompact, ref: "compact", global: true},
		{name: "fix", kind: storage.JobTypeFix, ref: head, global: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(caller, ".roborev.toml")
			require.NoError(t, os.WriteFile(path, []byte(tc.repoConfig), 0o600))
			cfg := config.DefaultConfig()
			cfg.ReviewInWorktree = &tc.global
			pool := NewWorkerPool(nil, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil)
			job := &storage.ReviewJob{ID: 42, RepoPath: repo.Path(), WorktreePath: caller, GitRef: tc.ref, JobType: tc.kind}
			if tc.legacy {
				job.CommitID = &commitID
			}
			checkout, err := pool.prepareJobCheckout(t.Context(), testWorkerID, job, cfg)
			require.NoError(t, err)
			cleanup := checkout.cleanup
			t.Cleanup(func() {
				if cleanup != nil {
					cleanup()
				}
			})
			assert.Equal(t, caller, checkout.eventWorktreePath)
			if !tc.isolate {
				assert.Equal(t, caller, checkout.promptRepoPath)
				assert.Equal(t, caller, checkout.configRepoPath)
				assert.Equal(t, caller, checkout.agentRepoPath)
				return
			}
			require.NotEqual(t, caller, checkout.agentRepoPath)
			assert.Equal(t, checkout.agentRepoPath, checkout.promptRepoPath)
			assert.Equal(t, caller, checkout.configRepoPath)
			cmd := exec.Command("git", "rev-parse", "HEAD")
			cmd.Dir = checkout.agentRepoPath
			output, err := cmd.Output()
			require.NoError(t, err)
			assert.Equal(t, head, strings.TrimSpace(string(output)))
			assert.Equal(t, checkout.agentRepoPath, checkout.snapshotTarget.RepoPath)
			assert.Equal(t, caller, checkout.snapshotTarget.ConfigRepoPath)
			cleanup()
			cleanup = nil
			assert.NoDirExists(t, checkout.agentRepoPath)
		})
	}
	t.Run("invalid ref fails without fallback", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.ReviewInWorktree = new(true)
		pool := NewWorkerPool(nil, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil)
		_, err := pool.prepareJobCheckout(t.Context(), testWorkerID, &storage.ReviewJob{RepoPath: repo.Path(), GitRef: strings.Repeat("a", 40), JobType: storage.JobTypeReview}, cfg)
		require.Error(t, err)
		dirs, err := staleCIWorktreeDirs(ciWorktreeParentDir())
		require.NoError(t, err)
		assert.Empty(t, dirs)
	})
	t.Run("canceled checkout", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.ReviewInWorktree = new(true)
		pool := NewWorkerPool(nil, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := pool.prepareJobCheckout(ctx, testWorkerID, &storage.ReviewJob{RepoPath: repo.Path(), GitRef: head, JobType: storage.JobTypeReview}, cfg)
		require.Error(t, err)
		dirs, err := staleCIWorktreeDirs(ciWorktreeParentDir())
		require.NoError(t, err)
		assert.Empty(t, dirs)
	})
	t.Run("invalid config fails", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(caller, ".roborev.toml"), []byte("review_in_worktree=["), 0o600))
		cfg := config.DefaultConfig()
		pool := NewWorkerPool(nil, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil)
		_, err := pool.prepareJobCheckout(t.Context(), testWorkerID, &storage.ReviewJob{RepoPath: repo.Path(), WorktreePath: caller, GitRef: head, JobType: storage.JobTypeReview}, cfg)
		require.Error(t, err)
	})
}

func TestWorkerIsolatedReviewSurvivesCallerRemoval(t *testing.T) {
	for _, prebuilt := range []bool{false, true} {
		name := "worker prompt"
		if prebuilt {
			name = "prebuilt prompt"
		}
		t.Run(name, func(t *testing.T) {
			testIsolatedReviewCallerRemoval(t, prebuilt)
		})
	}
}

const agentNameForCheckoutTest = "isolated-checkout-reader"

func testIsolatedReviewCallerRemoval(t *testing.T, prebuilt bool) {
	t.Helper()
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	tc := newWorkerTestContext(t, 1)
	cfg := tc.Pool.cfgGetter.Config()
	cfg.ReviewInWorktree = new(true)
	cfg.DefaultMaxPromptSize = 10000
	cfg.ReviewContextCount = 3
	base := tc.GitRepo.CommitFile("marker.txt", "old\n", "base")
	first := tc.GitRepo.CommitFile("first.txt", "first\n", "first")
	prior, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: base + ".." + first, JobType: storage.JobTypeRange, Agent: "test"})
	require.NoError(t, err)
	_, err = tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NoError(t, testutil.CompleteReviewFixture(tc.DB, prior.ID, "test", "prior prompt", "earlier range finding"))
	head := tc.GitRepo.CommitFile("marker.txt", "reviewed\n"+strings.Repeat("large diff line\n", 2000), "head")
	tc.GitRepo.Checkout("--detach", base)
	caller := filepath.Join(t.TempDir(), "caller")
	tc.GitRepo.Run("worktree", "add", "--detach", caller, head)
	require.NoError(t, os.WriteFile(filepath.Join(caller, ".roborev.toml"), []byte("snapshot_dir='review-snapshots'"), 0o600))
	var storedPrompt string
	if prebuilt {
		storedPrompt, err = promptpkg.NewBuilder(tc.DB).ForRepo(caller, tc.Repo.ID).Build(base+".."+head, 3, agentNameForCheckoutTest, "", "")
		require.NoError(t, err)
		storedPrompt += "\nRead diff: " + promptpkg.DiffFilePathPlaceholder
	}
	started := make(chan string, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	var snapshotPath string
	const agentName = agentNameForCheckoutTest
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: agentName, ReviewFn: func(ctx context.Context, path, ref, prompt string, out io.Writer) (string, error) {
		// A live subprocess waits with cwd in the agent checkout while the caller
		// removes its linked worktree. No real AI agent is involved.
		cmd := exec.CommandContext(ctx, "git", "cat-file", "--batch")
		cmd.Dir = path
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return "", err
		}
		if err = cmd.Start(); err != nil {
			return "", err
		}
		started <- path
		<-release
		_ = stdin.Close()
		if err = cmd.Wait(); err != nil {
			return "", err
		}
		data, err := os.ReadFile(filepath.Join(path, "marker.txt"))
		if err != nil {
			return "", err
		}
		if !strings.HasPrefix(string(data), "reviewed\n") {
			return "", fmt.Errorf("wrong checkout content")
		}
		files, err := filepath.Glob(filepath.Join(path, "review-snapshots", "*", "prompt.md"))
		if err != nil {
			return "", err
		}
		if len(files) != 1 {
			return "", fmt.Errorf("expected one prompt snapshot, got %d", len(files))
		}
		snapshotPath = files[0]
		data, err = os.ReadFile(snapshotPath)
		if err != nil {
			return "", err
		}
		if !strings.Contains(string(data), "large diff line") {
			return "", fmt.Errorf("snapshot missing reviewed diff")
		}
		priorFiles, err := filepath.Glob(filepath.Join(path, "review-snapshots", "*", "prior-range-reviews.xml"))
		if err != nil {
			return "", fmt.Errorf("find prior review snapshot: %w", err)
		}
		if len(priorFiles) != 1 {
			return "", fmt.Errorf("expected one prior review snapshot, got %d", len(priorFiles))
		}
		priorData, err := os.ReadFile(priorFiles[0])
		if err != nil {
			return "", err
		}
		if !strings.Contains(string(priorData), "earlier range finding") {
			return "", fmt.Errorf("prior snapshot missing finding")
		}
		if prebuilt {
			diffFiles, err := filepath.Glob(filepath.Join(path, "review-snapshots", "*", "*.diff"))
			if err != nil {
				return "", fmt.Errorf("find diff snapshot: %w", err)
			}
			if len(diffFiles) != 1 {
				return "", fmt.Errorf("expected one diff snapshot, got %d", len(diffFiles))
			}
			diff, err := os.ReadFile(diffFiles[0])
			if err != nil {
				return "", err
			}
			if !strings.Contains(string(diff), "large diff line") {
				return "", fmt.Errorf("diff snapshot missing reviewed content")
			}
		}
		return string(testutil.ReviewFixtureJSON("No issues found.")), nil
	}})
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: base + ".." + head, JobType: storage.JobTypeRange, Agent: agentName, WorktreePath: caller, Prompt: storedPrompt, PromptPrebuilt: prebuilt})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	_, events := tc.Broadcaster.Subscribe("")
	go func() { defer close(done); tc.Pool.processJob(testWorkerID, claimed) }()
	// Release before waiting for the worker in cleanup, even on failed assertions.
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Minute):
		}
	})
	var path string
	select {
	case path = <-started:
	case <-done:

	case <-time.After(2 * time.Minute):

	}
	require.NotEmpty(t, path, "agent must start before the caller is removed")
	require.NotEqual(t, caller, path)
	// Remove local untracked config too, just as an ephemeral task checkout is removed.
	tc.GitRepo.Run("worktree", "remove", "--force", caller)
	close(release)
	finished := false
	select {
	case <-done:
		finished = true
	case <-time.After(2 * time.Minute):
	}
	require.True(t, finished, "worker must finish after release")
	completed, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	require.Equal(t, storage.JobStatusDone, completed.Status)
	assert.Equal(t, caller, completed.WorktreePath)
	assert.NoDirExists(t, path)
	assert.NoFileExists(t, snapshotPath)
	for range 2 {
		var event Event
		select {
		case event = <-events:
		case <-time.After(5 * time.Second):
		}
		require.NotEmpty(t, event.Type, "expected lifecycle event")
		assert.Equal(t, caller, event.WorktreePath)
	}
}

func TestWorkerIsolatedReviewStartsFreshSession(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	tc := newWorkerTestContext(t, 1)
	tc.Pool.cfgGetter.Config().ReviewInWorktree = new(true)
	sha := tc.GitRepo.HeadSHA()
	job := tc.createJob(t, sha)
	source := uuid.New()
	_, err := tc.DB.Exec("UPDATE review_jobs SET session_id='prior-session',session_resumed=1,resume_source_job_uuid=? WHERE id=?", source, job.ID)
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	tc.Pool.processJob(testWorkerID, claimed)
	got, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusDone, got.Status)
	require.NotEmpty(t, got.SessionID)
	assert.NotEqual(t, "prior-session", got.SessionID)
	assert.Nil(t, got.ResumeSourceJobUUID)
	var resumed int
	require.NoError(t, tc.DB.QueryRow("SELECT session_resumed FROM review_jobs WHERE id=?", job.ID).Scan(&resumed))
	assert.Zero(t, resumed)
}

func TestLocalReviewCheckoutRetryAfterCallerRemoval(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := testutil.NewGitRepo(t)
	sha := repo.CommitFile("file.txt", "reviewed\n", "head")
	caller := filepath.Join(t.TempDir(), "caller")
	repo.Run("worktree", "add", "--detach", caller, sha)
	cfg := config.DefaultConfig()
	cfg.ReviewInWorktree = new(true)
	pool := NewWorkerPool(nil, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil)
	job := &storage.ReviewJob{ID: 42, RepoPath: repo.Path(), WorktreePath: caller, GitRef: sha, JobType: storage.JobTypeReview}
	first, err := pool.prepareJobCheckout(t.Context(), testWorkerID, job, cfg)
	require.NoError(t, err)
	require.NotNil(t, first.cleanup)
	t.Cleanup(first.cleanup)
	repo.Run("worktree", "remove", caller)
	retry, err := pool.prepareJobCheckout(t.Context(), testWorkerID, job, cfg)
	require.NoError(t, err)
	require.NotNil(t, retry.cleanup)
	t.Cleanup(retry.cleanup)
	assert.NotEqual(t, first.agentRepoPath, retry.agentRepoPath)
	assert.Equal(t, retry.agentRepoPath, retry.promptRepoPath)
	assert.Equal(t, repo.Path(), retry.configRepoPath)
	assert.Equal(t, caller, retry.eventWorktreePath)
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("review_in_worktree=false"), 0o600))
	disabled, err := pool.prepareJobCheckout(t.Context(), testWorkerID, job, cfg)
	require.NoError(t, err)
	assert.Equal(t, repo.Path(), disabled.agentRepoPath)
	assert.Nil(t, disabled.cleanup)
	assert.Equal(t, caller, disabled.eventWorktreePath)
}

func TestIsolatedReviewPromptBuildSurvivesCallerRemoval(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := testutil.NewGitRepo(t)
	base := repo.CommitFile("base.txt", "base\n", "base")
	repo.CommitFile("excluded.txt", "content that should be excluded\n", "excluded")
	head := repo.CommitFile("reviewed.txt", "owned checkout content\n", "reviewed")
	repo.Checkout("--detach", base)
	caller := filepath.Join(t.TempDir(), "caller")
	repo.Run("worktree", "add", "--detach", caller, head)
	require.NoError(t, os.WriteFile(
		filepath.Join(repo.Path(), ".roborev.toml"),
		[]byte("exclude_patterns=['excluded.txt']\nmax_prompt_size=64\nsnapshot_dir='review-snapshots'\n"),
		0o600,
	))

	cfg := config.DefaultConfig()
	cfg.ReviewInWorktree = new(true)
	cfg.DefaultMaxPromptSize = 4096
	pool := NewWorkerPool(nil, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil)
	job := &storage.ReviewJob{
		ID:           42,
		RepoPath:     repo.Path(),
		WorktreePath: caller,
		GitRef:       base + ".." + head,
		JobType:      storage.JobTypeRange,
	}
	checkout, err := pool.prepareJobCheckout(t.Context(), testWorkerID, job, cfg)
	require.NoError(t, err)
	require.NotEqual(t, caller, checkout.agentRepoPath)
	t.Cleanup(checkout.cleanup)

	repo.Run("worktree", "remove", "--force", caller)
	checkout = pool.refreshIsolatedReviewConfigPath(testWorkerID, checkout, job)
	assert.Equal(t, repo.Path(), checkout.configRepoPath)
	builder, err := pool.promptBuilderForJob(t.Context(), checkout, job, cfg)
	require.NoError(t, err)
	got, err := builder.Build(job.GitRef, 0, "test", "", "")
	require.NoError(t, err, "prompt construction should use the owned checkout after the caller is removed")
	assert.Contains(t, got, "owned checkout content")
	assert.NotContains(t, got, "content that should be excluded")
	prepared, err := builder.Prepare(got, checkout.snapshotTarget)
	require.NoError(t, err)
	require.NotEmpty(t, prepared.FilePath, "repo prompt size should come from the fallback config repo")
	require.NotNil(t, prepared.Cleanup)
	t.Cleanup(prepared.Cleanup)
	assert.True(t, strings.HasPrefix(
		prepared.FilePath,
		filepath.Join(checkout.agentRepoPath, "review-snapshots"),
	))
	snapshot, err := os.ReadFile(prepared.FilePath)
	require.NoError(t, err)
	assert.Contains(t, string(snapshot), "owned checkout content")
}

func TestIsolatedReviewCleanupUsesCallerCheckout(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile("reviewed.txt", "reviewed\n", "reviewed")
	caller := filepath.Join(t.TempDir(), "caller")
	repo.Run("worktree", "add", "--detach", caller, head)

	cfg := config.DefaultConfig()
	cfg.ReviewInWorktree = new(true)
	pool := NewWorkerPool(nil, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil)
	job := &storage.ReviewJob{
		RepoID:       42,
		RepoPath:     repo.Path(),
		WorktreePath: caller,
		GitRef:       head,
		JobType:      storage.JobTypeReview,
	}
	checkout, err := pool.prepareJobCheckout(t.Context(), testWorkerID, job, cfg)
	require.NoError(t, err)
	t.Cleanup(checkout.cleanup)

	staleSnapshot := filepath.Join(caller, ".roborev", "roborev-snapshot-stale")
	require.NoError(t, os.MkdirAll(staleSnapshot, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(staleSnapshot, ".roborev-snapshot"), []byte("snapshot\n"), 0o600))
	oldTime := time.Now().Add(-2 * promptpkg.DefaultStaleSnapshotAge)
	require.NoError(t, os.Chtimes(staleSnapshot, oldTime, oldTime))

	builder := pool.basePromptBuilderForJob(t.Context(), checkout, job, cfg)
	require.NoError(t, cleanupStaleSnapshotsForJob(builder, checkout, job.RepoID))
	assert.NoDirExists(t, staleSnapshot)
}

func TestLocalReviewEventsPreserveRemovedCaller(t *testing.T) {
	pool := NewWorkerPool(nil, NewStaticConfig(config.DefaultConfig()), 1, NewBroadcaster(), nil, nil)
	_, events := pool.broadcaster.Subscribe("")
	caller := filepath.Join(t.TempDir(), "removed-caller")
	job := &storage.ReviewJob{ID: 42, RepoPath: t.TempDir(), WorktreePath: caller, GitRef: "head", JobType: storage.JobTypeReview}
	canceled := eventForJob("review.canceled", job, job.ID)
	assert.Equal(t, caller, canceled.WorktreePath)
	pool.broadcastFailed(job, "test", "provider unavailable")
	failed := <-events
	assert.Equal(t, caller, failed.WorktreePath)
	job.Source = storage.JobSourceCI
	assert.Empty(t, eventForJob("review.canceled", job, job.ID).WorktreePath)
}
