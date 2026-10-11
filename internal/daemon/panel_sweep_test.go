package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// stuckPanelMember builds an EnqueueOpts for a panel member targeting the
// given repo/commit, all sharing runUUID.
func stuckPanelMember(repoID, commitID int64, runUUID *uuid.UUID, index int) storage.EnqueueOpts {
	return storage.EnqueueOpts{
		RepoID:           repoID,
		CommitID:         commitID,
		GitRef:           "deadbeef",
		Agent:            "test",
		JobType:          storage.JobTypeReview,
		PanelRunUUID:     runUUID,
		PanelRole:        storage.PanelRoleMember,
		PanelName:        "sweep-panel",
		PanelMemberIndex: index,
	}
}

func TestPanelSweepReleasesStuck(t *testing.T) {
	assert := assert.New(t)
	server, db, tmpDir := newTestServer(t)

	repo, err := db.GetOrCreateRepo(tmpDir)
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "deadbeef", "Author", "Subject", time.Now())
	require.NoError(t, err)

	runUUID := uuid.New()
	members := []storage.EnqueueOpts{
		stuckPanelMember(repo.ID, commit.ID, &runUUID, 0),
		stuckPanelMember(repo.ID, commit.ID, &runUUID, 1),
	}
	synthesis := storage.EnqueueOpts{
		RepoID:       repo.ID,
		CommitID:     commit.ID,
		GitRef:       "deadbeef",
		Agent:        "test",
		PanelRunUUID: &runUUID,
		PanelRole:    storage.PanelRoleSynthesis,
		PanelName:    "sweep-panel",
	}
	memberJobs, synthJob, err := db.EnqueuePanelRun(members, synthesis)
	require.NoError(t, err)
	require.Len(t, memberJobs, 2)
	require.NotNil(t, synthJob)

	// Drive both members to terminal WITHOUT releasing the synthesis gate,
	// simulating a missed worker release (e.g. a crash between completion and
	// the MaybeReleasePanelSynthesis call). ClaimJob skips the blocked
	// synthesis row, so each claim yields a member.
	for range members {
		claimed, err := db.ClaimJob("w")
		require.NoError(t, err)
		require.NotNil(t, claimed)
		require.NoError(t, testutil.CompleteReviewFixture(db, claimed.ID, "test", "", "No issues found."))
	}

	// Sanity: synthesis is still blocked because nothing released it.
	synth, err := db.GetSynthesisJob(runUUID)
	require.NoError(t, err)
	assert.True(synth.ClaimBlocked, "synthesis should remain blocked before the sweep")

	server.sweepStuckPanels()

	synth, err = db.GetSynthesisJob(runUUID)
	require.NoError(t, err)
	assert.False(synth.ClaimBlocked, "sweep should release the stuck synthesis job")
}

func TestPanelSweepRecoversOrphanedMember(t *testing.T) {
	for _, test := range []struct {
		name            string
		goalReview      bool
		missingWorktree bool
	}{
		{name: "panel member"},
		{name: "goal review", goalReview: true},
		{name: "goal review missing worktree", goalReview: true, missingWorktree: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			tc := newWorkerTestContext(t, 1)
			runUUID := uuid.Nil()
			eventType := "review.failed"
			worktreePath := ""
			if test.goalReview {
				eventType = "goal_review.failed"
				worktreePath = tc.TmpDir
				if test.missingWorktree {
					worktreePath = filepath.Join(tc.TmpDir, "missing-worktree")
				}
				_, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
					RepoID: tc.Repo.ID, Agent: "test", GitRef: "snapshot-digest",
					JobType: storage.JobTypeGoalReview, ReviewType: "goal", WorktreePath: worktreePath,
				})
				require.NoError(t, err)
				if test.missingWorktree {
					worktreePath = ""
				}
			} else {
				runUUID, _, _ = enqueuePanelRun(t, tc, "sweep-panel", []memberSpec{{name: "m0", agent: "test"}})
			}
			claimed, err := tc.DB.ClaimJob("worker-a")
			require.NoError(t, err)
			require.NotNil(t, claimed)
			subscriber, events := tc.Broadcaster.Subscribe(tc.Repo.RootPath)
			defer tc.Broadcaster.Unsubscribe(subscriber)
			server := &Server{db: tc.DB, workerPool: tc.Pool, broadcaster: tc.Broadcaster}

			server.sweepStuckPanels()

			job := tc.assertJobStatus(t, claimed.ID, storage.JobStatusFailed)
			assert.Equal("daemon did not save the job's outcome", job.Error)
			require.Len(t, events, 1)
			event := <-events
			assert.Equal(eventType, event.Type)
			assert.Equal(job.ID, event.JobID)
			assert.Equal(job.Error, event.Error)
			assert.True(event.SuppressHooks)
			assert.Equal(worktreePath, event.WorktreePath)
			if !test.goalReview {
				synth, err := tc.DB.GetSynthesisJob(runUUID)
				require.NoError(t, err)
				assert.False(synth.ClaimBlocked)
			}
		})
	}
}

func TestPanelSweepClaimOwnership(t *testing.T) {
	for _, test := range []struct {
		name           string
		liveWorker     bool
		pendingRequeue bool
		clockRollback  bool
		wantStatus     storage.JobStatus
	}{
		{name: "live worker", liveWorker: true, wantStatus: storage.JobStatusRunning},
		{name: "pending requeue", pendingRequeue: true, wantStatus: storage.JobStatusRunning},
		{name: "claim gap", wantStatus: storage.JobStatusRunning},
		{name: "claim gap after clock rollback", clockRollback: true, wantStatus: storage.JobStatusRunning},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			tc := newWorkerTestContext(t, 1)
			runUUID, _, _ := enqueuePanelRun(t, tc, "sweep-panel", []memberSpec{{name: "m0", agent: "test"}})
			tc.Pool.runningJobsMu.Lock()
			tc.Pool.workerJobs["worker-a"] = workerClaimingJobID
			tc.Pool.runningJobsMu.Unlock()
			claimed, err := tc.DB.ClaimJob("worker-a")
			require.NoError(t, err)
			require.NotNil(t, claimed)
			tc.Pool.runningJobsMu.Lock()
			switch {
			case test.liveWorker:
				tc.Pool.workerJobs["worker-a"] = claimed.ID
			case test.pendingRequeue:
				delete(tc.Pool.workerJobs, "worker-a")
				tc.Pool.failedUpdateRequeues[claimed.ID] = "worker-a"
			}
			tc.Pool.runningJobsMu.Unlock()
			if test.clockRollback {
				_, err := tc.DB.Exec(`UPDATE review_jobs SET started_at = ? WHERE id = ?`, time.Now().Add(-time.Hour).Format(time.RFC3339Nano), claimed.ID)
				require.NoError(t, err)
			}
			server := &Server{db: tc.DB, workerPool: tc.Pool, broadcaster: tc.Broadcaster}
			server.sweepStuckPanels()

			job := tc.assertJobStatus(t, claimed.ID, test.wantStatus)
			if test.wantStatus == storage.JobStatusRunning {
				assert.Empty(job.Error)
			}
			synth, err := tc.DB.GetSynthesisJob(runUUID)
			require.NoError(t, err)
			assert.True(synth.ClaimBlocked)
		})
	}
}

func TestPanelSweepRecoversRejectedWorkerFailure(t *testing.T) {
	for _, test := range []struct {
		name    string
		trigger string
		err     error
	}{
		{name: "result write", trigger: "BEFORE INSERT ON reviews WHEN NEW.job_id = %d"},
		{name: "failed-status write", trigger: "BEFORE UPDATE OF status ON review_jobs WHEN NEW.id = %d AND NEW.status = 'failed'", err: errors.New("synthetic agent failure")},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			tc := newWorkerTestContext(t, 1)
			const agentName = "panel-sweep-blocking"
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			releaseAgent := sync.OnceFunc(func() { close(release) })
			agent.RegisterForTest(t, &agent.FakeAgent{
				NameStr: agentName,
				ReviewFn: func(ctx context.Context, _, _, _ string, _ io.Writer) (string, error) {
					started <- struct{}{}
					select {
					case <-release:
						return string(testutil.ReviewFixtureJSON("No issues found.")), test.err
					case <-ctx.Done():
						return "", ctx.Err()
					}
				},
			})
			_, members, synth := enqueuePanelRun(t, tc, "sweep-panel", []memberSpec{{name: "m0", agent: agentName}})
			if test.err != nil {
				_, err := tc.DB.Exec(`UPDATE review_jobs SET retry_count = ? WHERE id = ?`, maxRetries, members[0].ID)
				require.NoError(t, err)
			}
			_, err := tc.DB.Exec("CREATE TRIGGER reject_worker_write " + fmt.Sprintf(test.trigger, members[0].ID) + " BEGIN SELECT RAISE(ABORT, 'synthetic write failure'); END")
			require.NoError(t, err)
			subscriber, events := tc.Broadcaster.Subscribe(tc.Repo.RootPath)
			defer tc.Broadcaster.Unsubscribe(subscriber)

			tc.Pool.Start()
			defer tc.Pool.Stop()
			defer releaseAgent()
			testutil.ReceiveWithTimeout(t, started, 10*time.Second)
			startedEvent := testutil.ReceiveWithTimeout(t, events, 10*time.Second)
			require.Equal(t, "review.started", startedEvent.Type)
			server := &Server{db: tc.DB, workerPool: tc.Pool, broadcaster: tc.Broadcaster}

			server.sweepStuckPanels()
			live := tc.assertJobStatus(t, members[0].ID, storage.JobStatusRunning)
			assert.Empty(live.Error)
			assert.Empty(events, "sweeping a live worker must not announce a failure")
			blocked := tc.assertJobStatus(t, synth.ID, storage.JobStatusQueued)
			assert.True(blocked.ClaimBlocked)

			// A later job on the single worker proves the rejected attempt returned.
			probe := tc.createJob(t, members[0].GitRef)
			releaseAgent()
			for {
				event := testutil.ReceiveWithTimeout(t, events, 10*time.Second)
				assert.NotEqual(members[0].ID, event.JobID, "a rejected write must not announce a terminal member event")
				if event.JobID == probe.ID && event.Type == "review.completed" {
					break
				}
			}
			tc.assertJobStatus(t, probe.ID, storage.JobStatusDone)
			before := tc.assertJobStatus(t, members[0].ID, storage.JobStatusRunning)
			_, err = tc.DB.GetReviewByJobID(members[0].ID)
			require.Error(t, err)
			_, err = tc.DB.Exec(`DROP TRIGGER reject_worker_write`)
			require.NoError(t, err)

			server.sweepStuckPanels()

			job := tc.assertJobStatus(t, members[0].ID, storage.JobStatusFailed)
			assert.Equal(before.RetryCount, job.RetryCount)
			event := testutil.ReceiveWithTimeout(t, events, 10*time.Second)
			assert.Equal(job.ID, event.JobID)
			for {
				event := testutil.ReceiveWithTimeout(t, events, 10*time.Second)
				require.Equal(t, synth.ID, event.JobID)
				if event.Type == "review.failed" {
					assert.Equal("all review agents failed or produced no usable output", event.Error)
					assert.True(event.SuppressHooks)
					break
				}
				require.Equal(t, "review.started", event.Type)
			}
			terminal := tc.assertJobStatus(t, synth.ID, storage.JobStatusFailed)
			assert.False(terminal.ClaimBlocked)
			assert.Equal(synth.RetryCount, terminal.RetryCount)
		})
	}
}

func TestPanelSweepPreservesReclaimedAttempt(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)
	runUUID, _, _ := enqueuePanelRun(t, tc, "sweep-panel", []memberSpec{{name: "m0", agent: "test"}})
	claimed, err := tc.DB.ClaimJob("worker-a")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	stale := claimed
	changed, err := tc.DB.FailoverJob(stale.ID, stale.WorkerID, "replacement", "")
	require.NoError(t, err)
	require.True(t, changed)
	reclaimed, err := tc.DB.ClaimJob(stale.WorkerID)
	require.NoError(t, err)
	require.NotNil(t, reclaimed)
	require.Equal(t, stale.ID, reclaimed.ID)
	require.Equal(t, stale.WorkerID, reclaimed.WorkerID)
	require.NotEqual(t, stale.StartedAtRaw, reclaimed.StartedAtRaw)
	server := &Server{db: tc.DB, workerPool: tc.Pool}

	server.failOrphanedJob(stale)

	job := tc.assertJobStatus(t, reclaimed.ID, storage.JobStatusRunning)
	assert.Empty(job.Error)
	synth, err := tc.DB.GetSynthesisJob(runUUID)
	require.NoError(t, err)
	assert.True(synth.ClaimBlocked)
}

func TestRunPanelSweepStopsOnContextCancel(t *testing.T) {
	server, _, _ := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		server.runPanelSweep(ctx, 5*time.Millisecond)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.Fail(t, "runPanelSweep did not return after context cancel")
	}
}
