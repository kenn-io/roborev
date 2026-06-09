package storage

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpsertPulledResponse_MissingParentJob(t *testing.T) {
	// This test verifies that UpsertPulledResponse gracefully handles responses
	// for jobs that don't exist locally (returns nil, doesn't error)
	db := openTestDB(t)
	defer db.Close()

	// Try to upsert a response for a job that doesn't exist
	nonexistentJobUUID := GenerateUUID()
	response := PulledResponse{
		UUID:            GenerateUUID(),
		JobUUID:         nonexistentJobUUID,
		Responder:       "human",
		Response:        "Test response for missing job",
		SourceMachineID: GenerateUUID(),
		CreatedAt:       time.Now(),
	}

	// Should return nil (not error) for missing parent job
	err := db.UpsertPulledResponse(response)
	require.NoError(t, err, "Expected nil error for missing parent job")

	// Verify no response was inserted
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM responses WHERE uuid = ?`, response.UUID).Scan(&count)
	require.NoError(t, err, "Failed to count responses")
	assert.Equal(t, 0, count, "Expected 0 responses for missing parent job")
}

func TestUpsertPulledResponse_WithParentJob(t *testing.T) {
	// This test verifies UpsertPulledResponse works when the parent job exists
	h := newSyncTestHelper(t)
	job := h.createPendingJob("parent-job-sha")

	// Upsert a response for the existing job
	response := PulledResponse{
		UUID:            GenerateUUID(),
		JobUUID:         job.UUID,
		Responder:       "human",
		Response:        "Test response for existing job",
		SourceMachineID: GenerateUUID(),
		CreatedAt:       time.Now(),
	}

	err := h.db.UpsertPulledResponse(response)
	require.NoError(t, err, "UpsertPulledResponse failed: %v")

	// Verify response was inserted
	var count int
	err = h.db.QueryRow(`SELECT COUNT(*) FROM responses WHERE uuid = ?`, response.UUID).Scan(&count)
	require.NoError(t, err, "Failed to count responses: %v")

	assert.Equal(t, 1, count)
}

func TestSyncCursorLookbackDefaultAndOverride(t *testing.T) {
	t.Setenv(syncCursorLookbackEnv, "")
	assert.Equal(t, defaultSyncCursorLookback, syncCursorLookback())

	t.Setenv(syncCursorLookbackEnv, "30s")
	assert.Equal(t, 30*time.Second, syncCursorLookback())

	t.Setenv(syncCursorLookbackEnv, "bad")
	assert.Equal(t, defaultSyncCursorLookback, syncCursorLookback())
}

func TestRewindResponseCursorRewindsTimestampAndResetsLegacyID(t *testing.T) {
	cursorTime := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	cursor := formatTimestampIDCursor(cursorTime, 42)

	assert.Equal(t, formatTimestampIDCursor(cursorTime.Add(-30*time.Second), 42), rewindResponseCursor(cursor, 30*time.Second))
	assert.Empty(t, rewindResponseCursor("42", 30*time.Second))
	assert.Empty(t, responseCursorForMax("42"))
	assert.Equal(t, cursor, responseCursorForMax(cursor))
}

func TestUpsertPulledReviewSkipsStaleRemoteUpdate(t *testing.T) {
	h := newSyncTestHelper(t)
	job := h.createCompletedJob("stale-review-sha")
	review, err := h.db.GetReviewByJobID(job.ID)
	require.NoError(t, err)

	require.NoError(t, h.db.MarkReviewClosed(review.ID, true))

	err = h.db.UpsertPulledReview(PulledReview{
		UUID:               review.UUID,
		JobUUID:            job.UUID,
		Agent:              review.Agent,
		Prompt:             review.Prompt,
		Output:             review.Output,
		Closed:             false,
		UpdatedByMachineID: GenerateUUID(),
		CreatedAt:          review.CreatedAt,
		UpdatedAt:          time.Now().Add(-time.Hour),
	})
	require.NoError(t, err)

	review, err = h.db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assert.True(t, review.Closed)
}

// TestClearAllSyncedAt verifies that ClearAllSyncedAt clears synced_at
// on all tables (jobs, reviews, responses).
func TestClearAllSyncedAt(t *testing.T) {
	h := newSyncTestHelper(t)

	// Create a completed job with a review
	job := h.createCompletedJob("clear-test-sha")

	// Add a response
	_, err := h.db.AddCommentToJob(job.ID, "user", "test response")
	require.NoError(t, err, "AddCommentToJob failed: %v")

	// Mark everything as synced
	if err := h.db.MarkJobSynced(job.ID); err != nil {
		require.NoError(t, err, "MarkJobSynced failed: %v")
	}
	review, err := h.db.GetReviewByJobID(job.ID)
	require.NoError(t, err, "GetReviewByJobID failed: %v")

	if err := h.db.MarkReviewSynced(review.ID); err != nil {
		require.NoError(t, err, "MarkReviewSynced failed: %v")
	}

	// Verify nothing needs to sync
	jobs, _ := h.db.GetJobsToSync(h.machineID, 100)
	assert.Empty(t, jobs)
	reviews, _ := h.db.GetReviewsToSync(h.machineID, 100)
	assert.Empty(t, reviews)

	// Clear all synced_at
	if err := h.db.ClearAllSyncedAt(); err != nil {
		require.NoError(t, err, "ClearAllSyncedAt failed: %v")
	}

	// Now everything should need to sync again
	jobs, err = h.db.GetJobsToSync(h.machineID, 100)
	require.NoError(t, err, "GetJobsToSync failed: %v")

	assert.Len(t, jobs, 1)

	// Mark job synced so reviews become available
	if err := h.db.MarkJobSynced(job.ID); err != nil {
		require.NoError(t, err, "MarkJobSynced failed: %v")
	}

	reviews, err = h.db.GetReviewsToSync(h.machineID, 100)
	require.NoError(t, err, "GetReviewsToSync failed: %v")

	assert.Len(t, reviews, 1)

	responses, err := h.db.GetCommentsToSync(h.machineID, 100)
	require.NoError(t, err, "GetCommentsToSync failed: %v")

	assert.Len(t, responses, 1)
}

// TestBatchMarkSynced verifies the batch MarkXSynced functions work correctly.
func TestBatchMarkSynced(t *testing.T) {
	h := newSyncTestHelper(t)

	// Create multiple jobs with reviews and responses
	var jobs []*ReviewJob
	for i := range 5 {
		job := h.createCompletedJob(fmt.Sprintf("batch-test-sha-%d", i))
		jobs = append(jobs, job)
		_, err := h.db.AddCommentToJob(job.ID, "user", fmt.Sprintf("response %d", i))
		require.NoError(t, err, "AddCommentToJob failed: %v")

	}

	t.Run("MarkJobsSynced marks multiple jobs", func(t *testing.T) {
		// Get jobs to sync before
		toSync, err := h.db.GetJobsToSync(h.machineID, 100)
		require.NoError(t, err, "GetJobsToSync failed: %v")

		assert.Len(t, toSync, 5)

		// Mark first 3 as synced, carrying each job's snapshot updated_at.
		markIDs := map[int64]bool{jobs[0].ID: true, jobs[1].ID: true, jobs[2].ID: true}
		var marks []JobSyncMark
		for _, j := range toSync {
			if markIDs[j.ID] {
				marks = append(marks, NewJobSyncMark(j))
			}
		}
		require.NoError(t, h.db.MarkJobsSynced(marks), "MarkJobsSynced failed")

		// Verify only 2 jobs left to sync
		toSync, err = h.db.GetJobsToSync(h.machineID, 100)
		require.NoError(t, err, "GetJobsToSync failed: %v")

		assert.Len(t, toSync, 2)
	})

	t.Run("MarkReviewsSynced marks multiple reviews", func(t *testing.T) {
		// Get reviews for synced jobs
		reviews, err := h.db.GetReviewsToSync(h.machineID, 100)
		require.NoError(t, err, "GetReviewsToSync failed: %v")

		assert.Len(t, reviews, 3)

		// Mark all 3 as synced
		reviewIDs := make([]int64, len(reviews))
		for i, r := range reviews {
			reviewIDs[i] = r.ID
		}
		if err := h.db.MarkReviewsSynced(reviewIDs); err != nil {
			require.NoError(t, err, "MarkReviewsSynced failed: %v")
		}

		// Verify no reviews left to sync (for synced jobs)
		reviews, err = h.db.GetReviewsToSync(h.machineID, 100)
		require.NoError(t, err, "GetReviewsToSync failed: %v")

		assert.Empty(t, reviews)
	})

	t.Run("MarkCommentsSynced marks multiple comments", func(t *testing.T) {
		// Get responses for synced jobs
		responses, err := h.db.GetCommentsToSync(h.machineID, 100)
		require.NoError(t, err, "GetCommentsToSync failed: %v")

		assert.Len(t, responses, 3)

		// Mark all 3 as synced
		responseIDs := make([]int64, len(responses))
		for i, r := range responses {
			responseIDs[i] = r.ID
		}
		if err := h.db.MarkCommentsSynced(responseIDs); err != nil {
			require.NoError(t, err, "MarkCommentsSynced failed: %v")
		}

		// Verify no responses left to sync (for synced jobs)
		responses, err = h.db.GetCommentsToSync(h.machineID, 100)
		require.NoError(t, err, "GetCommentsToSync failed: %v")

		assert.Empty(t, responses)
	})

	t.Run("empty slice is no-op", func(t *testing.T) {
		// Empty slices should not error
		if err := h.db.MarkJobsSynced([]JobSyncMark{}); err != nil {
			require.NoError(t, err)
		}
		if err := h.db.MarkReviewsSynced([]int64{}); err != nil {
			require.NoError(t, err)
		}
		if err := h.db.MarkCommentsSynced([]int64{}); err != nil {
			require.NoError(t, err)
		}
	})
}

// TestMarkJobsSyncedSkipsRowsChangedSinceSnapshot verifies the push-cursor
// guard: when a job's updated_at changes between the pushed snapshot and the
// mark (as happens when token usage is captured just after the job goes
// terminal), MarkJobsSynced must not advance synced_at, so the newer value is
// re-pushed on the next cycle instead of being stranded behind the cursor.
func TestMarkJobsSyncedSkipsRowsChangedSinceSnapshot(t *testing.T) {
	h := newSyncTestHelper(t)
	job := h.createCompletedJob("cursor-race-sha")

	// Snapshot the job as the push loop would.
	snapshot, err := h.db.GetJobsToSync(h.machineID, 100)
	require.NoError(t, err)
	require.Len(t, snapshot, 1)
	snapshotUpdatedAt := snapshot[0].UpdatedAtRaw
	require.NotEmpty(t, snapshotUpdatedAt)

	// A concurrent write (e.g. SaveJobTokenUsage capturing cost) bumps
	// updated_at after the snapshot was taken but before the mark runs.
	const laterUpdatedAt = "2026-06-01T00:00:00Z"
	h.setJobTimestamps(job.ID, sql.NullString{}, laterUpdatedAt)

	// Marking with the stale snapshot value must not advance synced_at.
	require.NoError(t, h.db.MarkJobsSynced([]JobSyncMark{NewJobSyncMark(snapshot[0])}))

	var syncedAt sql.NullString
	require.NoError(t, h.db.QueryRow(`SELECT synced_at FROM review_jobs WHERE id = ?`, job.ID).Scan(&syncedAt))
	assert.False(t, syncedAt.Valid, "synced_at must stay NULL for a row changed since the snapshot")

	stillToSync, err := h.db.GetJobsToSync(h.machineID, 100)
	require.NoError(t, err)
	assert.Len(t, stillToSync, 1, "changed job must remain eligible for the next push")

	// Marking with the current updated_at advances the cursor as normal.
	currentMark := NewJobSyncMark(snapshot[0])
	currentMark.UpdatedAt = laterUpdatedAt
	require.NoError(t, h.db.MarkJobsSynced([]JobSyncMark{currentMark}))
	done, err := h.db.GetJobsToSync(h.machineID, 100)
	require.NoError(t, err)
	assert.Empty(t, done, "job marked with the matching updated_at must be synced")
}

// TestMarkJobsSyncedSkipsCostWrittenInSameSecond covers the same-second case:
// a job is marked terminal and its token usage is captured within the same
// RFC3339 second, so updated_at is byte-identical between the pushed snapshot
// and the post-capture row. The token_usage change alone must still keep the
// row eligible so the cost is re-pushed instead of stranded.
func TestMarkJobsSyncedSkipsCostWrittenInSameSecond(t *testing.T) {
	h := newSyncTestHelper(t)
	job := h.createCompletedJob("same-second-cost-sha")

	// Pin the row to a fixed second with no token usage, as the terminal write
	// leaves it during the capture window.
	const terminalUpdatedAt = "2026-06-07T15:26:10Z"
	_, err := h.db.Exec(
		`UPDATE review_jobs SET updated_at = ?, token_usage = NULL, synced_at = NULL WHERE id = ?`,
		terminalUpdatedAt, job.ID)
	require.NoError(t, err)

	// Snapshot the transient terminal row (token_usage NULL).
	snapshot, err := h.db.GetJobsToSync(h.machineID, 100)
	require.NoError(t, err)
	require.Len(t, snapshot, 1)
	require.Equal(t, terminalUpdatedAt, snapshot[0].UpdatedAtRaw)
	require.Empty(t, snapshot[0].TokenUsage)

	// Token usage is captured in the same second: token_usage changes but the
	// formatted updated_at second is unchanged (identical string).
	_, err = h.db.Exec(
		`UPDATE review_jobs SET token_usage = ?, updated_at = ? WHERE id = ?`,
		`{"has_cost":true,"total_cost_usd":0.5}`, terminalUpdatedAt, job.ID)
	require.NoError(t, err)

	// Marking with the snapshot (no token usage) must not advance synced_at.
	require.NoError(t, h.db.MarkJobsSynced([]JobSyncMark{NewJobSyncMark(snapshot[0])}))

	var syncedAt sql.NullString
	require.NoError(t, h.db.QueryRow(`SELECT synced_at FROM review_jobs WHERE id = ?`, job.ID).Scan(&syncedAt))
	assert.False(t, syncedAt.Valid, "synced_at must stay NULL when token usage changed in the same second")

	stillToSync, err := h.db.GetJobsToSync(h.machineID, 100)
	require.NoError(t, err)
	assert.Len(t, stillToSync, 1, "cost write must keep the job eligible for re-push")
	assert.NotEmpty(t, stillToSync[0].TokenUsage, "re-pushed snapshot must carry the captured cost")
}

// TestMarkJobsSyncedSkipsReenqueuedRowInSameSecond covers the reset race: an
// unpriced terminal job is pushed, then re-enqueued (clearing cost metadata and
// synced_at) in the same RFC3339 second, leaving updated_at and token_usage
// matching the snapshot. The status change (done -> queued) must keep the stale
// mark from restoring synced_at over the reset's NULL, so the cleared-cost rerun
// stays eligible and PostgreSQL does not keep stale spend.
func TestMarkJobsSyncedSkipsReenqueuedRowInSameSecond(t *testing.T) {
	h := newSyncTestHelper(t)
	job := h.createCompletedJob("reenqueue-mark-race-sha")

	// Pin the terminal row to a fixed second with no cost (unpriced done).
	const sameSecond = "2026-06-07T15:26:10Z"
	_, err := h.db.Exec(
		`UPDATE review_jobs SET updated_at = ?, token_usage = NULL, synced_at = NULL WHERE id = ?`,
		sameSecond, job.ID)
	require.NoError(t, err)

	// Snapshot it as the push loop would.
	snapshot, err := h.db.GetJobsToSync(h.machineID, 100)
	require.NoError(t, err)
	require.Len(t, snapshot, 1)
	mark := NewJobSyncMark(snapshot[0])

	// Re-enqueue in the same second: clears cost metadata and synced_at.
	// ReenqueueJob also sets updated_at, so pin it back to the same second; now
	// updated_at and token_usage alone would still match the snapshot.
	require.NoError(t, h.db.ReenqueueJob(job.ID, ReenqueueOpts{}))
	_, err = h.db.Exec(`UPDATE review_jobs SET updated_at = ? WHERE id = ?`, sameSecond, job.ID)
	require.NoError(t, err)

	require.NoError(t, h.db.MarkJobsSynced([]JobSyncMark{mark}))

	var syncedAt sql.NullString
	require.NoError(t, h.db.QueryRow(`SELECT synced_at FROM review_jobs WHERE id = ?`, job.ID).Scan(&syncedAt))
	assert.False(t, syncedAt.Valid,
		"reset synced_at=NULL must survive: a stale mark cannot match the re-enqueued row")
}

// TestMarkJobsSyncedSkipsRowWithChangedAttemptMarkers verifies the snapshot guard
// also pins the specific attempt: if session_id or agent_invoked changed since
// the push (as a same-second re-completion would) while updated_at, token_usage,
// and status stay identical, MarkJobsSynced must not advance synced_at.
func TestMarkJobsSyncedSkipsRowWithChangedAttemptMarkers(t *testing.T) {
	const fixedSecond = "2026-06-07T15:26:10Z"
	cases := []struct {
		name    string
		diverge func(t *testing.T, h *syncTestHelper, jobID int64)
	}{
		{
			name: "session_id changed",
			diverge: func(t *testing.T, h *syncTestHelper, jobID int64) {
				_, err := h.db.Exec(`UPDATE review_jobs SET session_id = 's2' WHERE id = ?`, jobID)
				require.NoError(t, err)
			},
		},
		{
			name: "agent_invoked changed",
			diverge: func(t *testing.T, h *syncTestHelper, jobID int64) {
				_, err := h.db.Exec(`UPDATE review_jobs SET agent_invoked = 0 WHERE id = ?`, jobID)
				require.NoError(t, err)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newSyncTestHelper(t)
			job := h.createCompletedJob("attempt-marker-sha")

			// A pushed terminal attempt with a known session and agent marker.
			_, err := h.db.Exec(
				`UPDATE review_jobs SET updated_at = ?, token_usage = NULL,
				     session_id = 's1', agent_invoked = 1, synced_at = NULL WHERE id = ?`,
				fixedSecond, job.ID)
			require.NoError(t, err)

			snapshot, err := h.db.GetJobsToSync(h.machineID, 100)
			require.NoError(t, err)
			require.Len(t, snapshot, 1)
			mark := NewJobSyncMark(snapshot[0])

			// The attempt marker changes in the same second; nothing else does.
			tc.diverge(t, h, job.ID)

			require.NoError(t, h.db.MarkJobsSynced([]JobSyncMark{mark}))

			var syncedAt sql.NullString
			require.NoError(t, h.db.QueryRow(
				`SELECT synced_at FROM review_jobs WHERE id = ?`, job.ID).Scan(&syncedAt))
			assert.False(t, syncedAt.Valid,
				"synced_at must stay NULL when %s since the snapshot", tc.name)
		})
	}
}

// TestMarkJobsSyncedSkipsSameSecondRecompletionWithChangedAttemptMetadata covers
// a stale push snapshot followed by a same-second terminal re-completion from an
// unpriced, sessionless agent. The existing cost guard fields can all match the
// stale snapshot, so attempt metadata must also pin the exact row that was pushed.
func TestMarkJobsSyncedSkipsSameSecondRecompletionWithChangedAttemptMetadata(t *testing.T) {
	const fixedSecond = "2026-06-07T15:26:10Z"
	const originalStartedAt = "2026-06-07T15:26:09Z"
	const changedStartedAt = "2026-06-07T15:26:10Z"
	const changedFinishedAt = "2026-06-07T15:26:11Z"

	cases := []struct {
		name    string
		diverge string
	}{
		{name: "agent changed", diverge: `agent = 'claude'`},
		{name: "model changed", diverge: `model = 'gpt-5'`},
		{name: "provider changed", diverge: `provider = 'anthropic'`},
		{name: "error changed", diverge: `error = 'new failure'`},
		{name: "started_at changed", diverge: `started_at = '` + changedStartedAt + `'`},
		{name: "finished_at changed", diverge: `finished_at = '` + changedFinishedAt + `'`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newSyncTestHelper(t)
			job := h.createCompletedJob("same-second-recompletion-sha")

			_, err := h.db.Exec(
				`UPDATE review_jobs SET updated_at = ?, token_usage = NULL,
				     status = 'failed', session_id = NULL, agent_invoked = 1,
				     agent = 'codex', model = 'o3', provider = 'openai',
				     error = 'old failure', started_at = ?, finished_at = ?,
				     synced_at = NULL
				 WHERE id = ?`,
				fixedSecond, originalStartedAt, fixedSecond, job.ID)
			require.NoError(t, err)

			snapshot, err := h.db.GetJobsToSync(h.machineID, 100)
			require.NoError(t, err)
			require.Len(t, snapshot, 1)
			mark := NewJobSyncMark(snapshot[0])

			_, err = h.db.Exec(
				`UPDATE review_jobs SET `+tc.diverge+`,
				     updated_at = ?, token_usage = NULL,
				     status = 'failed', session_id = NULL, agent_invoked = 1
				 WHERE id = ?`,
				fixedSecond, job.ID)
			require.NoError(t, err)

			require.NoError(t, h.db.MarkJobsSynced([]JobSyncMark{mark}))

			var syncedAt sql.NullString
			require.NoError(t, h.db.QueryRow(
				`SELECT synced_at FROM review_jobs WHERE id = ?`, job.ID).Scan(&syncedAt))
			assert.False(t, syncedAt.Valid,
				"synced_at must stay NULL when %s since the pushed snapshot", tc.name)
		})
	}
}

// TestSaveJobTokenUsageInvalidatesSyncCursor covers the ordering where the cost
// capture lands after MarkJobsSynced. The job is already marked synced with
// updated_at in the same RFC3339 second as synced_at, so the cursor comparison
// alone (updated_at > synced_at) can never re-select it. SaveJobTokenUsage must
// clear synced_at so the cost still reaches PostgreSQL.
func TestSaveJobTokenUsageInvalidatesSyncCursor(t *testing.T) {
	h := newSyncTestHelper(t)
	job := h.createCompletedJob("post-mark-cost-sha")

	// Model a job already marked synced under a known session, with updated_at
	// in the same second as synced_at, so the cursor would never re-select it
	// on its own.
	const sameSecond = "2026-06-07T15:26:10Z"
	_, err := h.db.Exec(
		`UPDATE review_jobs SET session_id = ?, synced_at = ?, updated_at = ? WHERE id = ?`,
		"sess-1", sameSecond, sameSecond, job.ID)
	require.NoError(t, err)

	before, err := h.db.GetJobsToSync(h.machineID, 100)
	require.NoError(t, err)
	require.Empty(t, before, "precondition: synced_at == updated_at must not re-select")

	// A token-usage capture lands after the mark, in the same second.
	require.NoError(t, h.db.SaveJobTokenUsage(job.ID, "sess-1", `{"has_cost":true,"total_cost_usd":0.5}`))

	var syncedAt sql.NullString
	require.NoError(t, h.db.QueryRow(`SELECT synced_at FROM review_jobs WHERE id = ?`, job.ID).Scan(&syncedAt))
	assert.False(t, syncedAt.Valid, "token-usage write must clear synced_at")

	after, err := h.db.GetJobsToSync(h.machineID, 100)
	require.NoError(t, err)
	require.Len(t, after, 1, "cost write after marking synced must re-select the job")
	assert.NotEmpty(t, after[0].TokenUsage, "re-selected snapshot carries the captured cost")
}

// TestGetReviewsToSync_RequiresJobSynced verifies that reviews are only
// returned when their parent job has been synced (j.synced_at IS NOT NULL).
func TestGetReviewsToSync_RequiresJobSynced(t *testing.T) {
	h := newSyncTestHelper(t)

	// Create a completed job (not synced yet)
	job := h.createCompletedJob("sync-order-sha")

	// Before job is synced, GetReviewsToSync should return nothing
	reviews, err := h.db.GetReviewsToSync(h.machineID, 100)
	require.NoError(t, err, "GetReviewsToSync failed: %v")

	assert.Empty(t, reviews)

	// Mark job as synced
	if err := h.db.MarkJobSynced(job.ID); err != nil {
		require.NoError(t, err, "Failed to mark job synced: %v")
	}

	// Now GetReviewsToSync should return the review
	reviews, err = h.db.GetReviewsToSync(h.machineID, 100)
	require.NoError(t, err, "GetReviewsToSync failed: %v")

	assert.Len(t, reviews, 1)
}

// TestGetCommentsToSync_RequiresJobSynced verifies that responses are only
// returned when their parent job has been synced (j.synced_at IS NOT NULL).
func TestGetCommentsToSync_RequiresJobSynced(t *testing.T) {
	h := newSyncTestHelper(t)

	// Create a completed job (not synced yet)
	job := h.createCompletedJob("response-sync-sha")

	// Add a response to the job
	_, err := h.db.AddCommentToJob(job.ID, "test-user", "test response")
	require.NoError(t, err, "Failed to add response: %v")

	// Before job is synced, GetResponsesToSync should return nothing
	responses, err := h.db.GetCommentsToSync(h.machineID, 100)
	require.NoError(t, err, "GetCommentsToSync failed: %v")

	assert.Empty(t, responses)

	// Mark job as synced
	if err := h.db.MarkJobSynced(job.ID); err != nil {
		require.NoError(t, err, "Failed to mark job synced: %v")
	}

	// Now GetResponsesToSync should return the response
	responses, err = h.db.GetCommentsToSync(h.machineID, 100)
	require.NoError(t, err, "GetCommentsToSync failed: %v")

	assert.Len(t, responses, 1)
}

// TestGetJobsToSync_RequiresRepoIdentity verifies that jobs without a
// repo identity are still returned (the identity check happens at push time).
func TestGetJobsToSync_RequiresRepoIdentity(t *testing.T) {
	h := newSyncTestHelper(t)

	// Create a completed job
	_ = h.createCompletedJob("identity-test-sha")

	// Verify repo has no identity initially (GetOrCreateRepo doesn't set one)
	var identity sql.NullString
	err := h.db.QueryRow(`SELECT identity FROM repos WHERE id = ?`, h.repo.ID).Scan(&identity)
	require.NoError(t, err, "Failed to query repo: %v")

	assert.False(t, identity.Valid && identity.String != "")

	// GetJobsToSync should return the job (with empty identity)
	jobs, err := h.db.GetJobsToSync(h.machineID, 100)
	require.NoError(t, err, "GetJobsToSync failed: %v")

	assert.Len(t, jobs, 1)
	assert.Empty(t, jobs[0].RepoIdentity)

	// Now set the repo identity
	if err := h.db.SetRepoIdentity(h.repo.ID, "git@github.com:test/repo.git"); err != nil {
		require.NoError(t, err, "Failed to set repo identity: %v")
	}

	// GetJobsToSync should now return the job with identity
	jobs, err = h.db.GetJobsToSync(h.machineID, 100)
	require.NoError(t, err, "GetJobsToSync failed: %v")

	assert.Len(t, jobs, 1)
	assert.Equal(t, "git@github.com:test/repo.git", jobs[0].RepoIdentity)
}

// TestSyncOrder_FullWorkflow tests the complete sync ordering:
// 1. Jobs must be synced first
// 2. Reviews can only sync after their job is synced
// 3. Responses can only sync after their job is synced
func TestSyncOrder_FullWorkflow(t *testing.T) {
	h := newSyncTestHelper(t)

	// Set repo identity
	if err := h.db.SetRepoIdentity(h.repo.ID, "git@github.com:test/workflow.git"); err != nil {
		require.NoError(t, err, "Failed to set repo identity: %v")
	}

	// Create 3 jobs with reviews and responses
	var createdJobs []*ReviewJob
	for i := range 3 {
		job := h.createCompletedJob("workflow-sha-" + string(rune('a'+i)))
		createdJobs = append(createdJobs, job)
		// Add a response
		_, err := h.db.AddCommentToJob(job.ID, "user", "response")
		require.NoError(t, err, "Failed to add response %d: %v", i)

	}

	// Initial state: 3 jobs to sync, 0 reviews (jobs not synced), 0 responses (jobs not synced)
	jobs, err := h.db.GetJobsToSync(h.machineID, 100)
	require.NoError(t, err, "GetJobsToSync failed: %v")

	assert.Len(t, jobs, 3)

	reviews, err := h.db.GetReviewsToSync(h.machineID, 100)
	require.NoError(t, err, "GetReviewsToSync failed: %v")

	assert.Empty(t, reviews)

	responses, err := h.db.GetCommentsToSync(h.machineID, 100)
	require.NoError(t, err, "GetCommentsToSync failed: %v")

	assert.Empty(t, responses)

	// Sync first job
	if err := h.db.MarkJobSynced(createdJobs[0].ID); err != nil {
		require.NoError(t, err, "Failed to mark job synced: %v")
	}

	// Now: 2 jobs to sync, 1 review (first job synced), 1 response (first job synced)
	jobs, err = h.db.GetJobsToSync(h.machineID, 100)
	require.NoError(t, err, "GetJobsToSync failed: %v")

	assert.Len(t, jobs, 2)

	reviews, err = h.db.GetReviewsToSync(h.machineID, 100)
	require.NoError(t, err, "GetReviewsToSync failed: %v")

	assert.Len(t, reviews, 1)

	responses, err = h.db.GetCommentsToSync(h.machineID, 100)
	require.NoError(t, err, "GetCommentsToSync failed: %v")

	assert.Len(t, responses, 1)

	// Sync remaining jobs
	for _, j := range createdJobs[1:] {
		if err := h.db.MarkJobSynced(j.ID); err != nil {
			require.NoError(t, err, "Failed to mark job synced: %v")
		}
	}

	// Now: 0 jobs to sync, 3 reviews, 3 responses
	jobs, err = h.db.GetJobsToSync(h.machineID, 100)
	require.NoError(t, err, "GetJobsToSync failed: %v")

	assert.Empty(t, jobs)

	reviews, err = h.db.GetReviewsToSync(h.machineID, 100)
	require.NoError(t, err, "GetReviewsToSync failed: %v")

	assert.Len(t, reviews, 3)

	responses, err = h.db.GetCommentsToSync(h.machineID, 100)
	require.NoError(t, err, "GetCommentsToSync failed: %v")

	assert.Len(t, responses, 3)
}
