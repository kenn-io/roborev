package daemon

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func postRepoManagement(t *testing.T, server *Server, action string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/repos/"+action, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(recorder, req)
	return recorder
}

func TestRepoManagementDetailsAndRename(t *testing.T) {
	server, db, dir := newTestServer(t)
	job := createTestJob(t, db, dir, "repo-management-details", "test")
	repo, err := db.GetRepoByID(job.RepoID)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/api/repos/detail?identifier="+url.QueryEscape(repo.Name), nil)
	recorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var details storage.RepoStats
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &details))
	assert.Equal(t, repo.ID, details.Repo.ID)
	assert.Equal(t, 1, details.TotalJobs)
	assert.Equal(t, 1, details.QueuedJobs)

	renamed := postRepoManagement(t, server, "rename", map[string]any{"identifier": repo.RootPath, "name": "friendly", "by_path": true})
	require.Equal(t, http.StatusOK, renamed.Code, renamed.Body.String())
	stored, err := db.GetRepoByID(repo.ID)
	require.NoError(t, err)
	assert.Equal(t, "friendly", stored.Name)
	assert.Equal(t, repo.RootPath, stored.RootPath)

	missing := postRepoManagement(t, server, "rename", map[string]any{"identifier": "missing", "name": "friendly", "by_path": false})
	assert.Equal(t, http.StatusNotFound, missing.Code)
}

func TestRepoManagementMoveAndConflict(t *testing.T) {
	server, db, dir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(filepath.Join(dir, "source"), "local://source")
	require.NoError(t, err)
	target, err := db.GetOrCreateRepo(filepath.Join(dir, "target"))
	require.NoError(t, err)
	conflict := postRepoManagement(t, server, "move", map[string]any{
		"repo_id": repo.ID, "path": target.RootPath, "identity": "local://target",
	})
	require.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())
	stored, err := db.GetRepoByID(repo.ID)
	require.NoError(t, err)
	assert.Equal(t, repo.RootPath, stored.RootPath)

	path := filepath.ToSlash(filepath.Join(dir, "moved"))
	moved := postRepoManagement(t, server, "move", map[string]any{
		"repo_id": repo.ID, "path": path, "identity": "local://moved",
	})
	require.Equal(t, http.StatusNoContent, moved.Code, moved.Body.String())
	stored, err = db.GetRepoByID(repo.ID)
	require.NoError(t, err)
	assert.Equal(t, path, stored.RootPath)
	assert.Equal(t, "local://moved", stored.Identity)
	assert.Equal(t, repo.Name, stored.Name)
}

func TestRepoManagementDeleteRequiresCascade(t *testing.T) {
	server, db, dir := newTestServer(t)
	job := createTestJob(t, db, dir, "repo-management-delete", "test")
	refusal := postRepoManagement(t, server, "delete", map[string]any{"repo_id": job.RepoID, "cascade": false})
	require.Equal(t, http.StatusConflict, refusal.Code, refusal.Body.String())
	_, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	deleted := postRepoManagement(t, server, "delete", map[string]any{"repo_id": job.RepoID, "cascade": true})
	require.Equal(t, http.StatusNoContent, deleted.Code, deleted.Body.String())
	_, err = db.GetRepoByID(job.RepoID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	_, err = db.GetJobByID(job.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestRepoManagementMergeUsesRepositoryIDs(t *testing.T) {
	server, db, dir := newTestServer(t)
	job := createTestJob(t, db, filepath.Join(dir, "source"), "repo-management-merge", "test")
	target, err := db.GetOrCreateRepo(filepath.Join(dir, "target"))
	require.NoError(t, err)
	same := postRepoManagement(t, server, "merge", map[string]any{"source_id": target.ID, "target_id": target.ID})
	assert.Equal(t, http.StatusBadRequest, same.Code)
	merged := postRepoManagement(t, server, "merge", map[string]any{"source_id": job.RepoID, "target_id": target.ID})
	require.Equal(t, http.StatusOK, merged.Code, merged.Body.String())
	var output struct {
		Moved int64 `json:"moved"`
	}
	require.NoError(t, json.Unmarshal(merged.Body.Bytes(), &output))
	assert.Equal(t, int64(1), output.Moved)
	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, target.ID, stored.RepoID)
	_, err = db.GetRepoByID(job.RepoID)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestRepoManagementMissingIDs(t *testing.T) {
	server, db, dir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(dir)
	require.NoError(t, err)
	cases := []struct {
		action string
		body   any
	}{
		{"move", map[string]any{"repo_id": repo.ID + 100, "path": filepath.Join(dir, "moved"), "identity": "local://moved"}},
		{"delete", map[string]any{"repo_id": repo.ID + 100, "cascade": false}},
		{"merge", map[string]any{"source_id": repo.ID, "target_id": repo.ID + 100}},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			response := postRepoManagement(t, server, tc.action, tc.body)
			assert.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
		})
	}

	_, err = db.GetRepoByID(repo.ID)
	require.NoError(t, err)
}

func TestRepoManagementNameDoesNotResolveAgainstDaemonDirectory(t *testing.T) {
	server, db, dir := newTestServer(t)
	name := "project"
	daemonRelativePath, err := filepath.Abs(name)
	require.NoError(t, err)
	pathRepo, err := db.GetOrCreateRepo(daemonRelativePath)
	require.NoError(t, err)
	_, err = db.RenameRepo(pathRepo.RootPath, "path-owner")
	require.NoError(t, err)
	namedRepo, err := db.GetOrCreateRepo(filepath.Join(dir, "named"))
	require.NoError(t, err)
	_, err = db.RenameRepo(namedRepo.RootPath, name)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/repos/detail?identifier="+url.QueryEscape(name), nil)
	recorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var details storage.RepoStats
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &details))
	require.Equal(t, namedRepo.ID, details.Repo.ID)

	deleted := postRepoManagement(t, server, "delete", map[string]any{"repo_id": details.Repo.ID, "cascade": false})
	require.Equal(t, http.StatusNoContent, deleted.Code, deleted.Body.String())
	_, err = db.GetRepoByID(namedRepo.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	storedPathRepo, err := db.GetRepoByID(pathRepo.ID)
	require.NoError(t, err)
	assert.Equal(t, "path-owner", storedPathRepo.Name)

	pathRequest := httptest.NewRequest(http.MethodGet, "/api/repos/detail?by_path=true&identifier="+url.QueryEscape(pathRepo.RootPath), nil)
	pathRecorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(pathRecorder, pathRequest)
	require.Equal(t, http.StatusOK, pathRecorder.Code, pathRecorder.Body.String())
	var pathDetails storage.RepoStats
	require.NoError(t, json.Unmarshal(pathRecorder.Body.Bytes(), &pathDetails))
	assert.Equal(t, pathRepo.ID, pathDetails.Repo.ID)

	// Display names are not unique; renaming a name still updates every match.
	var namedIDs []int64
	for _, path := range []string{filepath.Join(dir, "named"), filepath.Join(dir, "duplicate")} {
		repo, err := db.GetOrCreateRepo(path)
		require.NoError(t, err)
		_, err = db.RenameRepo(repo.RootPath, name)
		require.NoError(t, err)
		namedIDs = append(namedIDs, repo.ID)
	}
	renamed := postRepoManagement(t, server, "rename", map[string]any{"identifier": name, "name": "friendly", "by_path": false})
	require.Equal(t, http.StatusOK, renamed.Code, renamed.Body.String())
	for _, id := range namedIDs {
		repo, err := db.GetRepoByID(id)
		require.NoError(t, err)
		assert.Equal(t, "friendly", repo.Name)
	}
	storedPathRepo, err = db.GetRepoByID(pathRepo.ID)
	require.NoError(t, err)
	assert.Equal(t, "path-owner", storedPathRepo.Name)
}

func TestRepoManagementPathDoesNotFallBackToName(t *testing.T) {
	assert := assert.New(t)
	server, db, dir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(filepath.Join(dir, "tracked"))
	require.NoError(t, err)
	untracked := filepath.Join(dir, "untracked")
	_, err = db.RenameRepo(repo.RootPath, untracked)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/repos/detail?by_path=true&identifier="+url.QueryEscape(untracked), nil)
	recorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(recorder, req)
	assert.Equal(http.StatusNotFound, recorder.Code, recorder.Body.String())

	renamed := postRepoManagement(t, server, "rename", map[string]any{
		"identifier": untracked, "name": "changed", "by_path": true,
	})
	assert.Equal(http.StatusNotFound, renamed.Code, renamed.Body.String())
	stored, err := db.GetRepoByID(repo.ID)
	require.NoError(t, err)
	assert.Equal(untracked, stored.Name)

	tracked, err := db.GetOrCreateRepo(filepath.Join(dir, "another"))
	require.NoError(t, err)
	renamed = postRepoManagement(t, server, "rename", map[string]any{
		"identifier": tracked.RootPath, "name": untracked, "by_path": true,
	})
	require.Equal(t, http.StatusOK, renamed.Code, renamed.Body.String())
	var result RenameRepoOutput
	require.NoError(t, json.Unmarshal(renamed.Body.Bytes(), &result.Body))
	assert.Equal(tracked.ID, result.Body.Repo.ID)
}
