package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

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

func TestHTTPBackendSearchSerializesEveryFilterAndPreservesResponse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d, backend := newFakeDaemon(t)
	d.routes["/api/search"] = jsonResponse(http.StatusOK, `{
		"query":"quoted error","mode":"hybrid","degraded":true,
		"degraded_reason":"semantic search is unavailable","bounded":true,
		"bounded_reason":"semantic candidate ceiling reached","partial":true,
		"coverage":{"mirror_complete":false,"mirror_backlog":3,
			"embeddings_configured":true,"vector_state":"replacing",
			"embedding_backlog":4,"skipped":2},
		"hits":[{"job_id":11,"job_uuid":"job-uuid","review_id":12,
			"review_uuid":"review-uuid","repo_name":"repo","repo_path":"/src/repo",
			"git_ref":"abc123","commit_sha":"abc123","commit_subject":"fix search",
			"branch":"Feature/Search","review_type":"security","panel_role":"member",
			"agent":"test","verdict":"fail","closed":true,
			"finished_at":"2026-09-14T10:30:00Z","score":0.75,
			"matched_in":["lexical","semantic"],"excerpt":"matching excerpt"}]}`)

	got, err := backend.Search(t.Context(), SearchQuery{
		Query: "error: token=a&b + c", Mode: "hybrid", Repo: "/src/acme repo",
		Branch: "Feature/Search & Fix", Since: "2026-09-01T03:04:05+02:00",
		Verdict: "fail", State: "closed", Limit: 7,
	})
	require.NoError(err)
	require.Len(d.requests, 1)
	assert.Equal("branch=Feature%2FSearch+%26+Fix&limit=7&mode=hybrid&q=error%3A+token%3Da%26b+%2B+c&repo=%2Fsrc%2Facme+repo&since=2026-09-01T03%3A04%3A05%2B02%3A00&state=closed&verdict=fail",
		d.requests[0].RawQuery)

	backlog := int64(3)
	finished := time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC)
	assert.Equal(storage.SearchResponse{
		Query: "quoted error", Mode: "hybrid", Degraded: true,
		DegradedReason: "semantic search is unavailable", Bounded: true,
		BoundedReason: "semantic candidate ceiling reached", Partial: true,
		Coverage: storage.SearchCoverage{
			MirrorComplete: false, MirrorBacklog: &backlog, EmbeddingsConfigured: true,
			VectorState: "replacing", EmbeddingBacklog: 4, Skipped: 2,
		},
		Hits: []storage.SearchHit{{
			JobID: 11, JobUUID: "job-uuid", ReviewID: 12, ReviewUUID: "review-uuid",
			RepoName: "repo", RepoPath: "/src/repo", GitRef: "abc123", CommitSHA: "abc123",
			CommitSubject: "fix search", Branch: "Feature/Search", ReviewType: "security",
			PanelRole: "member", Agent: "test", Verdict: "fail", Closed: true,
			FinishedAt: finished, Score: 0.75, MatchedIn: []string{"lexical", "semantic"},
			Excerpt: "matching excerpt",
		}},
	}, got)
}

func TestHTTPBackendSearchReturnsNonNilEmptyHits(t *testing.T) {
	d, backend := newFakeDaemon(t)
	d.routes["/api/search"] = jsonResponse(http.StatusOK,
		`{"query":"absent","mode":"lexical","degraded":false,"bounded":false,"partial":false,
			"coverage":{"mirror_complete":true,"embeddings_configured":false,
				"vector_state":"disabled","embedding_backlog":0,"skipped":0},"hits":null}`)

	got, err := backend.Search(t.Context(), SearchQuery{Query: "absent"})
	require.NoError(t, err)
	assert.NotNil(t, got.Hits)
	assert.Empty(t, got.Hits)
}

func TestHTTPBackendSearchUsesStablePrivateErrors(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		mode     string
		wantCode string
		wantText string
	}{
		{
			name:     "invalid",
			status:   http.StatusBadRequest,
			body:     `{"detail":"bad query needle-secret"}`,
			mode:     "auto",
			wantCode: ErrorCodeInvalidArgument,
			wantText: "search request is invalid",
		},
		{
			name:     "explicit semantic unavailable",
			status:   http.StatusServiceUnavailable,
			body:     `{"detail":"semantic search is unavailable"}`,
			mode:     "semantic",
			wantCode: ErrorCodeUnavailable,
			wantText: "semantic search is unavailable",
		},
		{
			name:     "unconfigured semantic",
			status:   http.StatusBadRequest,
			body:     `{"detail":"embeddings are not configured"}`,
			mode:     "hybrid",
			wantCode: ErrorCodeInvalidArgument,
			wantText: "embeddings are not configured",
		},
		{
			name:     "provider failure",
			status:   http.StatusInternalServerError,
			body:     `{"detail":"https://provider.invalid provider-secret needle-secret"}`,
			mode:     "semantic",
			wantCode: ErrorCodeUnavailable,
			wantText: "review search is unavailable",
		},
		{
			name:     "daemon failure",
			status:   http.StatusBadGateway,
			body:     `https://daemon.invalid needle-secret`,
			mode:     "auto",
			wantCode: ErrorCodeUnavailable,
			wantText: "roborev daemon is unavailable",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, backend := newFakeDaemon(t)
			d.routes["/api/search"] = jsonResponse(tc.status, tc.body)
			_, err := backend.Search(t.Context(), SearchQuery{Query: "needle-secret", Mode: tc.mode})
			backendErr, ok := errors.AsType[*Error](err)
			require.True(t, ok, "expected *Error, got %T", err)
			assert.Equal(t, tc.wantCode, backendErr.Code)
			assert.Equal(t, tc.wantText, backendErr.Message)
			for _, secret := range []string{"needle-secret", "provider.invalid", "provider-secret", "daemon.invalid"} {
				assert.NotContains(t, err.Error(), secret)
			}
		})
	}
}

func TestHTTPBackendSearchRejectsMalformedAndEmptyResponsesPrivately(t *testing.T) {
	for _, body := range []string{
		"",
		`{}`,
		`{"query":"needle-secret"`,
		`{"query":"needle-secret","mode":"lexical"}`,
	} {
		t.Run(strings.ReplaceAll(body, "needle-secret", "malformed"), func(t *testing.T) {
			d, backend := newFakeDaemon(t)
			d.routes["/api/search"] = jsonResponse(http.StatusOK, body)
			_, err := backend.Search(t.Context(), SearchQuery{Query: "needle-secret"})
			backendErr, ok := errors.AsType[*Error](err)
			require.True(t, ok, "expected *Error, got %T", err)
			assert.Equal(t, ErrorCodeInternal, backendErr.Code)
			assert.Equal(t, "invalid response from roborev daemon", backendErr.Message)
			assert.NotContains(t, err.Error(), "needle-secret")
		})
	}
}

func TestHTTPBackendSearchRejectsTrailingJSONPrivately(t *testing.T) {
	d, backend := newFakeDaemon(t)
	d.routes["/api/search"] = jsonResponse(http.StatusOK,
		`{"query":"needle-secret","mode":"lexical","degraded":false,"bounded":false,"partial":false,`+
			`"coverage":{"mirror_complete":true,"embeddings_configured":false,"vector_state":"disabled",`+
			`"embedding_backlog":0,"skipped":0},"hits":[]} {"provider":"provider-secret"}`)

	_, err := backend.Search(t.Context(), SearchQuery{Query: "needle-secret"})
	backendErr, ok := errors.AsType[*Error](err)
	require.True(t, ok, "expected *Error, got %T", err)
	assert.Equal(t, ErrorCodeInternal, backendErr.Code)
	assert.Equal(t, "invalid response from roborev daemon", backendErr.Message)
	assert.NotContains(t, err.Error(), "needle-secret")
	assert.NotContains(t, err.Error(), "provider-secret")
}

func TestHTTPBackendSearchRequiresEveryTopLevelResponseMember(t *testing.T) {
	for _, member := range []string{"query", "mode", "degraded", "bounded", "partial", "coverage", "hits"} {
		t.Run(member, func(t *testing.T) {
			response := completeSearchResponseJSON()
			delete(response, member)
			assertHTTPBackendRejectsSearchResponsePrivately(t, response)
		})
	}
}

func TestHTTPBackendSearchRequiresEveryCoverageMember(t *testing.T) {
	for _, member := range []string{
		"mirror_complete", "embeddings_configured", "vector_state", "embedding_backlog", "skipped",
	} {
		t.Run(member, func(t *testing.T) {
			response := completeSearchResponseJSON()
			coverage := response["coverage"].(map[string]any)
			delete(coverage, member)
			assertHTTPBackendRejectsSearchResponsePrivately(t, response)
		})
	}
}

func TestHTTPBackendSearchRequiresEveryNonOptionalHitMember(t *testing.T) {
	for _, member := range []string{
		"job_id", "review_id", "repo_name", "repo_path", "git_ref", "review_type", "agent",
		"closed", "finished_at", "score", "matched_in", "excerpt",
	} {
		t.Run(member, func(t *testing.T) {
			response := completeSearchResponseJSON()
			hit := completeSearchHitJSON()
			delete(hit, member)
			response["hits"] = []any{hit}
			assertHTTPBackendRejectsSearchResponsePrivately(t, response)
		})
	}
}

func completeSearchResponseJSON() map[string]any {
	return map[string]any{
		"query": "needle-secret", "mode": "lexical",
		"degraded": false, "bounded": false, "partial": false,
		"coverage": map[string]any{
			"mirror_complete": true, "embeddings_configured": false,
			"vector_state": "disabled", "embedding_backlog": 0, "skipped": 0,
		},
		"hits": nil,
	}
}

func completeSearchHitJSON() map[string]any {
	return map[string]any{
		"job_id": 11, "review_id": 12, "repo_name": "repo", "repo_path": "/repo",
		"git_ref": "abc123", "review_type": "review", "agent": "codex", "closed": false,
		"finished_at": "2026-09-14T10:30:00Z", "score": 0, "matched_in": []any{}, "excerpt": "",
	}
}

func assertHTTPBackendRejectsSearchResponsePrivately(t *testing.T, response map[string]any) {
	t.Helper()
	body, err := json.Marshal(response)
	require.NoError(t, err)
	d, backend := newFakeDaemon(t)
	d.routes["/api/search"] = jsonResponse(http.StatusOK, string(body))
	_, err = backend.Search(t.Context(), SearchQuery{Query: "needle-secret"})
	backendErr, ok := errors.AsType[*Error](err)
	require.True(t, ok, "expected *Error, got %T", err)
	assert.Equal(t, ErrorCodeInternal, backendErr.Code)
	assert.Equal(t, "invalid response from roborev daemon", backendErr.Message)
	assert.NotContains(t, err.Error(), "needle-secret")
}

func TestHTTPBackendSearchTransportErrorDoesNotLeakURLOrQuery(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed for " + req.URL.String() + " provider-secret")
	})}
	backend := NewHTTPBackend("https://daemon.invalid/private", client)

	_, err := backend.Search(context.Background(), SearchQuery{Query: "needle-secret", Mode: "auto"})
	backendErr, ok := errors.AsType[*Error](err)
	require.True(t, ok, "expected *Error, got %T", err)
	assert.Equal(t, ErrorCodeUnavailable, backendErr.Code)
	assert.Equal(t, "roborev daemon is unavailable", backendErr.Message)
	assert.NotContains(t, err.Error(), "needle-secret")
	assert.NotContains(t, err.Error(), "daemon.invalid")
	assert.NotContains(t, err.Error(), "provider-secret")
}
