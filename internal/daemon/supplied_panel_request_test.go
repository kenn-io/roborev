package daemon

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
	roborevclient "go.kenn.io/roborev/pkg/client"
	"go.kenn.io/roborev/pkg/client/generated"
)

func panelRequestFixture(t *testing.T) (*Server, *storage.DB, string, map[string]any) {
	t.Helper()
	testenv.SetDataDir(t)
	server, db, dir := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile("file.txt", "target\n", "target")
	return server, db, dir, map[string]any{
		"request_id": uuid.New(), "repo_path": repo.Path(), "git_ref": head,
		"hold_synthesis": true,
		"members": []any{map[string]any{
			"name": "reviewer", "agent": "test", "review_prompt": "Review this commit.", "review_settings": map[string]any{},
		}},
		"synthesis": map[string]any{"agent": "test"},
	}
}

func postPanelRequest(t *testing.T, server *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/panel/enqueue", body))
	return w
}

func TestSuppliedPanelRequestSurvivesRestart(t *testing.T) {
	assert := assert.New(t)
	server, db, dir, body := panelRequestFixture(t)
	first := postPanelRequest(t, server, body)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	var accepted PanelEnqueueResponse
	testutil.DecodeJSON(t, first, &accepted)
	// The caller loses this response, then reconnects after a daemon restart.
	require.NoError(t, server.Close())
	require.NoError(t, db.Close())
	reopened, err := storage.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := newServerWithLogs(reopened, config.DefaultConfig(), "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = restarted.Close() })
	// A committed receipt must not require access to the old checkout.
	repoPath := body["repo_path"].(string)
	require.NoError(t, os.Rename(repoPath, repoPath+"-moved"))
	t.Cleanup(func() { _ = os.Rename(repoPath+"-moved", repoPath) })
	retry := postPanelRequest(t, restarted, body)
	require.Equal(t, http.StatusCreated, retry.Code, retry.Body.String())
	var replay PanelEnqueueResponse
	testutil.DecodeJSON(t, retry, &replay)
	assert.Equal(accepted.PanelRunUUID, replay.PanelRunUUID)
	assert.Equal(accepted.ID, replay.ID)
	assert.Equal(accepted.MemberJobIDs, replay.MemberJobIDs)
	var jobs int
	require.NoError(t, reopened.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM review_jobs").Scan(&jobs))
	assert.Equal(2, jobs)
}

func TestSuppliedPanelRequestRejectsChangedInputAndDeletedPanel(t *testing.T) {
	server, db, _, body := panelRequestFixture(t)
	first := postPanelRequest(t, server, body)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	var panel PanelEnqueueResponse
	testutil.DecodeJSON(t, first, &panel)
	body["hold_synthesis"] = false
	conflict := postPanelRequest(t, server, body)
	require.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())
	body["hold_synthesis"] = true
	require.NoError(t, db.DeleteRepo(panel.RepoID, true))
	gone := postPanelRequest(t, server, body)
	require.Equal(t, http.StatusGone, gone.Code, gone.Body.String())
	var jobs int
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM review_jobs").Scan(&jobs))
	assert.Zero(t, jobs)
	body["request_id"] = uuid.New()
	fresh := postPanelRequest(t, server, body)
	require.Equal(t, http.StatusCreated, fresh.Code, fresh.Body.String())
}

func TestSuppliedPanelRequestConcurrentReplay(t *testing.T) {
	server, db, _, body := panelRequestFixture(t)
	data, err := json.Marshal(body)
	require.NoError(t, err)
	const callers = 8
	results := make([]*httptest.ResponseRecorder, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			w := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/panel/enqueue", bytes.NewReader(data))
			req.Header.Set("Content-Type", "application/json")
			server.httpServer.Handler.ServeHTTP(w, req)
			results[i] = w
		})
	}
	wg.Wait()
	var run uuid.UUID
	for _, w := range results {
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		var panel PanelEnqueueResponse
		testutil.DecodeJSON(t, w, &panel)
		if run == uuid.Nil() {
			run = panel.PanelRunUUID
		}
		assert.Equal(t, run, panel.PanelRunUUID)
	}
	var jobs int
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM review_jobs").Scan(&jobs))
	assert.Equal(t, 2, jobs)
}

func TestSuppliedPanelRequestReceiptFailureRollsBackJobs(t *testing.T) {
	server, db, _, body := panelRequestFixture(t)
	_, err := db.ExecContext(t.Context(), `CREATE TRIGGER reject_panel_receipt BEFORE INSERT ON panel_enqueue_requests
        BEGIN SELECT RAISE(ABORT, 'receipt write failed'); END`)
	require.NoError(t, err)
	failed := postPanelRequest(t, server, body)
	require.Equal(t, http.StatusInternalServerError, failed.Code, failed.Body.String())
	var jobs int
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM review_jobs").Scan(&jobs))
	assert.Zero(t, jobs)
	_, err = db.ExecContext(t.Context(), "DROP TRIGGER reject_panel_receipt")
	require.NoError(t, err)
	retry := postPanelRequest(t, server, body)
	require.Equal(t, http.StatusCreated, retry.Code, retry.Body.String())
}

func TestSuppliedPanelRequestGeneratedClientRecoversLostResponse(t *testing.T) {
	server, db, _, body := panelRequestFixture(t)
	data, err := json.Marshal(body)
	require.NoError(t, err)
	var input generated.EnqueueSuppliedPanelInputBody
	require.NoError(t, json.Unmarshal(data, &input))
	var dropResponse atomic.Bool
	dropResponse.Store(true)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dropResponse.Swap(false) {
			server.httpServer.Handler.ServeHTTP(httptest.NewRecorder(), r)
			// Close the connection after enqueue commits, before the client sees its receipt.
			panic(http.ErrAbortHandler)
		}
		server.httpServer.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	api, err := roborevclient.New(httpServer.URL)
	require.NoError(t, err)
	options := &generated.EnqueueSuppliedPanelRequestOptions{Body: &input}
	first, err := api.EnqueueSuppliedPanel(t.Context(), options)
	require.Error(t, err)
	require.Nil(t, first)
	// The member has already run when the caller recovers the submission.
	job, err := db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, job)
	server.workerPool.processJob(testWorkerID, job)
	done, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	require.Equal(t, storage.JobStatusDone, done.Status, done.Error)
	replay, err := api.EnqueueSuppliedPanel(t.Context(), options)
	require.NoError(t, err)
	assert.Equal(t, *done.PanelRunUUID, replay.PanelRunUUID)
	assert.Equal(t, []int64{done.ID}, replay.MemberJobIds)
	job, err = db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	assert.Nil(t, job, "replay must not queue another member or release held synthesis")
}
