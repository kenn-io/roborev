package daemon

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"io"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestSyncReplacementStopsBlockedWorker(t *testing.T) {
	t.Parallel()
	tc := newWorkerTestContext(t, 1)
	cfg := config.DefaultConfig()
	cfg.AutoClosePassingReviews = true
	tc.reconfigurePool(cfg)
	_, events := tc.Broadcaster.Subscribe("")
	started, canceled, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseAgent := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() { releaseAgent(); <-finished })
	const agentName = "sync-replaced-worker"
	agent.RegisterForTest(t, &agent.FakeAgent{
		NameStr: agentName,
		ReviewFn: func(ctx context.Context, _, _, _ string, _ io.Writer) (string, error) {
			close(started)
			select {
			case <-ctx.Done():
				close(canceled)
			case <-release:
			}
			<-release
			// An adapter can still return a successful result while unwinding.
			return string(testutil.ReviewFixtureJSON("No issues found.")), nil
		},
	})
	job := tc.createAndClaimJobWithAgent(t, testutil.GetHeadSHA(t, tc.TmpDir), testWorkerID, agentName)
	go func() { defer close(finished); tc.Pool.processJob(testWorkerID, job) }()
	<-started
	remote := storage.PulledJob{
		UUID: *job.UUID, SourceMachineID: uuid.New(), GitRef: job.GitRef,
		Agent: "remote-agent", JobType: job.JobType, Status: "done", Prompt: "newer prompt",
		EnqueuedAt: job.EnqueuedAt.Add(time.Hour), UpdatedAt: time.Now(),
	}
	require.NoError(t, tc.DB.UpsertPulledJob(remote, tc.Repo.ID, job.CommitID))
	require.NoError(t, tc.DB.UpsertPulledReview(storage.PulledReview{
		UUID: uuid.New(), JobUUID: remote.UUID, Agent: remote.Agent,
		JobPrompt: remote.Prompt, JobEnqueuedAt: remote.EnqueuedAt,
		StructuredOutput: jsontext.Value(`{"schema_version":2,"summary":"Newer failure.","verdict":"fail","findings":[{"severity":"high","problem":"Missing record.","fix":"Keep the record.","location":null}]}`),
		CreatedAt:        time.Now(), UpdatedAt: time.Now(), UpdatedByMachineID: remote.SourceMachineID,
	}))
	// The process is still unwinding; a rerun must not reuse its stream or
	// cancellation registration until the old worker has finished cleanup.
	require.ErrorIs(t, tc.DB.ReenqueueJob(job.ID, storage.ReenqueueOpts{}), sql.ErrNoRows)
	// The worker executes real Git and SQLite I/O outside virtual time.
	assert.True(t, waitForUpdateSignal(canceled, time.Second), "sync must cancel the displaced worker")
	releaseAgent()
	<-finished
	current, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "newer prompt", current.Prompt)
	review, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assert.False(t, review.Closed, "an obsolete passing result must not close the newer failure")
	assert.Equal(t, "Newer failure.", review.StructuredOutput["summary"])
	for len(events) > 0 {
		event := <-events
		assert.NotContains(t, []string{"review.completed", "review.closed", "review.canceled"}, event.Type)
	}
	require.NoError(t, tc.DB.ReenqueueJob(job.ID, storage.ReenqueueOpts{}))
}
