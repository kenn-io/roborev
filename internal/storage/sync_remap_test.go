package storage

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPatchIDSyncRoundTrip(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	machineID, err := db.GetMachineID()
	require.NoError(t, err, "GetMachineID failed")

	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err, "GetOrCreateRepo failed")
	commit, err := db.GetOrCreateCommit(
		repo.ID, "patchid-sync-sha", "Author", "Subject", time.Now(),
	)
	require.NoError(t, err, "GetOrCreateCommit failed")

	// Enqueue a job with a patch_id
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:   repo.ID,
		CommitID: commit.ID,
		GitRef:   "patchid-sync-sha",
		Agent:    "test",
		PatchID:  "deadbeef9999",
	})
	require.NoError(t, err, "EnqueueJob failed")

	// Complete the job so it becomes sync-eligible
	_, err = db.ClaimJob("worker-sync")
	require.NoError(t, err, "ClaimJob: %v")

	err = completeReviewFixture(db, job.ID, "test", "prompt", "output")
	require.NoError(t, err, "CompleteJob: %v")

	// Verify patch_id appears in GetJobsToSync
	syncJobs, err := db.GetJobsToSync(machineID, 100)
	require.NoError(t, err, "GetJobsToSync: %v")

	var found *SyncableJob
	for i := range syncJobs {
		if syncJobs[i].ID == job.ID {
			found = &syncJobs[i]
			break
		}
	}
	assert.NotNil(t, found)
	assert.Equal(t, "deadbeef9999", found.PatchID)

	// Simulate pull: upsert back into a fresh DB via UpsertPulledJob
	db2 := openTestDB(t)
	defer db2.Close()

	repo2, err := db2.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err, "db2 GetOrCreateRepo: %v")

	commitID2 := int64(0)
	if found.CommitSHA != "" {
		c, err := db2.GetOrCreateCommit(
			repo2.ID, found.CommitSHA,
			found.CommitAuthor, found.CommitSubject, found.CommitTimestamp,
		)
		require.NoError(t, err, "db2 GetOrCreateCommit: %v")

		commitID2 = c.ID
	}

	pulledJob := PulledJob{
		UUID:            found.UUID,
		RepoIdentity:    found.RepoIdentity,
		GitRef:          found.GitRef,
		Agent:           found.Agent,
		Model:           found.Model,
		Reasoning:       found.Reasoning,
		JobType:         found.JobType,
		ReviewType:      found.ReviewType,
		PatchID:         found.PatchID,
		Status:          found.Status,
		Agentic:         found.Agentic,
		EnqueuedAt:      found.EnqueuedAt,
		StartedAt:       found.StartedAt,
		FinishedAt:      found.FinishedAt,
		Prompt:          found.Prompt,
		DiffContent:     found.DiffContent,
		Error:           found.Error,
		SourceMachineID: found.SourceMachineID,
		UpdatedAt:       found.UpdatedAt,
	}

	cid := &commitID2
	err = db2.UpsertPulledJob(pulledJob, repo2.ID, cid)
	require.NoError(t, err, "UpsertPulledJob: %v")

	// Verify patch_id survived the round trip
	var patchIDVal sql.NullString
	err = db2.QueryRow(
		`SELECT patch_id FROM review_jobs WHERE uuid = ?`, found.UUID,
	).Scan(&patchIDVal)
	require.NoError(t, err, "query patch_id in db2: %v")

	assert.False(t, !patchIDVal.Valid || patchIDVal.String != "deadbeef9999")
}

func TestRemapJobGitRef_RunningJob(t *testing.T) {
	// Running jobs must be skipped by remap: the worker has already
	// built the prompt with the old SHA, so updating git_ref would
	// create a mismatch between the stored prompt and the ref.
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-remap-running")
	commit := createCommit(t, db, repo.ID, "running-oldsha")

	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:   repo.ID,
		CommitID: commit.ID,
		GitRef:   "running-oldsha",
		Agent:    "test",
		PatchID:  "patchRUN",
	})
	require.NoError(t, err, "EnqueueJob: %v")

	// Set job to running
	setJobStatus(t, db, job.ID, JobStatusRunning)

	// Remap should skip running jobs
	newCommit := createCommit(t, db, repo.ID, "running-newsha")
	n, err := db.RemapJobGitRef(
		repo.ID, "running-oldsha", "running-newsha",
		"patchRUN", newCommit.ID,
	)
	require.NoError(t, err, "RemapJobGitRef: %v")

	assert.Equal(t, 0, n)

	got, err := db.GetJobByID(job.ID)
	require.NoError(t, err, "GetJobByID: %v")

	assert.Equal(t, "running-oldsha", got.GitRef,

		"running job git_ref should be unchanged, got %q",
		got.GitRef)

	assert.Equal(t, JobStatusRunning, got.Status)
}

func TestRemapJob_RunningJob(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-remap-job-running")
	commit := createCommit(t, db, repo.ID, "rjrun-oldsha")

	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:   repo.ID,
		CommitID: commit.ID,
		GitRef:   "rjrun-oldsha",
		Agent:    "test",
		PatchID:  "patchRJRUN",
	})
	require.NoError(t, err, "EnqueueJob: %v")

	setJobStatus(t, db, job.ID, JobStatusRunning)

	n, err := db.RemapJob(
		repo.ID, "rjrun-oldsha", "rjrun-newsha",
		"patchRJRUN", "author", "subject", time.Now(),
	)
	require.NoError(t, err, "RemapJob: %v")

	assert.Equal(t, 0, n)

	got, err := db.GetJobByID(job.ID)
	require.NoError(t, err, "GetJobByID: %v")

	assert.Equal(t, "rjrun-oldsha", got.GitRef)
	assert.Equal(t, JobStatusRunning, got.Status)
}

func TestRemapTriggersResync(t *testing.T) {
	// After remapping a synced job, updated_at should exceed synced_at,
	// causing GetJobsToSync to include it again.
	db := openTestDB(t)
	defer db.Close()

	machineID, err := db.GetMachineID()
	require.NoError(t, err, "GetMachineID: %v")

	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err, "GetOrCreateRepo: %v")

	commit, err := db.GetOrCreateCommit(
		repo.ID, "resync-oldsha", "Author", "Subject", time.Now(),
	)
	require.NoError(t, err, "GetOrCreateCommit: %v")

	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:   repo.ID,
		CommitID: commit.ID,
		GitRef:   "resync-oldsha",
		Agent:    "test",
		PatchID:  "patchRESYNC",
	})
	require.NoError(t, err, "EnqueueJob: %v")

	// Complete and mark synced
	_, err = db.ClaimJob("worker-resync")
	require.NoError(t, err, "ClaimJob: %v")

	err = completeReviewFixture(db, job.ID, "test", "prompt", "output")
	require.NoError(t, err, "CompleteJob: %v")

	// Set synced_at to a past time so remap's updated_at is guaranteed later
	pastTime := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	_, err = db.Exec(
		`UPDATE review_jobs SET synced_at = ?, updated_at = ? WHERE id = ?`,
		pastTime, pastTime, job.ID,
	)
	require.NoError(t, err, "set synced_at: %v")

	// Verify job is NOT returned by GetJobsToSync (updated_at == synced_at)
	preJobs, err := db.GetJobsToSync(machineID, 100)
	require.NoError(t, err, "GetJobsToSync (pre): %v")

	for _, j := range preJobs {
		assert.NotEqual(t, j.ID, job.ID)
	}

	// Remap the job — this sets updated_at to time.Now() which is after pastTime
	newCommit := createCommit(t, db, repo.ID, "resync-newsha")
	n, err := db.RemapJobGitRef(
		repo.ID, "resync-oldsha", "resync-newsha",
		"patchRESYNC", newCommit.ID,
	)
	require.NoError(t, err, "RemapJobGitRef: %v")

	assert.Equal(t, 1, n)

	// Now GetJobsToSync should include it again
	postJobs, err := db.GetJobsToSync(machineID, 100)
	require.NoError(t, err, "GetJobsToSync (post): %v")

	found := false
	for _, j := range postJobs {
		if j.ID == job.ID {
			found = true
			assert.Equal(t, "resync-newsha", j.GitRef)
			break
		}
	}
	assert.True(t, found)
}
