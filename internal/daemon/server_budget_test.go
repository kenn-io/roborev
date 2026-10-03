package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestRerunRefreshesBudgetSpend(t *testing.T) {
	setupTestEnv(t)
	tc := newWorkerTestContext(t, 1)
	cfg, now := configureUnpricedBudgetRouting(t, tc)
	cfg.Budget.ReserveFloorCents = 20
	server := NewServer(tc.DB, cfg, "")
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	// Keep cached spend fresh so expiry cannot hide a missed invalidation.
	server.workerPool.budgetRouter.now = func() time.Time { return now }
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: tc.Repo.ID, GitRef: testutil.GetHeadSHA(t, tc.TmpDir), Agent: "codex",
	})
	require.NoError(t, err)
	_, err = tc.DB.Exec(`UPDATE review_jobs SET status='done',started_at=?,finished_at=?,
		agent_invoked=1,token_usage='{"has_cost":true,"cost_usd":1}' WHERE id=?`,
		now.Format(time.RFC3339), now.Format(time.RFC3339), job.ID)
	require.NoError(t, err)
	before, err := server.workerPool.budgetRouter.ResolveAgent("codex", nil, cfg, config.ReviewTypeDefault)
	require.NoError(t, err)
	require.Equal(t, "gemini", before.Name())

	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost,
		"/api/job/rerun", RerunJobRequest{JobID: job.ID}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	spend, err := tc.DB.GetBudgetSpend(now)
	require.NoError(t, err)
	require.Zero(t, spend.TotalUSD)
	rerun, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, rerun)
	server.workerPool.processJob(testWorkerID, rerun)
	done := tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	assert.Equal(t, "codex", done.Agent)
}
