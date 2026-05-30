package storage

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetKnownJobUUIDs(t *testing.T) {
	h := newSyncTestHelper(t)

	t.Run("returns empty when no jobs exist", func(t *testing.T) {
		uuids, err := h.db.GetKnownJobUUIDs()
		require.NoError(t, err, "GetKnownJobUUIDs failed: %v")

		assert.Empty(t, uuids)
	})

	t.Run("returns UUIDs of jobs with UUIDs", func(t *testing.T) {
		job1 := h.createPendingJob("abc123")
		job2 := h.createPendingJob("def456")

		uuids, err := h.db.GetKnownJobUUIDs()
		require.NoError(t, err, "GetKnownJobUUIDs failed: %v")

		assert.Len(t, uuids, 2)

		uuidMap := make(map[string]bool)
		for _, u := range uuids {
			uuidMap[u] = true
		}

		assert.True(t, uuidMap[job1.UUID])
		assert.True(t, uuidMap[job2.UUID])
	})
}

func TestParseSQLiteTime(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantYear int
		wantZero bool
	}{
		{
			name:     "RFC3339 with Z",
			input:    "2024-06-15T10:30:00Z",
			wantYear: 2024,
		},
		{
			name:     "RFC3339 with offset",
			input:    "2024-06-15T10:30:00-05:00",
			wantYear: 2024,
		},
		{
			name:     "RFC3339 with positive offset",
			input:    "2024-06-15T10:30:00+02:00",
			wantYear: 2024,
		},
		{
			name:     "SQLite datetime format",
			input:    "2024-06-15 10:30:00",
			wantYear: 2024,
		},
		{
			name:     "empty string",
			input:    "",
			wantZero: true,
		},
		{
			name:     "invalid format",
			input:    "not-a-date",
			wantZero: true,
		},
		{
			name:     "partial date",
			input:    "2024-06-15",
			wantZero: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSQLiteTime(tt.input)
			if tt.wantZero {
				assert.True(t, got.IsZero())
				return
			}
			assert.False(t, got.IsZero())
			assert.Equal(t, tt.wantYear, got.Year())
		})
	}
}

// syncTimestampTestCallbacks parameterizes the shared timestamp comparison
// test runner so it can be used for both job and review sync paths.
type syncTimestampTestCallbacks struct {
	// entityName is used in assertion messages (e.g. "job", "review").
	entityName string

	// setup prepares the entity under test and returns (helper, entityID).
	// It is called once per top-level test; subtests share the returned state.
	setup func(t *testing.T) (*syncTestHelper, int64)

	// setupForTZ is like setup but called inside the non-UTC timezone subtest
	// (after TZ has been changed). It returns a fresh helper and entity ID.
	setupForTZ func(t *testing.T) (*syncTestHelper, int64)

	// getToSync returns IDs of entities that need syncing.
	getToSync func(h *syncTestHelper) ([]int64, error)

	// markSynced marks the entity as synced.
	markSynced func(h *syncTestHelper, id int64) error

	// setTimestamps sets synced_at and updated_at on the entity.
	setTimestamps func(h *syncTestHelper, id int64, syncedAt sql.NullString, updatedAt string)

	// createExtra creates an additional entity for mixed-format tests.
	// Returns the helper (may be the same) and the new entity ID.
	createExtra func(h *syncTestHelper, suffix string) (*syncTestHelper, int64)
}

// testSyncTimestampComparison is a generic test runner that validates the
// timestamp comparison logic shared between job sync and review sync.
func testSyncTimestampComparison(t *testing.T, cb syncTimestampTestCallbacks) {
	t.Helper()

	h, entityID := cb.setup(t)

	t.Run(cb.entityName+" with null synced_at is returned", func(t *testing.T) {
		ids, err := cb.getToSync(h)
		require.NoError(t, err)

		assert.True(t, containsID(ids, entityID))
	})

	t.Run(cb.entityName+" after marking synced is not returned", func(t *testing.T) {
		err := cb.markSynced(h, entityID)
		require.NoError(t, err)

		ids, err := cb.getToSync(h)
		require.NoError(t, err)

		assert.False(t, containsID(ids, entityID))
	})

	t.Run(cb.entityName+" with updated_at after synced_at is returned", func(t *testing.T) {
		pastTime := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
		futureTime := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
		cb.setTimestamps(h, entityID, sql.NullString{String: pastTime, Valid: true}, futureTime)

		ids, err := cb.getToSync(h)
		require.NoError(t, err)

		assert.True(t, containsID(ids, entityID),
			"Expected %s with updated_at > synced_at to be returned for sync", cb.entityName)
	})

	t.Run("mixed format timestamps compare correctly", func(t *testing.T) {
		_, extraID := cb.createExtra(h, "mixed-format")

		cb.setTimestamps(h, extraID,
			sql.NullString{String: "2024-06-15 10:30:00", Valid: true},
			"2024-06-15T14:30:00+02:00")

		ids, err := cb.getToSync(h)
		require.NoError(t, err)

		assert.True(t, containsID(ids, extraID),
			"Expected %s with mixed format timestamps (updated_at > synced_at) to be returned", cb.entityName)

		cb.setTimestamps(h, extraID,
			sql.NullString{String: "2024-06-15 20:00:00", Valid: true},
			"2024-06-15T10:30:00Z")

		ids, err = cb.getToSync(h)
		require.NoError(t, err)

		assert.False(t, containsID(ids, extraID),
			"Expected %s with synced_at > updated_at to NOT be returned", cb.entityName)
	})

	t.Run("mixed format timestamps work correctly in non-UTC timezone", func(t *testing.T) {
		t.Setenv("TZ", "America/New_York")

		hTZ, tzEntityID := cb.setupForTZ(t)

		cb.setTimestamps(hTZ, tzEntityID,
			sql.NullString{String: "2024-06-15 10:30:00", Valid: true},
			"2024-06-15T12:30:00Z")

		ids, err := cb.getToSync(hTZ)
		require.NoError(t, err)

		assert.True(t, containsID(ids, tzEntityID),
			"Expected %s with updated_at > synced_at to be returned regardless of local timezone", cb.entityName)

		cb.setTimestamps(hTZ, tzEntityID,
			sql.NullString{String: "2024-06-15 14:00:00", Valid: true},
			"2024-06-15T12:30:00Z")

		ids, err = cb.getToSync(hTZ)
		require.NoError(t, err)

		assert.False(t, containsID(ids, tzEntityID),
			"Expected %s with synced_at > updated_at to NOT be returned regardless of local timezone", cb.entityName)
	})
}

// containsID reports whether ids contains the given id.
func containsID(ids []int64, id int64) bool {
	return slices.Contains(ids, id)
}

// jobSyncIDs extracts job IDs from GetJobsToSync results.
func jobSyncIDs(h *syncTestHelper) ([]int64, error) {
	jobs, err := h.db.GetJobsToSync(h.machineID, 10)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}
	return ids, nil
}

// reviewSyncIDs extracts review IDs from GetReviewsToSync results.
func reviewSyncIDs(h *syncTestHelper) ([]int64, error) {
	reviews, err := h.db.GetReviewsToSync(h.machineID, 10)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(reviews))
	for i, r := range reviews {
		ids[i] = r.ID
	}
	return ids, nil
}

func TestGetJobsToSync_TimestampComparison(t *testing.T) {
	testSyncTimestampComparison(t, syncTimestampTestCallbacks{
		entityName: "job",

		setup: func(t *testing.T) (*syncTestHelper, int64) {
			h := newSyncTestHelper(t)
			job := h.createCompletedJob("sync-test-sha")
			return h, job.ID
		},

		setupForTZ: func(t *testing.T) (*syncTestHelper, int64) {
			h := newSyncTestHelper(t)
			job := h.createCompletedJob("tz-test-sha")
			return h, job.ID
		},

		getToSync: jobSyncIDs,

		markSynced: func(h *syncTestHelper, id int64) error {
			return h.db.MarkJobSynced(id)
		},

		setTimestamps: func(h *syncTestHelper, id int64, syncedAt sql.NullString, updatedAt string) {
			h.setJobTimestamps(id, syncedAt, updatedAt)
		},

		createExtra: func(h *syncTestHelper, suffix string) (*syncTestHelper, int64) {
			job := h.createCompletedJob(suffix + "-sha")
			return h, job.ID
		},
	})
}

func TestGetReviewsToSync_TimestampComparison(t *testing.T) {
	testSyncTimestampComparison(t, syncTimestampTestCallbacks{
		entityName: "review",

		setup: func(t *testing.T) (*syncTestHelper, int64) {
			h := newSyncTestHelper(t)
			job := h.createCompletedJob("review-sync-sha")
			err := h.db.MarkJobSynced(job.ID)
			require.NoError(t, err, "MarkJobSynced failed: %v")
			review, err := h.db.GetReviewByJobID(job.ID)
			require.NoError(t, err, "GetReviewByJobID failed: %v")
			return h, review.ID
		},

		setupForTZ: func(t *testing.T) (*syncTestHelper, int64) {
			h := newSyncTestHelper(t)
			job := h.createCompletedJob("tz-review-sha")
			err := h.db.MarkJobSynced(job.ID)
			require.NoError(t, err, "MarkJobSynced failed: %v")
			review, err := h.db.GetReviewByJobID(job.ID)
			require.NoError(t, err, "GetReviewByJobID failed: %v")
			return h, review.ID
		},

		getToSync: reviewSyncIDs,

		markSynced: func(h *syncTestHelper, id int64) error {
			return h.db.MarkReviewSynced(id)
		},

		setTimestamps: func(h *syncTestHelper, id int64, syncedAt sql.NullString, updatedAt string) {
			h.setReviewTimestamps(id, syncedAt, updatedAt)
		},

		createExtra: func(h *syncTestHelper, _ string) (*syncTestHelper, int64) {
			// Reviews share the same helper; just need a new review entity.
			// We don't need a separate job for the mixed-format subtest,
			// but we do need a review linked to a completed job.
			job := h.createCompletedJob("mixed-fmt-review-sha")
			err := h.db.MarkJobSynced(job.ID)
			require.NoError(h.t, err, "MarkJobSynced failed: %v")
			review, err := h.db.GetReviewByJobID(job.ID)
			require.NoError(h.t, err, "GetReviewByJobID failed: %v")
			return h, review.ID
		},
	})
}

func TestSessionID_SyncRoundTrip(t *testing.T) {
	src := newSyncTestHelper(t)

	job := src.createCompletedJob("session-sync-sha")
	_, err := src.db.Exec(
		`UPDATE review_jobs SET session_id = ? WHERE id = ?`,
		"agent-session-abc", job.ID)
	require.NoError(t, err, "set session_id: %v")

	exported, err := src.db.GetJobsToSync(src.machineID, 10)
	require.NoError(t, err, "GetJobsToSync: %v")

	var syncJob *SyncableJob
	for i := range exported {
		if exported[i].ID == job.ID {
			syncJob = &exported[i]
			break
		}
	}
	assert.NotNil(t, syncJob)
	assert.Equal(t, "agent-session-abc", syncJob.SessionID)

	dst := newSyncTestHelper(t)
	pulled := PulledJob{
		UUID:            syncJob.UUID,
		RepoIdentity:    syncJob.RepoIdentity,
		CommitSHA:       syncJob.CommitSHA,
		CommitAuthor:    syncJob.CommitAuthor,
		CommitSubject:   syncJob.CommitSubject,
		CommitTimestamp: syncJob.CommitTimestamp,
		GitRef:          syncJob.GitRef,
		SessionID:       syncJob.SessionID,
		Agent:           syncJob.Agent,
		Model:           syncJob.Model,
		Reasoning:       syncJob.Reasoning,
		JobType:         syncJob.JobType,
		ReviewType:      syncJob.ReviewType,
		PatchID:         syncJob.PatchID,
		Status:          syncJob.Status,
		Agentic:         syncJob.Agentic,
		EnqueuedAt:      syncJob.EnqueuedAt,
		StartedAt:       syncJob.StartedAt,
		FinishedAt:      syncJob.FinishedAt,
		Prompt:          syncJob.Prompt,
		DiffContent:     syncJob.DiffContent,
		Error:           syncJob.Error,
		SourceMachineID: syncJob.SourceMachineID,
		UpdatedAt:       syncJob.UpdatedAt,
	}
	if err := dst.db.UpsertPulledJob(pulled, dst.repo.ID, nil); err != nil {
		require.NoError(t, err, "UpsertPulledJob: %v")
	}

	var gotSessionID sql.NullString
	err = dst.db.QueryRow(
		`SELECT session_id FROM review_jobs WHERE uuid = ?`,
		syncJob.UUID).Scan(&gotSessionID)
	require.NoError(t, err, "query imported session_id: %v")

	assert.False(t, !gotSessionID.Valid || gotSessionID.String != "agent-session-abc")
}

func TestGetCommentsToSync_LegacyCommentsExcluded(t *testing.T) {
	h := newSyncTestHelper(t)
	job := h.createCompletedJob("legacy-resp-sha")

	commit, err := h.db.GetCommitBySHA("legacy-resp-sha")
	require.NoError(t, err, "GetCommitBySHA failed: %v")

	err = h.db.MarkJobSynced(job.ID)
	require.NoError(t, err, "MarkJobSynced failed: %v")

	jobResp, err := h.db.AddCommentToJob(job.ID, "human", "This is a job response")
	require.NoError(t, err, "AddCommentToJob failed: %v")

	result, err := h.db.Exec(`
		INSERT INTO responses (commit_id, responder, response, uuid, source_machine_id, created_at)
		VALUES (?, 'human', 'This is a legacy response', ?, ?, datetime('now'))
	`, commit.ID, GenerateUUID(), h.machineID)
	require.NoError(t, err, "Failed to insert legacy response: %v")

	legacyRespID, _ := result.LastInsertId()

	responses, err := h.db.GetCommentsToSync(h.machineID, 100)
	require.NoError(t, err, "GetCommentsToSync failed: %v")

	foundJobResp := false
	foundLegacyResp := false
	for _, r := range responses {
		if r.ID == jobResp.ID {
			foundJobResp = true
		}
		if r.ID == legacyRespID {
			foundLegacyResp = true
		}
	}

	assert.True(t, foundJobResp)
	assert.False(t, foundLegacyResp, "Expected legacy response (job_id IS NULL) to be EXCLUDED from sync")
}

func TestGetJobsToSync_IncludesSkipped(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repoID := createRepo(t, db, "/tmp/repo-sync-skipped").ID
	commitID := createCommit(t, db, repoID, "deadf00d").ID
	_, err := db.Exec(`
		INSERT INTO review_jobs
		  (repo_id, commit_id, git_ref, status, review_type, uuid, source_machine_id, updated_at)
		VALUES (?, ?, 'deadf00d', 'skipped', 'design', 'test-uuid-1', 'test-machine', datetime('now'))
	`, repoID, commitID)
	require.NoError(t, err)

	jobs, err := db.GetJobsToSync("test-machine", 100)
	require.NoError(t, err)
	found := false
	for _, j := range jobs {
		if j.UUID == "test-uuid-1" {
			found = true
			assert.Equal(t, "skipped", j.Status)
		}
	}
	assert.True(t, found, "expected skipped job to be syncable")
}
