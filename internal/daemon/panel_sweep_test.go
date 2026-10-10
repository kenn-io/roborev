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
	for _, tc := range []struct {
		name    string
		age     time.Duration
		running bool
		status  storage.JobStatus
	}{
		{name: "orphan", age: orphanJobGrace + time.Minute, status: storage.JobStatusFailed},
		{name: "inside grace", age: orphanJobGrace / 2, status: storage.JobStatusRunning},
		{name: "active worker", age: orphanJobGrace + time.Minute, running: true, status: storage.JobStatusRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			startedAt := time.Now().Add(-tc.age).In(time.FixedZone("offset", -5*60*60))
			_, err = db.Exec(`UPDATE review_jobs SET started_at = ? WHERE id = ?`, startedAt.Format(time.RFC3339Nano), claimed.ID)
			require.NoError(t, err)
			if tc.running {
				server.workerPool.registerRunningJob(claimed.ID, func() {}, time.Now().Add(time.Hour))
				defer server.workerPool.unregisterRunningJob(claimed.ID)
			}
			subscriber, events := server.broadcaster.Subscribe(claimed.RepoPath)
			defer server.broadcaster.Unsubscribe(subscriber)

			server.sweepStuckPanels()

			job, err := db.GetJobByID(claimed.ID)
			require.NoError(t, err)
			assert.Equal(tc.status, job.Status)
			synth, err = db.GetSynthesisJob(runUUID)
			require.NoError(t, err)
			if tc.status == storage.JobStatusFailed {
				assert.Equal("worker exited before the job finished", job.Error)
				assert.False(synth.ClaimBlocked)
				require.Len(t, events, 1)
				event := <-events
				assert.Equal("review.failed", event.Type)
				assert.Equal(job.ID, event.JobID)
				assert.Equal(job.Error, event.Error)
			} else {
				assert.Empty(job.Error)
				assert.True(synth.ClaimBlocked)
				assert.Empty(events)
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
