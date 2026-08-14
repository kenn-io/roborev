package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReviewProjectionByJobID(t *testing.T) {
	server, db, tempDir := newTestServer(t)
	repo, jobs := seedRepoWithJobs(t, db, filepath.Join(tempDir, "project-a"), 1, "project-a")
	job := jobs[0]
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', branch = 'main', model = 'model-a' WHERE id = ?`, job.ID)
	require.NoError(t, err)
	require.NoError(t, db.CompleteJob(job.ID, "test", "prompt", "PASS\n\nReview output"))
	_, err = db.AddCommentToJob(job.ID, "reviewer-a", "Existing response")
	require.NoError(t, err)

	response := serveHuma(t, server, http.MethodGet,
		"/api/ui/review-projection?job_id="+url.QueryEscape(stringID(job.ID)), nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	var projection ReviewProjection
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &projection))
	assert.Equal(t, ReviewProjectionSchemaVersion, projection.SchemaVersion)
	assert.Equal(t, job.ID, projection.Job.ID)
	assert.Equal(t, repo.Name, projection.Job.Project)
	assert.Equal(t, "main", projection.Job.Branch)
	assert.Equal(t, "model-a", projection.Job.Model)
	assert.Equal(t, "P", projection.Job.Verdict)
	require.NotNil(t, projection.Review)
	assert.Equal(t, "PASS\n\nReview output", projection.Review.Output)
	require.Len(t, projection.Responses, 1)
	assert.Equal(t, "Existing response", projection.Responses[0].Response)
	assert.NotContains(t, response.Body.String(), repo.RootPath)
}

func TestReviewProjectionSelectsNewestContextualReview(t *testing.T) {
	server, db, tempDir := newTestServer(t)
	repo, jobs := seedRepoWithJobs(t, db, filepath.Join(tempDir, "project-a"), 2, "project-a")
	for _, job := range jobs {
		_, err := db.Exec(`UPDATE review_jobs SET status = 'running', branch = 'feature' WHERE id = ?`, job.ID)
		require.NoError(t, err)
		require.NoError(t, db.CompleteJob(job.ID, "test", "prompt", "PASS"))
	}

	response := serveHuma(t, server, http.MethodGet,
		"/api/ui/review-projection?repo="+url.QueryEscape(repo.RootPath)+"&branch=feature", nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var projection ReviewProjection
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &projection))
	assert.Equal(t, jobs[len(jobs)-1].ID, projection.Job.ID)
}

func TestReviewProjectionRejectsMixedOrIncompleteSelectors(t *testing.T) {
	server, _, _ := newTestServer(t)
	for _, query := range []string{
		"",
		"job_id=1&repo=%2Fproject-a",
		"branch=main",
	} {
		response := serveHuma(t, server, http.MethodGet,
			"/api/ui/review-projection?"+query, nil)
		assert.Equal(t, http.StatusBadRequest, response.Code, query)
	}
}

func TestReviewProjectionReturnsNotFound(t *testing.T) {
	server, _, _ := newTestServer(t)
	response := serveHuma(t, server, http.MethodGet,
		"/api/ui/review-projection?job_id=999999", nil)
	assert.Equal(t, http.StatusNotFound, response.Code)
}

func stringID(id int64) string {
	return fmt.Sprintf("%d", id)
}
