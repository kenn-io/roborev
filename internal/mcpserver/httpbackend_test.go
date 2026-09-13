package mcpserver

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeDaemon struct {
	t        *testing.T
	requests []*url.URL
	routes   map[string]func(w http.ResponseWriter, r *http.Request)
}

func newFakeDaemon(t *testing.T) (*fakeDaemon, *HTTPBackend) {
	t.Helper()
	d := &fakeDaemon{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.requests = append(d.requests, r.URL)
		handler, ok := d.routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return d, NewHTTPBackend(srv.URL+"/", srv.Client())
}

func jsonResponse(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestHTTPBackendListJobsSendsFiltersAndOmitsPrompts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d, backend := newFakeDaemon(t)
	d.routes["/api/jobs"] = jsonResponse(http.StatusOK,
		`{"jobs":[{"id":12,"git_ref":"abc","agent":"codex","job_type":"review","status":"done"}],"has_more":true,"next_cursor":"c2"}`)
	closed := false

	page, err := backend.ListJobs(t.Context(), JobsQuery{
		RepoPath: "/repo", Branch: "main", Status: "done", JobType: "review",
		Closed: &closed, Limit: 25, Cursor: "c1",
	})
	require.NoError(err)
	require.Len(page.Jobs, 1)
	assert.Equal(int64(12), page.Jobs[0].ID)
	assert.True(page.HasMore)
	assert.Equal("c2", page.NextCursor)

	require.Len(d.requests, 1)
	q := d.requests[0].Query()
	assert.Equal("/repo", q.Get("repo"))
	assert.Equal("main", q.Get("branch"))
	assert.Equal("done", q.Get("status"))
	assert.Equal("review", q.Get("job_type"))
	assert.Equal("false", q.Get("closed"))
	assert.Equal("25", q.Get("limit"))
	assert.Equal("c1", q.Get("cursor"))
	assert.Equal("true", q.Get("omit_prompt"))
	assert.Equal("true", q.Get("include_findings"))
}

func TestHTTPBackendReviewRefPrefersJobID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d, backend := newFakeDaemon(t)
	d.routes["/api/review"] = jsonResponse(http.StatusOK, `{"id":1,"job_id":5,"agent":"codex","output":"ok"}`)
	d.routes["/api/comments"] = jsonResponse(http.StatusOK, `{"responses":[{"id":2,"responder":"dev","response":"fixed"}]}`)

	review, err := backend.GetReview(t.Context(), ReviewRef{JobID: 5, SHA: "ignored"})
	require.NoError(err)
	assert.Equal(int64(5), review.JobID)
	assert.Equal("5", d.requests[0].Query().Get("job_id"))
	assert.Empty(d.requests[0].Query().Get("sha"))

	comments, err := backend.ListComments(t.Context(), CommentRef{SHA: "abc"})
	require.NoError(err)
	require.Len(comments, 1)
	assert.Equal("fixed", comments[0].Response)
	assert.Equal("abc", d.requests[1].Query().Get("sha"))
}

func TestHTTPBackendMapsStatusCodesToErrorCodes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d, backend := newFakeDaemon(t)
	d.routes["/api/review"] = jsonResponse(http.StatusNotFound, `{"title":"Not Found","status":404,"detail":"review not found"}`)
	d.routes["/api/comments"] = jsonResponse(http.StatusBadRequest, `{"error":"job_id, commit_id, or sha parameter required"}`)
	d.routes["/api/status"] = jsonResponse(http.StatusInternalServerError, `boom`)

	_, err := backend.GetReview(t.Context(), ReviewRef{JobID: 1})
	backendErr, ok := errors.AsType[*Error](err)
	require.True(ok, "expected *Error, got %T", err)
	assert.Equal(ErrorCodeNotFound, backendErr.Code)
	assert.Equal("review not found", backendErr.Message)

	_, err = backend.ListComments(t.Context(), CommentRef{})
	backendErr, ok = errors.AsType[*Error](err)
	require.True(ok)
	assert.Equal(ErrorCodeInvalidArgument, backendErr.Code)
	assert.Contains(backendErr.Message, "parameter required")

	_, err = backend.Status(t.Context())
	backendErr, ok = errors.AsType[*Error](err)
	require.True(ok)
	assert.Equal(ErrorCodeInternal, backendErr.Code)
	assert.Equal("boom", backendErr.Message)
}

func TestHTTPBackendReportsUnreachableDaemon(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	backend := NewHTTPBackend(srv.URL, srv.Client())

	_, err := backend.Status(t.Context())
	backendErr, ok := errors.AsType[*Error](err)
	require.True(t, ok, "expected *Error, got %T", err)
	assert.Equal(t, ErrorCodeUnavailable, backendErr.Code)
}

func TestHTTPBackendJobOutputAndBranches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d, backend := newFakeDaemon(t)
	d.routes["/api/job/output"] = jsonResponse(http.StatusOK,
		`{"job_id":3,"status":"running","lines":[{"ts":"2026-01-01T00:00:00Z","text":"hi","line_type":"text"}],"has_more":true}`)
	d.routes["/api/branches"] = jsonResponse(http.StatusOK, `{"branches":[{"name":"main","count":2}],"total_count":1}`)

	output, err := backend.GetJobOutput(t.Context(), 3)
	require.NoError(err)
	assert.Equal("running", output.Status)
	require.Len(output.Lines, 1)
	assert.Equal("hi", output.Lines[0].Text)
	assert.True(output.HasMore)
	assert.Equal("3", d.requests[0].Query().Get("job_id"))

	branches, err := backend.ListBranches(t.Context(), "/repo")
	require.NoError(err)
	require.Len(branches, 1)
	assert.Equal("main", branches[0].Name)
	assert.Equal("/repo", d.requests[1].Query().Get("repo"))
}

func TestHTTPBackendListCommentsByCommitID(t *testing.T) {
	d, backend := newFakeDaemon(t)
	d.routes["/api/comments"] = jsonResponse(http.StatusOK, `{"responses":[]}`)
	_, err := backend.ListComments(t.Context(), CommentRef{CommitID: 77, SHA: "ignored"})
	require.NoError(t, err)
	q := d.requests[0].Query()
	assert.Equal(t, "77", q.Get("commit_id"))
	assert.Empty(t, q.Get("sha"))
}

func TestHTTPBackendListJobsByIDAndGitRef(t *testing.T) {
	d, backend := newFakeDaemon(t)
	d.routes["/api/jobs"] = jsonResponse(http.StatusOK, `{"jobs":[],"has_more":false,"next_cursor":null}`)
	_, err := backend.ListJobs(t.Context(), JobsQuery{ID: 12})
	require.NoError(t, err)
	assert.Equal(t, "12", d.requests[0].Query().Get("id"))
	_, err = backend.ListJobs(t.Context(), JobsQuery{GitRef: "abc123", Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, "abc123", d.requests[1].Query().Get("git_ref"))
	assert.Equal(t, "1", d.requests[1].Query().Get("limit"))
}
