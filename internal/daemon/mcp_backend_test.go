package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/mcpserver"
	"go.kenn.io/roborev/internal/searchindex"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func newMCPTestServer(t *testing.T, enabled bool) (*Server, *storage.DB) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	cfg := config.DefaultConfig()
	cfg.MCP.Enabled = enabled
	server := newServerWithLogs(db, cfg, "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = server.Close() })
	return server, db
}

func seedCompletedReview(t *testing.T, db *storage.DB, repoPath string) *storage.ReviewJob {
	t.Helper()
	repo, err := db.GetOrCreateRepo(repoPath)
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "mcp-sha-1", "Author", "Subject", time.Now())
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID, GitRef: commit.SHA, Branch: "main", Agent: "test",
	})
	require.NoError(t, err)
	_, err = db.ClaimJob("mcp-worker")
	require.NoError(t, err)
	require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "test", "prompt text", "No issues found."))
	_, err = db.AddCommentToJob(job.ID, "dev", "thanks")
	require.NoError(t, err)
	_, err = db.AddComment(commit.ID, "dev", "legacy thanks")
	require.NoError(t, err)
	return job
}

func TestMCPEndpointIsOffByDefault(t *testing.T) {
	server, _ := newMCPTestServer(t, false)
	req := httptest.NewRequest(http.MethodPost, mcpserver.HTTPPath, nil)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestMCPEndpointServesInProcessBackend(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server, db := newMCPTestServer(t, true)
	server.browserRuntime = &BrowserRuntimeInfo{Origin: "https://reviews.example", WebBasePath: "/team"}
	repoPath := filepath.ToSlash(t.TempDir())
	job := seedCompletedReview(t, db, repoPath)

	httpSrv := httptest.NewServer(server.httpServer.Handler)
	t.Cleanup(httpSrv.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:             httpSrv.URL + mcpserver.HTTPPath,
		HTTPClient:           httpSrv.Client(),
		DisableStandaloneSSE: true,
	}, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = session.Close() })

	call := func(name string, args map[string]any) map[string]any {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
		require.NoError(err)
		require.False(result.IsError, "%s returned error: %v", name, result.Content)
		text, ok := result.Content[0].(*mcp.TextContent)
		require.True(ok)
		var out map[string]any
		require.NoError(json.Unmarshal([]byte(text.Text), &out))
		return out
	}

	status := call("roborev_status", nil)
	assert.EqualValues(1, status["completed_jobs"])

	repos := call("roborev_list_repos", nil)["repos"].([]any)
	require.Len(repos, 1)
	assert.Equal(repoPath, repos[0].(map[string]any)["root_path"])

	branches := call("roborev_list_branches", map[string]any{"repo_path": repoPath})["branches"].([]any)
	require.Len(branches, 1)
	assert.Equal("main", branches[0].(map[string]any)["name"])

	jobsOut := call("roborev_list_jobs", map[string]any{"repo_path": repoPath, "status": "done"})
	jobs := jobsOut["jobs"].([]any)
	require.Len(jobs, 1)
	row := jobs[0].(map[string]any)
	assert.Equal("https://reviews.example/team/reviews/1", row["web_url"])
	assert.EqualValues(job.ID, row["id"])
	assert.Equal("pass", row["verdict"])
	assert.Equal(false, row["closed"])

	review := call("roborev_get_review", map[string]any{"job_id": job.ID})
	assert.Equal("https://reviews.example/team/reviews/1", review["web_url"])
	assert.Contains(review["output"], "No issues found")
	assert.Equal("pass", review["verdict"])
	assert.NotContains(review, "prompt")

	comments := call("roborev_list_comments", map[string]any{"job_id": job.ID})["comments"].([]any)
	require.Len(comments, 2, "job-linked and legacy commit-linked comments are merged")
	assert.Equal("thanks", comments[0].(map[string]any)["response"])
	assert.Equal("legacy thanks", comments[1].(map[string]any)["response"])

	output := call("roborev_get_job_output", map[string]any{"job_id": job.ID})
	assert.Equal("done", output["status"])
	assert.Equal(false, output["has_more"])

	missing, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "roborev_get_review", Arguments: map[string]any{"job_id": 9999},
	})
	require.NoError(err)
	require.True(missing.IsError)
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(json.Unmarshal([]byte(missing.Content[0].(*mcp.TextContent).Text), &failure))
	assert.Equal(mcpserver.ErrorCodeNotFound, failure.Error.Code)
}

func TestMCPBackendListJobsMatchesHTTPDefaults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server, db := newMCPTestServer(t, true)
	seedCompletedReview(t, db, t.TempDir())

	page, err := server.mcpBackend().ListJobs(t.Context(), mcpserver.JobsQuery{})
	require.NoError(err)
	require.Len(page.Jobs, 1)
	assert.Empty(page.Jobs[0].Prompt, "list rows omit prompts")
	assert.False(page.HasMore)

	page, err = server.mcpBackend().ListJobs(t.Context(), mcpserver.JobsQuery{Limit: 1, Branch: "other"})
	require.NoError(err)
	assert.Empty(page.Jobs)
}

func TestPingAdvertisesMCPURLOnlyForEnabledTCPListeners(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tcp := DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"}
	unix := DaemonEndpoint{Network: "unix", Address: "/tmp/roborev.sock"}
	assert.Equal("http://127.0.0.1:7373/mcp", mcpURLForEndpoint(true, tcp))
	assert.Empty(mcpURLForEndpoint(false, tcp))
	assert.Empty(mcpURLForEndpoint(true, unix))
	assert.Empty(mcpURLForEndpoint(true, DaemonEndpoint{}))

	server, _ := newMCPTestServer(t, true)
	server.endpointMu.Lock()
	server.endpoint = tcp
	server.endpointMu.Unlock()
	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	require.Equal(http.StatusOK, w.Code)
	var ping PingInfo
	require.NoError(json.Unmarshal(w.Body.Bytes(), &ping))
	assert.Equal("http://127.0.0.1:7373/mcp", ping.MCPURL)

	disabled, _ := newMCPTestServer(t, false)
	disabled.endpointMu.Lock()
	disabled.endpoint = tcp
	disabled.endpointMu.Unlock()
	w = httptest.NewRecorder()
	disabled.httpServer.Handler.ServeHTTP(w, req)
	require.Equal(http.StatusOK, w.Code)
	assert.NotContains(w.Body.String(), "mcp_url")
}

func TestProbeDaemonPingKeepsMCPURL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server, _ := newMCPTestServer(t, true)
	httpSrv := httptest.NewServer(server.httpServer.Handler)
	t.Cleanup(httpSrv.Close)
	ep, err := ParseEndpoint(strings.TrimPrefix(httpSrv.URL, "http://"))
	require.NoError(err)
	server.endpointMu.Lock()
	server.endpoint = ep
	server.endpointMu.Unlock()

	ping, err := ProbeDaemonPing(ep, 2*time.Second)
	require.NoError(err)
	assert.Equal(daemonServiceName, ping.Service)
	assert.Equal(os.Getpid(), ping.PID)
	assert.Equal(httpSrv.URL+mcpserver.HTTPPath, ping.MCPURL)

	_, err = ProbeDaemonPing(DaemonEndpoint{Network: "tcp", Address: "10.0.0.1:7373"}, time.Second)
	assert.Error(err, "non-loopback addresses are refused")
}

func TestMCPBackendCommentsByCommitID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server, db := newMCPTestServer(t, true)
	job := seedCompletedReview(t, db, t.TempDir())
	full, err := db.GetJobByID(job.ID)
	require.NoError(err)
	require.NotNil(full.CommitID)

	comments, err := server.mcpBackend().ListComments(t.Context(), mcpserver.CommentRef{CommitID: *full.CommitID})
	require.NoError(err)
	require.Len(comments, 1, "only the legacy commit-linked comment from the seed")
	assert.Equal("legacy thanks", comments[0].Response)
}

func TestMCPBackendSearchCallsSharedServiceWithExactParameters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server, db := newMCPTestServer(t, true)
	repo, err := db.GetOrCreateRepo("/src/example", "github.com/example/repo")
	require.NoError(err)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	server.searchNow = func() time.Time { return now }
	searcher := &recordingReviewSearcher{result: searchindex.SearchResult{
		Query: "needle", Mode: searchindex.ModeHybrid,
		Coverage: searchindex.SearchCoverage{MirrorComplete: true, VectorState: searchindex.VectorActive},
		Hits:     []searchindex.SearchHit{{JobID: 11, ReviewID: 12, RepoName: "repo", RepoPath: "/src/example"}},
	}}
	server.search = searcher

	got, err := server.mcpBackend().Search(t.Context(), mcpserver.SearchQuery{
		Query: "needle", Mode: "hybrid", Repo: repo.Identity, Branch: "Feature/Search",
		Since: "24h", Verdict: "fail", State: "open", Limit: 7,
	})
	require.NoError(err)
	params := searcher.lastParams(t)
	assert.Equal("needle", params.Query)
	assert.Equal(searchindex.ModeHybrid, params.Mode)
	assert.Equal(repo.ID, params.RepoID)
	assert.Equal("Feature/Search", params.Branch)
	require.NotNil(params.Since)
	assert.Equal(now.Add(-24*time.Hour), *params.Since)
	assert.Equal("fail", params.Verdict)
	assert.Equal(searchindex.StateOpen, params.State)
	assert.Equal(7, params.Limit)
	assert.Equal(searchResponseFromResult(searcher.result), got)
}

func TestMCPBackendSearchClassifiesRepoResolutionErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server, db := newMCPTestServer(t, true)
	server.search = &recordingReviewSearcher{}

	call := func(repo string) (*mcpserver.Error, error) {
		t.Helper()
		_, err := server.mcpBackend().Search(t.Context(), mcpserver.SearchQuery{
			Query: "needle", Repo: repo, Limit: 20,
		})
		require.Error(err)
		backendErr, ok := errors.AsType[*mcpserver.Error](err)
		require.True(ok, "expected *mcpserver.Error, got %T", err)
		return backendErr, err
	}

	// Unknown repositories keep the stable invalid-argument message.
	backendErr, _ := call("missing")
	assert.Equal(mcpserver.ErrorCodeInvalidArgument, backendErr.Code)
	assert.Equal("repository not found", backendErr.Message)

	// Repository lookup failures keep the generic private message and never
	// surface database details to the client.
	require.NoError(db.Close())
	backendErr, err := call("/src/example")
	assert.Equal(mcpserver.ErrorCodeInvalidArgument, backendErr.Code)
	assert.Equal("search request is invalid", backendErr.Message)
	assert.NotContains(err.Error(), "sql: database is closed")
}

func TestMCPBackendSearchReturnsStablePrivateErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantCode string
		wantText string
	}{
		{
			name: "embeddings unconfigured",
			err: &searchindex.ModeError{
				Status: http.StatusBadRequest, Reason: searchindex.ReasonEmbeddingsUnconfigured,
			},
			wantCode: mcpserver.ErrorCodeInvalidArgument,
			wantText: "embeddings are not configured",
		},
		{
			name: "mode unavailable",
			err: &searchindex.ModeError{
				Status: http.StatusServiceUnavailable, Reason: searchindex.ReasonSemanticUnavailable,
				Err: errors.New("provider-secret"),
			},
			wantCode: mcpserver.ErrorCodeUnavailable,
			wantText: "semantic search is unavailable",
		},
		{
			name:     "runtime",
			err:      errors.New("query needle-secret failed at https://provider.invalid"),
			wantCode: mcpserver.ErrorCodeInternal,
			wantText: "review search failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newMCPTestServer(t, true)
			server.search = &recordingReviewSearcher{err: tc.err}
			_, err := server.mcpBackend().Search(t.Context(), mcpserver.SearchQuery{
				Query: "needle-secret", Mode: "semantic", State: "all", Limit: 20,
			})
			backendErr, ok := errors.AsType[*mcpserver.Error](err)
			require.True(t, ok, "expected *mcpserver.Error, got %T", err)
			assert.Equal(t, tc.wantCode, backendErr.Code)
			assert.Equal(t, tc.wantText, backendErr.Message)
			assert.NotContains(t, err.Error(), "needle-secret")
			assert.NotContains(t, err.Error(), "provider.invalid")
			assert.NotContains(t, err.Error(), "provider-secret")
		})
	}
}

func TestReviewBrowserURLsInAPIResponses(t *testing.T) {
	server, db := newMCPTestServer(t, false)
	job := seedCompletedReview(t, db, filepath.ToSlash(t.TempDir()))
	require.EqualValues(t, 1, job.ID)
	for _, tc := range []struct {
		name    string
		runtime *BrowserRuntimeInfo
		want    string
	}{
		{name: "disabled"},
		{name: "local", runtime: &BrowserRuntimeInfo{Origin: "http://127.0.0.1:7400"}, want: "http://127.0.0.1:7400/reviews/1"},
		{name: "mounted", runtime: &BrowserRuntimeInfo{Origin: "https://reviews.example", WebBasePath: "/team"}, want: "https://reviews.example/team/reviews/1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server.browserRuntime = tc.runtime
			for _, path := range []string{"/api/jobs", "/api/jobs?id=1", "/api/review?job_id=1"} {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				w := httptest.NewRecorder()
				server.httpServer.Handler.ServeHTTP(w, req)
				require.Equal(t, http.StatusOK, w.Code)
				var out map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
				if jobs, ok := out["jobs"].([]any); ok {
					require.Len(t, jobs, 1)
					out = jobs[0].(map[string]any)
				}
				if tc.want == "" {
					assert.NotContains(t, out, "web_url")
				} else {
					assert.Equal(t, tc.want, out["web_url"])
				}
			}
		})
	}
}

func TestMCPWriteToolsPersistThroughBothBackends(t *testing.T) {
	for _, transport := range []string{"http", "stdio-backend"} {
		t.Run(transport, func(t *testing.T) {
			assert := assert.New(t)
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			id := uuid.MustParse("00000000-0000-4000-8000-000000000001")
			state, err := json.Marshal(agenthook.Snapshot{FixSessions: map[string]agenthook.FixSession{
				"worktree": {ID: id, Agent: "codex", SessionID: "session", ExpiresAt: time.Now().Add(time.Hour)},
			}})
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Dir(agenthook.StatePath()), 0o700))
			require.NoError(t, os.WriteFile(agenthook.StatePath(), state, 0o600))
			server, db := newMCPTestServer(t, true)
			repo := filepath.ToSlash(t.TempDir())
			job := seedCompletedReview(t, db, repo)
			api := httptest.NewServer(server.httpServer.Handler)
			t.Cleanup(api.Close)
			endpoint := api.URL + mcpserver.HTTPPath
			if transport == "stdio-backend" {
				bridge := mcpserver.New(mcpserver.NewHTTPBackend(api.URL, api.Client()), "test")
				httpBridge := httptest.NewServer(bridge.HTTPHandler())
				t.Cleanup(httpBridge.Close)
				endpoint = httpBridge.URL + mcpserver.HTTPPath
			}
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
			session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: endpoint, DisableStandaloneSSE: true}, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = session.Close() })
			call := func(name string, args map[string]any) {
				result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
				require.NoError(t, err)
				require.False(t, result.IsError, "%s: %v", name, result.Content[0].(*mcp.TextContent).Text)
			}
			call("roborev_add_comment", map[string]any{"job_id": job.ID, "commenter": "agent", "comment": "Verified the fix."})
			comments, err := db.GetCommentsForJob(job.ID)
			require.NoError(t, err)
			require.NotEmpty(t, comments)
			assert.Equal("Verified the fix.", comments[len(comments)-1].Response)
			call("roborev_close_review", map[string]any{"job_id": job.ID})
			review, err := db.GetReviewByJobID(job.ID)
			require.NoError(t, err)
			assert.True(review.Closed)
			until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
			call("roborev_snooze", map[string]any{"repo_path": repo, "worktree_path": repo, "branch": "main", "enabled": true, "snoozed_until": until.Format(time.RFC3339)})
			snoozes, err := db.ListActiveAgentHookSnoozes(time.Now())
			require.NoError(t, err)
			require.Len(t, snoozes, 1)
			assert.Equal(until, snoozes[0].SnoozedUntil)
			call("roborev_snooze", map[string]any{"repo_path": repo, "worktree_path": repo, "branch": "main", "enabled": false})
			snoozes, err = db.ListActiveAgentHookSnoozes(time.Now())
			require.NoError(t, err)
			assert.Empty(snoozes)
			call("roborev_complete_fix", map[string]any{"fix_session_id": id.String()})
			persisted, err := os.ReadFile(agenthook.StatePath())
			require.NoError(t, err)
			var snapshot agenthook.Snapshot
			require.NoError(t, json.Unmarshal(persisted, &snapshot))
			assert.Empty(snapshot.FixSessions)
		})
	}
}
