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

// TestAgentInvoked_SyncRoundTrip verifies the agent_invoked marker survives a
// full push/pull cycle: GetJobsToSync must export it (push side) and
// UpsertPulledJob must import it (pull side). The marker carries the "an agent
// ran" cost-eligibility signal across machines, since command_line is not synced.
func TestAgentInvoked_SyncRoundTrip(t *testing.T) {
	src := newSyncTestHelper(t)

	job := src.createCompletedJob("agent-invoked-sync-sha")
	setJobAgentInvoked(t, src.db, job.ID)

	exported, err := src.db.GetJobsToSync(src.machineID, 10)
	require.NoError(t, err, "GetJobsToSync: %v")

	var syncJob *SyncableJob
	for i := range exported {
		if exported[i].ID == job.ID {
			syncJob = &exported[i]
			break
		}
	}
	require.NotNil(t, syncJob)
	assert.True(t, syncJob.AgentInvoked, "push side exports the agent_invoked marker")

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
		AgentInvoked:    syncJob.AgentInvoked,
		EnqueuedAt:      syncJob.EnqueuedAt,
		StartedAt:       syncJob.StartedAt,
		FinishedAt:      syncJob.FinishedAt,
		Prompt:          syncJob.Prompt,
		DiffContent:     syncJob.DiffContent,
		Error:           syncJob.Error,
		SourceMachineID: syncJob.SourceMachineID,
		UpdatedAt:       syncJob.UpdatedAt,
	}
	require.NoError(t, dst.db.UpsertPulledJob(pulled, dst.repo.ID, nil),
		"UpsertPulledJob: %v")

	var gotInvoked int
	require.NoError(t, dst.db.QueryRow(
		`SELECT agent_invoked FROM review_jobs WHERE uuid = ?`, syncJob.UUID).Scan(&gotInvoked),
		"query imported agent_invoked: %v")
	assert.Equal(t, 1, gotInvoked, "pull side imports the agent_invoked marker")
}

// TestUpsertPulledJob_SessionTerminalOverwrite verifies the SQLite pull path
// treats session_id like token_usage and agent_invoked: a terminal row
// overwrites it (including to empty), so a rerun whose terminal attempt captured
// no session cannot retain the prior attempt's session id and reattach stale
// cost to it. A non-terminal row still preserves an existing session.
func TestUpsertPulledJob_SessionTerminalOverwrite(t *testing.T) {
	assert := assert.New(t)
	dst := newSyncTestHelper(t)

	t1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

	pull := func(uuid, sessionID, status string, updatedAt time.Time) {
		started := updatedAt.Add(-time.Minute)
		var finished *time.Time
		if status != "running" && status != "queued" {
			finished = &updatedAt
		}
		pj := PulledJob{
			UUID:            uuid,
			GitRef:          "ref-" + uuid,
			SessionID:       sessionID,
			Agent:           "test",
			JobType:         "review",
			Status:          status,
			EnqueuedAt:      t1,
			StartedAt:       &started,
			FinishedAt:      finished,
			SourceMachineID: "remote-machine",
			UpdatedAt:       updatedAt,
		}
		require.NoError(t, dst.db.UpsertPulledJob(pj, dst.repo.ID, nil),
			"UpsertPulledJob(%s, status=%s)", uuid, status)
	}

	sessionOf := func(uuid string) string {
		var s sql.NullString
		require.NoError(t, dst.db.QueryRow(
			`SELECT session_id FROM review_jobs WHERE uuid = ?`, uuid).Scan(&s))
		return s.String
	}

	// A terminal rerun overwrites the synced session, even when empty.
	pull("term-uuid", "session-1", "done", t1)
	require.Equal(t, "session-1", sessionOf("term-uuid"), "first sync stores the session")
	pull("term-uuid", "", "done", t2)
	assert.Empty(sessionOf("term-uuid"),
		"terminal rerun with no session clears the stale session id")

	// A non-terminal update preserves an existing session.
	pull("run-uuid", "keep-me", "done", t1)
	pull("run-uuid", "", "running", t2)
	assert.Equal("keep-me", sessionOf("run-uuid"),
		"non-terminal update preserves the existing session id")
}

// TestUpsertPulledJob_SkippedRerunOverwritesStaleMarkers verifies that a
// skipped row — a synced, rerun-eligible terminal state — overwrites the
// per-attempt markers (session_id, token_usage, agent_invoked) instead of
// merging them. A rerun that ends in skip after a priced attempt must not
// retain the prior attempt's cost, session, or agent-ran markers.
func TestUpsertPulledJob_SkippedRerunOverwritesStaleMarkers(t *testing.T) {
	assert := assert.New(t)
	dst := newSyncTestHelper(t)

	t1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	started := t1.Add(-time.Minute)
	const uuid = "skip-rerun-uuid"

	// Attempt 1: a priced agent run synced as done.
	require.NoError(t, dst.db.UpsertPulledJob(PulledJob{
		UUID:            uuid,
		GitRef:          "ref",
		SessionID:       "session-1",
		Agent:           "test",
		JobType:         "review",
		Status:          "done",
		AgentInvoked:    true,
		TokenUsage:      `{"cost_usd":5.0,"has_cost":true}`,
		EnqueuedAt:      t1,
		StartedAt:       &started,
		FinishedAt:      &t1,
		SourceMachineID: "remote-machine",
		UpdatedAt:       t1,
	}, dst.repo.ID, nil), "UpsertPulledJob (done)")

	// Attempt 2: a rerun that ends in skip, with the markers cleared at source.
	require.NoError(t, dst.db.UpsertPulledJob(PulledJob{
		UUID:            uuid,
		GitRef:          "ref",
		SessionID:       "",
		Agent:           "test",
		JobType:         "review",
		Status:          "skipped",
		AgentInvoked:    false,
		TokenUsage:      "",
		EnqueuedAt:      t1,
		FinishedAt:      &t2,
		SourceMachineID: "remote-machine",
		UpdatedAt:       t2,
	}, dst.repo.ID, nil), "UpsertPulledJob (skipped)")

	var session, tokenUsage sql.NullString
	var invoked int
	require.NoError(t, dst.db.QueryRow(
		`SELECT session_id, token_usage, agent_invoked FROM review_jobs WHERE uuid = ?`,
		uuid).Scan(&session, &tokenUsage, &invoked))

	assert.Empty(session.String, "skipped rerun clears the stale session id")
	assert.Empty(tokenUsage.String, "skipped rerun clears the stale token usage")
	assert.Equal(0, invoked, "skipped rerun clears the stale agent_invoked marker")
}

// TestReenqueueClearsSyncedAtForSameSecondRerun reproduces the priced-to-unpriced
// rerun race: an attempt synced as priced, then rerun and completed unpriced
// within the same RFC3339 second, must still be pushed so PostgreSQL drops the
// stale cost. The second-precise updated_at vs synced_at comparison compares
// equal in that window; ReenqueueJob clears synced_at so the row re-selects
// regardless of timestamp granularity.
func TestReenqueueClearsSyncedAtForSameSecondRerun(t *testing.T) {
	assert := assert.New(t)
	h := newSyncTestHelper(t)

	pending := func() []int64 {
		jobs, err := h.db.GetJobsToSync(h.machineID, 100)
		require.NoError(t, err)
		ids := make([]int64, len(jobs))
		for i, j := range jobs {
			ids[i] = j.ID
		}
		return ids
	}

	job := h.createCompletedJob("same-second-rerun-sha")
	// Attempt 1 ran an agent and recorded cost.
	seedCost(t, h.db, job.ID, `{"cost_usd":5.0,"has_cost":true}`)

	// It was synced. Pin updated_at and synced_at to the same RFC3339 second —
	// the worst case for the second-precise sync comparison.
	const sameSecond = "2024-01-01T00:00:00Z"
	_, err := h.db.Exec(
		`UPDATE review_jobs SET updated_at = ?, synced_at = ? WHERE id = ?`,
		sameSecond, sameSecond, job.ID)
	require.NoError(t, err)
	assert.NotContains(pending(), job.ID,
		"a fully-synced row (updated_at == synced_at) is not pending")

	// Rerun clears the prior attempt's cost metadata, and with it synced_at.
	require.NoError(t, h.db.ReenqueueJob(job.ID, ReenqueueOpts{}))
	assert.False(getJobAgentInvoked(t, h.db, job.ID), "rerun clears the agent-ran marker")

	// Attempt 2 completes unpriced, in the same second as the prior sync mark.
	claimed, err := h.db.ClaimJob("worker")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.NoError(t, h.db.CompleteJob(job.ID, "test", "prompt", "output"))
	_, err = h.db.Exec(`UPDATE review_jobs SET updated_at = ? WHERE id = ?`, sameSecond, job.ID)
	require.NoError(t, err)

	assert.Contains(pending(), job.ID,
		"cleared cost state must re-sync even when updated_at == prior synced_at")
}

// TestResetPathsClearSyncedAt guards the invariant for every attempt reset that
// clears cost metadata: clearing token_usage/agent_invoked must also clear
// synced_at, or a same-second rerun can leave stale spend in PostgreSQL (see
// TestReenqueueClearsSyncedAtForSameSecondRerun for the end-to-end behavior).
func TestResetPathsClearSyncedAt(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, db *DB) int64
		reset func(t *testing.T, db *DB, jobID int64)
	}{
		{
			name: "reenqueue",
			setup: func(t *testing.T, db *DB) int64 {
				_, _, job := createJobChain(t, db, "/tmp/reset-reenqueue", "sha")
				setJobStatus(t, db, job.ID, JobStatusDone)
				return job.ID
			},
			reset: func(t *testing.T, db *DB, jobID int64) {
				require.NoError(t, db.ReenqueueJob(jobID, ReenqueueOpts{}))
			},
		},
		{
			name: "retry-scoped",
			setup: func(t *testing.T, db *DB) int64 {
				_, _, job := createJobChain(t, db, "/tmp/reset-retry-scoped", "sha")
				claimJob(t, db, "worker-1")
				return job.ID
			},
			reset: func(t *testing.T, db *DB, jobID int64) {
				ok, err := db.RetryJob(jobID, "worker-1", 3, 0)
				require.NoError(t, err)
				require.True(t, ok)
			},
		},
		{
			// RetryJob has a separate unscoped SQL branch (empty workerID) that
			// must clear synced_at too.
			name: "retry-unscoped",
			setup: func(t *testing.T, db *DB) int64 {
				_, _, job := createJobChain(t, db, "/tmp/reset-retry-unscoped", "sha")
				claimJob(t, db, "worker-1")
				return job.ID
			},
			reset: func(t *testing.T, db *DB, jobID int64) {
				ok, err := db.RetryJob(jobID, "", 3, 0)
				require.NoError(t, err)
				require.True(t, ok)
			},
		},
		{
			name: "failover",
			setup: func(t *testing.T, db *DB) int64 {
				_, _, job := createJobChain(t, db, "/tmp/reset-failover", "sha")
				claimJob(t, db, "worker-1")
				return job.ID
			},
			reset: func(t *testing.T, db *DB, jobID int64) {
				ok, err := db.FailoverJob(jobID, "worker-1", "backup", "")
				require.NoError(t, err)
				require.True(t, ok)
			},
		},
		{
			name: "promote-classify",
			setup: func(t *testing.T, db *DB) int64 {
				return seedRunningClassify(t, db, "/tmp/reset-promote", "sha", "w1")
			},
			reset: func(t *testing.T, db *DB, jobID int64) {
				require.NoError(t, db.PromoteClassifyToDesignReview(jobID, "w1", "claude-code", ""))
			},
		},
		{
			name: "reset-stale",
			setup: func(t *testing.T, db *DB) int64 {
				_, _, job := createJobChain(t, db, "/tmp/reset-stale", "sha")
				claimJob(t, db, "worker-1")
				return job.ID
			},
			reset: func(t *testing.T, db *DB, jobID int64) {
				require.NoError(t, db.ResetStaleJobs())
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			defer db.Close()

			jobID := tc.setup(t, db)
			_, err := db.Exec(`UPDATE review_jobs SET synced_at = ? WHERE id = ?`,
				"2024-01-01T00:00:00Z", jobID)
			require.NoError(t, err)

			tc.reset(t, db, jobID)

			var syncedAt sql.NullString
			require.NoError(t, db.QueryRow(
				`SELECT synced_at FROM review_jobs WHERE id = ?`, jobID).Scan(&syncedAt))
			assert.False(t, syncedAt.Valid,
				"%s must clear synced_at when it clears cost metadata", tc.name)
		})
	}
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
