package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
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
		updateTarget    bool
	}{
		{name: "panel member"},
		{name: "update target without owner", updateTarget: true},
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

			_, err = tc.DB.Exec("UPDATE review_jobs SET started_at = datetime('now','-1 minute') WHERE id = ?", claimed.ID)
			require.NoError(t, err)
			server.sweepStuckPanels()
			tc.assertJobStatus(t, claimed.ID, storage.JobStatusRunning)
			assert.Empty(events)
			if !test.goalReview {
				synth, err := tc.DB.GetSynthesisJob(runUUID)
				require.NoError(t, err)
				assert.True(synth.ClaimBlocked)
			}
			_, err = tc.DB.Exec("UPDATE review_jobs SET started_at = datetime('now','-1 hour') WHERE id = ?", claimed.ID)
			require.NoError(t, err)
			if test.updateTarget {
				tc.Pool.InterruptJobsForUpdate([]int64{claimed.ID})
			}

			server.sweepStuckPanels()

			job := tc.assertJobStatus(t, claimed.ID, storage.JobStatusFailed)
			assert.Equal("worker stopped without saving the job's outcome", job.Error)
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

func TestPanelSweepPreservesOwnedJob(t *testing.T) {
	for _, test := range []struct {
		name           string
		pendingRequeue bool
	}{
		{name: "update-owned live worker"},
		{name: "update-owned failed requeue", pendingRequeue: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			tc := newWorkerTestContext(t, 1)
			runUUID, _, _ := enqueuePanelRun(t, tc, "sweep-panel", []memberSpec{{name: "m0", agent: "test"}})
			claimed, err := tc.DB.ClaimJob("worker-a")
			require.NoError(t, err)
			require.NotNil(t, claimed)
			_, err = tc.DB.Exec("UPDATE review_jobs SET started_at = datetime('now','-1 hour') WHERE id = ?", claimed.ID)
			require.NoError(t, err)
			tc.Pool.runningJobsMu.Lock()
			switch {
			case test.pendingRequeue:
				tc.Pool.failedUpdateRequeues[claimed.ID] = "worker-a"
			default:
				tc.Pool.workerJobs["worker-a"] = claimed.ID
			}
			tc.Pool.runningJobsMu.Unlock()
			tc.Pool.InterruptJobsForUpdate([]int64{claimed.ID})
			server := &Server{db: tc.DB, workerPool: tc.Pool}

			server.sweepStuckPanels()

			job := tc.assertJobStatus(t, claimed.ID, storage.JobStatusRunning)
			assert.Empty(job.Error)
			synth, err := tc.DB.GetSynthesisJob(runUUID)
			require.NoError(t, err)
			assert.True(synth.ClaimBlocked)
		})
	}
}

func TestPanelSweepClaimOwnership(t *testing.T) {
	for _, test := range []struct {
		name        string
		olderOrphan bool
		wantStatus  storage.JobStatus
	}{
		{name: "claim gap", wantStatus: storage.JobStatusRunning},
		{name: "older orphan of claiming worker", olderOrphan: true, wantStatus: storage.JobStatusFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			tc := newWorkerTestContext(t, 1)
			runUUID, _, _ := enqueuePanelRun(t, tc, "sweep-panel", []memberSpec{{name: "m0", agent: "test"}})
			tc.Pool.runningJobsMu.Lock()
			tc.Pool.workerJobs["worker-a"] = workerClaimingJobID
			tc.Pool.workerClaimStartedAt["worker-a"] = time.Now()
			tc.Pool.runningJobsMu.Unlock()
			claimed, err := tc.DB.ClaimJob("worker-a")
			require.NoError(t, err)
			require.NotNil(t, claimed)
			if test.olderOrphan {
				claimed.StartedAtRaw = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
				_, err = tc.DB.Exec("UPDATE review_jobs SET started_at = ? WHERE id = ?", claimed.StartedAtRaw, claimed.ID)
				require.NoError(t, err)
			}
			server := &Server{db: tc.DB, workerPool: tc.Pool, broadcaster: tc.Broadcaster}
			if !test.olderOrphan {
				// Bypass the grace period to exercise ownership during claim publication.
				server.failOrphanedJob(&storage.StalledJob{ID: claimed.ID, WorkerID: claimed.WorkerID, StartedAt: claimed.StartedAtRaw})
			}
			server.sweepStuckPanels()

			tc.assertJobStatus(t, claimed.ID, test.wantStatus)
			synth, err := tc.DB.GetSynthesisJob(runUUID)
			require.NoError(t, err)
			assert.Equal(!test.olderOrphan, synth.ClaimBlocked)
		})
	}
}

func TestPanelSweepRecoversRejectedWorkerFailure(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)
	const agentName = "panel-sweep-blocking"
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	agent.RegisterForTest(t, &agent.FakeAgent{
		NameStr: agentName,
		ReviewFn: func(ctx context.Context, _, _, _ string, _ io.Writer) (string, error) {
			started <- struct{}{}
			select {
			case <-release:
				return "", errors.New("synthetic agent failure")
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	})
	_, members, _ := enqueuePanelRun(t, tc, "sweep-panel", []memberSpec{{name: "m0", agent: agentName}})
	_, err := tc.DB.Exec(`UPDATE review_jobs SET retry_count = ? WHERE id = ?`, maxRetries, members[0].ID)
	require.NoError(t, err)
	_, err = tc.DB.Exec(fmt.Sprintf(`CREATE TRIGGER reject_worker_write BEFORE UPDATE OF status ON review_jobs
		WHEN NEW.id = %d AND NEW.status = 'failed' BEGIN SELECT RAISE(ABORT, 'synthetic write failure'); END`, members[0].ID))
	require.NoError(t, err)
	subscriber, events := tc.Broadcaster.Subscribe(tc.Repo.RootPath)
	defer tc.Broadcaster.Unsubscribe(subscriber)

	tc.Pool.Start()
	defer tc.Pool.Stop()
	testutil.ReceiveWithTimeout(t, started, 10*time.Second)
	startedEvent := testutil.ReceiveWithTimeout(t, events, 10*time.Second)
	require.Equal(t, "review.started", startedEvent.Type)
	_, err = tc.DB.Exec("UPDATE review_jobs SET started_at = datetime('now','-1 hour') WHERE id = ?", members[0].ID)
	require.NoError(t, err)
	server := &Server{db: tc.DB, workerPool: tc.Pool, broadcaster: tc.Broadcaster}

	// A later job on the single worker proves the rejected attempt returned.
	probe := tc.createJob(t, members[0].GitRef)
	close(release)
	for {
		event := testutil.ReceiveWithTimeout(t, events, 10*time.Second)
		if event.JobID == probe.ID && event.Type == "review.completed" {
			break
		}
	}
	tc.assertJobStatus(t, probe.ID, storage.JobStatusDone)
	before := tc.assertJobStatus(t, members[0].ID, storage.JobStatusRunning)
	_, err = tc.DB.Exec(`DROP TRIGGER reject_worker_write`)
	require.NoError(t, err)

	server.sweepStuckPanels()

	job := tc.assertJobStatus(t, members[0].ID, storage.JobStatusFailed)
	assert.Equal("worker stopped without saving the job's outcome", job.Error)
	assert.Equal(maxRetries, before.RetryCount)
	assert.Equal(before.RetryCount, job.RetryCount)
	event := testutil.ReceiveWithTimeout(t, events, 10*time.Second)
	assert.Equal("review.failed", event.Type)
	assert.Equal(job.ID, event.JobID)
	assert.Equal(job.Error, event.Error)
	assert.True(event.SuppressHooks)
}

func TestPanelSweepPreservesReclaimedAttempt(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)
	runUUID, members, _ := enqueuePanelRun(t, tc, "sweep-panel", []memberSpec{{name: "m0", agent: "test"}})
	claimed, err := tc.DB.ClaimJob("worker-a")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	_, err = tc.DB.Exec("UPDATE review_jobs SET started_at = datetime('now','-1 hour') WHERE id = ?", members[0].ID)
	require.NoError(t, err)
	stalled, err := tc.DB.ListStalledJobs(orphanJobGrace)
	require.NoError(t, err)
	require.Len(t, stalled, 1)
	stale := &stalled[0]
	changed, err := tc.DB.FailoverJob(stale.ID, stale.WorkerID, "replacement", "")
	require.NoError(t, err)
	require.True(t, changed)
	reclaimed, err := tc.DB.ClaimJob(stale.WorkerID)
	require.NoError(t, err)
	require.NotNil(t, reclaimed)
	require.Equal(t, stale.ID, reclaimed.ID)
	require.Equal(t, stale.WorkerID, reclaimed.WorkerID)
	require.NotEqual(t, stale.StartedAt, reclaimed.StartedAtRaw)
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
