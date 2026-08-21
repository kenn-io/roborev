package backfill

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/tokens"
)

// driftedUsage is the exact token_usage shape agentsview v0.39.0 produced once
// it moved cost into a microdollar envelope roborev could not read: the priced
// flag survived, the dollar figure did not.
const driftedUsage = `{"input_tokens":275361,"cached_input_tokens":204672,` +
	`"total_output_tokens":2257,"peak_context_tokens":64393,"has_cost":true,` +
	`"usage_source":"job_log_turn_completed","thread_id":"t-1","event_offset":1}`

func TestNeedsTokenCostBackfill(t *testing.T) {
	assert := assert.New(t)

	assert.True(NeedsTokenCostBackfill(""), "no usage at all needs backfill")
	assert.True(
		NeedsTokenCostBackfill(`{"total_output_tokens":300}`),
		"usage without a cost flag needs backfill",
	)
	assert.True(
		NeedsTokenCostBackfill(driftedUsage),
		"priced flag with no dollar amount needs backfill",
	)
	assert.False(
		NeedsTokenCostBackfill(`{"has_cost":true,"cost_usd":0.42}`),
		"a real recorded cost is already backfilled",
	)
	assert.False(
		NeedsTokenCostBackfill(`{"has_cost":true,"cost_usd":0}`),
		"an explicit $0 is a real free run, not a lost amount",
	)
	assert.True(
		NeedsTokenCostBackfill(`{"has_cost":true,"cost_usd":null}`),
		"a null amount is absent, not $0",
	)
}

func TestStoreCapturedTokenUsageRejectsReenqueuedJob(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "reviews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo, err := db.GetOrCreateRepo(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(
		repo.ID, "capture-race", "test", "capture race", time.Now(),
	)
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID, GitRef: commit.SHA, Agent: "codex",
	})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("capture-worker")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.CompleteJob(
		job.ID, "codex", "prompt", "No issues found.",
	))
	selected, err := db.GetJobByID(job.ID)
	require.NoError(t, err)

	_, err = db.Exec(`
		UPDATE review_jobs
		SET started_at = '2026-08-20T16:00:00.987654321Z',
		    finished_at = '2026-08-20T16:00:02Z'
		WHERE id = ?`, job.ID)
	require.NoError(t, err)
	_, saved, err := StoreCapturedTokenUsage(
		db,
		CapturedUsage{
			JobID:             job.ID,
			SessionID:         "prior-session",
			ExistingJSON:      selected.TokenUsage,
			ExpectedStartedAt: selected.StartedAtRaw,
		},
		&tokens.Usage{OutputTokens: 32, ThreadID: "prior-session"},
		nil,
	)
	require.NoError(t, err)
	assert.False(t, saved)

	current, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Empty(t, current.SessionID)
	assert.Empty(t, current.TokenUsage)
}

func TestStoreMergedTokenUsagePreservesNewerUsageAfterConflict(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "reviews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo, err := db.GetOrCreateRepo(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(
		repo.ID, "usage-conflict", "test", "usage conflict", time.Now(),
	)
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID, GitRef: commit.SHA, Agent: "codex",
	})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("usage-worker")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.MarkJobAgentInvoked(
		job.ID, "usage-worker", "codex review",
	))
	require.NoError(t, db.SaveJobSessionID(
		job.ID, "usage-worker", "usage-session",
	))
	require.NoError(t, db.CompleteJob(
		job.ID, "codex", "prompt", "No issues found.",
	))

	oldUsage := `{"total_output_tokens":10,"peak_context_tokens":100}`
	updated, err := db.BackfillJobTokenUsageIfCurrent(storage.TokenUsageWrite{
		JobID:                job.ID,
		SessionID:            "usage-session",
		TokenUsageJSON:       oldUsage,
		ExpectedStartedAt:    claimed.StartedAtRaw,
		RequireUniqueSession: true,
	})
	require.NoError(t, err)
	require.True(t, updated)
	newerUsage := `{"total_output_tokens":20,"peak_context_tokens":200,"has_cost":true,"cost_usd":0.66}`
	require.NoError(t, db.SaveJobTokenUsage(
		job.ID, "usage-session", newerUsage,
	))

	_, saved, err := StoreMergedTokenUsage(
		db,
		CapturedUsage{
			JobID:             job.ID,
			SessionID:         "usage-session",
			ExistingJSON:      oldUsage,
			ExpectedStartedAt: claimed.StartedAtRaw,
		},
		&tokens.Usage{
			OutputTokens:      10,
			PeakContextTokens: 100,
			HasCost:           true,
			CostUSD:           0.33,
		},
		true,
	)
	require.NoError(t, err)
	assert.False(t, saved)

	current, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	usage := tokens.ParseJSON(current.TokenUsage)
	require.NotNil(t, usage)
	assert.Equal(t, int64(20), usage.OutputTokens)
	assert.Equal(t, int64(200), usage.PeakContextTokens)
	assert.InDelta(t, 0.66, usage.CostUSD, 1e-9)
}

// A genuine free run must survive a merge intact rather than being re-flagged
// as unpriced, which would drop it back out of the priced numerator.
func TestMergeTokenUsageKeepsExplicitZeroCost(t *testing.T) {
	existing := `{"total_output_tokens":300,"has_cost":true,"cost_usd":0}`
	fetched := &tokens.Usage{OutputTokens: 300}

	merged := MergeTokenUsage(existing, fetched)
	require.NotNil(t, merged)
	assert.True(t, merged.HasCost, "an explicit $0 stays priced")
	assert.Zero(t, merged.CostUSD)
}

func TestNeedsTokenUsageBackfill(t *testing.T) {
	assert := assert.New(t)

	assert.True(
		NeedsTokenUsageBackfill(driftedUsage),
		"complete token counts do not excuse a missing dollar amount",
	)
	assert.True(
		NeedsTokenUsageBackfill(`{"has_cost":true,"cost_usd":0.42}`),
		"cost without token counts still needs backfill",
	)
	assert.False(
		NeedsTokenUsageBackfill(
			`{"total_output_tokens":300,"has_cost":true,"cost_usd":0.42}`,
		),
		"counts plus a real cost is complete",
	)
	assert.False(
		NeedsTokenUsageBackfill(
			`{"cache_creation_tokens":512,"has_cost":true,"cost_usd":0.42}`,
		),
		"cache-creation tokens count as token data",
	)
}

// The drifted rows are the whole point of the backfill, so they must survive
// the log-candidate filter used by `roborev backfill-tokens`.
func TestLogTokenCandidatesIncludesDriftedCostRow(t *testing.T) {
	started := time.Now()
	job := storage.ReviewJob{
		ID:         5297,
		Status:     storage.JobStatusDone,
		SessionID:  "019fb0c3-3c66-7e52-aa96-2935983320f3",
		StartedAt:  &started,
		TokenUsage: driftedUsage,
	}

	require.Len(t, LogTokenCandidates([]storage.ReviewJob{job}), 1)
}

func TestLogTokenCandidatesIncludeEveryTerminalStatus(t *testing.T) {
	started := time.Now()
	statuses := []storage.JobStatus{
		storage.JobStatusDone,
		storage.JobStatusApplied,
		storage.JobStatusRebased,
		storage.JobStatusFailed,
		storage.JobStatusCanceled,
		storage.JobStatusSkipped,
	}
	jobs := make([]storage.ReviewJob, 0, len(statuses)+2)
	for i, status := range statuses {
		jobs = append(jobs, storage.ReviewJob{
			ID:         int64(i + 1),
			Status:     status,
			SessionID:  fmt.Sprintf("session-%d", i+1),
			StartedAt:  &started,
			TokenUsage: `{"total_output_tokens":42}`,
		})
	}
	jobs = append(jobs,
		storage.ReviewJob{
			ID: 100, Status: storage.JobStatusQueued,
			SessionID: "queued-session", TokenUsage: `{"total_output_tokens":42}`,
		},
		storage.ReviewJob{
			ID: 101, Status: storage.JobStatusRunning,
			SessionID: "running-session", StartedAt: &started,
			TokenUsage: `{"total_output_tokens":42}`,
		},
	)

	assert.Len(t, LogTokenCandidates(jobs), len(statuses))
}

func TestLogTokenCandidatesRequiresStartedAttempt(t *testing.T) {
	job := storage.ReviewJob{
		ID:     1,
		Status: storage.JobStatusCanceled,
	}

	assert.Empty(t, LogTokenCandidates([]storage.ReviewJob{job}))
}

func TestMergeTokenUsageKeepsRecordedCostOverUnpricedFetch(t *testing.T) {
	existing := `{"total_output_tokens":300,"has_cost":true,"cost_usd":1.23}`
	fetched := &tokens.Usage{OutputTokens: 300, HasCost: true, CostUSD: 0}

	merged := MergeTokenUsage(existing, fetched)
	require.NotNil(t, merged)
	assert.True(t, merged.HasCost)
	assert.InDelta(t, 1.23, merged.CostUSD, 1e-9,
		"a fresh $0 must not overwrite real recorded spend")
}

func TestMergeTokenUsageTakesFetchedCostOverDriftedZero(t *testing.T) {
	fetched := &tokens.Usage{HasCost: true, CostUSD: 0.598641}

	merged := MergeTokenUsage(driftedUsage, fetched)
	require.NotNil(t, merged)
	assert.True(t, merged.HasCost)
	assert.InDelta(t, 0.598641, merged.CostUSD, 1e-9)
	assert.Equal(t, int64(2257), merged.OutputTokens,
		"token counts carry over from the existing row")
	assert.Equal(t, int64(64393), merged.PeakContextTokens)
}

// A drifted row must not have its bare has_cost flag carried forward when no
// side has dollars: that is the $0-priced state the repair exists to remove, and
// resurrecting it keeps the row in the priced numerator at $0.
func TestMergeTokenUsageDropsUnpricedCostFlag(t *testing.T) {
	fetched := &tokens.Usage{OutputTokens: 2257, PeakContextTokens: 64393}

	merged := MergeTokenUsage(driftedUsage, fetched)
	require.NotNil(t, merged)
	assert.False(t, merged.HasCost,
		"a cost flag with no dollars anywhere must not survive the merge")
	assert.Zero(t, merged.CostUSD)
}

func TestMergeTokenUsageCarriesCacheCreationTokens(t *testing.T) {
	existing := `{"cache_creation_tokens":8192,"total_output_tokens":300}`
	fetched := &tokens.Usage{HasCost: true, CostUSD: 0.42}

	merged := MergeTokenUsage(existing, fetched)
	require.NotNil(t, merged)
	assert.Equal(t, int64(8192), merged.CacheCreationTokens)
}
