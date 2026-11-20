package daemon

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestScheduledCheckoutSkipsUnselectedLFSContent(t *testing.T) {
	server, _, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFiles(map[string]string{"selected.go": "package selected\n", ".gitattributes": "*.bin filter=lfs\n", "unselected.bin": "pointer\n"}, "add files")
	repo.Run("config", "filter.lfs.smudge", "false")
	repo.Run("config", "filter.lfs.required", "true")
	path, cleanup, err := server.workerPool.createExactCheckout(t.Context(), testWorkerID, &storage.ReviewJob{ID: 1, RepoPath: repo.Path(), Source: storage.JobSourceScheduled, AnalysisCommitSHA: repo.HeadSHA()}, gitcmd.New())
	require.NoError(t, err)
	t.Cleanup(cleanup)
	content, err := os.ReadFile(filepath.Join(path, "selected.go"))
	require.NoError(t, err)
	assert.Equal(t, "package selected\n", string(content))
}

func TestScheduledCheckoutIgnoresUnavailableUnselectedSubmodule(t *testing.T) {
	server, db, _ := newTestServer(t)
	submodule := testutil.NewGitRepo(t)
	subSHA := submodule.CommitFile("sub.txt", "submodule\n", "add file")
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("selected.go", "package selected\n", "add file")
	repo.WriteFile(".gitmodules", "[submodule \"vendor/sub\"]\n\tpath = vendor/sub\n\turl = file:///roborev-test-unavailable/sub.git\n")
	repo.Run("add", ".gitmodules")
	repo.Run("update-index", "--add", "--cacheinfo", "160000,"+subSHA+",vendor/sub")
	repo.Run("commit", "-m", "add unavailable submodule")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	yes, interval, budget := true, "1h", 1
	server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}, Paths: []string{"selected.go"}}
	server.configWatcher.Config().AnalyzeAgent = "test"
	content := make(chan string, 1)
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "scheduled-submodule-check", ReviewFn: func(_ context.Context, root, _, _ string, _ io.Writer) (string, error) {
		data, err := os.ReadFile(filepath.Join(root, "selected.go"))
		if err == nil {
			content <- string(data)
		}
		return "done", err
	}})
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	watcher.poll(t.Context(), time.Now())
	jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	_, err = db.ExecContext(t.Context(), `UPDATE review_jobs SET agent = ? WHERE id = ?`, "scheduled-submodule-check", jobs[0].ID)
	require.NoError(t, err)
	claimed, err := db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	server.workerPool.processJob(testWorkerID, claimed)
	finished, err := db.GetJobByID(claimed.ID)
	require.NoError(t, err)
	require.Equal(t, storage.JobStatusDone, finished.Status, "job: %+v", finished)
	assert.Equal(t, "package selected\n", testutil.ReceiveWithTimeout(t, content, workerStartTimeout))
}

func TestScheduledAnalysisPreservesInvalidUTF8Filename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows filenames must be valid UTF-8")
	}
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	name := "invalid-\xff.go"
	repo.CommitFile(name, "package example\n", "add file")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	yes, interval, budget := true, "1h", 1
	server.configWatcher.Config().Schedule = config.ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &budget, Types: []string{"refactor"}}
	server.configWatcher.Config().AnalyzeAgent = "test"
	watcher := &analysisScheduler{server: server, lastRun: map[int64]time.Time{}}
	watcher.poll(t.Context(), time.Now())
	jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, []string{name}, jobs[0].AnalysisFiles)
	stored, err := db.GetJobByID(jobs[0].ID)
	require.NoError(t, err)
	assert.Contains(t, stored.Prompt, name)
	filtered, err := db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}), storage.WithAnalysisFile(name))
	require.NoError(t, err)
	require.Len(t, filtered, 1)
	assert.Equal(t, stored.ID, filtered[0].ID)
}

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
	repo.CommitFile("a.go", "package a\n", "add file")
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
	jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithRepoIDs([]int64{registered.ID}))
	require.NoError(t, err)
	if assert.Len(t, jobs, 1) {
		assert.Equal(t, "complexity", jobs[0].AnalysisType)
	}
	assert.Equal(t, 1, strings.Count(output.String(), "analysis scheduler "+repo.Path()+":"), "logs: %s", output.String())
}
