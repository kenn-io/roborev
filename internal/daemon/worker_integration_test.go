//go:build integration

package daemon

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// waitForTerminalEvent returns the completion or failure event the pool
// broadcasts for jobID after writing the job's final state.
func waitForTerminalEvent(t *testing.T, events <-chan Event, jobID int64) Event {
	t.Helper()
	for {
		event := testutil.ReceiveWithTimeout(t, events, 10*time.Second)
		if event.JobID == jobID && (event.Type == "review.completed" || event.Type == "review.failed") {
			return event
		}
	}
}

func TestWorkerPoolE2E(t *testing.T) {
	tc := newWorkerTestContext(t, 2)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createJob(t, sha)
	subID, events := tc.Broadcaster.Subscribe("")
	defer tc.Broadcaster.Unsubscribe(subID)

	tc.Pool.Start()
	defer tc.Pool.Stop()
	event := waitForTerminalEvent(t, events, job.ID)
	require.Equal(t, "review.completed", event.Type, "job failed: %s", event.Error)
	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)

	review, err := tc.DB.GetReviewByCommitSHA(sha)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetReviewByCommitSHA failed: %v", err)
	}
	if review.Agent != "test" {
		assert.Condition(t, func() bool {
			return false
		}, "Expected agent 'test', got '%s'", review.Agent)
	}
	if review.Output == "" {
		assert.Condition(t, func() bool {
			return false
		}, "Review output should not be empty")
	}
}

func TestWorkerPoolCancelRunningJob(t *testing.T) {
	originalTestAgent, err := agent.Get("test")
	require.NoError(t, err)
	started := make(chan struct{}, 1)
	agent.Register(&agent.FakeAgent{
		NameStr: "test",
		ReviewFn: func(ctx context.Context, _, _, _ string, _ io.Writer) (string, error) {
			started <- struct{}{}
			<-ctx.Done()
			return "", ctx.Err()
		},
	})
	t.Cleanup(func() { agent.Register(originalTestAgent) })

	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createJob(t, sha)

	tc.Pool.Start()
	defer tc.Pool.Stop()

	// The agent runs only after the worker claims the job.
	testutil.ReceiveWithTimeout(t, started, 10*time.Second)
	tc.assertJobStatus(t, job.ID, storage.JobStatusRunning)

	// Cancel the job
	if err := tc.DB.CancelJob(job.ID); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "CancelJob failed: %v", err)
	}
	tc.Pool.CancelJob(job.ID)

	// Stop waits for the worker to finish handling the canceled job.
	tc.Pool.Stop()
	tc.assertJobStatus(t, job.ID, storage.JobStatusCanceled)

	_, err = tc.DB.GetReviewByJobID(job.ID)
	if err == nil {
		assert.Condition(t, func() bool {
			return false
		}, "Expected no review for canceled job, but found one")
	}
}
