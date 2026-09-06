package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestBrowserReviewProjectionOmitsFileCoverage(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	job := testutil.CreateCompletedReview(t, db, repo.ID, "coverage-api", "test", "PASS")
	_, err := db.Exec(`UPDATE reviews SET reviewed_file_count = 0, excluded_file_count = 3 WHERE job_id = ?`, job.ID)
	require.NoError(t, err)
	loopbackResponse := serveHuma(t, server, http.MethodGet,
		fmt.Sprintf("/api/review?job_id=%d", job.ID), nil)
	require.Equal(t, http.StatusOK, loopbackResponse.Code)
	assert.Contains(t, loopbackResponse.Body.String(), "file_coverage")

	browserResponse := httptest.NewRecorder()
	serveBrowserReview(browserResponse, httptest.NewRequest(
		http.MethodGet, "/api/review?job_id="+fmt.Sprint(job.ID), nil,
	), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(loopbackResponse.Body.Bytes())
	}))
	assert.Equal(t, http.StatusOK, browserResponse.Code)
	assert.NotContains(t, browserResponse.Body.String(), "file_coverage")

	zero, excluded := 0, 3
	review := storage.Review{
		ID: 1, JobID: 2, Agent: "test", Prompt: "prompt", Output: "output",
		FileCoverage: &storage.ReviewFileCoverage{Reviewed: &zero, Excluded: &excluded},
	}
	loopback, err := json.Marshal(review)
	require.NoError(t, err)
	assert.Contains(t, string(loopback), "file_coverage")

	core := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(loopback)
	})
	request := httptest.NewRequest(http.MethodGet, "/api/review?job_id=2", nil)
	recorder := httptest.NewRecorder()
	serveBrowserReview(recorder, request, core)
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "file_coverage")
}
