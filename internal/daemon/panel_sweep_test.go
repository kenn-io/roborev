package daemon

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestPanelSweepRecoversOrphanedJobs(t *testing.T) {
	for _, name := range []string{"failed write", "active worker", "failover replacement", "update owned"} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			server, db, tmpDir := newTestServer(t)
			repo, err := db.GetOrCreateRepo(tmpDir)
			require.NoError(t, err)
			commit, err := db.GetOrCreateCommit(repo.ID, "deadbeef", "Author", "Subject", time.Now())
			require.NoError(t, err)
			runUUID := uuid.New()
			members, synth, err := db.EnqueuePanelRun(
				[]storage.EnqueueOpts{stuckPanelMember(repo.ID, commit.ID, &runUUID, 0)},
				storage.EnqueueOpts{
					RepoID: repo.ID, CommitID: commit.ID, GitRef: "deadbeef", Agent: "test",
					PanelRunUUID: &runUUID, PanelRole: storage.PanelRoleSynthesis, PanelName: "sweep-panel",
				},
			)
			require.NoError(t, err)
			require.Len(t, members, 1)
			require.NotNil(t, synth)
			claimed, err := db.ClaimJob("worker-a")
			require.NoError(t, err)
			require.NotNil(t, claimed)
			require.Equal(t, members[0].ID, claimed.ID)
			subscriber, events := server.broadcaster.Subscribe(claimed.RepoPath)
			defer server.broadcaster.Unsubscribe(subscriber)
			wp := server.workerPool
			wp.workerJobs[claimed.WorkerID] = claimed.ID

			switch name {
			case "failed write":
				_, err = db.Exec(`CREATE TRIGGER reject_failed_status BEFORE UPDATE OF status ON review_jobs
					WHEN NEW.status = 'failed' BEGIN SELECT RAISE(ABORT, 'synthetic write failure'); END`)
				require.NoError(t, err)
				// The fixture has no Git checkout, so prompt preparation fails before an agent runs.
				_, err = db.Exec(`UPDATE review_jobs SET retry_count = ? WHERE id = ?`, maxRetries, claimed.ID)
				require.NoError(t, err)
				wp.processJob(claimed.WorkerID, claimed)
				delete(wp.workerJobs, claimed.WorkerID)
				require.Empty(t, wp.runningJobs)
			case "failover replacement":
				changed, err := db.FailoverJob(claimed.ID, claimed.WorkerID, "test-backup", "")
				require.NoError(t, err)
				require.True(t, changed)
				claimed, err = db.ClaimJob("worker-b")
				require.NoError(t, err)
				require.NotNil(t, claimed)
				require.Equal(t, members[0].ID, claimed.ID)
				wp.workerJobs[claimed.WorkerID] = claimed.ID
				wp.registerRunningJob(claimed.ID, func() {}, time.Time{})
				wp.finishRunningJob("worker-a", claimed.ID)
				delete(wp.workerJobs, "worker-a")
			case "update owned":
				wp.InterruptJobsForUpdate([]int64{claimed.ID})
				delete(wp.workerJobs, claimed.WorkerID)
			}
			startedAt := time.Now().Add(-orphanJobGrace - time.Minute).In(time.FixedZone("offset", -5*60*60))
			_, err = db.Exec(`UPDATE review_jobs SET started_at = ? WHERE id = ?`, startedAt.Format(time.RFC3339Nano), claimed.ID)
			require.NoError(t, err)

			server.sweepStuckPanels()
			job, err := db.GetJobByID(claimed.ID)
			require.NoError(t, err)
			assert.Equal(storage.JobStatusRunning, job.Status)
			assert.Empty(job.Error)
			assert.Empty(events)
			synth, err = db.GetSynthesisJob(runUUID)
			require.NoError(t, err)
			assert.True(synth.ClaimBlocked)

			if name == "failed write" {
				_, err = db.Exec(`DROP TRIGGER reject_failed_status`)
				require.NoError(t, err)
				server.sweepStuckPanels()
				job, err = db.GetJobByID(claimed.ID)
				require.NoError(t, err)
				assert.Equal(storage.JobStatusFailed, job.Status)
				assert.Equal("worker exited before the job finished", job.Error)
				require.Len(t, events, 1)
				event := <-events
				assert.Equal("review.failed", event.Type)
				assert.Equal(job.ID, event.JobID)
				assert.Equal(job.Error, event.Error)
				synth, err = db.GetSynthesisJob(runUUID)
				require.NoError(t, err)
				assert.False(synth.ClaimBlocked)
			}
		})
	}
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
