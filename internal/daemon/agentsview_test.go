package daemon

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/tokens"
)

func agentsViewFixture(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "fixture-token")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/api/v1/sessions/raw-id/usage":
			w.WriteHeader(404)
		case "/api/v1/session-ids/resolve":
			assert.Equal(t, "raw-id", r.URL.Query().Get("partial"))
			fmt.Fprint(w, `{"raw_suffix":true,"ids":["codex:raw-id"]}`)
		case "/api/v1/sessions/codex:raw-id/usage":
			assert.Equal(t, "true", r.URL.Query().Get("subagents"))
			fmt.Fprint(w, `{"has_token_data":false,"has_cost":true,"cost_usd":0.75}`)
		default:
			assert.Contains(t, []string{"/api/v1/sessions/raw-id/usage", "/api/v1/session-ids/resolve", "/api/v1/sessions/codex:raw-id/usage"}, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := config.DefaultConfig()
	cfg.AgentsView.URL = srv.URL
	return cfg
}

func TestAgentsViewWorkerAndReconciliation(t *testing.T) {
	cfg := agentsViewFixture(t)
	tc := newWorkerTestContext(t, 1)
	tc.reconfigurePool(cfg)
	agent.RegisterForTest(t, &sessionStreamingTestAgent{name: "agentsview-session", streamLine: `{"type":"thread.started","thread_id":"raw-id"}`})
	job := tc.createAndClaimJobWithAgent(t, testutil.GetHeadSHA(t, tc.TmpDir), testWorkerID, "agentsview-session")
	tc.Pool.processJob(testWorkerID, job)
	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.InDelta(t, 0.75, usage.CostUSD, 1e-9)

	tc = newWorkerTestContext(t, 1)
	tc.reconfigurePool(cfg)
	candidateJob := seedTokenCostCandidate(t, tc, "raw-id", `{"total_output_tokens":12}`)
	candidate, err := tc.DB.GetTokenCostCandidate(candidateJob.ID)
	require.NoError(t, err)
	require.NotNil(t, candidate)
	resolved, err := tc.Pool.reconcileTokenCostCandidate(t.Context(), *candidate)
	require.NoError(t, err)
	assert.True(t, resolved)
	updated, err = tc.DB.GetJobByID(candidateJob.ID)
	require.NoError(t, err)
	usage = tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.InDelta(t, 0.75, usage.CostUSD, 1e-9)
	assert.Equal(t, int64(12), usage.OutputTokens)
}

func TestAgentsViewMaintenance(t *testing.T) {
	cfg := agentsViewFixture(t)
	server, db, _ := newTestServer(t)
	server.configWatcher.cfgMu.Lock()
	server.configWatcher.cfg = cfg
	server.configWatcher.cfgMu.Unlock()
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "abc123", "Author", "Subject", time.Now())
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123", Agent: "test"})
	require.NoError(t, err)
	claimed, err := db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.MarkJobAgentInvoked(job.ID, testWorkerID, "test review"))
	require.NoError(t, db.SaveJobSessionID(job.ID, testWorkerID, "raw-id"))
	require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "test", "prompt", "No issues found."))
	response := maintenanceRequest(t, server, "/api/maintenance/tokens/backfill", map[string]any{"dry_run": false})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	updated, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.InDelta(t, 0.75, usage.CostUSD, 1e-9)
}
