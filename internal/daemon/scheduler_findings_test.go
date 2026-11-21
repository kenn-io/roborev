package daemon

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

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
}
