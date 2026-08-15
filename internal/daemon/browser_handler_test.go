package daemon

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
