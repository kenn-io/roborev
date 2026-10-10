package daemon

import (
	"context"
	"errors"
	"io"
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

func TestPanelSweepRecoversOrphanedJobs(t *testing.T) {
	for _, name := range []string{"failed write", "failover replacement", "stale attempt", "update owned"} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			server, db, tmpDir := newTestServer(t)
			gitRepo := testutil.InitTestGitRepo(t, tmpDir)
			sha := gitRepo.RevParse("HEAD")
			repo, err := db.GetOrCreateRepo(tmpDir)
			require.NoError(t, err)
			commit, err := db.GetOrCreateCommit(repo.ID, sha, "Author", "Subject", time.Now())
			require.NoError(t, err)
			runUUID := uuid.New()
			member := stuckPanelMember(repo.ID, commit.ID, &runUUID, 0)
			member.GitRef = sha
			const agentName = "panel-sweep-blocking"
			if name == "failed write" {
				member.Agent = agentName
			}
			members, synth, err := db.EnqueuePanelRun(
				[]storage.EnqueueOpts{member},
				storage.EnqueueOpts{
					RepoID: repo.ID, CommitID: commit.ID, GitRef: sha, Agent: "test",
					PanelRunUUID: &runUUID, PanelRole: storage.PanelRoleSynthesis, PanelName: "sweep-panel",
				},
			)
			require.NoError(t, err)
			require.Len(t, members, 1)
			require.NotNil(t, synth)
			subscriber, events := server.broadcaster.Subscribe(repo.RootPath)
			defer server.broadcaster.Unsubscribe(subscriber)
			wp := server.workerPool
			release := make(chan struct{})

			if name == "failed write" || name == "failover replacement" {
				started := make(chan struct{}, 1)
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
				if name == "failover replacement" {
					old, err := db.ClaimJob("worker-a")
					require.NoError(t, err)
					require.NotNil(t, old)
					changed, err := db.FailoverJob(old.ID, old.WorkerID, agentName, "")
					require.NoError(t, err)
					require.True(t, changed)
				}
				_, err = db.Exec(`UPDATE review_jobs SET retry_count = ? WHERE id = ?`, maxRetries, members[0].ID)
				require.NoError(t, err)
				_, err = db.Exec(`CREATE TRIGGER reject_failed_status BEFORE UPDATE OF status ON review_jobs
					WHEN NEW.status = 'failed' BEGIN SELECT RAISE(ABORT, 'synthetic write failure'); END`)
				require.NoError(t, err)
				wp.Start()
				defer wp.Stop()
				defer func() {
					select {
					case <-release:
					default:
						close(release)
					}
				}()
				testutil.ReceiveWithTimeout(t, started, 10*time.Second)
				startedEvent := testutil.ReceiveWithTimeout(t, events, 10*time.Second)
				require.Equal(t, "review.started", startedEvent.Type)
				if name == "failover replacement" {
					wp.finishRunningJob("worker-a", members[0].ID)
				}
			} else {
				claimed, err := db.ClaimJob("worker-a")
				require.NoError(t, err)
				require.NotNil(t, claimed)
				if name == "update owned" {
					wp.InterruptJobsForUpdate([]int64{claimed.ID})
				}
			}
			startedAt := time.Now().Add(-orphanJobGrace - time.Minute).In(time.FixedZone("offset", -5*60*60))
			_, err = db.Exec(`UPDATE review_jobs SET started_at = ? WHERE id = ?`, startedAt.Format(time.RFC3339Nano), members[0].ID)
			require.NoError(t, err)

			if name == "stale attempt" {
				stale, err := db.GetJobByID(members[0].ID)
				require.NoError(t, err)
				changed, err := db.FailoverJob(stale.ID, stale.WorkerID, agentName, "")
				require.NoError(t, err)
				require.True(t, changed)
				reclaimed, err := db.ClaimJob(stale.WorkerID)
				require.NoError(t, err)
				require.NotNil(t, reclaimed)
				require.Equal(t, stale.ID, reclaimed.ID)
				require.Equal(t, stale.WorkerID, reclaimed.WorkerID)
				require.NotEqual(t, stale.StartedAtRaw, reclaimed.StartedAtRaw)
				server.failOrphanedJob(stale)
			} else {
				server.sweepStuckPanels()
			}
			job, err := db.GetJobByID(members[0].ID)
			require.NoError(t, err)
			assert.Equal(storage.JobStatusRunning, job.Status)
			assert.Empty(job.Error)
			assert.Empty(events)
			synth, err = db.GetSynthesisJob(runUUID)
			require.NoError(t, err)
			assert.True(synth.ClaimBlocked)

			if name == "failed write" || name == "failover replacement" {
				close(release)
				// The worker writes to SQLite before releasing its ownership.
				require.Eventually(t, func() bool {
					wp.runningJobsMu.Lock()
					defer wp.runningJobsMu.Unlock()
					return len(wp.workerJobs) == 0
				}, 10*time.Second, 10*time.Millisecond)
				wp.Stop()
				server.sweepStuckPanels()
				job, err = db.GetJobByID(job.ID)
				require.NoError(t, err)
				assert.Equal(storage.JobStatusRunning, job.Status)
				assert.Empty(events)
				synth, err = db.GetSynthesisJob(runUUID)
				require.NoError(t, err)
				assert.True(synth.ClaimBlocked)
				_, err = db.Exec(`DROP TRIGGER reject_failed_status`)
				require.NoError(t, err)
				server.sweepStuckPanels()
				job, err = db.GetJobByID(job.ID)
				require.NoError(t, err)
				assert.Equal(storage.JobStatusFailed, job.Status)
				assert.Equal("worker stopped without saving the job's outcome", job.Error)
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
