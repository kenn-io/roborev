package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestScheduledAnalysis(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFiles(map[string]string{"a.go": "package a\n", "b.go": "package b\n"}, "add sources")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	no := false
	interval := "1h"
	maxFiles := 1
	server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &no, Interval: &interval, MaxFiles: &maxFiles, Types: []string{"refactor"}}
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	now := time.Now()
	watcher.poll(t.Context(), now)
	jobs, err := db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Empty(t, jobs)
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("analyze_agent = 'test'\nanalyze_model = 'cheap'\n[schedule]\nenabled = true\n"), 0o644))
	watcher.poll(t.Context(), now)
	jobs, err = db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, []string{"a.go"}, jobs[0].AnalysisFiles)
	assert.Equal(t, repo.HeadSHA(), jobs[0].AnalysisCommitSHA)
	assert.Equal(t, storage.JobSourceScheduled, jobs[0].Source)
}

func TestScheduledAnalysisCancelledRead(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.go", "package a\n", "add source")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	watcher := &analysisScheduler{server: server}
	err = watcher.scheduleRepo(ctx, *registered, &config.RepoConfig{}, server.configWatcher.Config(), config.Schedule{Types: []string{"refactor"}, MaxFiles: 1})
	require.ErrorIs(t, err, context.Canceled)
}

func TestScheduledAnalysisDeletedRepo(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.go", "package a\n", "add source")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	require.NoError(t, db.DeleteRepo(registered.ID, true))
	watcher := &analysisScheduler{server: server}
	err = watcher.enqueueType(t.Context(), *registered, "refactor", []string{"a.go"}, repo.HeadSHA(), map[string]string{"a.go": "package a\n"})
	require.Error(t, err)
	jobs, err := db.ListJobs("", "", 0, 0)
	require.NoError(t, err)
	assert.Empty(t, jobs)
	repos, err := db.ListRepos()
	require.NoError(t, err)
	assert.Empty(t, repos)
}

func TestScheduledAnalysisMovedRepo(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.go", "package a\n", "add source")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	moved := filepath.Join(t.TempDir(), "moved")
	require.NoError(t, db.MoveRepo(registered.ID, moved, ""))
	watcher := &analysisScheduler{server: server}
	err = watcher.enqueueType(t.Context(), *registered, "refactor", []string{"a.go"}, repo.HeadSHA(), map[string]string{"a.go": "package a\n"})
	require.Error(t, err)
	jobs, err := db.ListJobs("", "", 0, 0)
	require.NoError(t, err)
	assert.Empty(t, jobs)
	repos, err := db.ListRepos()
	require.NoError(t, err)
	require.Len(t, repos, 1)
	assert.Equal(t, registered.ID, repos[0].ID)
	assert.Equal(t, filepath.ToSlash(moved), repos[0].RootPath)
}

func TestScheduledAnalysisInvalidRepoDoesNotBlockOtherRepos(t *testing.T) {
	server, db, _ := newTestServer(t)
	bad, good := testutil.NewGitRepo(t), testutil.NewGitRepo(t)
	bad.CommitFile("a.go", "package a\n", "add source")
	good.CommitFile("b.go", "package b\n", "add source")
	_, err := db.GetOrCreateRepo(bad.Path())
	require.NoError(t, err)
	registered, err := db.GetOrCreateRepo(good.Path())
	require.NoError(t, err)
	yes := true
	interval := "1h"
	budget := 2
	server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}}
	require.NoError(t, os.WriteFile(filepath.Join(bad.Path(), ".roborev.toml"), []byte("[schedule]\nenabled = true\ninterval = 'invalid'\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(good.Path(), ".roborev.toml"), []byte("[schedule]\nenabled = true\n[analyze.refactor]\nagent = 'test'\n"), 0o644))
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	watcher.poll(t.Context(), time.Now())
	jobs, err := db.ListJobs("", "", 0, 0)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, registered.ID, jobs[0].RepoID)
}

func TestScheduledAnalysisShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		repo.CommitFile("a.go", "package a\n", "add file")
		_, err := db.GetOrCreateRepo(repo.Path())
		require.NoError(t, err)
		yes, interval, budget := true, "1s", 1
		server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}}
		require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[schedule]\nenabled = true\n[analyze.refactor]\nagent = 'test'\n"), 0o644))
		server.startAnalysisScheduler(t.Context())
		require.Eventually(t, func() bool { jobs, err := db.ListJobs("", "", 0, 0); return err == nil && len(jobs) == 1 }, workerStartTimeout, 10*time.Millisecond)
		server.stopAnalysisScheduler()
		server.scheduleMu.Lock()
		assert.Nil(t, server.scheduleCancel)
		assert.Nil(t, server.scheduleDone)
		server.scheduleMu.Unlock()
	})
}

func TestScheduledAnalysisTrackedFilesAndDirectories(t *testing.T) {
	t.Run("paths and directories", func(t *testing.T) {
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		repo.CommitFiles(map[string]string{"Dockerfile": "FROM scratch\n", "Makefile": "all:\n", "internal/a.go": "package a\n", "internal/data.bin": "binary\xffbytes"}, "add files")
		registered, err := db.GetOrCreateRepo(repo.Path())
		require.NoError(t, err)
		yes, interval, budget := true, "1h", 10
		server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}}
		require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[schedule]\nenabled = true\n[analyze.refactor]\nagent = 'test'\n"), 0o644))
		watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
		watcher.poll(t.Context(), time.Now())
		jobs, err := db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 1)
		assert.ElementsMatch(t, []string{"internal/a.go"}, jobs[0].AnalysisFiles)
		assert.Contains(t, jobs[0].Prompt, "package a")
		setJobStatus(t, db, jobs[0].ID, storage.JobStatusDone)
		server.configWatcher.Config().Schedule.Paths = []string{"internal/"}
		repo.CommitFiles(map[string]string{"internal/a.go": "package changed\n", "Dockerfile": "FROM other\n"}, "change files")
		watcher.poll(t.Context(), time.Now().Add(time.Hour))
		jobs, err = db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 1)
		assert.Equal(t, []string{"internal/a.go"}, jobs[0].AnalysisFiles)
	})
	t.Run("AnalysisOldestBaselineFirst", func(t *testing.T) {
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		base := repo.CommitFiles(map[string]string{"a.go": "package a\n", "b.go": "package b\n", "c.go": "package c\n"}, "add sources")
		registered, err := db.GetOrCreateRepo(repo.Path())
		require.NoError(t, err)
		for _, file := range []string{"a.go", "b.go"} {
			job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "refactor", Agent: "test", Prompt: "old", AnalysisType: "refactor", AnalysisFiles: []string{file}, AnalysisCommitSHA: base})
			require.NoError(t, err)
			setJobStatus(t, db, job.ID, storage.JobStatusDone)
			date := "2026-01-02 00:00:00"
			if file == "b.go" {
				date = "2026-01-01 00:00:00"
			}
			_, err = db.ExecContext(t.Context(), "UPDATE review_jobs SET enqueued_at = ? WHERE id = ?", date, job.ID)
			require.NoError(t, err)
		}
		repo.CommitFiles(map[string]string{"a.go": "package changedA\n", "b.go": "package changedB\n"}, "change sources")
		yes := true
		interval := "1h"
		budget := 2
		server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}}
		require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[schedule]\nenabled = true\n[analyze.refactor]\nagent = 'test'\n"), 0o644))
		watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
		watcher.poll(t.Context(), time.Now())
		jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 1)
		assert.Equal(t, []string{"b.go", "c.go"}, jobs[0].AnalysisFiles)
	})
	t.Run("AnalysisBundlesTypesAndMissingBaseline", func(t *testing.T) {
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		head := repo.CommitFiles(map[string]string{"a.go": "original\n", "normal.go": "package normal\n"}, "add files")
		registered, err := db.GetOrCreateRepo(repo.Path())
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("analyze_agent = 'test'\n[schedule]\nenabled = true\ninterval = '1h'\nmax_files = 10\ntypes = ['refactor', 'complexity']\n"), 0o644))
		watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
		now := time.Now()
		watcher.poll(t.Context(), now)
		jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 2)
		for _, job := range jobs {
			assert.Equal(t, []string{"a.go", "normal.go"}, job.AnalysisFiles)
			assert.Equal(t, head, job.AnalysisCommitSHA)
			setJobStatus(t, db, job.ID, storage.JobStatusDone)
		}
		repo.CommitFile("a.go", "changed\n", "change a")
		watcher.poll(t.Context(), now.Add(time.Hour))
		jobs, err = db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 2)
		for _, job := range jobs {
			assert.Equal(t, []string{"a.go"}, job.AnalysisFiles)
			setJobStatus(t, db, job.ID, storage.JobStatusDone)
		}
		_, err = db.ExecContext(t.Context(), `UPDATE review_jobs SET analysis_commit_sha = ? WHERE repo_id = ?`, "0000000000000000000000000000000000000001", registered.ID)
		require.NoError(t, err)
		watcher.poll(t.Context(), now.Add(2*time.Hour))
		jobs, err = db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 2)
		for _, job := range jobs {
			assert.Equal(t, []string{"a.go", "normal.go"}, job.AnalysisFiles)
		}
	})
	t.Run("AnalysisIncludesDeletedTrackedFile", func(t *testing.T) {
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		repo.CommitFiles(map[string]string{"a.go": "package committedA\n", "b.go": "package committedB\n"}, "add sources")
		registered, err := db.GetOrCreateRepo(repo.Path())
		require.NoError(t, err)
		require.NoError(t, os.Remove(filepath.Join(repo.Path(), "a.go")))
		repo.WriteFile("b.go", "dirty edit\n")
		yes, interval, budget := true, "1h", 2
		server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}}
		server.configWatcher.Config().AnalyzeAgent = "test"
		watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
		watcher.poll(t.Context(), time.Now())
		jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 1)
		assert.Equal(t, []string{"a.go", "b.go"}, jobs[0].AnalysisFiles)
		assert.Contains(t, jobs[0].Prompt, "package committedA")
		assert.Contains(t, jobs[0].Prompt, "package committedB")
		assert.NotContains(t, jobs[0].Prompt, "dirty edit")
	})
}

func TestScheduledAnalysisRetryAfterIntervalChange(t *testing.T) {
	setup := func(t *testing.T) (*Server, *storage.DB, *testutil.TestRepo, *storage.Repo, *analysisScheduler) {
		t.Helper()
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		repo.CommitFiles(map[string]string{"a.go": "package a\n", "b.go": "package b\n"}, "add sources")
		registered, err := db.GetOrCreateRepo(repo.Path())
		require.NoError(t, err)
		return server, db, repo, registered, &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	}
	t.Run("interval change", func(t *testing.T) {
		server, db, repo, registered, watcher := setup(t)
		var err error
		yes := true
		interval := "1h"
		budget := 2
		server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}}
		require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[schedule]\nenabled = true\n[analyze.refactor]\nagent = 'test'\n"), 0o644))
		pending, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "refactor", Agent: "test", Prompt: "pending", AnalysisType: "refactor", AnalysisFiles: []string{"b.go"}, AnalysisCommitSHA: repo.HeadSHA()})
		require.NoError(t, err)
		now := time.Now()
		watcher.poll(t.Context(), now)
		jobs, err := db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 2)
		assert.Equal(t, []string{"a.go"}, jobs[0].AnalysisFiles)
		setJobStatus(t, db, pending.ID, storage.JobStatusFailed)
		interval = "2h"
		watcher.poll(t.Context(), now.Add(time.Hour))
		jobs, err = db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 2)
		interval = "30m"
		watcher.poll(t.Context(), now.Add(time.Hour))
		jobs, err = db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 3)
		assert.Equal(t, []string{"b.go"}, jobs[0].AnalysisFiles)
	})
	t.Run("AnalysisRetriesPartialEnqueue", func(t *testing.T) {
		server, db, repo, registered, watcher := setup(t)
		var err error
		yes, interval, budget := true, "1h", 10
		server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor", "complexity"}}
		require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[schedule]\nenabled = true\n[analyze.refactor]\nagent = 'test'\n[analyze.complexity]\nagent = 'test'\n"), 0o644))
		_, err = db.ExecContext(t.Context(), `CREATE TRIGGER fail_complexity BEFORE INSERT ON review_jobs WHEN NEW.analysis_type = 'complexity' BEGIN SELECT RAISE(FAIL, 'temporary'); END`)
		require.NoError(t, err)
		now := time.Now()
		watcher.poll(t.Context(), now)
		jobs, err := db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 1)
		setJobStatus(t, db, jobs[0].ID, storage.JobStatusDone)
		_, err = db.ExecContext(t.Context(), `DROP TRIGGER fail_complexity`)
		require.NoError(t, err)
		watcher.poll(t.Context(), now.Add(30*time.Second))
		jobs, err = db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 2)
		watcher.poll(t.Context(), now.Add(time.Hour))
		jobs, err = db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 2)
		assert.Equal(t, "complexity", jobs[0].AnalysisType)
		assert.Equal(t, "refactor", jobs[1].AnalysisType)
		assert.Equal(t, []string{"a.go", "b.go"}, jobs[0].AnalysisFiles)
		assert.Equal(t, []string{"a.go", "b.go"}, jobs[1].AnalysisFiles)
	})
	t.Run("AnalysisTypeFailureDoesNotBlockOtherType", func(t *testing.T) {
		server, db, _, registered, watcher := setup(t)
		var err error
		yes, interval, budget := true, "1h", 1
		server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor", "complexity"}}
		server.configWatcher.Config().AnalyzeAgent = "no-such-agent"
		server.configWatcher.Config().Analyze = map[string]config.AnalyzeConfig{"complexity": {Agent: "test"}}
		now := time.Now()
		watcher.poll(t.Context(), now)
		jobs, err := db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 1)
		assert.Equal(t, "complexity", jobs[0].AnalysisType)
		assert.Equal(t, []string{"a.go"}, jobs[0].AnalysisFiles)
		setJobStatus(t, db, jobs[0].ID, storage.JobStatusDone)
		watcher.poll(t.Context(), now.Add(time.Hour))
		jobs, err = db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 1)
		assert.Equal(t, []string{"b.go"}, jobs[0].AnalysisFiles)
	})
	t.Run("AnalysisFailedTypeDoesNotStarveOtherFiles", func(t *testing.T) {
		server, db, _, registered, watcher := setup(t)
		var err error
		yes, interval, budget := true, "1h", 1
		server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor", "complexity"}}
		server.configWatcher.Config().AnalyzeAgent = "test"
		now := time.Now()
		watcher.poll(t.Context(), now)
		jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 2)
		for _, job := range jobs {
			require.Equal(t, []string{"a.go"}, job.AnalysisFiles)
			status := storage.JobStatusDone
			if job.AnalysisType == "refactor" {
				status = storage.JobStatusFailed
			}
			setJobStatus(t, db, job.ID, status)
		}
		watcher.poll(t.Context(), now.Add(time.Hour))
		jobs, err = db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 2)
		for _, job := range jobs {
			assert.Equal(t, []string{"b.go"}, job.AnalysisFiles, "failed analysis of a.go must not block b.go")
		}
	})
}

func TestScheduledCancellationMatchesReviewDemand(t *testing.T) {
	t.Run("review demand", func(t *testing.T) {
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		registered, err := db.GetOrCreateRepo(repo.Path())
		require.NoError(t, err)
		for i := range 3 {
			job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "analysis", Agent: "test", Prompt: "analysis", Source: storage.JobSourceScheduled})
			require.NoError(t, err)
			claimed, err := db.ClaimJob(fmt.Sprintf("worker-%d", i))
			require.NoError(t, err)
			require.Equal(t, job.ID, claimed.ID)
			server.workerPool.workerJobs[fmt.Sprintf("worker-%d", i)] = job.ID
		}
		server.workerPool.SetMaxWorkers(3)
		_, err = db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "HEAD", Agent: "test", JobType: storage.JobTypeReview, Source: storage.JobSourceCI})
		require.NoError(t, err)
		for range 5 {
			server.workerPool.wake()
		}
		canceled, err := db.ListJobs("canceled", "", 0, 0)
		require.NoError(t, err)
		assert.Len(t, canceled, 1)
		_, err = db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "HEAD", Agent: "test", JobType: storage.JobTypeReview})
		require.NoError(t, err)
		server.workerPool.wake()
		canceled, err = db.ListJobs("canceled", "", 0, 0)
		require.NoError(t, err)
		assert.Len(t, canceled, 2)
	})
	t.Run("WakeDoesNotCancelAnotherWorkersClaim", func(t *testing.T) {
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		registered, err := db.GetOrCreateRepo(repo.Path())
		require.NoError(t, err)
		job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "analysis", Agent: "test", Prompt: "analysis", Source: storage.JobSourceScheduled})
		require.NoError(t, err)
		_, err = db.ClaimJob("worker-1")
		require.NoError(t, err)
		_, err = db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "HEAD", Agent: "test", JobType: storage.JobTypeReview})
		require.NoError(t, err)
		server.workerPool.SetMaxWorkers(2)
		server.workerPool.runningJobsMu.Lock()
		server.workerPool.workerJobs["worker-1"] = job.ID
		server.workerPool.workerJobs["worker-0"] = workerClaimingJobID
		server.workerPool.runningJobsMu.Unlock()
		server.workerPool.wake()
		current, err := db.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Equal(t, storage.JobStatusRunning, current.Status)
	})
	t.Run("YieldIgnoresRetiringWorker", func(t *testing.T) {
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		registered, err := db.GetOrCreateRepo(repo.Path())
		require.NoError(t, err)
		busy, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "task", Agent: "test", Prompt: "task", JobType: storage.JobTypeTask})
		require.NoError(t, err)
		_, err = db.ClaimJob("worker-0")
		require.NoError(t, err)
		scheduled, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "analysis", Agent: "test", Prompt: "analysis", Source: storage.JobSourceScheduled})
		require.NoError(t, err)
		_, err = db.ClaimJob("worker-1")
		require.NoError(t, err)
		server.workerPool.SetMaxWorkers(2)
		server.workerPool.runningJobsMu.Lock()
		server.workerPool.workerJobs["worker-0"] = busy.ID
		server.workerPool.workerJobs["worker-1"] = scheduled.ID
		server.workerPool.runningJobsMu.Unlock()
		_, err = db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "HEAD", Agent: "test", JobType: storage.JobTypeReview})
		require.NoError(t, err)
		server.workerPool.SetMaxWorkers(1)
		job, err := db.GetJobByID(scheduled.ID)
		require.NoError(t, err)
		assert.Equal(t, storage.JobStatusRunning, job.Status)
	})
}

func TestScheduledAnalysisYieldsWorkerToReview(t *testing.T) {
	t.Run("PreemptionInterruptsScheduledAgent", func(t *testing.T) {
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		repo.CommitFile("a.go", "package a\n", "add file")
		registered, err := db.GetOrCreateRepo(repo.Path())
		require.NoError(t, err)
		started := make(chan struct{})
		foreground := make(chan struct{}, 1)
		interrupted := make(chan struct{})
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "scheduled-order-check", ReviewFn: func(ctx context.Context, _, _, prompt string, _ io.Writer) (string, error) {
			if strings.Contains(prompt, "## Analysis Request") {
				close(started)
				<-ctx.Done()
				close(interrupted)
				return "", ctx.Err()
			}
			foreground <- struct{}{}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-release:
				return "done", nil
			}
		}})
		server.workerPool.SetMaxWorkers(1)
		scheduled, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "refactor", Agent: "scheduled-order-check", Prompt: "## Analysis Request", Source: storage.JobSourceScheduled})
		require.NoError(t, err)
		server.workerPool.Start()
		testutil.ReceiveWithTimeout(t, started, workerStartTimeout)
		review, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: repo.HeadSHA(), Agent: "scheduled-order-check", JobType: storage.JobTypeReview})
		require.NoError(t, err)
		server.workerPool.wake()
		testutil.ReceiveWithTimeout(t, interrupted, workerStartTimeout)
		testutil.ReceiveWithTimeout(t, foreground, workerStartTimeout)
		currentAnalysis, err := db.GetJobByID(scheduled.ID)
		require.NoError(t, err)
		assert.Equal(t, storage.JobStatusCanceled, currentAnalysis.Status)
		currentReview, err := db.GetJobByID(review.ID)
		require.NoError(t, err)
		assert.Equal(t, storage.JobStatusRunning, currentReview.Status)
	})
	t.Run("YieldAfterReviewRetryDelay", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			server, db, _ := newTestServer(t)
			repo := testutil.NewGitRepo(t)
			repo.CommitFile("a.go", "package a\n", "add file")
			registered, err := db.GetOrCreateRepo(repo.Path())
			require.NoError(t, err)
			started := make(chan struct{})
			interrupted := make(chan struct{})
			reviewStarted := make(chan struct{})
			releaseReview := make(chan struct{})
			t.Cleanup(func() { close(releaseReview) })
			agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "delayed-review-start", ReviewFn: func(ctx context.Context, _, _, _ string, _ io.Writer) (string, error) {
				close(reviewStarted)
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-releaseReview:
					return "done", nil
				}
			}})
			agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "scheduled-retry-yield", ReviewFn: func(ctx context.Context, _, _, _ string, _ io.Writer) (string, error) {
				close(started)
				<-ctx.Done()
				close(interrupted)
				return "", ctx.Err()
			}})
			_, err = db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "refactor", Agent: "scheduled-retry-yield", Prompt: "analysis", Source: storage.JobSourceScheduled})
			require.NoError(t, err)
			server.workerPool.SetMaxWorkers(1)
			server.workerPool.Start()
			testutil.ReceiveWithTimeout(t, started, workerStartTimeout)
			review, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: repo.HeadSHA(), Agent: "delayed-review-start", JobType: storage.JobTypeReview})
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), `UPDATE review_jobs SET retry_not_before = ? WHERE id = ?`, time.Now().Add(500*time.Millisecond).UTC().Format("2006-01-02T15:04:05.000000000Z"), review.ID)
			require.NoError(t, err)
			server.workerPool.wake()
			testutil.ReceiveWithTimeout(t, interrupted, workerStartTimeout)
			testutil.ReceiveWithTimeout(t, reviewStarted, workerStartTimeout)
			current, err := db.GetJobByID(review.ID)
			require.NoError(t, err)
			assert.Equal(t, storage.JobStatusRunning, current.Status)
		})
	})
}

func TestScheduledAnalysisRestartRespectsInterval(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFiles(map[string]string{"a.go": "package a\n", "b.go": "package b\n"}, "add files")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	yes, interval, budget := true, "1h", 1
	server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}}
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[analyze.refactor]\nagent = 'test'\n"), 0o644))

	now := time.Now().Add(time.Second)
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	watcher.poll(t.Context(), now)
	jobs, err := db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, []string{"a.go"}, jobs[0].AnalysisFiles)

	restarted := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	restarted.poll(t.Context(), now.Add(5*time.Minute))
	jobs, err = db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	restarted.poll(t.Context(), now.Add(time.Hour))
	jobs, err = db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 2)
	assert.Equal(t, []string{"b.go"}, jobs[0].AnalysisFiles)
}

func TestScheduledAnalysisUsesBackupAgent(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.go", "package a\n", "add source")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	cfg := server.configWatcher.Config()
	yes, interval, budget := true, "1h", 1
	cfg.Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}}
	cfg.AnalyzeAgent = "codex"
	cfg.CodexCmd = "missing-codex-command"
	cfg.ReviewBackupAgent = "test"
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	watcher.poll(t.Context(), time.Now())
	jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "test", jobs[0].Agent)
}

func TestScheduledAnalysisDefaultBudget(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	files := map[string]string{}
	for i := range 25 {
		files[fmt.Sprintf("src/file%02d.go", i)] = "package sample\n"
	}
	repo.CommitFiles(files, "add sources")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("analyze_agent = 'test'\n[schedule]\nenabled = true\ninterval = '24h'\ntypes = ['complexity', 'duplication']\npaths = ['src/']\n"), 0o644))
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	watcher.poll(t.Context(), time.Now())
	jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 2)
	for _, job := range jobs {
		assert.Len(t, job.AnalysisFiles, config.DefaultScheduleMaxFiles)
	}
}

func TestScheduledAnalysisPromptBudget(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFiles(map[string]string{"a.go": strings.Repeat("a", 2000), "b.go": strings.Repeat("b", 2000), "large.go": strings.Repeat("x", 4000), "big.json": strings.Repeat("x", 6000), "pdf.go": "%PDF-1.4\n", "invalid.go": "\xff", "nullbyte.go": "a\x00b", ".gitattributes": "marked.go binary\n", "marked.go": "ordinary text\n"}, "add files")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	repo.WriteFile("a.go", "dirty edit")
	cfg := server.configWatcher.Config()
	yes, interval := true, "1h"
	cfg.Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, Types: []string{"refactor"}}
	agentName := "scheduled-prompt-handoff"
	var handoff string
	agent.RegisterForTest(t, &agent.FakeAgent{
		NameStr: agentName,
		ReviewFn: func(_ context.Context, repoPath, _, task string, _ io.Writer) (string, error) {
			files, err := filepath.Glob(filepath.Join(repoPath, ".roborev", "*", "prompt.md"))
			require.NoError(t, err)
			require.Len(t, files, 1)
			data, err := os.ReadFile(files[0])
			require.NoError(t, err)
			handoff = string(data)
			assert.Contains(t, task, "Read the complete task prompt")
			return string(testutil.ReviewFixtureJSON("No issues found.")), nil
		},
	})
	cfg.AnalyzeAgent = agentName
	cfg.DefaultMaxPromptSize = 3500
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	watcher.poll(t.Context(), time.Now())
	jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, []string{"a.go", "b.go"}, jobs[0].AnalysisFiles)
	assert.Greater(t, len(jobs[0].Prompt), cfg.DefaultMaxPromptSize)
	assert.Contains(t, jobs[0].Prompt, strings.Repeat("a", 2000))
	assert.NotContains(t, jobs[0].Prompt, "dirty edit")
	claimed, err := db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	pool := NewWorkerPool(db, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil)
	pool.processJob(testWorkerID, claimed)
	updated, err := db.GetJobByID(claimed.ID)
	require.NoError(t, err)
	require.Equal(t, storage.JobStatusDone, updated.Status)
	assert.Contains(t, handoff, strings.Repeat("a", 2000))
	assert.Contains(t, handoff, strings.Repeat("b", 2000))
}

func TestScheduledAnalysisRetriesFailedPass(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.go", "package a\n", "add source")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	cfg := server.configWatcher.Config()
	yes, interval := true, "1h"
	cfg.Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, Types: []string{"refactor"}}
	cfg.AnalyzeAgent = "no-such-agent"
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	now := time.Now()
	watcher.poll(t.Context(), now)
	last, err := db.AnalysisScheduleLastRun(t.Context(), registered.ID)
	require.NoError(t, err)
	assert.Equal(t, now.Unix(), last.Unix())
	cfg.AnalyzeAgent = "test"
	restarted := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	restarted.poll(t.Context(), now.Add(30*time.Second))
	jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	assert.Empty(t, jobs)
	watcher.poll(t.Context(), now.Add(30*time.Second))
	jobs, err = db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
}

func TestScheduledAnalysisBinaryAttributesAndManyBinaryFiles(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	files := map[string]string{".gitattributes": "a.go binary\n", "a.go": "ordinary text\n", "z.go": "package z\n"}
	for i := range 500 {
		files[fmt.Sprintf("binary/%04d.go", i)] = "\x00binary"
	}
	repo.CommitFiles(files, "add files")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	cfg := server.configWatcher.Config()
	yes, interval := true, "1h"
	cfg.Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, Types: []string{"refactor"}, Paths: []string{"*.go", "binary/"}}
	cfg.AnalyzeAgent = "test"
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	watcher.poll(t.Context(), time.Now())
	jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, []string{"z.go"}, jobs[0].AnalysisFiles)
}

func TestScheduledAnalysisPartialClone(t *testing.T) {
	server, db, _ := newTestServer(t)
	source := testutil.NewGitRepo(t)
	source.CommitFiles(map[string]string{"a.go": "package a\n", "excluded.go": "package excluded\n"}, "add files")
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		return string(out)
	}
	runGit(source.Path(), "config", "uploadpack.allowFilter", "true")
	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t.TempDir(), "clone", "--no-checkout", "--filter=blob:none", "file://"+source.Path(), clone)
	excluded := strings.TrimSpace(runGit(source.Path(), "rev-parse", "HEAD:excluded.go"))
	runGit(clone, "show", "HEAD:a.go")
	before := runGit(clone, "rev-list", "--objects", "--missing=print", "HEAD")
	require.Contains(t, before, "?"+excluded)
	registered, err := db.GetOrCreateRepo(clone)
	require.NoError(t, err)
	cfg := server.configWatcher.Config()
	yes, interval := true, "1h"
	cfg.Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, Types: []string{"refactor"}, Paths: []string{"*.go"}}
	cfg.AnalyzeAgent = "test"
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	watcher.poll(t.Context(), time.Now())
	jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, []string{"a.go"}, jobs[0].AnalysisFiles)
	after := runGit(clone, "rev-list", "--objects", "--missing=print", "HEAD")
	assert.Contains(t, after, "?"+excluded)
}

func TestScheduledAnalysisUnreadableBlob(t *testing.T) {
	server, db, _ := newTestServer(t)
	source := testutil.NewGitRepo(t)
	source.CommitFiles(map[string]string{"a.go": "package a\n", "b.go": "package b\n"}, "add files")
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		return string(out)
	}
	runGit(source.Path(), "config", "uploadpack.allowFilter", "true")
	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t.TempDir(), "clone", "--no-checkout", "--filter=blob:none", "file://"+source.Path(), clone)
	runGit(clone, "show", "HEAD:b.go")
	require.NoError(t, os.Rename(filepath.Join(source.Path(), ".git"), filepath.Join(source.Path(), "hidden-git")))
	registered, err := db.GetOrCreateRepo(clone)
	require.NoError(t, err)
	cfg := server.configWatcher.Config()
	yes, interval := true, "1h"
	cfg.Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, Types: []string{"refactor"}}
	cfg.AnalyzeAgent = "test"
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	watcher.poll(t.Context(), time.Now())
	jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, []string{"b.go"}, jobs[0].AnalysisFiles)
}
