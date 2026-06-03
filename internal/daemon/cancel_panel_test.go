package daemon

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

// enqueueServerPanelRun builds a panel run (members queued, synthesis blocked)
// directly against the server's DB and returns the run UUID, member jobs, and
// the synthesis job. It mirrors db.EnqueuePanelRun's queued/blocked layout.
func enqueueServerPanelRun(
	t *testing.T, db *storage.DB, memberCount int,
) (string, []*storage.ReviewJob, *storage.ReviewJob) {
	t.Helper()
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "deadbeef", "Author", "Subject", time.Now())
	require.NoError(t, err)

	runUUID := uuid.NewString()
	opts := make([]storage.EnqueueOpts, 0, memberCount)
	for i := range memberCount {
		opts = append(opts, storage.EnqueueOpts{
			RepoID:           repo.ID,
			CommitID:         commit.ID,
			GitRef:           "deadbeef",
			Agent:            "test",
			JobType:          storage.JobTypeReview,
			PanelRunUUID:     runUUID,
			PanelRole:        storage.PanelRoleMember,
			PanelName:        "panel",
			PanelMemberName:  "member",
			PanelMemberIndex: i,
		})
	}
	synthesis := storage.EnqueueOpts{
		RepoID:       repo.ID,
		CommitID:     commit.ID,
		GitRef:       "deadbeef",
		Agent:        "test",
		PanelRunUUID: runUUID,
		PanelRole:    storage.PanelRoleSynthesis,
		PanelName:    "panel",
	}
	members, synth, err := db.EnqueuePanelRun(opts, synthesis)
	require.NoError(t, err)
	require.Len(t, members, memberCount)
	require.NotNil(t, synth)
	return runUUID, members, synth
}

func enqueueServerCIPanelRun(
	t *testing.T, db *storage.DB, memberCount int,
) (*storage.CIPanel, []*storage.ReviewJob, *storage.ReviewJob) {
	t.Helper()
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)

	opts := make([]storage.EnqueueOpts, 0, memberCount)
	for i := range memberCount {
		opts = append(opts, storage.EnqueueOpts{
			RepoID:           repo.ID,
			GitRef:           "base..headsha",
			Agent:            "test",
			JobType:          storage.JobTypeRange,
			PanelName:        "ci",
			PanelMemberName:  "member",
			PanelMemberIndex: i,
		})
	}
	synthesis := storage.EnqueueOpts{
		RepoID:  repo.ID,
		GitRef:  "base..headsha",
		Agent:   "test",
		JobType: storage.JobTypeSynthesis,
	}
	created, members, synth, err := db.CreateCIPanelRun("acme/api", 17, "headsha", opts, synthesis)
	require.NoError(t, err)
	require.True(t, created, "CI panel should be created")
	panel, err := db.GetCIPanelByPRSHA("acme/api", 17, "headsha")
	require.NoError(t, err)
	return panel, members, synth
}

func TestCancelSynthesisCascades(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	runUUID, members, synth := enqueueServerPanelRun(t, db, 2)

	_, err := server.humaCancelJob(context.Background(), &CancelJobInput{
		Body: CancelJobRequest{JobID: synth.ID},
	})
	require.NoError(t, err)

	for _, m := range members {
		got, err := db.GetJobByID(m.ID)
		require.NoError(t, err)
		assert.Equal(storage.JobStatusCanceled, got.Status, "member should be canceled")
	}
	gotSynth, err := db.GetJobByID(synth.ID)
	require.NoError(t, err)
	assert.Equal(storage.JobStatusCanceled, gotSynth.Status, "synthesis should be canceled")
	_ = runUUID
}

func TestCancelCISynthesisRetiresPanelMapping(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	panel, members, synth := enqueueServerCIPanelRun(t, db, 1)

	_, err := server.humaCancelJob(context.Background(), &CancelJobInput{
		Body: CancelJobRequest{JobID: synth.ID},
	})
	require.NoError(t, err)

	gotSynth, err := db.GetJobByID(synth.ID)
	require.NoError(t, err)
	assert.Equal(storage.JobStatusCanceled, gotSynth.Status, "synthesis should be canceled")
	gotMember, err := db.GetJobByID(members[0].ID)
	require.NoError(t, err)
	assert.Equal(storage.JobStatusCanceled, gotMember.Status, "member should be canceled")

	_, err = db.GetActiveCIPanelByPRSHA("acme/api", 17, "headsha")
	require.ErrorIs(t, err, sql.ErrNoRows, "canceled synthesis must not leave an active CI panel")
	gotPanel, err := db.GetCIPanelByPRSHA("acme/api", 17, "headsha")
	require.NoError(t, err)
	assert.Equal(panel.ID, gotPanel.ID)
	assert.Nil(gotPanel.PostedAt, "canceled panel is not marked posted")
	assert.NotNil(gotPanel.RetiredAt, "canceled panel is retired for throttle memory")
	attempt, err := db.GetReviewAttempt("acme/api", 17, "headsha")
	require.NoError(t, err)
	assert.Nil(attempt, "canceled CI panel deletes the reserved retry attempt")
}

func TestCancelQueuedMemberReleasesSynthesis(t *testing.T) {
	server, db, _ := newTestServer(t)

	runUUID, members, _ := enqueueServerPanelRun(t, db, 1)

	_, err := server.humaCancelJob(context.Background(), &CancelJobInput{
		Body: CancelJobRequest{JobID: members[0].ID},
	})
	require.NoError(t, err)

	got, err := db.GetJobByID(members[0].ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusCanceled, got.Status, "member should be canceled")

	synth, err := db.GetSynthesisJob(runUUID)
	require.NoError(t, err)
	require.NotNil(t, synth)
	assert.False(t, synth.ClaimBlocked, "synthesis should be released immediately")
}

func TestCancelMemberDoesNotCascade(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	runUUID, members, _ := enqueueServerPanelRun(t, db, 2)

	_, err := server.humaCancelJob(context.Background(), &CancelJobInput{
		Body: CancelJobRequest{JobID: members[0].ID},
	})
	require.NoError(t, err)

	canceled, err := db.GetJobByID(members[0].ID)
	require.NoError(t, err)
	assert.Equal(storage.JobStatusCanceled, canceled.Status, "canceled member")

	other, err := db.GetJobByID(members[1].ID)
	require.NoError(t, err)
	assert.Equal(storage.JobStatusQueued, other.Status, "other member stays queued")

	synth, err := db.GetSynthesisJob(runUUID)
	require.NoError(t, err)
	require.NotNil(t, synth)
	assert.True(synth.ClaimBlocked, "synthesis stays blocked while a member is alive")
}

// TestListPanelRunReturnsFullRun verifies a panel_run expansion is not truncated
// at the default 50-row limit: a run with more than 50 rows returns every member
// plus the synthesis when the caller provides no explicit limit.
func TestListPanelRunReturnsFullRun(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	// 51 members + 1 synthesis = 52 rows, above the default 50-row cap.
	const memberCount = 51
	runUUID, _, _ := enqueueServerPanelRun(t, db, memberCount)

	// Mirror huma's query defaults for a request that omits id/limit/offset.
	out, err := server.humaListJobs(context.Background(), &ListJobsInput{
		ID:       -1,
		PanelRun: runUUID,
		Limit:    limitNotProvided,
		Offset:   -1,
	})
	require.NoError(t, err)
	assert.Len(out.Body.Jobs, memberCount+1, "full run returned, not truncated at 50")
	assert.False(out.Body.HasMore, "a limitless panel_run query has no further pages")
	for _, j := range out.Body.Jobs {
		assert.Equal(runUUID, j.PanelRunUUID)
	}
}
