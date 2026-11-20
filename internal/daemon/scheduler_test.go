package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

func TestScheduledAnalysisRetryAfterIntervalChange(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFiles(map[string]string{"a.go": "package a\n", "b.go": "package b\n"}, "add sources")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	yes := true
	interval := "1h"
	budget := 2
	server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}, Paths: []string{"*.go"}}
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[schedule]\nenabled = true\n[analyze.refactor]\nagent = 'test'\n"), 0o644))
	pending, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "refactor", Agent: "test", Prompt: "pending", AnalysisType: "refactor", AnalysisFiles: []string{"b.go"}, AnalysisCommitSHA: repo.HeadSHA()})
	require.NoError(t, err)
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
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

func TestScheduledAnalysisOldestBaselineFirst(t *testing.T) {
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
}

func TestScheduledAnalysisTrackedFilesAndDirectories(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFiles(map[string]string{"Dockerfile": "FROM scratch\n", "Makefile": "all:\n", "internal/a.go": "package a\n", "internal/data.bin": "binary\x00bytes"}, "add files")
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
	setJobStatus(t, db, jobs[0].ID, storage.JobStatusDone)
	server.configWatcher.Config().Schedule.Paths = []string{"internal/"}
	repo.CommitFiles(map[string]string{"internal/a.go": "package changed\n", "Dockerfile": "FROM other\n"}, "change files")
	watcher.poll(t.Context(), time.Now().Add(time.Hour))
	jobs, err = db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, []string{"internal/a.go"}, jobs[0].AnalysisFiles)
}

func TestScheduledAnalysisUsesRecordedCommit(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	base := repo.CommitFile("source.txt", "committed content\n", "add file")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	yes, interval, budget := true, "1h", 10
	server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}, Paths: []string{"*.txt"}}
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[schedule]\nenabled = true\n[analyze.refactor]\nagent = 'test'\n"), 0o644))
	captured := make(chan string, 1)
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "capture-committed-analysis", ReviewFn: func(_ context.Context, root, _, prompt string, _ io.Writer) (string, error) {
		if !strings.Contains(prompt, "source.txt") {
			return "unexpected file", nil
		}
		content, err := os.ReadFile(filepath.Join(root, "source.txt"))
		if err != nil {
			return "", err
		}
		captured <- string(content)
		return "done", nil
	}})
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	watcher.poll(t.Context(), time.Now())
	jobs, err := db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	job := jobs[0]
	assert.Equal(t, base, job.AnalysisCommitSHA)
	assert.Contains(t, job.Prompt, "source.txt")
	assert.NotContains(t, job.Prompt, "committed content")
	repo.CommitFile("source.txt", "new HEAD", "advance HEAD")
	repo.WriteFile("source.txt", "dirty contents")
	_, err = db.ExecContext(t.Context(), `UPDATE review_jobs SET agent = ? WHERE id = ?`, "capture-committed-analysis", job.ID)
	require.NoError(t, err)
	claimed, err := db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, job.ID, claimed.ID)
	server.workerPool.processJob(testWorkerID, claimed)
	assert.Equal(t, "committed content\n", testutil.ReceiveWithTimeout(t, captured, workerStartTimeout))
	completed, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusDone, completed.Status)
	assert.Equal(t, base, completed.AnalysisCommitSHA)
}

func TestScheduledAnalysisYieldsWorkerToReview(t *testing.T) {
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
	yes, interval, budget := true, "1h", 1
	server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}, Paths: []string{"*.go"}}
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[schedule]\nenabled = true\n[analyze.refactor]\nagent = 'test'\n"), 0o644))
	server.workerPool.SetMaxWorkers(1)
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	watcher.poll(t.Context(), time.Now())
	_, err = db.ExecContext(t.Context(), `UPDATE review_jobs SET agent = ? WHERE source = ?`, "scheduled-blocking", storage.JobSourceScheduled)
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
	blocked, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: repo.HeadSHA(), Agent: "test", JobType: storage.JobTypeReview, ClaimBlocked: true})
	require.NoError(t, err)
	server.workerPool.wake()
	current, err = db.GetJobByID(scheduled[0].ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusRunning, current.Status)
	require.NoError(t, db.CancelJob(blocked.ID))
	require.NoError(t, db.SetQueuePaused(true))
	paused, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: repo.HeadSHA(), Agent: "test", JobType: storage.JobTypeReview})
	require.NoError(t, err)
	server.workerPool.wake()
	current, err = db.GetJobByID(scheduled[0].ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusRunning, current.Status)
	require.NoError(t, db.CancelJob(paused.ID))
	require.NoError(t, db.SetQueuePaused(false))
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
}

func TestScheduledWakeDoesNotCancelAnotherWorkersClaim(t *testing.T) {
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
	server.workerPool.runningJobsMu.Lock()
	delete(server.workerPool.workerJobs, "worker-1")
	server.workerPool.workerJobs["worker-0"] = job.ID
	server.workerPool.workerJobs["worker-1"] = workerClaimingJobID
	server.workerPool.runningJobsMu.Unlock()
	server.workerPool.SetMaxWorkers(1)
	server.workerPool.wake()
	current, err = db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusRunning, current.Status)
}

func TestScheduledCancellationMatchesReviewDemand(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	var jobs []*storage.ReviewJob
	for i := range 3 {
		job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: registered.ID, GitRef: "analysis", Agent: "test", Prompt: "analysis", Source: storage.JobSourceScheduled})
		require.NoError(t, err)
		claimed, err := db.ClaimJob(fmt.Sprintf("worker-%d", i))
		require.NoError(t, err)
		require.Equal(t, job.ID, claimed.ID)
		jobs = append(jobs, job)
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
}

func TestScheduledAnalysisRetriesPartialEnqueue(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFiles(map[string]string{"a.go": "package a\n", "b.go": "package b\n"}, "add files")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	yes, interval, budget := true, "1h", 10
	server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor", "complexity"}, Paths: []string{"*.go"}}
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[schedule]\nenabled = true\n[analyze.refactor]\nagent = 'test'\n[analyze.complexity]\nagent = 'test'\n"), 0o644))
	_, err = db.ExecContext(t.Context(), `CREATE TRIGGER fail_complexity BEFORE INSERT ON review_jobs WHEN NEW.analysis_type = 'complexity' BEGIN SELECT RAISE(FAIL, 'temporary'); END`)
	require.NoError(t, err)
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	now := time.Now()
	watcher.poll(t.Context(), now)
	jobs, err := db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	_, err = db.ExecContext(t.Context(), `DROP TRIGGER fail_complexity`)
	require.NoError(t, err)
	watcher.poll(t.Context(), now.Add(time.Second))
	jobs, err = db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 2)
	assert.Equal(t, "complexity", jobs[0].AnalysisType)
	assert.Equal(t, "refactor", jobs[1].AnalysisType)
	assert.Equal(t, []string{"a.go", "b.go"}, jobs[0].AnalysisFiles)
	assert.Equal(t, []string{"a.go", "b.go"}, jobs[1].AnalysisFiles)
}

func TestScheduledAnalysisDoesNotReadBlobs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell wrapper")
	}
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFiles(map[string]string{"a.txt": "unchanged", "b.bin": "\x00binary", "c.txt": "old"}, "add files")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	yes, interval, budget := true, "1h", 10
	server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}}
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[schedule]\nenabled = true\n[analyze.refactor]\nagent = 'test'\n"), 0o644))
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	dir, logPath := t.TempDir(), filepath.Join(t.TempDir(), "reads")
	wrapper := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in cat-file|show|hash-object) echo \"$*\" >> %q;; esac\nexec %q \"$@\"\n", logPath, realGit)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(wrapper), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	require.NoError(t, os.WriteFile(logPath, nil, 0o644))
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	now := time.Now()
	watcher.poll(t.Context(), now)
	reads, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Empty(t, reads)
	jobs, err := db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	for _, job := range jobs {
		setJobStatus(t, db, job.ID, storage.JobStatusDone)
	}
	repo.CommitFile("c.txt", "new", "change text")
	require.NoError(t, os.WriteFile(logPath, nil, 0o644))
	watcher.poll(t.Context(), now.Add(time.Hour))
	reads, err = os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Empty(t, reads)
}

func TestScheduledAnalysisShutdown(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.go", "package a\n", "add file")
	_, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	yes, interval, budget := true, "1h", 1
	server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}, Paths: []string{"*.go"}}
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[schedule]\nenabled = true\n[analyze.refactor]\nagent = 'test'\n"), 0o644))
	server.startAnalysisScheduler(t.Context())
	require.Eventually(t, func() bool { jobs, err := db.ListJobs("", "", 0, 0); return err == nil && len(jobs) == 1 }, workerStartTimeout, 10*time.Millisecond)
	server.stopAnalysisScheduler()
	server.scheduleMu.Lock()
	assert.Nil(t, server.scheduleCancel)
	assert.Nil(t, server.scheduleDone)
	server.scheduleMu.Unlock()
}

func TestScheduledAnalysisBundlesTypesAndLiteralStar(t *testing.T) {
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
}

func TestScheduledWakeYieldsToLegacyCommitReview(t *testing.T) {
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
}

func TestScheduledAnalysisRestartRespectsInterval(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFiles(map[string]string{"a.go": "package a\n", "b.go": "package b\n"}, "add files")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	yes, interval, budget := true, "1h", 1
	server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}, Paths: []string{"*.go"}}
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

	repo.CommitFile("c.go", "package c\n", "add file")
	no := false
	server.configWatcher.Config().Schedule.Enabled = &no
	restarted.poll(t.Context(), now.Add(time.Hour+5*time.Minute))
	server.configWatcher.Config().Schedule.Enabled = &yes
	restarted = &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	restarted.poll(t.Context(), now.Add(time.Hour+5*time.Minute))
	jobs, err = db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 3)
	assert.Equal(t, []string{"c.go"}, jobs[0].AnalysisFiles)
}

func TestScheduledAnalysisBacksOffInvalidRepository(t *testing.T) {
	server, db, _ := newTestServer(t)
	path := t.TempDir()
	registered, err := db.GetOrCreateRepo(path)
	require.NoError(t, err)
	yes, interval, budget := true, "1h", 1
	server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}}
	server.configWatcher.Config().AnalyzeAgent = "test"

	var output bytes.Buffer
	original := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(original) })
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	now := time.Now()
	watcher.poll(t.Context(), now)
	watcher.poll(t.Context(), now.Add(30*time.Second))
	require.Equal(t, 1, strings.Count(output.String(), "analysis scheduler "+path+":"))

	testutil.InitTestGitRepo(t, path)
	watcher.poll(t.Context(), now.Add(5*time.Minute))
	jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, []string{"test.txt"}, jobs[0].AnalysisFiles)
}
