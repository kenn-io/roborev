package daemon

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	gitpkg "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
	roborevclient "go.kenn.io/roborev/pkg/client"
	"go.kenn.io/roborev/pkg/client/generated"
)

func enqueueHeldPanel(t *testing.T, server *Server) PanelEnqueueResponse {
	t.Helper()
	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", panelTOML)
	repo.CommitFile("a.txt", "a", "add a")
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", map[string]any{
		"repo_path": repo.Path(), "git_ref": "HEAD", "panel": "trio", "hold_synthesis": true,
	}))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var panel PanelEnqueueResponse
	testutil.DecodeJSON(t, w, &panel)
	return panel
}

func requestSynthesis(t *testing.T, server *Server, run uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/panel/synthesize", map[string]any{
		"panel_run_uuid": run,
	}))
	return w
}

func finishHeldMembers(t *testing.T, server *Server, panel PanelEnqueueResponse) {
	t.Helper()
	for range panel.MemberJobIDs {
		job, err := server.db.ClaimJob(testWorkerID)
		require.NoError(t, err)
		require.NotNil(t, job)
		require.Equal(t, storage.PanelRoleMember, job.PanelRole)
		server.workerPool.processJob(testWorkerID, job)
		finished, err := server.db.GetJobByID(job.ID)
		require.NoError(t, err)
		require.Equal(t, storage.JobStatusDone, finished.Status, finished.Error)
	}
	job, err := server.db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	assert.Nil(t, job, "member completion must not release held synthesis")
}

func TestManualSynthesisSurvivesRestartAndStartsOnce(t *testing.T) {
	testenv.SetDataDir(t)
	server, db, dir := newTestServer(t)
	panel := enqueueHeldPanel(t, server)
	early := requestSynthesis(t, server, panel.PanelRunUUID)
	require.Equal(t, http.StatusConflict, early.Code, early.Body.String())
	finishHeldMembers(t, server, panel)

	require.NoError(t, server.Close())
	require.NoError(t, db.Close())
	reopened, err := storage.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	restarted := newServerWithLogs(reopened, config.DefaultConfig(), "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { require.NoError(t, restarted.Close()) })
	restarted.sweepStuckPanels(t.Context())
	job, err := reopened.ClaimJob(testWorkerID)
	require.NoError(t, err)
	assert.Nil(t, job, "the recovery sweep must preserve the hold")

	var wg sync.WaitGroup
	httpServer := httptest.NewServer(restarted.httpServer.Handler)
	t.Cleanup(httpServer.Close)
	client, err := roborevclient.New(httpServer.URL)
	require.NoError(t, err)
	for range 4 {
		wg.Go(func() {
			job, err := client.StartPanelSynthesis(t.Context(), &generated.StartPanelSynthesisRequestOptions{
				Body: &generated.StartPanelSynthesisBody{PanelRunUUID: panel.PanelRunUUID},
			})
			if assert.NoError(t, err) && assert.NotNil(t, job) {
				assert.Equal(t, panel.ID, job.ID)
			}
		})
	}
	wg.Wait()
	job, err = reopened.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, panel.ID, job.ID)
	none, err := reopened.ClaimJob("another-worker")
	require.NoError(t, err)
	assert.Nil(t, none)
	restarted.workerPool.processJob(testWorkerID, job)
	finished, err := reopened.GetJobByID(panel.ID)
	require.NoError(t, err)
	require.Equal(t, storage.JobStatusDone, finished.Status, finished.Error)
	w := requestSynthesis(t, restarted, panel.PanelRunUUID)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	none, err = reopened.ClaimJob(testWorkerID)
	require.NoError(t, err)
	assert.Nil(t, none, "replaying the request must not rerun synthesis")

	rerun, err := restarted.humaRerunJob(t.Context(), &RerunJobInput{Body: RerunJobRequest{JobID: panel.ID}})
	require.NoError(t, err)
	members, err := reopened.GetPanelMembers(*rerun.Body.RunUUID)
	require.NoError(t, err)
	next := PanelEnqueueResponse{PanelRunUUID: *rerun.Body.RunUUID}
	for _, member := range members {
		next.MemberJobIDs = append(next.MemberJobIDs, member.ID)
	}
	finishHeldMembers(t, restarted, next)
	w = requestSynthesis(t, restarted, next.PanelRunUUID)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	job, err = reopened.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.Equal(t, rerun.Body.JobID, job.ID)
}

func TestManualSynthesisDoesNotReviveCanceledPanel(t *testing.T) {
	server, db, _ := newTestServer(t)
	panel := enqueueHeldPanel(t, server)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/cancel", map[string]any{"job_id": panel.ID}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = requestSynthesis(t, server, panel.PanelRunUUID)
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	server.sweepStuckPanels(t.Context())
	job, err := db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	assert.Nil(t, job)
	w = requestSynthesis(t, server, uuid.New())
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestHoldSynthesisRequiresExplicitPanel(t *testing.T) {
	server, _, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", panelTOML)
	repo.CommitFile("a.txt", "a", "add a")
	for _, panel := range []string{"", "none"} {
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", map[string]any{
			"repo_path": repo.Path(), "git_ref": "HEAD", "panel": panel, "hold_synthesis": true,
		}))
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	}
}

func TestHoldSynthesisRejectsInsightsWithoutCreatingJob(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	repoDir := filepath.Join(tmpDir, "repo")
	testutil.InitTestGitRepo(t, repoDir)
	mainRoot, err := gitpkg.GetMainRepoRoot(repoDir)
	require.NoError(t, err)
	repo, err := db.GetOrCreateRepo(mainRoot)
	require.NoError(t, err)
	enqueueCompletedInsightsReviewJob(t, db, repo.ID, "aaa111", "main", storage.JobTypeReview, failingInsightsOutput("Included finding"))
	countJobs := func() int {
		var n int
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM review_jobs`).Scan(&n))
		return n
	}
	before := countJobs()

	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath:      repoDir,
		GitRef:        "insights",
		Branch:        "main",
		Agent:         "test",
		JobType:       storage.JobTypeInsights,
		Since:         time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
		Panel:         "trio",
		HoldSynthesis: true,
	}))

	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, before, countJobs())
}
