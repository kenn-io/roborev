package daemon

import (
	"bytes"
	"context"
	"io"
	"log"
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

func TestScheduledPreemptionStartsReviewBeforeOlderTask(t *testing.T) {
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
	yes, interval, budget := true, "1h", 1
	server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}, Paths: []string{"*.go"}}
	server.configWatcher.Config().AnalyzeAgent = "test"
	server.workerPool.SetMaxWorkers(1)
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	watcher.poll(t.Context(), time.Now())
	_, err = db.ExecContext(t.Context(), `UPDATE review_jobs SET agent = ? WHERE source = ?`, "scheduled-order-check", storage.JobSourceScheduled)
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
}

func TestScheduledAnalysisTypeFailureDoesNotBlockOrRetryImmediately(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFiles(map[string]string{"a.go": "package a\n", "b.go": "package b\n"}, "add files")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	yes, interval, budget := true, "1h", 1
	server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor", "complexity"}}
	server.configWatcher.Config().AnalyzeAgent = "no-such-agent"
	server.configWatcher.Config().Analyze = map[string]config.AnalyzeConfig{"complexity": {Agent: "test"}}
	var output bytes.Buffer
	original := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(original) })
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
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
}

func TestScheduledYieldAfterReviewRetryDelay(t *testing.T) {
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
}

func TestScheduledYieldIgnoresRetiringWorker(t *testing.T) {
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
}

func TestScheduledAnalysisPreservesDirtyContentsAndKeepsFileDue(t *testing.T) {
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
	assert.Contains(t, jobs[0].Prompt, "package dirty")
	assert.Empty(t, jobs[0].AnalysisCommitSHA)
	repo.WriteFile("a.go", "package committed\n")
	captured := make(chan string, 1)
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "capture-scheduled-content", ReviewFn: func(_ context.Context, _, _, prompt string, _ io.Writer) (string, error) {
		captured <- prompt
		return "done", nil
	}})
	_, err = db.ExecContext(t.Context(), `UPDATE review_jobs SET agent = ? WHERE id = ?`, "capture-scheduled-content", jobs[0].ID)
	require.NoError(t, err)
	claimed, err := db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	server.workerPool.processJob(testWorkerID, claimed)
	assert.Contains(t, testutil.ReceiveWithTimeout(t, captured, workerStartTimeout), "package dirty")
	completed, err := db.GetJobByID(claimed.ID)
	require.NoError(t, err)
	require.Equal(t, storage.JobStatusDone, completed.Status)
	watcher.poll(t.Context(), now.Add(time.Hour))
	jobs, err = db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 1, "discarded edits must not count as analysis of the commit")
	assert.Equal(t, []string{"a.go"}, jobs[0].AnalysisFiles)
}

func TestScheduledAnalysisFailedTypeDoesNotStarveOtherFiles(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFiles(map[string]string{"a.go": "package a\n", "b.go": "package b\n"}, "add sources")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	yes, interval, budget := true, "1h", 1
	server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor", "complexity"}}
	server.configWatcher.Config().AnalyzeAgent = "test"
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
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
}

func TestScheduledAnalysisKeepsNonUTF8Filename(t *testing.T) {
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
}

func TestScheduledAnalysisDetectsDirtyEditAfterCommittedCoverage(t *testing.T) {
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
}
