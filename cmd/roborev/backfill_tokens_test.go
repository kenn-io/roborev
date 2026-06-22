package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/backfill"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/tokens"
)

func TestBackfillCandidates(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name    string
		jobs    []storage.ReviewJob
		wantIDs []int64
	}{
		{
			name:    "empty input",
			jobs:    nil,
			wantIDs: nil,
		},
		{
			name: "single completed job with session",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: new(now),
				},
			},
			wantIDs: []int64{1},
		},
		{
			name: "skip job that already has token data and cost",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: new(now),
					TokenUsage: `{"peak_context_tokens":100,"cost_usd":0.12,"has_cost":true}`,
				},
			},
			wantIDs: nil,
		},
		{
			name: "include job with token data but no cost",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: new(now),
					TokenUsage: `{"peak_context_tokens":100}`,
				},
			},
			wantIDs: []int64{1},
		},
		{
			name: "skip job that already has cost",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: new(now),
					TokenUsage: `{"peak_context_tokens":100,"cost_usd":0,"has_cost":true}`,
				},
			},
			wantIDs: nil,
		},
		{
			name: "skip job with no session ID",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					StartedAt: new(now),
				},
			},
			wantIDs: nil,
		},
		{
			name: "skip queued job",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusQueued,
					SessionID: "s1",
				},
			},
			wantIDs: nil,
		},
		{
			name: "resumed session: two started jobs share session",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: new(now),
				},
				{
					ID: 2, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: new(now),
				},
			},
			wantIDs: nil,
		},
		{
			name: "canceled-before-start sibling does not block backfill",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: new(now),
				},
				{
					ID: 2, Status: storage.JobStatusCanceled,
					SessionID: "s1", StartedAt: nil,
				},
			},
			wantIDs: []int64{1},
		},
		{
			name: "canceled-after-start sibling blocks backfill",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: new(now),
				},
				{
					ID: 2, Status: storage.JobStatusCanceled,
					SessionID: "s1", StartedAt: new(now),
				},
			},
			wantIDs: nil,
		},
		{
			name: "failed-after-start sibling blocks backfill",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: new(now),
				},
				{
					ID: 2, Status: storage.JobStatusFailed,
					SessionID: "s1", StartedAt: new(now),
				},
			},
			wantIDs: nil,
		},
		{
			name: "independent sessions are both eligible",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: new(now),
				},
				{
					ID: 2, Status: storage.JobStatusDone,
					SessionID: "s2", StartedAt: new(now),
				},
			},
			wantIDs: []int64{1, 2},
		},
		{
			name: "applied/rebased jobs are eligible",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusApplied,
					SessionID: "s1", StartedAt: new(now),
				},
				{
					ID: 2, Status: storage.JobStatusRebased,
					SessionID: "s2", StartedAt: new(now),
				},
			},
			wantIDs: []int64{1, 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := backfill.TokenCandidates(tt.jobs)
			var gotIDs []int64
			for _, j := range got {
				gotIDs = append(gotIDs, j.ID)
			}
			assert.Equal(t, tt.wantIDs, gotIDs)
		})
	}
}

func TestBackfillCostFetchConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Cost.Endpoint = "https://usage.example.test/api/v1/sessions/{session_id}/usage"
	cfg.Cost.Timeout = "250ms"

	got := backfillCostFetchConfig(cfg)

	assert.Equal(t, "https://usage.example.test/api/v1/sessions/{session_id}/usage", got.Endpoint)
	assert.Equal(t, 250*time.Millisecond, got.Timeout)
	assert.True(t, got.RequireCLI)
}

func TestMergeBackfillTokenUsagePreservesExistingCountsForCostOnlyFetch(t *testing.T) {
	existing := `{"total_output_tokens":28800,"peak_context_tokens":118000}`
	fetched := &tokens.Usage{CostUSD: 0.42, HasCost: true}

	got := backfill.MergeTokenUsage(existing, fetched)

	assert.Equal(t, int64(28800), got.OutputTokens)
	assert.Equal(t, int64(118000), got.PeakContextTokens)
	assert.True(t, got.HasCost)
	assert.InDelta(t, 0.42, got.CostUSD, 1e-9)
}

func TestMergeBackfillTokenUsagePreservesCodexInputBucketsForCostOnlyFetch(t *testing.T) {
	existing := `{"input_tokens":79150,"cached_input_tokens":2560,` +
		`"total_output_tokens":3389,"usage_source":"job_log_turn_completed",` +
		`"thread_id":"thread-123","event_offset":91}`
	fetched := &tokens.Usage{CostUSD: 0.42, HasCost: true}

	got := backfill.MergeTokenUsage(existing, fetched)

	assert.Equal(t, int64(79150), got.InputTokens)
	assert.Equal(t, int64(2560), got.CachedInputTokens)
	assert.Equal(t, int64(3389), got.OutputTokens)
	assert.Equal(t, "job_log_turn_completed", got.UsageSource)
	assert.Equal(t, "thread-123", got.ThreadID)
	assert.Equal(t, int64(91), got.EventOffset)
	assert.True(t, got.HasCost)
	assert.InDelta(t, 0.42, got.CostUSD, 1e-9)
}

func TestBackfillTokensUsesCodexJobLogWhenAgentsviewMissing(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	t.Setenv("PATH", t.TempDir())

	db, err := storage.Open(storage.DefaultDBPath())
	require.NoError(t, err)
	defer db.Close()

	repo, err := db.GetOrCreateRepo(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "abc123", "Author", "Subject", time.Now())
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:    repo.ID,
		CommitID:  commit.ID,
		GitRef:    "abc123",
		Agent:     "codex",
		SessionID: "thread-123",
	})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.CompleteJob(job.ID, "codex", "prompt", "No issues found."))

	logPath := daemon.JobLogPath(job.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o700))
	require.NoError(t, os.WriteFile(logPath, []byte(
		`{"type":"thread.started","thread_id":"thread-123"}`+"\n"+
			`{"type":"turn.completed","usage":{"input_tokens":79150,`+
			`"cached_input_tokens":2560,"output_tokens":3389}}`+"\n",
	), 0o600))

	cmd := backfillTokensCmd()
	cmd.SetArgs(nil)
	require.NoError(t, cmd.Execute())

	updated, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.Equal(t, int64(79150), usage.InputTokens)
	assert.Equal(t, int64(2560), usage.CachedInputTokens)
	assert.Equal(t, int64(3389), usage.OutputTokens)
	assert.Equal(t, "job_log_turn_completed", usage.UsageSource)
	assert.Equal(t, "thread-123", usage.ThreadID)
}

func TestBackfillTokensUsesCodexJobLogsWithoutAgentsviewEligibleSession(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	t.Setenv("PATH", t.TempDir())

	db, err := storage.Open(storage.DefaultDBPath())
	require.NoError(t, err)
	defer db.Close()

	repo, err := db.GetOrCreateRepo(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "abc123", "Author", "Subject", time.Now())
	require.NoError(t, err)

	missingSession := enqueueCompleteJob(t, db, repo.ID, commit.ID, "")
	sharedSessionA := enqueueCompleteJob(t, db, repo.ID, commit.ID, "shared-session")
	sharedSessionB := enqueueCompleteJob(t, db, repo.ID, commit.ID, "shared-session")

	writeCodexUsageLog(t, missingSession.ID, "missing-session-thread", 1000, 100, 200)
	writeCodexUsageLog(t, sharedSessionA.ID, "shared-session", 2000, 200, 300)
	writeCodexUsageLog(t, sharedSessionB.ID, "shared-session", 3000, 300, 400)

	cmd := backfillTokensCmd()
	cmd.SetArgs(nil)
	require.NoError(t, cmd.Execute())

	assertJobUsage := func(jobID int64, threadID string, input, cached, output int64) {
		updated, err := db.GetJobByID(jobID)
		require.NoError(t, err)
		usage := tokens.ParseJSON(updated.TokenUsage)
		require.NotNil(t, usage)
		assert.Equal(t, input, usage.InputTokens)
		assert.Equal(t, cached, usage.CachedInputTokens)
		assert.Equal(t, output, usage.OutputTokens)
		assert.Equal(t, threadID, usage.ThreadID)
		assert.Equal(t, "job_log_turn_completed", usage.UsageSource)
	}
	assertJobUsage(missingSession.ID, "missing-session-thread", 1000, 100, 200)
	assertJobUsage(sharedSessionA.ID, "shared-session", 2000, 200, 300)
	assertJobUsage(sharedSessionB.ID, "shared-session", 3000, 300, 400)

	updatedMissingSession, err := db.GetJobByID(missingSession.ID)
	require.NoError(t, err)
	assert.Equal(t, "missing-session-thread", updatedMissingSession.SessionID)
}

func enqueueCompleteJob(
	t *testing.T, db *storage.DB, repoID, commitID int64, sessionID string,
) *storage.ReviewJob {
	t.Helper()
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:    repoID,
		CommitID:  commitID,
		GitRef:    "abc123",
		Agent:     "codex",
		SessionID: sessionID,
	})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.CompleteJob(job.ID, "codex", "prompt", "No issues found."))
	return job
}

func writeCodexUsageLog(
	t *testing.T, jobID int64, threadID string, input, cached, output int64,
) {
	t.Helper()
	logPath := daemon.JobLogPath(jobID)
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o700))
	require.NoError(t, os.WriteFile(logPath, []byte(
		`{"type":"thread.started","thread_id":"`+threadID+`"}`+"\n"+
			`{"type":"turn.completed","usage":{"input_tokens":`+
			fmt.Sprintf("%d", input)+`,"cached_input_tokens":`+
			fmt.Sprintf("%d", cached)+`,"output_tokens":`+
			fmt.Sprintf("%d", output)+`}}`+"\n",
	), 0o600))
}
