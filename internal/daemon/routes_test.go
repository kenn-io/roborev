package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// serveHuma sends a request through the server's mux (which
// includes Huma-registered routes) and returns the recorder.
func serveHuma(
	t *testing.T, srv *Server, method, path string, body []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(
			method, path, bytes.NewReader(body),
		)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rr := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rr, req)
	return rr
}

func TestHumaListJobs(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	testutil.CreateTestJobs(t, db, repo, 5, "test-agent")

	t.Run("returns all jobs", func(t *testing.T) {
		rr := serveHuma(
			t, srv, http.MethodGet, "/api/jobs", nil,
		)
		require.Equal(t, http.StatusOK, rr.Code)

		var body struct {
			Jobs    []storage.ReviewJob `json:"jobs"`
			HasMore bool                `json:"has_more"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.Len(t, body.Jobs, 5)
		assert.False(t, body.HasMore)
	})

	t.Run("limit and has_more", func(t *testing.T) {
		rr := serveHuma(
			t, srv, http.MethodGet, "/api/jobs?limit=3", nil,
		)
		require.Equal(t, http.StatusOK, rr.Code)

		var body struct {
			Jobs    []storage.ReviewJob `json:"jobs"`
			HasMore bool                `json:"has_more"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.Len(t, body.Jobs, 3)
		assert.True(t, body.HasMore)
	})
}

func TestHumaListJobsCursorPagination(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	jobs := testutil.CreateTestJobs(t, db, repo, 5, "test-agent")

	// First page: 3 jobs (newest first by descending ID).
	rr := serveHuma(
		t, srv, http.MethodGet, "/api/jobs?limit=3", nil,
	)
	require.Equal(t, http.StatusOK, rr.Code)

	var page1 struct {
		Jobs    []storage.ReviewJob `json:"jobs"`
		HasMore bool                `json:"has_more"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &page1))
	require.Len(t, page1.Jobs, 3)
	assert.True(t, page1.HasMore)

	// Cursor = smallest ID in page 1.
	cursor := page1.Jobs[len(page1.Jobs)-1].ID

	rr2 := serveHuma(t, srv, http.MethodGet,
		fmt.Sprintf("/api/jobs?limit=10&before=%d", cursor), nil,
	)
	require.Equal(t, http.StatusOK, rr2.Code)

	var page2 struct {
		Jobs    []storage.ReviewJob `json:"jobs"`
		HasMore bool                `json:"has_more"`
	}
	require.NoError(t, json.Unmarshal(rr2.Body.Bytes(), &page2))
	assert.False(t, page2.HasMore)
	for _, j := range page2.Jobs {
		assert.Less(t, j.ID, cursor,
			"all page2 jobs should have ID < cursor")
	}

	// Both pages together should cover all jobs.
	allIDs := make(map[int64]bool)
	for _, j := range page1.Jobs {
		allIDs[j.ID] = true
	}
	for _, j := range page2.Jobs {
		allIDs[j.ID] = true
	}
	assert.Len(t, allIDs, len(jobs),
		"all jobs accounted for across both pages")
}

func TestHumaGetStatus(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	testutil.CreateTestJobs(t, db, repo, 2, "test-agent")
	srv.endpoint = DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"}

	rr := serveHuma(
		t, srv, http.MethodGet, "/api/status", nil,
	)
	require.Equal(t, http.StatusOK, rr.Code)

	var status storage.DaemonStatus
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &status))
	assert.NotEmpty(t, status.Version)
	assert.Equal(t, 2, status.QueuedJobs)
	assert.Equal(t, "tcp", status.Network)
	assert.Equal(t, "127.0.0.1:7373", status.Address)
	assert.Equal(t, 7373, status.Port)
}

func TestHumaGetReview_NotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)

	rr := serveHuma(
		t, srv, http.MethodGet, "/api/review?job_id=99999", nil,
	)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestHumaGetReview_Found(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	job := testutil.CreateCompletedReview(
		t, db, repo.ID, "abc123", "test-agent", "LGTM",
	)

	rr := serveHuma(t, srv, http.MethodGet,
		fmt.Sprintf("/api/review?job_id=%d", job.ID), nil,
	)
	require.Equal(t, http.StatusOK, rr.Code)

	var review storage.Review
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &review))
	assert.Equal(t, job.ID, review.JobID)
}

func TestHumaCancelJob(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	jobs := testutil.CreateTestJobs(t, db, repo, 1, "test-agent")
	jobID := jobs[0].ID

	t.Run("cancel queued job succeeds", func(t *testing.T) {
		body, _ := json.Marshal(CancelJobRequest{JobID: jobID})
		rr := serveHuma(
			t, srv, http.MethodPost, "/api/job/cancel", body,
		)
		require.Equal(t, http.StatusOK, rr.Code)

		var resp struct{ Success bool }
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		assert.True(t, resp.Success)

		job, err := db.GetJobByID(jobID)
		require.NoError(t, err)
		assert.Equal(t, storage.JobStatusCanceled, job.Status)
	})

	t.Run("cancel already canceled returns 404", func(t *testing.T) {
		body, _ := json.Marshal(CancelJobRequest{JobID: jobID})
		rr := serveHuma(
			t, srv, http.MethodPost, "/api/job/cancel", body,
		)
		assert.Equal(t, http.StatusNotFound, rr.Code)
	})

	t.Run("cancel nonexistent returns 404", func(t *testing.T) {
		body, _ := json.Marshal(CancelJobRequest{JobID: 99999})
		rr := serveHuma(
			t, srv, http.MethodPost, "/api/job/cancel", body,
		)
		assert.Equal(t, http.StatusNotFound, rr.Code)
	})
}

func TestHumaRerunJob(t *testing.T) {
	srv, db, tmpDir := newTestServer(t)

	// Use tmpDir as repo path so resolveRerunModelProvider
	// finds a real directory for validation.
	repo, err := db.GetOrCreateRepo(tmpDir)
	require.NoError(t, err)

	commit, err := db.GetOrCreateCommit(
		repo.ID, "deadbeef", "A", "S", time.Now(),
	)
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID,
		GitRef: "deadbeef", Agent: "test",
	})
	require.NoError(t, err)
	_, err = db.ClaimJob("w")
	require.NoError(t, err)
	_, err = db.FailJob(job.ID, "", "some error")
	require.NoError(t, err)

	body, _ := json.Marshal(RerunJobRequest{JobID: job.ID})
	rr := serveHuma(
		t, srv, http.MethodPost, "/api/job/rerun", body,
	)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp struct{ Success bool }
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.True(t, resp.Success)
}

func TestHumaCloseReview(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	job := testutil.CreateCompletedReview(
		t, db, repo.ID, "closeme", "test-agent", "done",
	)

	t.Run("close review", func(t *testing.T) {
		body, _ := json.Marshal(CloseReviewRequest{
			JobID: job.ID, Closed: true,
		})
		rr := serveHuma(
			t, srv, http.MethodPost, "/api/review/close", body,
		)
		require.Equal(t, http.StatusOK, rr.Code)

		var resp struct{ Success bool }
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		assert.True(t, resp.Success)
	})

	t.Run("close nonexistent review returns 404", func(t *testing.T) {
		body, _ := json.Marshal(CloseReviewRequest{
			JobID: 99999, Closed: true,
		})
		rr := serveHuma(
			t, srv, http.MethodPost, "/api/review/close", body,
		)
		assert.Equal(t, http.StatusNotFound, rr.Code)
	})
}

func TestHumaOpenAPISpec(t *testing.T) {
	srv, _, _ := newTestServer(t)

	rr := serveHuma(
		t, srv, http.MethodGet, "/openapi.json", nil,
	)
	require.Equal(t, http.StatusOK, rr.Code)

	var spec map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &spec))

	assert.Equal(t, "3.1.0", spec["openapi"])

	paths, ok := spec["paths"].(map[string]any)
	require.True(t, ok, "spec must have paths object")

	wantPaths := map[string]string{
		"/api/jobs":              "get",
		"/api/review":            "get",
		"/api/comments":          "get",
		"/api/repos":             "get",
		"/api/repos/resolve":     "get",
		"/api/branches":          "get",
		"/api/status":            "get",
		"/api/summary":           "get",
		"/api/health":            "get",
		"/api/ping":              "get",
		"/api/sync/status":       "get",
		"/api/activity":          "get",
		"/api/job/output":        "get",
		"/api/job/log":           "get",
		"/api/job/patch":         "get",
		"/api/stream/events":     "get",
		"/api/job/cancel":        "post",
		"/api/job/rerun":         "post",
		"/api/review/close":      "post",
		"/api/comment":           "post",
		"/api/enqueue":           "post",
		"/api/jobs/batch":        "post",
		"/api/repos/register":    "post",
		"/api/job/update-branch": "post",
		"/api/remap":             "post",
		"/api/sync/now":          "post",
		"/api/job/fix":           "post",
		"/api/job/applied":       "post",
		"/api/job/rebased":       "post",
	}
	for p, method := range wantPaths {
		pathObj, exists := paths[p]
		assert.True(t, exists,
			"expected path %s in OpenAPI spec", p)
		if exists {
			methods, ok := pathObj.(map[string]any)
			require.True(t, ok,
				"path %s should be an object", p)
			_, hasMethod := methods[method]
			assert.True(t, hasMethod,
				"path %s should have method %s", p, method)
		}
	}

	enqueuePath, ok := paths["/api/enqueue"].(map[string]any)
	require.True(t, ok, "enqueue path should be an object")
	enqueuePost, ok := enqueuePath["post"].(map[string]any)
	require.True(t, ok, "enqueue post should be an object")
	responses, ok := enqueuePost["responses"].(map[string]any)
	require.True(t, ok, "enqueue responses should be an object")
	statusOK, ok := responses["200"].(map[string]any)
	require.True(t, ok, "enqueue should document skipped response")
	content, ok := statusOK["content"].(map[string]any)
	require.True(t, ok, "enqueue 200 response should have content")
	jsonContent, ok := content["application/json"].(map[string]any)
	require.True(t, ok, "enqueue 200 response should document JSON")
	schema, ok := jsonContent["schema"].(map[string]any)
	require.True(t, ok, "enqueue 200 response should have a schema")
	oneOf, ok := schema["oneOf"].([]any)
	require.True(t, ok, "enqueue 200 response should use oneOf")
	assert.Len(t, oneOf, 2)
}

func TestHumaListRepos(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	testutil.CreateTestJobs(t, db, repo, 2, "test-agent")

	rr := serveHuma(
		t, srv, http.MethodGet, "/api/repos", nil,
	)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp struct {
		Repos      []storage.RepoWithCount `json:"repos"`
		TotalCount int                     `json:"total_count"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.GreaterOrEqual(t, len(resp.Repos), 1)
}

func TestHumaListBranches(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)

	commit, err := db.GetOrCreateCommit(
		repo.ID, "brsha", "A", "S", time.Now(),
	)
	require.NoError(t, err)
	_, err = db.EnqueueJob(storage.EnqueueOpts{
		RepoID:   repo.ID,
		CommitID: commit.ID,
		GitRef:   "brsha",
		Branch:   "feature-x",
		Agent:    "test-agent",
	})
	require.NoError(t, err)

	rr := serveHuma(
		t, srv, http.MethodGet, "/api/branches", nil,
	)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp struct {
		Branches       []storage.BranchWithCount `json:"branches"`
		TotalCount     int                       `json:"total_count"`
		NullsRemaining int                       `json:"nulls_remaining"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.GreaterOrEqual(t, resp.TotalCount, 1)
}

func TestHumaGetSummary(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	testutil.CreateCompletedReview(
		t, db, repo.ID, "sumsha", "test-agent", "ok",
	)

	rr := serveHuma(
		t, srv, http.MethodGet, "/api/summary", nil,
	)
	require.Equal(t, http.StatusOK, rr.Code)

	var summary storage.Summary
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &summary))
	assert.GreaterOrEqual(t, summary.Overview.Total, 1)
}

func TestHumaListComments(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	job := testutil.CreateCompletedReview(
		t, db, repo.ID, "comsha", "test-agent", "review text",
	)

	_, err := db.AddCommentToJob(job.ID, "alice", "nice work")
	require.NoError(t, err)

	rr := serveHuma(t, srv, http.MethodGet,
		fmt.Sprintf("/api/comments?job_id=%d", job.ID), nil,
	)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp struct {
		Responses []storage.Response `json:"responses"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Len(t, resp.Responses, 1)
	assert.Equal(t, "alice", resp.Responses[0].Responder)
}

func TestHumaAddComment(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	job := testutil.CreateCompletedReview(
		t, db, repo.ID, "addcsha", "test-agent", "review text",
	)

	body, _ := json.Marshal(AddCommentRequest{
		JobID:     job.ID,
		Commenter: "bob",
		Comment:   "looks good",
	})
	rr := serveHuma(
		t, srv, http.MethodPost, "/api/comment", body,
	)
	require.Equal(t, http.StatusCreated, rr.Code)

	var resp storage.Response
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "bob", resp.Responder)
}
