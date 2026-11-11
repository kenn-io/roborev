package daemon

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/tokens"
)

// writeClaudeUsageLog writes a claude-code job log holding one finished CLI
// run per session. Every run reports 100 input, 10 output, 1000 cache-read
// and 500 cache-creation tokens at $0.25, on a result event whose top-level
// usage object undercounts the way the CLI's does.
func writeClaudeUsageLog(t *testing.T, job *storage.ReviewJob, sessionIDs ...string) {
	t.Helper()
	var lines []string
	for _, sessionID := range sessionIDs {
		lines = append(lines,
			fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q}`, sessionID),
			fmt.Sprintf(`{"type":"result","session_id":%q,"total_cost_usd":0.25,`+
				`"usage":{"input_tokens":1,"output_tokens":1},`+
				`"modelUsage":{"model-a":{"inputTokens":100,"outputTokens":10,`+
				`"cacheReadInputTokens":1000,"cacheCreationInputTokens":500,"costUSD":0.25}}}`, sessionID),
		)
	}
	logPath := JobLogPath(job.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o700))
	require.NoError(t, os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	// Filesystem write timestamps can lag the nanosecond-precision job start.
	require.NotNil(t, job.StartedAt)
	logTime := job.StartedAt.Add(time.Second).Truncate(time.Second)
	require.NoError(t, os.Chtimes(logPath, logTime, logTime))
}

func storedUsage(t *testing.T, db *storage.DB, jobID int64) (*storage.ReviewJob, *tokens.Usage) {
	t.Helper()
	job, err := db.GetJobByID(jobID)
	require.NoError(t, err)
	usage := tokens.ParseJSON(job.TokenUsage)
	require.NotNil(t, usage, "job %d has no token usage", jobID)
	return job, usage
}

func TestCaptureTokenUsageForSessionUsesClaudeJobLog(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	newJob := func(t *testing.T, agentName string) *storage.ReviewJob {
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agentName)
		require.NoError(t, testutil.CompleteReviewFixture(tc.DB, job.ID, agentName, "prompt", "No issues found."))
		return job
	}

	t.Run("no usage provider", func(t *testing.T) {
		assert := assert.New(t)
		job := newJob(t, "claude-code")
		writeClaudeUsageLog(t, job, "session-a")
		tc.Pool.tokenUsageFetcher = func(context.Context, string) (*tokens.Usage, error) {
			return nil, tokens.ErrUsageProviderUnavailable
		}

		tc.Pool.captureTokenUsageForSession(t.Context(), testWorkerID, job, "session-a")

		updated, usage := storedUsage(t, tc.DB, job.ID)
		assert.Equal("session-a", updated.SessionID)
		assert.Equal(int64(100), usage.InputTokens)
		assert.Equal(int64(10), usage.OutputTokens)
		assert.Equal(int64(1000), usage.CachedInputTokens)
		assert.Equal(int64(500), usage.CacheCreationTokens)
		assert.True(usage.HasCost)
		assert.InDelta(0.25, usage.CostUSD, 1e-9)
	})

	t.Run("provider adds what the log lacks", func(t *testing.T) {
		assert := assert.New(t)
		job := newJob(t, "claude-code")
		writeClaudeUsageLog(t, job, "session-b")
		tc.Pool.tokenUsageFetcher = func(context.Context, string) (*tokens.Usage, error) {
			return &tokens.Usage{OutputTokens: 12, PeakContextTokens: 9000, CostUSD: 0.3, HasCost: true}, nil
		}

		tc.Pool.captureTokenUsageForSession(t.Context(), testWorkerID, job, "session-b")

		_, usage := storedUsage(t, tc.DB, job.ID)
		assert.Equal(int64(100), usage.InputTokens)
		assert.Equal(int64(1000), usage.CachedInputTokens)
		assert.Equal(int64(500), usage.CacheCreationTokens)
		assert.Equal(int64(9000), usage.PeakContextTokens)
		assert.InDelta(0.3, usage.CostUSD, 1e-9)
	})

	t.Run("wrapper that ran the CLI twice", func(t *testing.T) {
		assert := assert.New(t)
		job := newJob(t, "claude-code")
		writeClaudeUsageLog(t, job, "session-c", "session-d")
		lookups := 0
		tc.Pool.tokenUsageFetcher = func(context.Context, string) (*tokens.Usage, error) {
			lookups++
			return &tokens.Usage{OutputTokens: 10, CostUSD: 0.25, HasCost: true}, nil
		}

		tc.Pool.captureTokenUsageForSession(t.Context(), testWorkerID, job, "session-d")

		_, usage := storedUsage(t, tc.DB, job.ID)
		assert.Equal(int64(200), usage.InputTokens)
		assert.Equal(int64(20), usage.OutputTokens)
		assert.Equal(int64(2000), usage.CachedInputTokens)
		assert.Equal(int64(1000), usage.CacheCreationTokens)
		assert.InDelta(0.5, usage.CostUSD, 1e-9)
		assert.True(usage.CostComplete())
		// One session's provider total must not replace the sum of both runs.
		assert.Zero(lookups)
	})

	t.Run("unpriced wrapper runs wait for every session's price", func(t *testing.T) {
		assert := assert.New(t)
		job := newJob(t, "claude-code")
		logPath := JobLogPath(job.ID)
		require.NoError(t, os.WriteFile(logPath, []byte(
			`{"type":"result","session_id":"session-f","modelUsage":{"model-a":{"inputTokens":100,"outputTokens":10}}}`+"\n"+
				`{"type":"result","session_id":"session-g","modelUsage":{"model-a":{"inputTokens":100,"outputTokens":10}}}`+"\n",
		), 0o600))
		lookups := 0
		tc.Pool.tokenUsageFetcher = func(context.Context, string) (*tokens.Usage, error) {
			lookups++
			return &tokens.Usage{OutputTokens: 10, CostUSD: 0.25, HasCost: true}, nil
		}

		tc.Pool.captureTokenUsageForSession(t.Context(), testWorkerID, job, "session-g")

		_, usage := storedUsage(t, tc.DB, job.ID)
		assert.Equal(int64(200), usage.InputTokens)
		assert.Equal(int64(20), usage.OutputTokens)
		assert.Equal([]string{"session-f", "session-g"}, usage.ProviderSessionIDs)
		// One session's price must not stand in for both runs.
		assert.False(usage.HasCost)
		assert.Zero(lookups)
	})

	t.Run("log written by a failover agent", func(t *testing.T) {
		job := newJob(t, "codex")
		writeClaudeUsageLog(t, job, "session-e")
		require.NoError(t, RecordJobLogAgent(job.ID, "claude-code"))
		tc.Pool.tokenUsageFetcher = func(context.Context, string) (*tokens.Usage, error) {
			return nil, tokens.ErrUsageProviderUnavailable
		}

		tc.Pool.captureTokenUsageForSession(t.Context(), testWorkerID, job, "session-e")

		_, usage := storedUsage(t, tc.DB, job.ID)
		assert.Equal(t, int64(100), usage.InputTokens)
	})
}

func TestTokenCostReconcilerRecoversClaudeJobLog(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "claude-code")
	require.NoError(t, testutil.CompleteReviewFixture(tc.DB, job.ID, "claude-code", "prompt", "No issues found."))
	writeClaudeUsageLog(t, job, "recovered-session")

	tc.Pool.recoverTokenUsageLogs(t.Context())

	assert := assert.New(t)
	recovered, usage := storedUsage(t, tc.DB, job.ID)
	assert.Equal("recovered-session", recovered.SessionID)
	assert.Equal(int64(100), usage.InputTokens)
	assert.Equal(int64(1000), usage.CachedInputTokens)
	assert.Equal(int64(500), usage.CacheCreationTokens)
	assert.Equal(int64(10), usage.OutputTokens)
	assert.True(usage.HasCost)
	assert.InDelta(0.25, usage.CostUSD, 1e-9)
}

func TestBackfillTokensUsesClaudeJobLog(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	// No agentsview on PATH: the log is the only source.
	t.Setenv("PATH", t.TempDir())
	server, db, _ := newTestServer(t)
	repo, err := db.GetOrCreateRepo(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "abc123", "Author", "Subject", time.Now())
	require.NoError(t, err)
	_, err = db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123", Agent: "claude-code",
	})
	require.NoError(t, err)
	job, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.NotNil(t, job)
	require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "claude-code", "prompt", "No issues found."))
	writeClaudeUsageLog(t, job, "session-a", "session-b")

	response := maintenanceRequest(t, server, "/api/maintenance/tokens/backfill", map[string]any{"dry_run": false})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var report ScanTokenUsageReport
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &report))

	assert := assert.New(t)
	assert.Equal(1, report.Updated)
	_, usage := storedUsage(t, db, job.ID)
	assert.Equal(int64(200), usage.InputTokens)
	assert.Equal(int64(2000), usage.CachedInputTokens)
	assert.Equal(int64(1000), usage.CacheCreationTokens)
	assert.Equal(int64(20), usage.OutputTokens)
	assert.True(usage.HasCost)
	assert.InDelta(0.5, usage.CostUSD, 1e-9)
}
