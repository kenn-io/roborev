package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func newBrowserHandlerFixture(t *testing.T, authToken string) (http.Handler, *BrowserSessionManager) {
	t.Helper()
	core := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"path":"` + request.URL.Path + `"}`))
	})
	return newBrowserHandlerFixtureWithCore(t, authToken, core)
}

func newBrowserHandlerFixtureWithCore(
	t *testing.T, authToken string, core http.Handler,
) (http.Handler, *BrowserSessionManager) {
	return newBrowserHandlerFixtureWithCoreAndTTL(t, authToken, core, 0)
}

func newBrowserHandlerFixtureWithCoreAndTTL(
	t *testing.T, authToken string, core http.Handler, ttl time.Duration,
) (http.Handler, *BrowserSessionManager) {
	t.Helper()
	policy, err := NewBrowserPolicy(BrowserEndpoint{
		Address:           "127.0.0.1:7374",
		Origin:            "http://127.0.0.1:7374",
		Enabled:           true,
		remoteAuthEnabled: authToken != "",
	}, "")
	require.NoError(t, err)
	sessions, err := NewBrowserSessionManager(BrowserSessionConfig{
		Origin:     "http://127.0.0.1:7374",
		AuthToken:  authToken,
		AllowLocal: authToken == "",
		TTL:        ttl,
		Entropy:    rand.Reader,
		Clock:      time.Now,
	})
	require.NoError(t, err)
	static := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("shell"))
	})
	server := &Server{}
	handler, err := server.newBrowserHandler(core, static, policy, sessions)
	require.NoError(t, err)
	return handler, sessions
}

func authenticatedBrowserRequest(
	t *testing.T, sessions *BrowserSessionManager, method, path string,
) *http.Request {
	t.Helper()
	credentials, err := sessions.Login(testBrowserAuthToken)
	require.NoError(t, err)
	request := browserRequest(method, path, nil)
	request.AddCookie(sessions.Cookie(credentials.Ambient))
	request.Header.Set(WebSessionHeader, credentials.Tab)
	return request
}

func authenticatedBrowserMutationRequest(
	t *testing.T, sessions *BrowserSessionManager, path string, body any,
) *http.Request {
	t.Helper()
	credentials, err := sessions.Login(testBrowserAuthToken)
	require.NoError(t, err)
	request := browserRequest(http.MethodPost, path, body)
	request.AddCookie(sessions.Cookie(credentials.Ambient))
	request.Header.Set(WebSessionHeader, credentials.Tab)
	request.Header.Set(WebCSRFHeader, credentials.CSRF)
	return request
}

func browserRequest(method, path string, body any) *http.Request {
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	request := httptest.NewRequest(method, "http://127.0.0.1:7374"+path, bytes.NewReader(data))
	request.Host = "127.0.0.1:7374"
	request.RemoteAddr = "127.0.0.1:50000"
	request.Header.Set("Origin", "http://127.0.0.1:7374")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func TestBrowserHandlerHostRunsBeforeAuthentication(t *testing.T) {
	handler, _ := newBrowserHandlerFixture(t, testBrowserAuthToken)
	request := browserRequest(http.MethodGet, "/api/status", nil)
	request.Host = "attacker.test"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.JSONEq(t, `{"error":"invalid_host"}`, recorder.Body.String())
}

func TestBrowserHandlerLoginBootstrapAndAuthenticatedRoutes(t *testing.T) {
	handler, sessions := newBrowserHandlerFixture(t, testBrowserAuthToken)
	login := browserRequest(http.MethodPost, "/api/ui/session/login", WebLoginRequest{Token: testBrowserAuthToken})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, login)
	require.Equal(t, http.StatusOK, recorder.Code)
	var credentials WebSessionCredentials
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &credentials))
	assert.NotEmpty(t, credentials.Session)
	assert.NotEmpty(t, credentials.CSRF)
	var loginEnvelope struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &loginEnvelope))
	assert.Equal(t, map[string]bool{
		"cancel_any_job":    false,
		"cancel_review_job": true,
		"rerun_job":         false,
	}, loginEnvelope.Capabilities)
	cookies := recorder.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.Equal(t, sessions.CookieName(), cookies[0].Name)

	status := browserRequest(http.MethodGet, "/api/status", nil)
	status.AddCookie(cookies[0])
	status.Header.Set(WebSessionHeader, credentials.Session)
	statusRecorder := httptest.NewRecorder()
	handler.ServeHTTP(statusRecorder, status)
	assert.Equal(t, http.StatusOK, statusRecorder.Code)
	assert.Equal(t, "private, no-store", statusRecorder.Header().Get("Cache-Control"))

	analytics := browserRequest(http.MethodGet, "/api/ui/analytics", nil)
	unauthorizedAnalytics := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedAnalytics, analytics)
	assert.Equal(t, http.StatusUnauthorized, unauthorizedAnalytics.Code)
	analytics.AddCookie(cookies[0])
	analytics.Header.Set(WebSessionHeader, credentials.Session)
	authorizedAnalytics := httptest.NewRecorder()
	handler.ServeHTTP(authorizedAnalytics, analytics)
	assert.Equal(t, http.StatusOK, authorizedAnalytics.Code)

	bootstrap := browserRequest(http.MethodPost, "/api/ui/session/bootstrap", map[string]any{})
	bootstrap.AddCookie(cookies[0])
	bootstrap.Header.Set("Sec-Fetch-Site", "same-origin")
	bootstrap.Header.Set("Sec-Fetch-Mode", "cors")
	bootstrap.Header.Set("Sec-Fetch-Dest", "empty")
	bootstrapRecorder := httptest.NewRecorder()
	handler.ServeHTTP(bootstrapRecorder, bootstrap)
	assert.Equal(t, http.StatusOK, bootstrapRecorder.Code)
	assert.Empty(t, bootstrapRecorder.Header().Values("Set-Cookie"))
}

func TestBrowserHandlerOmitsInternalJobMetadata(t *testing.T) {
	const secret = "SENTINEL_BROWSER_SECRET"
	core := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/jobs":
			_, _ = w.Write([]byte(`{"jobs":[{"id":7,"repo_id":2,"git_ref":"HEAD","agent":"test","job_type":"review","status":"done","enqueued_at":"2026-01-02T03:04:05Z","retry_count":0,"agentic":false,"prompt_prebuilt":false,"repo_name":"example","command_line":"agent --token ` + secret + `","session_id":"` + secret + `","panel_member_config_json":"{\"token\":\"` + secret + `\"}","worker_id":"` + secret + `","worktree_path":"/tmp/` + secret + `"}],"has_more":false,"next_cursor":null}`))
		case "/api/review":
			_, _ = w.Write([]byte(`{"id":3,"job_id":7,"agent":"test","prompt":"review","output":"P","created_at":"2026-01-02T03:05:05Z","closed":false,"job":{"id":7,"repo_id":2,"git_ref":"HEAD","agent":"test","job_type":"review","status":"done","enqueued_at":"2026-01-02T03:04:05Z","retry_count":0,"agentic":false,"prompt_prebuilt":false,"command_line":"agent --token ` + secret + `","session_id":"` + secret + `","panel_member_config_json":"{\"token\":\"` + secret + `\"}"}}`))
		default:
			http.NotFound(w, request)
		}
	})
	handler, sessions := newBrowserHandlerFixtureWithCore(t, testBrowserAuthToken, core)

	for _, path := range []string{"/api/jobs", "/api/review"} {
		request := authenticatedBrowserRequest(t, sessions, http.MethodGet, path)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		require.Equal(t, http.StatusOK, recorder.Code, path)
		assert.NotContains(t, recorder.Body.String(), secret, path)
		assert.NotContains(t, recorder.Body.String(), "command_line", path)
		assert.NotContains(t, recorder.Body.String(), "session_id", path)
		assert.NotContains(t, recorder.Body.String(), "panel_member_config_json", path)
	}
}

func TestBrowserHandlerRejectsRawJobLogs(t *testing.T) {
	const secret = "SENTINEL_RAW_LOG_SESSION"
	coreCalled := false
	core := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		coreCalled = true
		_, _ = w.Write([]byte(`{"type":"thread.started","thread_id":"` + secret + `"}`))
	})
	handler, sessions := newBrowserHandlerFixtureWithCore(t, testBrowserAuthToken, core)
	request := authenticatedBrowserRequest(
		t, sessions, http.MethodGet, "/api/job/log?job_id=7",
	)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	assert.Equal(t, http.StatusNotFound, recorder.Code)
	assert.False(t, coreCalled)
	assert.NotContains(t, recorder.Body.String(), secret)
}

func TestBrowserHandlerProjectsSafeTokenUsage(t *testing.T) {
	const secret = "SENTINEL_TOKEN_USAGE_SESSION"
	unsafeUsage := `{"input_tokens":231582,"cached_input_tokens":189952,"cache_creation_tokens":1200,"total_output_tokens":2542,"peak_context_tokens":47248,"cost_usd":0.347212,"has_cost":true,"thread_id":"` + secret + `","metadata":{"session_id":"` + secret + `"}}`
	core := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jobs": []map[string]any{{
				"id": 7, "repo_id": 2, "git_ref": "HEAD", "agent": "test",
				"job_type": "review", "status": "done",
				"enqueued_at": "2026-01-02T03:04:05Z", "retry_count": 0,
				"agentic": false, "prompt_prebuilt": false, "token_usage": unsafeUsage,
			}},
			"has_more": false, "next_cursor": nil,
		})
	})
	handler, sessions := newBrowserHandlerFixtureWithCore(t, testBrowserAuthToken, core)
	request := authenticatedBrowserRequest(t, sessions, http.MethodGet, "/api/jobs")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), secret)
	var response struct {
		Jobs []struct {
			TokenUsage string `json:"token_usage"`
		} `json:"jobs"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Len(t, response.Jobs, 1)
	assert.JSONEq(t, `{
		"input_tokens": 231582,
		"cached_input_tokens": 189952,
		"cache_creation_tokens": 1200,
		"total_output_tokens": 2542,
		"peak_context_tokens": 47248,
		"cost_usd": 0.347212,
		"has_cost": true
	}`, response.Jobs[0].TokenUsage)
}

func TestBrowserHandlerRateLimitsRepeatedLoginAttempts(t *testing.T) {
	handler, _ := newBrowserHandlerFixture(t, testBrowserAuthToken)

	first := browserRequest(http.MethodPost, "/api/ui/session/login", WebLoginRequest{Token: "wrong"})
	firstResponse := httptest.NewRecorder()
	handler.ServeHTTP(firstResponse, first)
	require.Equal(t, http.StatusUnauthorized, firstResponse.Code)

	second := browserRequest(http.MethodPost, "/api/ui/session/login", WebLoginRequest{Token: "wrong"})
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, second)
	assert.Equal(t, http.StatusTooManyRequests, secondResponse.Code)
	assert.Equal(t, "1", secondResponse.Header().Get("Retry-After"))
}

func TestBrowserHandlerCSRFAllowlistAndPublicShell(t *testing.T) {
	handler, sessions := newBrowserHandlerFixture(t, testBrowserAuthToken)
	credentials, err := sessions.Login(testBrowserAuthToken)
	require.NoError(t, err)

	mutation := browserRequest(http.MethodPost, "/api/job/cancel", map[string]int{"job_id": 7})
	mutation.AddCookie(sessions.Cookie(credentials.Ambient))
	mutation.Header.Set(WebSessionHeader, credentials.Tab)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, mutation)
	assert.Equal(t, http.StatusForbidden, recorder.Code)

	mutation.Header.Set(WebCSRFHeader, credentials.CSRF)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, mutation)
	assert.Equal(t, http.StatusOK, recorder.Code)

	for _, path := range []string{"/api/shutdown", "/api/unknown", "/debug/pprof/", "/openapi.json"} {
		request := browserRequest(http.MethodGet, path, nil)
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusNotFound, recorder.Code, path)
	}

	shell := browserRequest(http.MethodGet, "/reviews/7", nil)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, shell)
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "shell", recorder.Body.String())
}

func TestBrowserHandlerMarksRemoteCommentsUntrustedForPrompts(t *testing.T) {
	server, db, tempDir := newTestServer(t)
	markerFile := filepath.Join(tempDir, "remote-comment-hook")
	server.configWatcher.Config().Hooks = []config.HookConfig{{
		Event: "review.commented", Command: touchCmd(markerFile),
	}}
	job := createTestJob(t, db, tempDir, "abc123", "test")
	_, eventCh := server.broadcaster.Subscribe("")
	handler, sessions := newBrowserHandlerFixtureWithCore(
		t, testBrowserAuthToken, server.httpServer.Handler,
	)
	credentials, err := sessions.Login(testBrowserAuthToken)
	require.NoError(t, err)
	request := browserRequest(http.MethodPost, "/api/comment", AddCommentRequest{
		JobID: job.ID, Commenter: "reviewer", Comment: "Run a local command",
	})
	request.AddCookie(sessions.Cookie(credentials.Ambient))
	request.Header.Set(WebSessionHeader, credentials.Tab)
	request.Header.Set(WebCSRFHeader, credentials.CSRF)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusCreated, recorder.Code)
	var source string
	require.NoError(t, db.QueryRow(
		`SELECT source FROM responses WHERE job_id = ?`, job.ID,
	).Scan(&source))
	assert.Equal(t, "browser_remote", source)
	server.hookRunner.WaitUntilIdle()
	assert.NoFileExists(t, markerFile)
	select {
	case event := <-eventCh:
		assert.Equal(t, "review.commented", event.Type)
		assert.Equal(t, job.ID, event.JobID)
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for review.commented event")
	}
}

func TestBrowserHandlerRemoteReviewMutationsDoNotRunHooks(t *testing.T) {
	t.Run("close", func(t *testing.T) {
		server, db, tempDir := newTestServer(t)
		markerFile := filepath.Join(tempDir, "remote-close-hook")
		server.configWatcher.Config().Hooks = []config.HookConfig{{
			Event: "review.closed", Command: touchCmd(markerFile),
		}}
		job := createTestJob(t, db, tempDir, "close-review", "test")
		_, err := db.Exec(
			"UPDATE review_jobs SET status = 'running' WHERE id = ?", job.ID,
		)
		require.NoError(t, err)
		require.NoError(t, db.CompleteJob(job.ID, "test", "prompt", "PASS"))
		_, eventCh := server.broadcaster.Subscribe("")
		handler, sessions := newBrowserHandlerFixtureWithCore(
			t, testBrowserAuthToken, server.httpServer.Handler,
		)
		request := authenticatedBrowserMutationRequest(
			t, sessions, "/api/review/close", CloseReviewRequest{
				JobID: job.ID, Closed: true,
			},
		)
		recorder := httptest.NewRecorder()

		handler.ServeHTTP(recorder, request)

		require.Equal(t, http.StatusOK, recorder.Code)
		server.hookRunner.WaitUntilIdle()
		assert.NoFileExists(t, markerFile)
		require.Len(t, eventCh, 1)
		event := <-eventCh
		assert.Equal(t, "review.closed", event.Type)
		assert.Equal(t, job.ID, event.JobID)
	})

	t.Run("cancel", func(t *testing.T) {
		server, db, tempDir := newTestServer(t)
		markerFile := filepath.Join(tempDir, "remote-cancel-hook")
		server.configWatcher.Config().Hooks = []config.HookConfig{{
			Event: "review.canceled", Command: touchCmd(markerFile),
		}}
		job := createTestJob(t, db, tempDir, "cancel-review", "test")
		_, eventCh := server.broadcaster.Subscribe("")
		handler, sessions := newBrowserHandlerFixtureWithCore(
			t, testBrowserAuthToken, server.httpServer.Handler,
		)
		request := authenticatedBrowserMutationRequest(
			t, sessions, "/api/job/cancel", CancelJobRequest{JobID: job.ID},
		)
		recorder := httptest.NewRecorder()

		handler.ServeHTTP(recorder, request)

		require.Equal(t, http.StatusOK, recorder.Code)
		server.hookRunner.WaitUntilIdle()
		assert.NoFileExists(t, markerFile)
		require.Len(t, eventCh, 1)
		event := <-eventCh
		assert.Equal(t, "review.canceled", event.Type)
		assert.Equal(t, job.ID, event.JobID)
	})

	t.Run("cancel running", func(t *testing.T) {
		server, db, tempDir := newTestServer(t)
		testutil.InitTestGitRepo(t, tempDir)
		markerFile := filepath.Join(tempDir, "remote-running-cancel-hook")
		server.configWatcher.Config().Hooks = []config.HookConfig{{
			Event: "review.canceled", Command: touchCmd(markerFile),
		}}
		started := make(chan struct{})
		finished := make(chan struct{})
		const agentName = "remote-cancel-blocking"
		agent.Register(&agent.FakeAgent{
			NameStr: agentName,
			ReviewFn: func(ctx context.Context, _, _, _ string, _ io.Writer) (string, error) {
				close(started)
				<-ctx.Done()
				return "", ctx.Err()
			},
		})
		t.Cleanup(func() { agent.Unregister(agentName) })

		job := createTestJob(
			t, db, tempDir, testutil.GetHeadSHA(t, tempDir), agentName,
		)
		claimed, err := db.ClaimJob("remote-browser-worker")
		require.NoError(t, err)
		require.Equal(t, job.ID, claimed.ID)
		go func() {
			defer close(finished)
			server.workerPool.processJob("remote-browser-worker", claimed)
		}()
		t.Cleanup(func() {
			server.workerPool.CancelJob(job.ID)
			<-finished
		})
		require.Eventually(t, func() bool {
			select {
			case <-started:
				return true
			default:
				return false
			}
		}, 5*time.Second, 10*time.Millisecond)

		handler, sessions := newBrowserHandlerFixtureWithCore(
			t, testBrowserAuthToken, server.httpServer.Handler,
		)
		request := authenticatedBrowserMutationRequest(
			t, sessions, "/api/job/cancel", CancelJobRequest{JobID: job.ID},
		)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		require.Equal(t, http.StatusOK, recorder.Code)
		require.Eventually(t, func() bool {
			select {
			case <-finished:
				return true
			default:
				return false
			}
		}, 5*time.Second, 10*time.Millisecond)
		server.hookRunner.WaitUntilIdle()
		assert.NoFileExists(t, markerFile)
	})
}

func TestBrowserHandlerRemoteSessionRestrictsPrivilegedJobMutations(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		jobType    string
		agentic    bool
		prebuilt   bool
		source     string
		ciBase     string
		startState storage.JobStatus
		wantStatus int
		wantState  storage.JobStatus
	}{
		{
			name: "cancel stored-prompt task", path: "/api/job/cancel",
			jobType: storage.JobTypeTask, startState: storage.JobStatusQueued,
			wantStatus: http.StatusForbidden, wantState: storage.JobStatusQueued,
		},
		{
			name: "cancel agentic review", path: "/api/job/cancel",
			jobType: storage.JobTypeReview, agentic: true,
			startState: storage.JobStatusQueued,
			wantStatus: http.StatusForbidden, wantState: storage.JobStatusQueued,
		},
		{
			name: "cancel CI review", path: "/api/job/cancel",
			jobType: storage.JobTypeReview, source: storage.JobSourceCI,
			startState: storage.JobStatusQueued,
			wantStatus: http.StatusForbidden, wantState: storage.JobStatusQueued,
		},
		{
			name: "cancel legacy CI review", path: "/api/job/cancel",
			jobType: storage.JobTypeReview, ciBase: "main",
			startState: storage.JobStatusQueued,
			wantStatus: http.StatusForbidden, wantState: storage.JobStatusQueued,
		},
		{
			name: "rerun stored-prompt task", path: "/api/job/rerun",
			jobType: storage.JobTypeTask, startState: storage.JobStatusDone,
			wantStatus: http.StatusForbidden, wantState: storage.JobStatusDone,
		},
		{
			name: "rerun agentic review", path: "/api/job/rerun",
			jobType: storage.JobTypeReview, agentic: true,
			startState: storage.JobStatusDone,
			wantStatus: http.StatusForbidden, wantState: storage.JobStatusDone,
		},
		{
			name: "rerun prebuilt review prompt", path: "/api/job/rerun",
			jobType: storage.JobTypeReview, prebuilt: true,
			startState: storage.JobStatusDone,
			wantStatus: http.StatusForbidden, wantState: storage.JobStatusDone,
		},
		{
			name: "cancel ordinary review", path: "/api/job/cancel",
			jobType: storage.JobTypeReview, startState: storage.JobStatusQueued,
			wantStatus: http.StatusOK, wantState: storage.JobStatusCanceled,
		},
		{
			name: "rerun ordinary review", path: "/api/job/rerun",
			jobType: storage.JobTypeReview, startState: storage.JobStatusDone,
			wantStatus: http.StatusForbidden, wantState: storage.JobStatusDone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, db, tempDir := newTestServer(t)
			repo, err := db.GetOrCreateRepo(tempDir)
			require.NoError(t, err)
			commit, err := db.GetOrCreateCommit(
				repo.ID, "abc123", "Author", "Subject", time.Now(),
			)
			require.NoError(t, err)
			job, err := db.EnqueueJob(storage.EnqueueOpts{
				RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123",
				Agent: "test", JobType: tt.jobType, Agentic: tt.agentic,
				Prompt: "stored instruction", PromptPrebuilt: tt.prebuilt,
				Source: tt.source, CIBaseBranch: tt.ciBase,
			})
			require.NoError(t, err)
			if tt.startState != storage.JobStatusQueued {
				setJobStatus(t, db, job.ID, tt.startState)
			}

			handler, sessions := newBrowserHandlerFixtureWithCore(
				t, testBrowserAuthToken, server.httpServer.Handler,
			)
			body := map[string]any{"job_id": job.ID}
			if tt.path == "/api/job/rerun" {
				body["request_id"] = "browser-request"
			}
			request := authenticatedBrowserMutationRequest(t, sessions, tt.path, body)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, request)

			assert.Equal(t, tt.wantStatus, recorder.Code, recorder.Body.String())
			updated, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, tt.wantState, updated.Status)
		})
	}
}

func TestBrowserHandlerRemoteSessionRejectsPanelCancellation(t *testing.T) {
	for _, target := range []string{"member", "synthesis"} {
		t.Run(target, func(t *testing.T) {
			server, db, tempDir := newTestServer(t)
			repo, err := db.GetOrCreateRepo(tempDir)
			require.NoError(t, err)
			const runUUID = "remote-safe-panel-run"
			members, synth, err := db.EnqueuePanelRun(
				[]storage.EnqueueOpts{{
					RepoID: repo.ID, GitRef: "base..head", Agent: "test",
					JobType: storage.JobTypeRange, PanelRunUUID: runUUID,
					PanelRole: storage.PanelRoleMember, PanelName: "review",
					PanelMemberName: "default",
				}},
				storage.EnqueueOpts{
					RepoID: repo.ID, GitRef: "base..head", Agent: "test",
					PanelRunUUID: runUUID, PanelRole: storage.PanelRoleSynthesis,
					PanelName: "review",
				},
			)
			require.NoError(t, err)
			require.Len(t, members, 1)
			jobID := members[0].ID
			if target == "synthesis" {
				jobID = synth.ID
			}

			handler, sessions := newBrowserHandlerFixtureWithCore(
				t, testBrowserAuthToken, server.httpServer.Handler,
			)
			request := authenticatedBrowserMutationRequest(
				t, sessions, "/api/job/cancel", CancelJobRequest{JobID: jobID},
			)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, request)

			assert.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
			member, err := db.GetJobByID(members[0].ID)
			require.NoError(t, err)
			assert.Equal(t, storage.JobStatusQueued, member.Status)
			parent, err := db.GetJobByID(synth.ID)
			require.NoError(t, err)
			assert.Equal(t, storage.JobStatusQueued, parent.Status)
		})
	}
}

func TestBrowserHandlerStreamsStopWithSession(t *testing.T) {
	startStream := func(
		t *testing.T,
		handler http.Handler,
		sessions *BrowserSessionManager,
		credentials SessionCredentials,
	) (context.CancelFunc, <-chan struct{}) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		request := browserRequest(http.MethodGet, "/api/stream/events", nil).WithContext(ctx)
		request.AddCookie(sessions.Cookie(credentials.Ambient))
		request.Header.Set(WebSessionHeader, credentials.Tab)
		done := make(chan struct{})
		go func() {
			handler.ServeHTTP(httptest.NewRecorder(), request)
			close(done)
		}()
		return cancel, done
	}

	t.Run("logout", func(t *testing.T) {
		started := make(chan struct{})
		core := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			close(started)
			<-request.Context().Done()
		})
		handler, sessions := newBrowserHandlerFixtureWithCore(t, testBrowserAuthToken, core)
		credentials, err := sessions.Login(testBrowserAuthToken)
		require.NoError(t, err)
		cancel, done := startStream(t, handler, sessions, credentials)
		defer cancel()
		<-started

		logout := browserRequest(http.MethodDelete, "/api/ui/session", nil)
		logout.AddCookie(sessions.Cookie(credentials.Ambient))
		logout.Header.Set(WebSessionHeader, credentials.Tab)
		logout.Header.Set(WebCSRFHeader, credentials.CSRF)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, logout)
		require.Equal(t, http.StatusNoContent, recorder.Code)

		stopped := false
		select {
		case <-done:
			stopped = true
		case <-time.After(250 * time.Millisecond):
		}
		cancel()
		<-done
		assert.True(t, stopped, "logout must cancel an active browser stream")
	})

	t.Run("expiry", func(t *testing.T) {
		started := make(chan struct{})
		core := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			close(started)
			<-request.Context().Done()
		})
		handler, sessions := newBrowserHandlerFixtureWithCoreAndTTL(
			t, testBrowserAuthToken, core, 50*time.Millisecond,
		)
		credentials, err := sessions.Login(testBrowserAuthToken)
		require.NoError(t, err)
		cancel, done := startStream(t, handler, sessions, credentials)
		defer cancel()
		<-started

		expired := false
		select {
		case <-done:
			expired = true
		case <-time.After(500 * time.Millisecond):
		}
		cancel()
		<-done
		assert.True(t, expired, "session expiry must cancel an active browser stream")
	})
}

func TestBrowserHandlerRemoteSessionRejectsPrivilegedPanelMembers(t *testing.T) {
	for _, path := range []string{"/api/job/cancel", "/api/job/rerun"} {
		t.Run(path, func(t *testing.T) {
			server, db, tempDir := newTestServer(t)
			repo, err := db.GetOrCreateRepo(tempDir)
			require.NoError(t, err)
			const runUUID = "remote-panel-run"
			members, synth, err := db.EnqueuePanelRun(
				[]storage.EnqueueOpts{{
					RepoID: repo.ID, GitRef: "base..head", Agent: "test",
					JobType: storage.JobTypeRange, Agentic: true,
					PanelRunUUID: runUUID, PanelRole: storage.PanelRoleMember,
					PanelName: "review", PanelMemberName: "security",
				}},
				storage.EnqueueOpts{
					RepoID: repo.ID, GitRef: "base..head", Agent: "test",
					PanelRunUUID: runUUID, PanelRole: storage.PanelRoleSynthesis,
					PanelName: "review",
				},
			)
			require.NoError(t, err)
			require.Len(t, members, 1)
			startState := storage.JobStatusQueued
			if path == "/api/job/rerun" {
				startState = storage.JobStatusDone
				setJobStatus(t, db, synth.ID, startState)
			}
			handler, sessions := newBrowserHandlerFixtureWithCore(
				t, testBrowserAuthToken, server.httpServer.Handler,
			)
			body := map[string]any{"job_id": synth.ID}
			if path == "/api/job/rerun" {
				body["request_id"] = "panel-browser-request"
			}
			request := authenticatedBrowserMutationRequest(t, sessions, path, body)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, request)

			assert.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
			updatedSynth, err := db.GetJobByID(synth.ID)
			require.NoError(t, err)
			assert.Equal(t, startState, updatedSynth.Status)
			updatedMember, err := db.GetJobByID(members[0].ID)
			require.NoError(t, err)
			assert.Equal(t, storage.JobStatusQueued, updatedMember.Status)
		})
	}
}

func TestBrowserHandlerLocalBootstrapRequiresFetchMetadata(t *testing.T) {
	handler, _ := newBrowserHandlerFixture(t, "")
	request := browserRequest(http.MethodPost, "/api/ui/session/bootstrap", map[string]any{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusForbidden, recorder.Code)

	request = browserRequest(http.MethodPost, "/api/ui/session/bootstrap", map[string]any{})
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Sec-Fetch-Mode", "cors")
	request.Header.Set("Sec-Fetch-Dest", "empty")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.NotEmpty(t, recorder.Header().Get("Set-Cookie"))
	var envelope struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	assert.Equal(t, map[string]bool{
		"cancel_any_job":    true,
		"cancel_review_job": true,
		"rerun_job":         true,
	}, envelope.Capabilities)
}

func TestBrowserHandlerLocalBootstrapAllowsOriginlessOwnerHost(t *testing.T) {
	handler, _ := newBrowserHandlerFixture(t, "")
	request := browserRequest(http.MethodPost, "/api/ui/session/bootstrap", map[string]any{})
	request.Header.Del("Origin")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.NotEmpty(t, recorder.Header().Get("Set-Cookie"))
}

func TestBrowserHandlerOriginlessBootstrapKeepsLocalTrustBoundary(t *testing.T) {
	tests := []struct {
		name   string
		auth   string
		mutate func(*http.Request)
	}{
		{
			name: "cross-site fetch",
			mutate: func(request *http.Request) {
				request.Header.Set("Sec-Fetch-Site", "cross-site")
			},
		},
		{
			name: "forwarded request",
			mutate: func(request *http.Request) {
				request.Header.Set("X-Forwarded-For", "192.0.2.10")
			},
		},
		{
			name: "non-loopback peer",
			mutate: func(request *http.Request) {
				request.RemoteAddr = "192.0.2.10:50000"
			},
		},
		{name: "remote authentication configured", auth: testBrowserAuthToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, _ := newBrowserHandlerFixture(t, tt.auth)
			request := browserRequest(
				http.MethodPost, "/api/ui/session/bootstrap", map[string]any{},
			)
			request.Header.Del("Origin")
			if tt.mutate != nil {
				tt.mutate(request)
			}
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, request)

			assert.Equal(t, http.StatusForbidden, recorder.Code)
			assert.Empty(t, recorder.Header().Values("Set-Cookie"))
		})
	}

	for _, origin := range []string{"null", "https://attacker.example"} {
		t.Run("explicit origin "+origin, func(t *testing.T) {
			handler, _ := newBrowserHandlerFixture(t, "")
			request := browserRequest(
				http.MethodPost, "/api/ui/session/bootstrap", map[string]any{},
			)
			request.Header.Set("Origin", origin)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, request)

			assert.Equal(t, http.StatusForbidden, recorder.Code)
			assert.Empty(t, recorder.Header().Values("Set-Cookie"))
		})
	}
}

func TestBrowserSessionRoutesAppearOnlyInCombinedOpenAPI(t *testing.T) {
	spec, err := OpenAPISpec()
	require.NoError(t, err)
	assert.Contains(t, string(spec), `"/api/ui/session/login"`)
	assert.Contains(t, string(spec), `"login-web-session"`)

	server := &Server{}
	mux := http.NewServeMux()
	server.registerHumaAPI(mux)
	request := httptest.NewRequest(http.MethodPost, "/api/ui/session/login", nil)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusNotFound, recorder.Code)
}

func TestBrowserSessionOpenAPIDocumentsJSONErrors(t *testing.T) {
	spec, err := OpenAPISpec()
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal(spec, &document))
	paths := document["paths"].(map[string]any)
	login := paths["/api/ui/session/login"].(map[string]any)["post"].(map[string]any)
	responses := login["responses"].(map[string]any)
	assert.NotContains(t, responses, "default")
	assert.Contains(t, responses, "429")
	unauthorized := responses["401"].(map[string]any)
	content := unauthorized["content"].(map[string]any)
	assert.Contains(t, content, "application/json")
	assert.NotContains(t, content, "application/problem+json")
}
