package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
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
	server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &no, Interval: &interval, MaxFiles: &maxFiles, Types: []string{"refactor"}, Paths: []string{"*.go"}}
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	now := time.Now()
	watcher.poll(t.Context(), now)
	jobs, err := db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Empty(t, jobs)
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("analyze_agent = 'test'\nanalyze_model = 'cheap'\n[schedule]\nenabled = true\n"), 0o644))
	repo.WriteFile("a.go", "dirty edit\n")
	watcher.poll(t.Context(), now)
	jobs, err = db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, []string{"a.go"}, jobs[0].AnalysisFiles)
	assert.Equal(t, "test", jobs[0].Agent)
	assert.Equal(t, "cheap", jobs[0].Model)
	assert.Contains(t, jobs[0].Prompt, "a.go")
	review, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "HEAD", Agent: "test"})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("worker")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, review.ID, claimed.ID)
	setJobStatus(t, db, claimed.ID, storage.JobStatusDone)
	running, err := db.ClaimJob("worker")
	require.NoError(t, err)
	require.NotNil(t, running)
	assert.Equal(t, storage.JobSourceScheduled, running.Source)
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
		server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}, Paths: []string{"*.go"}}
		require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[schedule]\nenabled = true\n[analyze.refactor]\nagent = 'test'\n"), 0o644))
		server.startAnalysisScheduler(t.Context())
		require.Eventually(t, func() bool { jobs, err := db.ListJobs("", "", 0, 0); return err == nil && len(jobs) == 1 }, workerStartTimeout, 10*time.Millisecond)
		server.scheduleMu.Lock()
		done := server.scheduleDone
		server.scheduleMu.Unlock()
		server.stopAnalysisScheduler()
		select {
		case <-done:
		default:
			t.Fatal("scheduler has not stopped")
		}
		jobs, err := db.ListJobs("", "", 0, 0)
		require.NoError(t, err)
		setJobStatus(t, db, jobs[0].ID, storage.JobStatusDone)
		repo.CommitFile("a.go", "package changed\n", "change file")
		time.Sleep(time.Minute)
		jobs, err = db.ListJobs("", "", 0, 0)
		require.NoError(t, err)
		assert.Len(t, jobs, 1)
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
		assert.ElementsMatch(t, []string{"Dockerfile", "Makefile", "internal/a.go", "internal/data.bin"}, jobs[0].AnalysisFiles)
		assert.Contains(t, jobs[0].Prompt, "YmluYXJ5/2J5dGVz")
		setJobStatus(t, db, jobs[0].ID, storage.JobStatusDone)
		server.configWatcher.Config().Schedule.Paths = []string{"internal/"}
		repo.CommitFiles(map[string]string{"internal/a.go": "package changed\n", "Dockerfile": "FROM other\n"}, "change files")
		watcher.poll(t.Context(), time.Now().Add(time.Hour))
		jobs, err = db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 1)
		assert.Equal(t, []string{"internal/a.go"}, jobs[0].AnalysisFiles)
	})
	t.Run("discarded dirty content stays due", func(t *testing.T) {
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		repo.CommitFile("a.go", "package committed\n", "add source")
		registered, err := db.GetOrCreateRepo(repo.Path())
		require.NoError(t, err)
		yes, interval, budget := true, "1h", 1
		server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}}
		server.configWatcher.Config().AnalyzeAgent = "test"
		repo.WriteFile("a.go", "package dirty\n")
		watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
		now := time.Now()
		watcher.poll(t.Context(), now)
		jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 1)
		setJobStatus(t, db, jobs[0].ID, storage.JobStatusDone)
		repo.WriteFile("a.go", "package committed\n")
		watcher.poll(t.Context(), now.Add(time.Hour))
		jobs, err = db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 1, "discarded edits must not count as analysis of the commit")
		assert.Equal(t, []string{"a.go"}, jobs[0].AnalysisFiles)
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
		server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}, Paths: []string{"*.go"}}
		require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[schedule]\nenabled = true\n[analyze.refactor]\nagent = 'test'\n"), 0o644))
		watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
		watcher.poll(t.Context(), time.Now())
		jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 1)
		assert.Equal(t, []string{"b.go", "c.go"}, jobs[0].AnalysisFiles)
	})
	t.Run("AnalysisBundlesTypesAndLiteralStar", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("asterisk is not a valid Windows filename")
		}
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		head := repo.CommitFiles(map[string]string{"*": "original\n", "normal.go": "package normal\n"}, "add files")
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
			assert.Equal(t, []string{"*", "normal.go"}, job.AnalysisFiles)
			assert.Equal(t, head, job.AnalysisCommitSHA)
			setJobStatus(t, db, job.ID, storage.JobStatusDone)
		}
		repo.CommitFile("*", "changed\n", "change star")
		watcher.poll(t.Context(), now.Add(time.Hour))
		jobs, err = db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 2)
		for _, job := range jobs {
			assert.Equal(t, []string{"*"}, job.AnalysisFiles)
			setJobStatus(t, db, job.ID, storage.JobStatusDone)
		}
		_, err = db.ExecContext(t.Context(), `UPDATE review_jobs SET analysis_commit_sha = ? WHERE repo_id = ?`, "0000000000000000000000000000000000000001", registered.ID)
		require.NoError(t, err)
		watcher.poll(t.Context(), now.Add(2*time.Hour))
		jobs, err = db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 2)
		for _, job := range jobs {
			assert.Equal(t, []string{"*", "normal.go"}, job.AnalysisFiles)
		}
	})
	t.Run("AnalysisKeepsNonUTF8Filename", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("fixture requires a filesystem that accepts arbitrary filename bytes")
		}
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		name := "invalid-\xff.go"
		repo.CommitFile(name, "package example\n", "add source")
		registered, err := db.GetOrCreateRepo(repo.Path())
		require.NoError(t, err)
		yes, interval, budget := true, "1h", 1
		server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}}
		server.configWatcher.Config().AnalyzeAgent = "test"
		watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
		schedule, err := config.ResolveSchedule(server.configWatcher.Config().Schedule, config.ScheduleConfig{})
		require.NoError(t, err)
		require.NoError(t, watcher.scheduleRepo(t.Context(), *registered, &config.RepoConfig{}, server.configWatcher.Config(), schedule))
		jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}), storage.WithAnalysisFile(name))
		require.NoError(t, err)
		require.Len(t, jobs, 1)
		assert.Equal(t, []string{name}, jobs[0].AnalysisFiles)
		assert.Contains(t, jobs[0].Prompt, `invalid-\xff.go`)
		assert.Contains(t, jobs[0].Prompt, "package example")
	})
	t.Run("AnalysisIncludesDeletedTrackedFile", func(t *testing.T) {
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		repo.CommitFiles(map[string]string{"a.go": "package committedA\n", "b.go": "package committedB\n"}, "add sources")
		registered, err := db.GetOrCreateRepo(repo.Path())
		require.NoError(t, err)
		require.NoError(t, os.Remove(filepath.Join(repo.Path(), "a.go")))
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
	})
	t.Run("AnalysisDetectsDirtyEditAfterCommittedCoverage", func(t *testing.T) {
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		repo.CommitFile("a.go", "package committed\n", "add source")
		registered, err := db.GetOrCreateRepo(repo.Path())
		require.NoError(t, err)
		yes, interval, budget := true, "1h", 1
		server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}}
		server.configWatcher.Config().AnalyzeAgent = "test"
		watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
		now := time.Now()
		watcher.poll(t.Context(), now)
		jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 1)
		setJobStatus(t, db, jobs[0].ID, storage.JobStatusDone)
		repo.WriteFile("a.go", "package dirty\n")
		watcher.poll(t.Context(), now.Add(time.Hour))
		jobs, err = db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 1)
		assert.Contains(t, jobs[0].Prompt, "package dirty")
		assert.Empty(t, jobs[0].AnalysisCommitSHA)
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
		server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}, Paths: []string{"*.go"}}
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
		server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor", "complexity"}, Paths: []string{"*.go"}}
		require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[schedule]\nenabled = true\n[analyze.refactor]\nagent = 'test'\n[analyze.complexity]\nagent = 'test'\n"), 0o644))
		_, err = db.ExecContext(t.Context(), `CREATE TRIGGER fail_complexity BEFORE INSERT ON review_jobs WHEN NEW.analysis_type = 'complexity' BEGIN SELECT RAISE(FAIL, 'temporary'); END`)
		require.NoError(t, err)
		now := time.Now()
		watcher.poll(t.Context(), now)
		jobs, err := db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 1)
		_, err = db.ExecContext(t.Context(), `DROP TRIGGER fail_complexity`)
		require.NoError(t, err)
		watcher.poll(t.Context(), now.Add(time.Hour))
		jobs, err = db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		require.Len(t, jobs, 2)
		assert.Equal(t, "complexity", jobs[0].AnalysisType)
		assert.Equal(t, "refactor", jobs[1].AnalysisType)
		assert.Equal(t, []string{"a.go", "b.go"}, jobs[0].AnalysisFiles)
		assert.Equal(t, []string{"a.go", "b.go"}, jobs[1].AnalysisFiles)
	})
	t.Run("AnalysisTypeFailureDoesNotBlockOrRetryImmediately", func(t *testing.T) {
		server, db, _, registered, watcher := setup(t)
		var err error
		yes, interval, budget := true, "1h", 1
		server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor", "complexity"}}
		server.configWatcher.Config().AnalyzeAgent = "no-such-agent"
		server.configWatcher.Config().Analyze = map[string]config.AnalyzeConfig{"complexity": {Agent: "test"}}
		var output bytes.Buffer
		original := log.Writer()
		log.SetOutput(&output)
		t.Cleanup(func() { log.SetOutput(original) })
		now := time.Now()
		watcher.poll(t.Context(), now)
		watcher.poll(t.Context(), now.Add(30*time.Second))
		jobs, err := db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
		require.NoError(t, err)
		if assert.Len(t, jobs, 1) {
			assert.Equal(t, "complexity", jobs[0].AnalysisType)
		}
		assert.Equal(t, 1, strings.Count(output.String(), "analysis scheduler "+registered.RootPath+":"), "logs: %s", output.String())
		require.Len(t, jobs, 1)
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
	t.Run("WakeYieldsToLegacyCommitReview", func(t *testing.T) {
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		sha := repo.CommitFile("a.go", "package a\n", "add file")
		registered, err := db.GetOrCreateRepo(repo.Path())
		require.NoError(t, err)
		commit, err := db.GetOrCreateCommit(registered.ID, sha, "author", "add file", time.Now())
		require.NoError(t, err)

		scheduled, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "refactor", Agent: "test", Prompt: "analysis", Source: storage.JobSourceScheduled})
		require.NoError(t, err)
		claimed, err := db.ClaimJob("worker-0")
		require.NoError(t, err)
		require.Equal(t, scheduled.ID, claimed.ID)
		server.workerPool.SetMaxWorkers(1)
		server.workerPool.runningJobsMu.Lock()
		server.workerPool.workerJobs["worker-0"] = scheduled.ID
		server.workerPool.runningJobsMu.Unlock()

		_, err = db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "refactor", Agent: "test", Prompt: "analysis", Source: storage.JobSourceScheduled})
		require.NoError(t, err)
		review, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, CommitID: commit.ID, GitRef: sha, Agent: "test", JobType: storage.JobTypeReview})
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), `UPDATE review_jobs SET job_type = '' WHERE id = ?`, review.ID)
		require.NoError(t, err)

		server.workerPool.wake()
		current, err := db.GetJobByID(scheduled.ID)
		require.NoError(t, err)
		require.Equal(t, storage.JobStatusCanceled, current.Status)
		claimed, err = db.ClaimJob("worker-0")
		require.NoError(t, err)
		require.NotNil(t, claimed)
		assert.Equal(t, review.ID, claimed.ID)
	})
}

func TestScheduledAnalysisYieldsWorkerToReview(t *testing.T) {
	t.Run("review interrupts analysis", func(t *testing.T) {
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		repo.CommitFile("a.go", "package a\n", "add file")
		registered, err := db.GetOrCreateRepo(repo.Path())
		require.NoError(t, err)
		started := make(chan struct{})
		interrupted := make(chan struct{})
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "scheduled-blocking", ReviewFn: func(ctx context.Context, _, _, prompt string, _ io.Writer) (string, error) {
			if !strings.Contains(prompt, "## Analysis Request") {
				return `{"schema_version":3,"summary":"No issues found.","verdict":"pass","findings":[]}`, nil
			}
			close(started)
			select {
			case <-ctx.Done():
				close(interrupted)
				return "", ctx.Err()
			case <-release:
				return "done", nil
			}
		}})
		server.workerPool.SetMaxWorkers(1)
		_, err = db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "refactor", Agent: "scheduled-blocking", Prompt: "## Analysis Request", AnalysisType: "refactor", Source: storage.JobSourceScheduled})
		require.NoError(t, err)
		server.workerPool.Start()
		testutil.ReceiveWithTimeout(t, started, workerStartTimeout)
		scheduled, err := db.ListJobs("running", "", 0, 0, storage.WithAnalysisType("refactor"))
		require.NoError(t, err)
		require.Len(t, scheduled, 1)
		server.workerPool.wake()
		current, err := db.GetJobByID(scheduled[0].ID)
		require.NoError(t, err)
		assert.Equal(t, storage.JobStatusRunning, current.Status)
		review := enqueueViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), GitRef: repo.HeadSHA(), Agent: "test", Source: storage.JobSourceCI})
		testutil.ReceiveWithTimeout(t, interrupted, workerStartTimeout)
		require.Eventually(t, func() bool {
			job, err := db.GetJobByID(review.ID)
			return err == nil && (job.Status == storage.JobStatusRunning || job.Status == storage.JobStatusDone)
		}, workerStartTimeout, 20*time.Millisecond)
		jobs, err := db.ListJobs("canceled", "", 0, 0, storage.WithAnalysisType("refactor"))
		require.NoError(t, err)
		require.Len(t, jobs, 1)
		assert.Equal(t, storage.JobSourceScheduled, jobs[0].Source)
	})
	t.Run("PreemptionStartsReviewBeforeOlderTask", func(t *testing.T) {
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		repo.CommitFile("a.go", "package a\n", "add file")
		registered, err := db.GetOrCreateRepo(repo.Path())
		require.NoError(t, err)
		started := make(chan struct{})
		foreground := make(chan struct{}, 1)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "scheduled-order-check", ReviewFn: func(ctx context.Context, _, _, prompt string, _ io.Writer) (string, error) {
			if strings.Contains(prompt, "## Analysis Request") {
				close(started)
				<-ctx.Done()
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
		_, err = db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "refactor", Agent: "scheduled-order-check", Prompt: "## Analysis Request", Source: storage.JobSourceScheduled})
		require.NoError(t, err)
		server.workerPool.Start()
		testutil.ReceiveWithTimeout(t, started, workerStartTimeout)
		task, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "task", Prompt: "older task", Agent: "scheduled-order-check", JobType: storage.JobTypeTask})
		require.NoError(t, err)
		review, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: repo.HeadSHA(), Agent: "scheduled-order-check", JobType: storage.JobTypeReview})
		require.NoError(t, err)
		server.workerPool.wake()
		testutil.ReceiveWithTimeout(t, foreground, workerStartTimeout)
		currentReview, err := db.GetJobByID(review.ID)
		require.NoError(t, err)
		currentTask, err := db.GetJobByID(task.ID)
		require.NoError(t, err)
		assert.Equal(t, storage.JobStatusRunning, currentReview.Status)
		assert.Equal(t, storage.JobStatusQueued, currentTask.Status)
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
			time.Sleep(499 * time.Millisecond)
			select {
			case <-interrupted:
				t.Fatal("scheduled analysis yielded before the review became eligible")
			default:
			}
			testutil.ReceiveWithTimeout(t, interrupted, workerStartTimeout)
			testutil.ReceiveWithTimeout(t, reviewStarted, workerStartTimeout)
			current, err := db.GetJobByID(review.ID)
			require.NoError(t, err)
			assert.Equal(t, storage.JobStatusRunning, current.Status)
		})
	})
}
