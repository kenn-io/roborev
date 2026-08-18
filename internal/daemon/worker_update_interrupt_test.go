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
)

func TestHandleUpdateInterruptionRequeuesAttemptWithoutRetry(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "update-interrupt", "worker-update")

	tc.Pool.InterruptJobsForUpdate([]int64{job.ID})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	requeued := tc.Pool.handleUpdateInterruption(ctx, "worker-update", job)

	assert.True(t, requeued)
	stored := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	assert.Equal(t, 0, stored.RetryCount)
	assert.Empty(t, stored.WorkerID)
	assert.Nil(t, stored.StartedAt)
}

func TestHandleUpdateInterruptionDoesNotOverrideUserCancel(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "update-user-cancel", "worker-update")

	tc.Pool.InterruptJobsForUpdate([]int64{job.ID})
	require.NoError(t, tc.DB.CancelJob(job.ID))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	requeued := tc.Pool.handleUpdateInterruption(ctx, "worker-update", job)

	assert.False(t, requeued)
	tc.assertJobStatus(t, job.ID, storage.JobStatusCanceled)
}

func TestRegisterRunningJobCancelsUpdateTarget(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "update-register-race", "worker-update")
	canceled := make(chan struct{})

	tc.Pool.InterruptJobsForUpdate([]int64{job.ID})
	tc.Pool.registerRunningJob(job.ID, func() { close(canceled) })

	require.True(t, waitForUpdateSignal(canceled, time.Second),
		"update target was not canceled during registration")
}

func TestUpdateInterruptionSuppressesCancellationSideEffects(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	started := make(chan struct{})
	agentName := "update-interrupt-agent"
	agent.Register(&agent.FakeAgent{
		NameStr: agentName,
		ReviewFn: func(ctx context.Context, _, _, _ string, _ io.Writer) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		},
	})
	t.Cleanup(func() { agent.Unregister(agentName) })

	sha := tc.GitRepo.CommitFile("update.txt", "update\n", "update side effects")
	job := tc.createAndClaimJobWithAgent(t, sha, "worker-update", agentName)
	_, events := tc.Broadcaster.Subscribe("")
	done := make(chan struct{})
	go func() {
		defer close(done)
		tc.Pool.processJob("worker-update", job)
	}()

	require.True(t, waitForUpdateSignal(started, 5*time.Second), "agent did not start")
	tc.Pool.InterruptJobsForUpdate([]int64{job.ID})
	require.True(t, waitForUpdateSignal(done, 5*time.Second), "worker did not unwind")

	stored := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	assert.Equal(t, 0, stored.RetryCount)
	for {
		select {
		case event := <-events:
			assert.NotContains(t, []string{"review.canceled", "review.failed", "review.completed"}, event.Type)
		default:
			return
		}
	}
}

func TestUpdateInterruptionDoesNotReleasePanelSynthesis(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	started := make(chan struct{})
	agentName := "update-panel-interrupt-agent"
	agent.Register(&agent.FakeAgent{
		NameStr: agentName,
		ReviewFn: func(ctx context.Context, _, _, _ string, _ io.Writer) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		},
	})
	t.Cleanup(func() { agent.Unregister(agentName) })

	runUUID, _, _ := enqueuePanelRun(t, tc, "update-panel", []memberSpec{
		{name: "member", agent: agentName},
	})
	job := claimNext(t, tc)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tc.Pool.processJob(testWorkerID, job)
	}()

	require.True(t, waitForUpdateSignal(started, 5*time.Second), "panel member did not start")
	tc.Pool.InterruptJobsForUpdate([]int64{job.ID})
	require.True(t, waitForUpdateSignal(done, 5*time.Second), "panel member did not unwind")

	tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	synthesis, err := tc.DB.GetSynthesisJob(runUUID)
	require.NoError(t, err)
	assert.True(t, synthesis.ClaimBlocked)
}

func TestUpdateInterruptionPreemptsQuotaCooldown(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJobWithAgent(t, "update-quota", "worker-update", "codex")
	tc.Pool.InterruptJobsForUpdate([]int64{job.ID})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tc.Pool.failOrRetryAgentContext(
		ctx, "worker-update", job, "codex", "quota exceeded",
	)

	tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	assert.False(t, tc.Pool.isAgentCoolingDown("codex"))
}

func TestUpdateInterruptionPreemptsClassifierTerminalPath(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "update-classifier", "worker-update")
	tc.Pool.InterruptJobsForUpdate([]int64{job.ID})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tc.Pool.completeClassifyAsSkipContext(
		ctx, "worker-update", job, "classifier failed", "provider error",
	)

	tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
}

func TestUpdateInterruptionPreemptsSynthesisCompletion(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "update-synthesis", "worker-update")
	tc.Pool.InterruptJobsForUpdate([]int64{job.ID})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tc.Pool.completeSynthesisContext(
		ctx, "worker-update", job, "test", "prompt", "No issues found.",
	)

	tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
}

func waitForUpdateSignal(ch <-chan struct{}, timeout time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	}
}
