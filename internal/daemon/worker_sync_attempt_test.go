package daemon

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"io"
	"sync"
	"testing"
	"testing/synctest"
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
		JobPrompt: remote.Prompt, JobEnqueuedAt: remote.EnqueuedAt, JobSourceMachineID: remote.SourceMachineID,
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

func TestCanceledClaimDoesNotStartAgent(t *testing.T) {
	t.Parallel()
	tc := newWorkerTestContext(t, 1)
	_, events := tc.Broadcaster.Subscribe("")
	invoked := false
	const name = "canceled-before-registration"
	agent.RegisterForTest(t, &agent.FakeAgent{
		NameStr: name,
		ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
			invoked = true
			return string(testutil.ReviewFixtureJSON("No issues found.")), nil
		},
	})
	job := tc.createAndClaimJobWithAgent(t, testutil.GetHeadSHA(t, tc.TmpDir), testWorkerID, name)
	require.NoError(t, tc.DB.CancelJob(job.ID))
	tc.Pool.processJob(testWorkerID, job)
	assert.False(t, invoked)
	current, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Empty(t, current.WorkerID)
	assert.Equal(t, storage.JobStatusCanceled, current.Status)
	require.Len(t, events, 1)
	assert.Equal(t, "review.canceled", (<-events).Type)
}

func TestCanceledWaitingClaimKeepsCallerEventOwnership(t *testing.T) {
	t.Parallel()
	tc := newWorkerTestContext(t, 1)
	_, events := tc.Broadcaster.Subscribe("")
	job := tc.createAndClaimJob(t, testutil.GetHeadSHA(t, tc.TmpDir), "old-worker")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	release, err := tc.DB.StartJobAttempt(ctx, job, "old-worker", cancel)
	require.NoError(t, err)
	tc.Pool.registerRunningJob(job.ID, cancel, time.Time{})
	changed, err := tc.DB.FailoverJob(job.ID, "old-worker", "backup", "")
	require.NoError(t, err)
	require.True(t, changed)
	next, err := tc.DB.ClaimJob("new-worker")
	require.NoError(t, err)
	require.NotNil(t, next)
	require.NoError(t, tc.DB.CancelJob(job.ID))
	require.True(t, tc.Pool.cancelJob(job.ID, true))
	tc.Pool.finishRunningJob(t.Context(), "old-worker", job.ID)
	release()
	tc.Pool.processJob("new-worker", next)
	assert.Empty(t, events, "the caller already emitted the cancellation event")
	current, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Empty(t, current.WorkerID)
	var invoked bool
	require.NoError(t, tc.DB.QueryRowContext(t.Context(), `SELECT agent_invoked FROM review_jobs WHERE id = ?`, job.ID).Scan(&invoked))
	assert.False(t, invoked)
}

func TestRejectedWaitingClaimReleasesCanceledOwnership(t *testing.T) {
	t.Parallel()
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, testutil.GetHeadSHA(t, tc.TmpDir), "old-worker")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	release, err := tc.DB.StartJobAttempt(ctx, job, "old-worker", cancel)
	require.NoError(t, err)
	tc.Pool.registerRunningJob(job.ID, cancel, time.Time{})
	changed, err := tc.DB.FailoverJob(job.ID, "old-worker", "backup", "")
	require.NoError(t, err)
	require.True(t, changed)
	next, err := tc.DB.ClaimJob("new-worker")
	require.NoError(t, err)
	require.NotNil(t, next)
	require.NoError(t, tc.DB.UpsertPulledJob(storage.PulledJob{
		UUID: *job.UUID, SourceMachineID: uuid.New(), GitRef: job.GitRef,
		Agent: "test", JobType: job.JobType, Status: "canceled",
		EnqueuedAt: job.EnqueuedAt.Add(time.Hour), UpdatedAt: time.Now(),
	}, job.RepoID, job.CommitID))
	tc.Pool.finishRunningJob(t.Context(), "old-worker", job.ID)
	release()
	tc.Pool.processJob("new-worker", next)
	current, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusCanceled, current.Status)
	assert.Empty(t, current.WorkerID)
	require.NoError(t, tc.DB.ReenqueueJob(job.ID, storage.ReenqueueOpts{}))
}

func TestWaitingClaimTimeoutCleansOnlyItsOwnClaim(t *testing.T) {
	t.Parallel()
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "canceled"}[canceled], func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				tc := newWorkerTestContext(t, 1)
				job := tc.createAndClaimJob(t, testutil.GetHeadSHA(t, tc.TmpDir), "old-worker")
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				release, err := tc.DB.StartJobAttempt(ctx, job, "old-worker", cancel)
				require.NoError(t, err)
				defer release()
				tc.Pool.registerRunningJob(job.ID, cancel, time.Time{})
				changed, err := tc.DB.FailoverJob(job.ID, "old-worker", "backup", "")
				require.NoError(t, err)
				require.True(t, changed)
				next, err := tc.DB.ClaimJob("new-worker")
				require.NoError(t, err)
				require.NotNil(t, next)
				if canceled {
					require.NoError(t, tc.DB.CancelJob(job.ID))
				}
				// Virtual time expires the new claim while the old one still
				// owns its output and cancellation registration.
				tc.Pool.processJob("new-worker", next)
				current, err := tc.DB.GetJobByID(job.ID)
				require.NoError(t, err)
				assert.Empty(t, current.WorkerID)
				if canceled {
					assert.Equal(t, storage.JobStatusCanceled, current.Status)
				} else {
					assert.Equal(t, storage.JobStatusQueued, current.Status)
					retries, err := tc.DB.GetJobRetryCount(job.ID)
					require.NoError(t, err)
					assert.Equal(t, 1, retries)
				}
				assert.True(t, tc.Pool.CancelJob(job.ID), "the previous handler must remain registered")
				require.ErrorIs(t, ctx.Err(), context.Canceled)
				tc.Pool.finishRunningJob(t.Context(), "old-worker", job.ID)
			})
		})
	}
}
