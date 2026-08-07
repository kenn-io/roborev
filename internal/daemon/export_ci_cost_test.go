package daemon

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func seedRouteCICostJob(t *testing.T, db *storage.DB, repoID int64, gitRef, finishedAt string) *storage.ReviewJob {
	t.Helper()
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repoID, GitRef: gitRef, Agent: "test-agent",
		Model: "test-model", Provider: "test-provider",
		PanelRunUUID: "run-" + gitRef, PanelRole: storage.PanelRoleMember,
		Source: storage.JobSourceCI,
	})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs
		SET status = 'done', started_at = '2026-08-01 00:00:00',
		    finished_at = ?, agent_invoked = 1,
		    token_usage = '{"has_cost":true,"cost_usd":0.125}'
		WHERE id = ?`, finishedAt, job.ID)
	require.NoError(t, err)
	return job
}

func TestHumaExportCICosts(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	job := seedRouteCICostJob(t, db, repo.ID, "head-a", "2026-08-02 12:00:00")

	rr := serveHuma(t, srv, http.MethodGet,
		"/api/export/ci-costs?since=2026-08-01&until=2026-08-03&limit=1", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var doc ExportCICostDocument
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &doc))
	assert.Equal(t, 1, doc.SchemaVersion)
	assert.Equal(t, "roborev", doc.Tool)
	assert.NotEmpty(t, doc.ToolVersion)
	assert.NotEmpty(t, doc.GeneratedAt)
	assert.NotEmpty(t, doc.DatabaseID)
	assert.False(t, doc.Legacy)
	assert.Equal(t, "finished_at", doc.Window.Field)
	require.NotNil(t, doc.Window.Since)
	assert.Equal(t, "2026-08-01T00:00:00Z", *doc.Window.Since)
	require.NotNil(t, doc.Window.Until)
	assert.Equal(t, "2026-08-04T00:00:00Z", *doc.Window.Until)
	require.Len(t, doc.Jobs, 1)
	assert.Equal(t, job.UUID, doc.Jobs[0].JobUUID)
	assert.False(t, doc.Truncated)
	require.NotNil(t, doc.NextCursor)
}

func TestHumaExportCICostsLegacy(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	_, err := db.Exec(`INSERT INTO ci_pr_panels
		(github_repo, pr_number, head_sha, panel_run_uuid, created_at)
		VALUES ('example/project', 99, 'era-head', 'era-run', '2026-06-01 00:00:00')`)
	require.NoError(t, err)
	for i := range 2 {
		job, enqueueErr := db.EnqueueJob(storage.EnqueueOpts{
			RepoID: repo.ID, GitRef: "base..legacy-head",
			Agent: fmt.Sprintf("agent-%d", i), Model: "legacy-model",
		})
		require.NoError(t, enqueueErr)
		_, updateErr := db.Exec(`UPDATE review_jobs
			SET status = 'done', enqueued_at = '2026-03-01 10:00:00',
			    started_at = '2026-03-01 10:00:00', finished_at = ?,
			    agent_invoked = 1
			WHERE id = ?`, fmt.Sprintf("2026-03-01 10:0%d:00", 5+i), job.ID)
		require.NoError(t, updateErr)
	}

	rr := serveHuma(t, srv, http.MethodGet, "/api/export/ci-costs?legacy=true", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var doc ExportCICostDocument
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &doc))
	assert.True(t, doc.Legacy)
	require.Len(t, doc.Jobs, 2)
	assert.Equal(t, "review", doc.Jobs[0].Role)

	rr = serveHuma(t, srv, http.MethodGet, "/api/export/ci-costs", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &doc))
	assert.Empty(t, doc.Jobs)
}

func TestHumaExportCICostsValidation(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	seedRouteCICostJob(t, db, repo.ID, "head-validation", "2026-08-02 12:00:00")

	for _, path := range []string{
		"/api/export/ci-costs?format=csv",
		"/api/export/ci-costs?cursor=abc&since=2026-08-01",
		"/api/export/ci-costs?since=not-a-time",
		"/api/export/ci-costs?until=not-a-time",
	} {
		rr := serveHuma(t, srv, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusBadRequest, rr.Code, path+": "+rr.Body.String())
	}

	foreign, err := json.Marshal(map[string]any{
		"version": 1, "database_id": "foreign-database",
		"finished_at": "2026-08-02T12:00:00Z", "job_id": 1,
	})
	require.NoError(t, err)
	foreignCursor := base64.RawURLEncoding.EncodeToString(foreign)
	rr := serveHuma(t, srv, http.MethodGet,
		"/api/export/ci-costs?cursor="+url.QueryEscape(foreignCursor), nil)
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())

	regular := serveHuma(t, srv, http.MethodGet, "/api/export/ci-costs?limit=1", nil)
	require.Equal(t, http.StatusOK, regular.Code, regular.Body.String())
	var doc ExportCICostDocument
	require.NoError(t, json.Unmarshal(regular.Body.Bytes(), &doc))
	require.NotNil(t, doc.NextCursor)
	rr = serveHuma(t, srv, http.MethodGet,
		"/api/export/ci-costs?until=2026-08-03&cursor="+url.QueryEscape(*doc.NextCursor), nil)
	assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	rr = serveHuma(t, srv, http.MethodGet,
		"/api/export/ci-costs?legacy=true&cursor="+url.QueryEscape(*doc.NextCursor), nil)
	assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
}

func TestHumaExportCICostsCursorPreservesSince(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	oldJob := seedRouteCICostJob(t, db, repo.ID, "old", "2026-07-01 01:00:00")
	firstJob := seedRouteCICostJob(t, db, repo.ID, "first", "2026-08-01 01:00:00")
	secondJob := seedRouteCICostJob(t, db, repo.ID, "second", "2026-08-01 02:00:00")

	rr := serveHuma(t, srv, http.MethodGet,
		"/api/export/ci-costs?since=2026-08-01&limit=1", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var first ExportCICostDocument
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &first))
	require.Len(t, first.Jobs, 1)
	assert.Equal(t, firstJob.UUID, first.Jobs[0].JobUUID)
	require.NotNil(t, first.NextCursor)

	_, err := db.Exec(`UPDATE review_jobs
		SET token_usage = '{"has_cost":true,"cost_usd":0.75}'
		WHERE id = ?`, oldJob.ID)
	require.NoError(t, err)

	rr = serveHuma(t, srv, http.MethodGet,
		"/api/export/ci-costs?cursor="+url.QueryEscape(*first.NextCursor), nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resumed ExportCICostDocument
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resumed))
	require.NotNil(t, resumed.Window.Since)
	assert.Equal(t, "2026-08-01T00:00:00Z", *resumed.Window.Since)
	require.Len(t, resumed.Jobs, 1)
	assert.Equal(t, secondJob.UUID, resumed.Jobs[0].JobUUID)
}
