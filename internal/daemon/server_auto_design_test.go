package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func newAutoDesignTestServer(t *testing.T) (*Server, *storage.Repo) {
	t.Helper()
	db, repoPath := testutil.OpenTestDBWithDir(t)
	testutil.InitTestGitRepo(t, repoPath)

	cfg := config.DefaultConfig()
	srv := NewServer(db, cfg, "")

	repo, err := db.GetOrCreateRepo(repoPath)
	require.NoError(t, err)
	return srv, repo
}

func enableAutoDesignReviewForRepo(t *testing.T, repoPath string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, ".roborev.toml"),
		[]byte(`[auto_design_review]
enabled = true
`), 0o644))
}

func enableHookAutoDesignReviewForRepo(t *testing.T, repoPath string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, ".roborev.toml"),
		[]byte(`[auto_design_review]
hook_enabled = true
`), 0o644))
}

func TestResolveDesignAgentGenericDefaultAgentCanAutoDetect(t *testing.T) {
	t.Setenv("PATH", "")
	agent.Register(&agent.FakeAgent{NameStr: "local-auto-design"})
	t.Cleanup(func() { agent.Unregister("local-auto-design") })

	cfg := config.DefaultConfig()
	cfg.DefaultAgent = "claude-code"
	designAgent, designModel := resolveDesignAgent(t.TempDir(), cfg)

	assert.Equal(t, "local-auto-design", designAgent)
	assert.Empty(t, designModel)
}

func TestMaybeDispatchAutoDesign_HeuristicTrigger(t *testing.T) {
	srv, repo := newAutoDesignTestServer(t)
	enableAutoDesignReviewForRepo(t, repo.RootPath)

	commit, err := srv.db.GetOrCreateCommit(repo.ID, "feedcafe", "Author", "refactor: rework auth", time.Now())
	require.NoError(t, err)

	// Use a message-pattern trigger so we don't depend on git ChangedFiles
	// (which requires a real SHA in the test repo).
	diff := "+x\n+y\n+a\n+b\n+c\n+d\n+e\n+f\n+g\n+h\n+i\n+j\n+k\n+l\n"
	parent := &storage.ReviewJob{
		ID:            999,
		RepoID:        repo.ID,
		CommitID:      &commit.ID,
		GitRef:        "feedcafe",
		Agent:         "test",
		JobType:       storage.JobTypeReview,
		ReviewType:    "",
		Status:        storage.JobStatusQueued,
		EnqueuedAt:    time.Now(),
		RepoPath:      repo.RootPath,
		CommitSubject: "refactor: rework auth",
		DiffContent:   &diff,
	}

	require.NoError(t, srv.maybeDispatchAutoDesign(context.Background(), parent))

	queued, err := srv.db.ListJobsByStatus(repo.ID, storage.JobStatusQueued)
	require.NoError(t, err)
	var found *storage.ReviewJob
	for i := range queued {
		j := queued[i]
		if j.GitRef == "feedcafe" && j.ReviewType == "design" && j.Source == "auto_design" {
			found = &j
			break
		}
	}
	require.NotNil(t, found, "expected an auto_design design row")
	assert.Equal(t, "review", found.JobType, "direct heuristic trigger enqueues a design review, not a classify")
}

func TestMaybeDispatchAutoDesign_HeuristicUsesThoroughDesignAgentConfig(t *testing.T) {
	srv, repo := newAutoDesignTestServer(t)

	const primaryAgent = "local-design-thorough-primary"
	agent.Register(&unavailableSynthesisCommandAgent{
		name:    primaryAgent,
		command: "roborev-missing-local-design-thorough-primary",
	})
	t.Cleanup(func() { agent.Unregister(primaryAgent) })
	agent.Register(&agent.FakeAgent{NameStr: "local-design-auto-detect"})
	t.Cleanup(func() { agent.Unregister("local-design-auto-detect") })
	t.Setenv("PATH", "")

	require.NoError(t, os.WriteFile(filepath.Join(repo.RootPath, ".roborev.toml"), []byte(`
design_agent_thorough = "local-design-thorough-primary"
design_model_thorough = "local-thorough-model"

[auto_design_review]
enabled = true
`), 0o644))

	commit, err := srv.db.GetOrCreateCommit(repo.ID, "decafbad", "Author", "refactor: rework api", time.Now())
	require.NoError(t, err)
	diff := "+x\n+y\n+a\n+b\n+c\n+d\n+e\n+f\n+g\n+h\n+i\n+j\n+k\n+l\n"
	parent := &storage.ReviewJob{
		ID:            1000,
		RepoID:        repo.ID,
		CommitID:      &commit.ID,
		GitRef:        "decafbad",
		Agent:         "test",
		JobType:       storage.JobTypeReview,
		Status:        storage.JobStatusQueued,
		EnqueuedAt:    time.Now(),
		RepoPath:      repo.RootPath,
		CommitSubject: "refactor: rework api",
		DiffContent:   &diff,
	}

	require.NoError(t, srv.maybeDispatchAutoDesign(context.Background(), parent))

	queued, err := srv.db.ListJobsByStatus(repo.ID, storage.JobStatusQueued)
	require.NoError(t, err)
	var found *storage.ReviewJob
	for i := range queued {
		j := queued[i]
		if j.GitRef == "decafbad" && j.ReviewType == "design" && j.Source == "auto_design" {
			found = &j
			break
		}
	}
	require.NotNil(t, found, "expected an auto_design design row")
	full, err := srv.db.GetJobByID(found.ID)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.Equal(primaryAgent, full.Agent)
	assert.Equal("local-thorough-model", full.Model)
	assert.Equal("thorough", full.Reasoning)
}

func TestMaybeDispatchAutoDesign_HeuristicSkip_TrivialDiff(t *testing.T) {
	srv, repo := newAutoDesignTestServer(t)
	enableAutoDesignReviewForRepo(t, repo.RootPath)

	commit, err := srv.db.GetOrCreateCommit(repo.ID, "beefc0de", "Author", "fix: tiny", time.Now())
	require.NoError(t, err)
	tiny := "+x\n"
	parent := &storage.ReviewJob{
		ID:            999,
		RepoID:        repo.ID,
		CommitID:      &commit.ID,
		GitRef:        "beefc0de",
		Agent:         "test",
		JobType:       storage.JobTypeReview,
		Status:        storage.JobStatusQueued,
		EnqueuedAt:    time.Now(),
		RepoPath:      repo.RootPath,
		CommitSubject: "fix: tiny",
		DiffContent:   &tiny,
	}

	require.NoError(t, srv.maybeDispatchAutoDesign(context.Background(), parent))

	skipped, err := srv.db.ListJobsByStatus(repo.ID, storage.JobStatusSkipped)
	require.NoError(t, err)
	var found *storage.ReviewJob
	for i := range skipped {
		j := skipped[i]
		if j.GitRef == "beefc0de" && j.ReviewType == "design" && j.Source == "auto_design" {
			found = &j
			break
		}
	}
	require.NotNil(t, found, "expected a skipped auto_design design row")
	assert.Contains(t, found.SkipReason, "trivial")
}

func TestMaybeDispatchAutoDesign_HookEnabledRequiresPostCommitSource(t *testing.T) {
	srv, repo := newAutoDesignTestServer(t)
	enableHookAutoDesignReviewForRepo(t, repo.RootPath)

	commit, err := srv.db.GetOrCreateCommit(repo.ID, "cafebabe", "Author", "refactor: manual", time.Now())
	require.NoError(t, err)
	diff := "+x\n+y\n+a\n+b\n+c\n+d\n+e\n+f\n+g\n+h\n+i\n+j\n"
	parent := &storage.ReviewJob{
		ID:            999,
		RepoID:        repo.ID,
		CommitID:      &commit.ID,
		GitRef:        "cafebabe",
		Agent:         "test",
		JobType:       storage.JobTypeReview,
		Status:        storage.JobStatusQueued,
		EnqueuedAt:    time.Now(),
		RepoPath:      repo.RootPath,
		CommitSubject: "refactor: manual",
		DiffContent:   &diff,
	}

	require.NoError(t, srv.maybeDispatchAutoDesign(context.Background(), parent))

	for _, s := range []storage.JobStatus{storage.JobStatusQueued, storage.JobStatusSkipped} {
		jobs, err := srv.db.ListJobsByStatus(repo.ID, s)
		require.NoError(t, err)
		for _, j := range jobs {
			assert.NotEqual(t, "auto_design", j.Source,
				"manual enqueue must not create auto-design rows when only hook_enabled is set")
		}
	}
}

func TestMaybeDispatchAutoDesign_HookEnabledRunsForPostCommitSource(t *testing.T) {
	srv, repo := newAutoDesignTestServer(t)
	enableHookAutoDesignReviewForRepo(t, repo.RootPath)

	commit, err := srv.db.GetOrCreateCommit(repo.ID, "feedbabe", "Author", "refactor: hook", time.Now())
	require.NoError(t, err)
	diff := "+x\n+y\n+a\n+b\n+c\n+d\n+e\n+f\n+g\n+h\n+i\n+j\n"
	parent := &storage.ReviewJob{
		ID:            999,
		RepoID:        repo.ID,
		CommitID:      &commit.ID,
		GitRef:        "feedbabe",
		Agent:         "test",
		JobType:       storage.JobTypeReview,
		Status:        storage.JobStatusQueued,
		EnqueuedAt:    time.Now(),
		RepoPath:      repo.RootPath,
		CommitSubject: "refactor: hook",
		DiffContent:   &diff,
		Source:        "post_commit",
	}

	require.NoError(t, srv.maybeDispatchAutoDesign(context.Background(), parent))

	queued, err := srv.db.ListJobsByStatus(repo.ID, storage.JobStatusQueued)
	require.NoError(t, err)
	var found *storage.ReviewJob
	for i := range queued {
		j := queued[i]
		if j.GitRef == "feedbabe" && j.ReviewType == "design" && j.Source == "auto_design" {
			found = &j
			break
		}
	}
	require.NotNil(t, found, "expected hook-enabled auto-design row for post_commit source")
	assert.Equal(t, storage.JobTypeReview, found.JobType)
}

func TestPostCommitHookAutoDesignUsesActiveConfig(t *testing.T) {
	assert := assert.New(t)
	ResetAutoDesignMetricsForTest()
	t.Cleanup(ResetAutoDesignMetricsForTest)

	db, tmpDir := testutil.OpenTestDBWithDir(t)
	configPath := filepath.Join(tmpDir, "custom-config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
[auto_design_review]
hook_enabled = true
trigger_paths = ["migrations/**"]
`), 0o644))
	cfg, err := config.LoadGlobalFrom(configPath)
	require.NoError(t, err)
	require.True(t, cfg.AutoDesignReview.HookEnabled)

	srv := NewServer(db, cfg, configPath)
	t.Cleanup(func() { _ = srv.Close() })

	repo := testutil.NewGitRepo(t)
	repo.CommitFile("base.txt", "base", "base")
	sha := repo.CommitFile("migrations/001.sql", "create table t(id integer);\n", "feat: add migration")

	job := enqueueViaHTTP(t, srv, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   sha,
		Agent:    "test",
		Source:   "post_commit",
	})
	assert.Equal("post_commit", job.Source)

	storedRepo, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	assert.Equal(1, autoDesignRowsForSHA(t, db, storedRepo.ID, sha),
		"hook-only auto-design should use the daemon's active config, not the default global path")
	assert.EqualValues(1, AutoDesignMetricsSnapshot().TriggeredHeuristic)
}

func TestAutoDesignMetrics_RecordHeuristic(t *testing.T) {
	ResetAutoDesignMetricsForTest()
	t.Cleanup(ResetAutoDesignMetricsForTest)

	srv, repo := newAutoDesignTestServer(t)
	enableAutoDesignReviewForRepo(t, repo.RootPath)

	commit, err := srv.db.GetOrCreateCommit(repo.ID, "feedcafe", "Author", "refactor: rework", time.Now())
	require.NoError(t, err)
	bigDiff := "+x\n+y\n+a\n+b\n+c\n+d\n+e\n+f\n+g\n+h\n+i\n+j\n+k\n+l\n"
	parent := &storage.ReviewJob{
		RepoID: repo.ID, CommitID: &commit.ID, GitRef: "feedcafe",
		JobType: storage.JobTypeReview, Status: storage.JobStatusQueued,
		EnqueuedAt: time.Now(), RepoPath: repo.RootPath,
		CommitSubject: "refactor: rework", DiffContent: &bigDiff,
	}
	require.NoError(t, srv.maybeDispatchAutoDesign(context.Background(), parent))

	snap := AutoDesignMetricsSnapshot()
	assert.EqualValues(t, 1, snap.TriggeredHeuristic)
	assert.EqualValues(t, 0, snap.SkippedHeuristic)
}

func TestAutoDesignStatusForResponse_DisabledOmitted(t *testing.T) {
	ResetAutoDesignMetricsForTest()
	srv, _ := newAutoDesignTestServer(t)
	// No repo enables auto_design_review.
	got := srv.autoDesignStatusForResponse()
	assert.Nil(t, got)
}

func TestAutoDesignStatusForResponse_DisabledRegularRepoDoesNotSpawnGit(t *testing.T) {
	ResetAutoDesignMetricsForTest()
	db := testutil.OpenTestDB(t)
	repo := testutil.NewGitRepo(t)
	_, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)

	srv := NewServer(db, config.DefaultConfig(), "")
	marker := filepath.Join(t.TempDir(), "git-invoked")
	t.Setenv("ROBOREV_GIT_MARKER", marker)

	fakeGit := "#!/bin/sh\n: > \"$ROBOREV_GIT_MARKER\"\nexit 1\n"
	if runtime.GOOS == "windows" {
		fakeGit = "@echo git > \"%ROBOREV_GIT_MARKER%\"\r\n@exit /b 1\r\n"
	}
	restore := testutil.MockBinaryInPath(t, "git", fakeGit)
	defer restore()

	got := srv.autoDesignStatusForResponse()
	assert.Nil(t, got)
	_, err = os.Stat(marker)
	assert.True(t, os.IsNotExist(err), "disabled status path must not shell out to git for regular repos without config")
}

func TestAutoDesignStatusForResponse_EnabledRepoSurfaces(t *testing.T) {
	ResetAutoDesignMetricsForTest()
	srv, repo := newAutoDesignTestServer(t)
	enableAutoDesignReviewForRepo(t, repo.RootPath)

	got := srv.autoDesignStatusForResponse()
	require.NotNil(t, got)
	assert.True(t, got.Enabled)
}

func TestAutoDesignStatusForResponse_HookEnabledRepoSurfaces(t *testing.T) {
	ResetAutoDesignMetricsForTest()
	srv, repo := newAutoDesignTestServer(t)
	enableHookAutoDesignReviewForRepo(t, repo.RootPath)

	got := srv.autoDesignStatusForResponse()
	require.NotNil(t, got)
	assert.True(t, got.Enabled)
}

func TestMaybeDispatchAutoDesign_InvalidConfig_NoRow(t *testing.T) {
	srv, repo := newAutoDesignTestServer(t)
	// Enable auto-design but poison the heuristics with an invalid glob.
	// An unvalidated typo previously silently inserted a skipped row with
	// reason "auto-design: heuristic error" — this confirms the new
	// validation path logs and no-ops instead.
	require.NoError(t, os.WriteFile(filepath.Join(repo.RootPath, ".roborev.toml"),
		[]byte("[auto_design_review]\nenabled = true\ntrigger_paths = [\"[\"]\n"), 0o644))

	commit, err := srv.db.GetOrCreateCommit(repo.ID, "deadbeef", "Author", "feat: x", time.Now())
	require.NoError(t, err)
	d := "+x\n+y\n+a\n+b\n+c\n+d\n+e\n+f\n+g\n+h\n+i\n+j\n"
	parent := &storage.ReviewJob{
		RepoID: repo.ID, CommitID: &commit.ID, GitRef: "deadbeef",
		JobType: storage.JobTypeReview, Status: storage.JobStatusQueued,
		EnqueuedAt: time.Now(), RepoPath: repo.RootPath,
		CommitSubject: "feat: x", DiffContent: &d,
	}

	require.NoError(t, srv.maybeDispatchAutoDesign(context.Background(), parent))

	for _, s := range []storage.JobStatus{storage.JobStatusQueued, storage.JobStatusSkipped} {
		jobs, err := srv.db.ListJobsByStatus(repo.ID, s)
		require.NoError(t, err)
		for _, j := range jobs {
			assert.NotEqual(t, "auto_design", j.Source,
				"invalid config must not create an auto_design row (status=%s, reason=%q)",
				s, j.SkipReason)
		}
	}
}

func TestMaybeDispatchAutoDesign_Disabled_NoOp(t *testing.T) {
	srv, repo := newAutoDesignTestServer(t)
	// Don't enable.

	commit, err := srv.db.GetOrCreateCommit(repo.ID, "abc", "Author", "feat: x", time.Now())
	require.NoError(t, err)
	d := "+x\n"
	parent := &storage.ReviewJob{
		RepoID: repo.ID, CommitID: &commit.ID, GitRef: "abc",
		JobType: storage.JobTypeReview, Status: storage.JobStatusQueued,
		EnqueuedAt: time.Now(), RepoPath: repo.RootPath,
		CommitSubject: "feat: x", DiffContent: &d,
	}

	require.NoError(t, srv.maybeDispatchAutoDesign(context.Background(), parent))

	for _, s := range []storage.JobStatus{storage.JobStatusQueued, storage.JobStatusSkipped} {
		jobs, err := srv.db.ListJobsByStatus(repo.ID, s)
		require.NoError(t, err)
		for _, j := range jobs {
			assert.NotEqual(t, "auto_design", j.Source, "no auto-design rows should exist when disabled")
		}
	}
}
