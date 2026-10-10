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
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)
	runUUID, members, _ := enqueuePanelRun(t, tc, "sweep-panel", []memberSpec{{name: "m0", agent: "test"}})
	claimed, err := tc.DB.ClaimJob("worker-a")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	backdateJobStartedAt(t, tc.DB, members[0].ID)
	subscriber, events := tc.Broadcaster.Subscribe(tc.Repo.RootPath)
	defer tc.Broadcaster.Unsubscribe(subscriber)
	server := &Server{db: tc.DB, workerPool: tc.Pool, broadcaster: tc.Broadcaster}

	server.sweepStuckPanels()

	job := tc.assertJobStatus(t, claimed.ID, storage.JobStatusFailed)
	assert.Equal("worker stopped without saving the job's outcome", job.Error)
	require.Len(t, events, 1)
	event := <-events
	assert.Equal("review.failed", event.Type)
	assert.Equal(job.ID, event.JobID)
	assert.Equal(job.Error, event.Error)
	assert.True(event.SuppressHooks)
	synth, err := tc.DB.GetSynthesisJob(runUUID)
	require.NoError(t, err)
	assert.False(synth.ClaimBlocked)
}

func TestPanelSweepRecoversOrphanedGoalReview(t *testing.T) {
	for _, worktreeExists := range []bool{true, false} {
		t.Run(fmt.Sprintf("worktreeExists=%t", worktreeExists), func(t *testing.T) {
			assert := assert.New(t)
			tc := newWorkerTestContext(t, 1)
			worktreePath := tc.TmpDir
			if !worktreeExists {
				worktreePath = filepath.Join(tc.TmpDir, "missing-worktree")
			}
			job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
				RepoID: tc.Repo.ID, Agent: "test", GitRef: "snapshot-digest",
				JobType: storage.JobTypeGoalReview, ReviewType: "goal", WorktreePath: worktreePath,
			})
			require.NoError(t, err)
			claimed, err := tc.DB.ClaimJob("worker-a")
			require.NoError(t, err)
			require.NotNil(t, claimed)
			require.Equal(t, job.ID, claimed.ID)
			backdateJobStartedAt(t, tc.DB, job.ID)
			subscriber, events := tc.Broadcaster.Subscribe(tc.Repo.RootPath)
			defer tc.Broadcaster.Unsubscribe(subscriber)
			server := &Server{db: tc.DB, workerPool: tc.Pool, broadcaster: tc.Broadcaster}

			server.sweepStuckPanels()

			job = tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
			require.Len(t, events, 1)
			event := <-events
			assert.Equal("goal_review.failed", event.Type)
			assert.Equal(job.ID, event.JobID)
			assert.Equal(job.Error, event.Error)
			assert.True(event.SuppressHooks)
			if worktreeExists {
				assert.Equal(worktreePath, event.WorktreePath)
			} else {
				assert.Empty(event.WorktreePath)
			}
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
	runUUID, members, _ := enqueuePanelRun(t, tc, "sweep-panel", []memberSpec{{name: "m0", agent: agentName}})
	_, err := tc.DB.Exec(`UPDATE review_jobs SET retry_count = ? WHERE id = ?`, maxRetries, members[0].ID)
	require.NoError(t, err)
	_, err = tc.DB.Exec(`CREATE TRIGGER reject_failed_status BEFORE UPDATE OF status ON review_jobs
		WHEN NEW.status = 'failed' BEGIN SELECT RAISE(ABORT, 'synthetic write failure'); END`)
	require.NoError(t, err)
	subscriber, events := tc.Broadcaster.Subscribe(tc.Repo.RootPath)
	defer tc.Broadcaster.Unsubscribe(subscriber)
	tc.Pool.Start()
	defer tc.Pool.Stop()
	testutil.ReceiveWithTimeout(t, started, 10*time.Second)
	startedEvent := testutil.ReceiveWithTimeout(t, events, 10*time.Second)
	require.Equal(t, "review.started", startedEvent.Type)
	backdateJobStartedAt(t, tc.DB, members[0].ID)
	server := &Server{db: tc.DB, workerPool: tc.Pool, broadcaster: tc.Broadcaster}

	server.sweepStuckPanels()

	tc.assertJobStatus(t, members[0].ID, storage.JobStatusRunning)
	assert.Empty(events)
	synth, err := tc.DB.GetSynthesisJob(runUUID)
	require.NoError(t, err)
	assert.True(synth.ClaimBlocked)
	close(release)
	// The worker finishes its SQLite write before releasing ownership.
	require.Eventually(t, func() bool {
		tc.Pool.runningJobsMu.Lock()
		defer tc.Pool.runningJobsMu.Unlock()
		return len(tc.Pool.workerJobs) == 0
	}, 10*time.Second, 10*time.Millisecond)
	tc.Pool.Stop()
	server.sweepStuckPanels()
	tc.assertJobStatus(t, members[0].ID, storage.JobStatusRunning)
	assert.Empty(events)
	synth, err = tc.DB.GetSynthesisJob(runUUID)
	require.NoError(t, err)
	assert.True(synth.ClaimBlocked)
	_, err = tc.DB.Exec(`DROP TRIGGER reject_failed_status`)
	require.NoError(t, err)

	server.sweepStuckPanels()

	job := tc.assertJobStatus(t, members[0].ID, storage.JobStatusFailed)
	assert.Equal("worker stopped without saving the job's outcome", job.Error)
	require.Len(t, events, 1)
	event := <-events
	assert.Equal("review.failed", event.Type)
	assert.Equal(job.ID, event.JobID)
	assert.Equal(job.Error, event.Error)
	assert.True(event.SuppressHooks)
	synth, err = tc.DB.GetSynthesisJob(runUUID)
	require.NoError(t, err)
	assert.False(synth.ClaimBlocked)
}

func TestPanelSweepPreservesUpdateOwnedJob(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)
	runUUID, members, _ := enqueuePanelRun(t, tc, "sweep-panel", []memberSpec{{name: "m0", agent: "test"}})
	claimed, err := tc.DB.ClaimJob("worker-a")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	backdateJobStartedAt(t, tc.DB, members[0].ID)
	tc.Pool.InterruptJobsForUpdate([]int64{claimed.ID})
	server := &Server{db: tc.DB, workerPool: tc.Pool}

	server.sweepStuckPanels()

	job := tc.assertJobStatus(t, claimed.ID, storage.JobStatusRunning)
	assert.Empty(job.Error)
	synth, err := tc.DB.GetSynthesisJob(runUUID)
	require.NoError(t, err)
	assert.True(synth.ClaimBlocked)
}

func TestPanelSweepPreservesReclaimedAttempt(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)
	runUUID, members, _ := enqueuePanelRun(t, tc, "sweep-panel", []memberSpec{{name: "m0", agent: "test"}})
	claimed, err := tc.DB.ClaimJob("worker-a")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	backdateJobStartedAt(t, tc.DB, members[0].ID)
	stale, err := tc.DB.GetJobByID(claimed.ID)
	require.NoError(t, err)
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
