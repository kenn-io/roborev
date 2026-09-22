package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestSchedulerGlobalGateAndEnqueueMetadata(t *testing.T) {
	repoDir := t.TempDir()
	repo := testutil.InitTestGitRepo(t, repoDir)
	repo.CommitFile("main.go", "package main\nconst captured = \"old\"\n", "source")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".roborev.toml"), []byte("[schedule]\nenabled = true\n"), 0o600))

	enabled := false
	cfg := &config.Config{Schedule: config.ScheduleConfig{
		Enabled:  &enabled,
		Interval: "1h",
		Types:    []string{"complexity"},
		MaxFiles: 1,
		Agent:    "test",
	}}
	db := testutil.OpenTestDB(t)
	storedRepo, err := db.GetOrCreateRepo(repoDir)
	require.NoError(t, err)
	cw := NewConfigWatcher("", cfg, nil, nil)
	var captured []storage.EnqueueOpts
	var queuedJob *storage.ReviewJob
	s := NewSchedulerService(db, cw, func(_ context.Context, _ storage.Repo, opts storage.EnqueueOpts) (*storage.ReviewJob, error) {
		captured = append(captured, opts)
		var err error
		queuedJob, err = db.EnqueueJob(opts)
		return queuedJob, err
	})

	s.reconcile(context.Background())
	assert.Empty(t, captured)

	enabled = true
	s.reconcile(context.Background())
	require.Len(t, captured, 1)
	assert.Equal(t, storedRepo.ID, captured[0].RepoID)
	assert.Equal(t, storage.JobTypeTask, captured[0].JobType)
	assert.Equal(t, storage.JobSourceScheduled, captured[0].Source)
	assert.Equal(t, "complexity", captured[0].AnalysisType)
	assert.Equal(t, []string{"main.go"}, captured[0].AnalysisFiles)
	assert.Equal(t, repo.HeadSHA(), captured[0].AnalysisCommitSHA)
	assert.False(t, captured[0].PromptPrebuilt)
	assert.Contains(t, captured[0].Prompt, "const captured")
	assert.Contains(t, captured[0].OutputPrefix, "main.go")

	require.NotNil(t, queuedJob)
	job, err := db.GetJobByID(queuedJob.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobSourceScheduled, job.Source)
	assert.Equal(t, storage.JobTypeTask, job.JobType)
	assert.False(t, job.PromptPrebuilt)
}

func TestSchedulerStalenessAndCrossTypeBudget(t *testing.T) {
	repoDir := t.TempDir()
	repo := testutil.InitTestGitRepo(t, repoDir)
	repo.CommitFiles(map[string]string{
		"a.go": "package main\nconst a = 1\n",
		"b.go": "package main\nconst b = 1\n",
	}, "source")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".roborev.toml"), []byte("[schedule]\nenabled = true\n"), 0o600))

	enabled := true
	cfg := &config.Config{Schedule: config.ScheduleConfig{
		Enabled:  &enabled,
		Interval: "1h",
		Types:    []string{"complexity", "refactor"},
		Paths:    []string{"a.go", "b.go"},
		MaxFiles: 1,
		Agent:    "test",
	}}
	db := testutil.OpenTestDB(t)
	storedRepo, err := db.GetOrCreateRepo(repoDir)
	require.NoError(t, err)
	cw := NewConfigWatcher("", cfg, nil, nil)
	var captured []storage.EnqueueOpts
	s := NewSchedulerService(db, cw, func(_ context.Context, _ storage.Repo, opts storage.EnqueueOpts) (*storage.ReviewJob, error) {
		captured = append(captured, opts)
		return db.EnqueueJob(opts)
	})

	s.reconcile(context.Background())
	assert.Equal(t, []string{"complexity", "refactor"}, []string{captured[0].AnalysisType, captured[1].AnalysisType})
	assert.Equal(t, []string{"a.go", "a.go"}, []string{captured[0].AnalysisFiles[0], captured[1].AnalysisFiles[0]})

	s.due[storedRepo.ID] = time.Time{}
	s.reconcile(context.Background())
	assert.Len(t, captured, 4)
	assert.Equal(t, []string{"b.go", "b.go"}, []string{captured[2].AnalysisFiles[0], captured[3].AnalysisFiles[0]}, "queued a.go jobs suppress duplicates")

	jobs := mustQueuedJobs(t, db, storedRepo.ID)
	for _, job := range jobs {
		_, err = db.Exec("UPDATE review_jobs SET status = 'failed', finished_at = ?, updated_at = ? WHERE id = ?", time.Now().Format(time.RFC3339Nano), time.Now().Format(time.RFC3339Nano), job.ID)
		require.NoError(t, err)
	}
	s.due[storedRepo.ID] = time.Time{}
	s.reconcile(context.Background())
	assert.Len(t, captured, 6, "failed rows remain retryable")

	oldSHA := repo.HeadSHA()
	stamp := time.Now().Format(time.RFC3339Nano)
	_, err = db.Exec("UPDATE review_jobs SET status = 'done', finished_at = ?, updated_at = ? WHERE repo_id = ?", stamp, stamp, storedRepo.ID)
	require.NoError(t, err)
	repo.CommitFile("a.go", "package main\nconst a = 2\n", "change a")
	assert.NotEqual(t, oldSHA, repo.HeadSHA())
	restarted := NewSchedulerService(db, cw, func(_ context.Context, _ storage.Repo, opts storage.EnqueueOpts) (*storage.ReviewJob, error) {
		captured = append(captured, opts)
		return db.EnqueueJob(opts)
	})
	restarted.reconcile(context.Background())
	assert.Equal(t, []string{"a.go", "a.go"}, []string{captured[6].AnalysisFiles[0], captured[7].AnalysisFiles[0]})
	assert.Equal(t, repo.HeadSHA(), captured[6].AnalysisCommitSHA)
}

func TestSchedulerPolicyReloadAndFailureIsolation(t *testing.T) {
	goodDir := t.TempDir()
	good := testutil.InitTestGitRepo(t, goodDir)
	good.CommitFile("main.go", "package main\n", "source")
	badDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(goodDir, ".roborev.toml"), []byte("[schedule]\nenabled = true\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(badDir, ".roborev.toml"), []byte("[schedule]\nenabled = true\nmax_files = 0\n"), 0o600))

	enabled := true
	cfg := &config.Config{Schedule: config.ScheduleConfig{Enabled: &enabled, Interval: "1h", Types: []string{"complexity"}, Paths: []string{"main.go"}, MaxFiles: 1, Agent: "test"}}
	db := testutil.OpenTestDB(t)
	goodRepo, err := db.GetOrCreateRepo(goodDir)
	require.NoError(t, err)
	_, err = db.GetOrCreateRepo(badDir)
	require.NoError(t, err)
	cw := NewConfigWatcher("", cfg, nil, nil)
	var captured []storage.EnqueueOpts
	s := NewSchedulerService(db, cw, func(_ context.Context, _ storage.Repo, opts storage.EnqueueOpts) (*storage.ReviewJob, error) {
		captured = append(captured, opts)
		return db.EnqueueJob(opts)
	})

	now := time.Date(2026, 9, 21, 20, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.reconcile(context.Background())
	require.Len(t, captured, 1)
	due := s.due[goodRepo.ID]
	cfg.DefaultAgent = "unrelated-change"
	now = now.Add(time.Minute)
	s.reconcile(context.Background())
	assert.Equal(t, due, s.due[goodRepo.ID], "unrelated global reload keeps the due time")

	require.NoError(t, os.WriteFile(filepath.Join(goodDir, ".roborev.toml"), []byte("[schedule]\nenabled = true\ninterval = \"2h\"\n"), 0o600))
	now = now.Add(time.Minute)
	s.reconcile(context.Background())
	assert.Equal(t, now.Add(2*time.Hour), s.due[goodRepo.ID])
	assert.Len(t, captured, 1, "the pending job still suppresses a duplicate")
}

func TestSchedulerStartStop(t *testing.T) {
	db := testutil.OpenTestDB(t)
	enabled := false
	cfg := &config.Config{Schedule: config.ScheduleConfig{Enabled: &enabled}}
	s := NewSchedulerService(db, NewConfigWatcher("", cfg, nil, nil), func(context.Context, storage.Repo, storage.EnqueueOpts) (*storage.ReviewJob, error) {
		return nil, nil
	})
	s.Start(context.Background())
	s.Stop()
	assert.True(t, s.stopping.Load())
}

func TestSchedulerStopWaitsForActivePass(t *testing.T) {
	repoDir := t.TempDir()
	repo := testutil.InitTestGitRepo(t, repoDir)
	repo.CommitFile("main.go", "package main\n", "source")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".roborev.toml"), []byte("[schedule]\nenabled = true\n"), 0o600))
	enabled := true
	cfg := &config.Config{Schedule: config.ScheduleConfig{Enabled: &enabled, Interval: "1h", Types: []string{"complexity"}, MaxFiles: 1, Agent: "test"}}
	db := testutil.OpenTestDB(t)
	_, err := db.GetOrCreateRepo(repoDir)
	require.NoError(t, err)
	entered := make(chan struct{})
	release := make(chan struct{})
	s := NewSchedulerService(db, NewConfigWatcher("", cfg, nil, nil), func(_ context.Context, _ storage.Repo, opts storage.EnqueueOpts) (*storage.ReviewJob, error) {
		close(entered)
		<-release
		return db.EnqueueJob(opts)
	})

	s.Start(context.Background())
	<-entered
	secondDone := make(chan struct{})
	go func() {
		s.reconcile(context.Background())
		close(secondDone)
	}()
	require.Eventually(t, func() bool { return channelClosed(secondDone) }, time.Second, time.Millisecond)
	stopped := make(chan struct{})
	go func() {
		s.Stop()
		close(stopped)
	}()
	// The scheduler goroutine is external work for this test.
	require.Never(t, func() bool { return channelClosed(stopped) }, 20*time.Millisecond, time.Millisecond)
	close(release)
	require.Eventually(t, func() bool { return channelClosed(stopped) }, time.Second, time.Millisecond)
}

func channelClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func mustQueuedJobs(t *testing.T, db *storage.DB, repoID int64) []storage.ReviewJob {
	t.Helper()
	jobs, err := db.ListJobsByStatus(repoID, storage.JobStatusQueued)
	require.NoError(t, err)
	return jobs
}
