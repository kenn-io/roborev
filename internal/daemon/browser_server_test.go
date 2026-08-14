package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testutil"
)

func TestBrowserServerStartsReadyAndKeepsShutdownPrivate(t *testing.T) {
	server, _, _ := newTestServer(t)
	server.allowWebCompilationStub = true
	cfg := config.DefaultConfig()
	cfg.Web.Listen = "127.0.0.1:0"
	runtime, err := server.startBrowserServer(cfg.Web)
	require.NoError(t, err)
	require.NotNil(t, runtime)
	assert.NotEmpty(t, runtime.Address)
	assert.Equal(t, "http://"+runtime.Address, runtime.Origin)
	assert.ElementsMatch(t, []string{"web-ui-v1", "web-session-v1", "analytics-v1"}, runtime.Capabilities)
	require.NotNil(t, server.browserServer)
	assert.Equal(t, 5*time.Second, server.browserServer.ReadHeaderTimeout)
	assert.Equal(t, 30*time.Second, server.browserServer.ReadTimeout)
	assert.Equal(t, 2*time.Minute, server.browserServer.IdleTimeout)
	assert.Zero(t, server.browserServer.WriteTimeout, "streaming responses must not have a server write deadline")

	response, err := http.Get("http://" + runtime.Address + "/api/ping")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, response.Body.Close()) })
	assert.Equal(t, http.StatusOK, response.StatusCode)

	shutdownRequest, err := http.NewRequest(http.MethodPost, "http://"+runtime.Address+"/api/shutdown", nil)
	require.NoError(t, err)
	shutdownResponse, err := http.DefaultClient.Do(shutdownRequest)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, shutdownResponse.Body.Close()) })
	assert.Equal(t, http.StatusNotFound, shutdownResponse.StatusCode)
}

func TestBrowserServerSkipsCompilationStubOutsideDevelopment(t *testing.T) {
	server, _, _ := newTestServer(t)
	server.allowWebCompilationStub = false
	runtime, err := server.startBrowserServer(config.DefaultConfig().Web)
	require.NoError(t, err)
	assert.Nil(t, runtime)
}

func TestBrowserServerCannotStartAfterStop(t *testing.T) {
	server, _, _ := newTestServer(t)
	server.allowWebCompilationStub = true
	require.NoError(t, server.Stop())
	runtime, err := server.startBrowserServer(config.DefaultConfig().Web)
	require.Error(t, err)
	assert.Nil(t, runtime)
}

func TestBrowserServerClosesListenerWhenStartupRacesWithStop(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := reserved.Addr().String()
	require.NoError(t, reserved.Close())

	server, _, _ := newTestServer(t)
	server.allowWebCompilationStub = true
	require.NoError(t, server.Stop())
	cfg := config.DefaultConfig()
	cfg.Web.Listen = address
	runtime, err := server.startBrowserServer(cfg.Web)
	require.Error(t, err)
	assert.Nil(t, runtime)

	rebound, err := net.Listen("tcp", address)
	require.NoError(t, err)
	require.NoError(t, rebound.Close())
}

func TestBrowserServerDisabled(t *testing.T) {
	server, _, _ := newTestServer(t)
	runtime, err := server.startBrowserServer(config.WebConfig{Enabled: false})
	require.NoError(t, err)
	assert.Nil(t, runtime)
}

func TestBrowserDevelopmentOriginOption(t *testing.T) {
	server, _, _ := newTestServer(t)
	WithWebDevelopmentOrigin("http://127.0.0.1:5173")(server)
	assert.Equal(t, "http://127.0.0.1:5173", server.webDevOrigin)
}

func TestServerBrowserLifecycleAndRuntimePublication(t *testing.T) {
	db, _ := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.ServerAddr = "127.0.0.1:0"
	cfg.Web.Listen = "127.0.0.1:0"
	server := NewServer(db, cfg, "", withWebCompilationStub())
	t.Cleanup(func() { require.NoError(t, server.Close()) })

	errCh, runtime := startServerAndWaitForRuntime(t, server)
	require.NotEmpty(t, runtime.WebAddress)
	assert.NotEqual(t, runtime.Address, runtime.WebAddress)
	assert.Equal(t, "http://"+runtime.WebAddress, runtime.WebOrigin)
	assert.ElementsMatch(t, browserCapabilities, runtime.WebCapabilities)

	request, err := http.NewRequest(http.MethodGet, runtime.WebOrigin+"/reviews/7", nil)
	require.NoError(t, err)
	request.Header.Set("Accept", "text/html")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, response.Body.Close())
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Contains(t, string(body), "Roborev")

	response, err = http.Get(runtime.WebOrigin + "/api/status")
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)

	stopTestServer(t, server, errCh)
	assert.Eventually(t, func() bool {
		client := &http.Client{Timeout: 50 * time.Millisecond}
		coreResponse, coreErr := client.Get("http://" + runtime.Address + "/api/ping")
		if coreResponse != nil {
			_ = coreResponse.Body.Close()
		}
		browserResponse, browserErr := client.Get(runtime.WebOrigin + "/api/ping")
		if browserResponse != nil {
			_ = browserResponse.Body.Close()
		}
		return coreErr != nil && browserErr != nil
	}, time.Second, 10*time.Millisecond)
}

func TestServerBrowserShutdownCancelsActiveEventStream(t *testing.T) {
	db, _ := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.ServerAddr = "127.0.0.1:0"
	cfg.Web.Listen = "127.0.0.1:0"
	cfg.MaxWorkers = 0
	server := NewServer(db, cfg, "", withWebCompilationStub())
	errCh, runtime := startServerAndWaitForRuntime(t, server)
	credentials, cookie := bootstrapLocalBrowserSession(t, runtime.WebOrigin)

	request, err := http.NewRequest(http.MethodGet, runtime.WebOrigin+"/api/stream/events", nil)
	require.NoError(t, err)
	request.AddCookie(cookie)
	request.Header.Set(WebSessionHeader, credentials.Session)
	initialSubscribers := server.broadcaster.SubscriberCount()
	responseCh := make(chan *http.Response, 1)
	requestErrCh := make(chan error, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			requestErrCh <- requestErr
			return
		}
		responseCh <- response
	}()
	require.Eventually(t, func() bool {
		return server.broadcaster.SubscriberCount() > initialSubscribers
	}, time.Second, 10*time.Millisecond)
	server.broadcaster.Broadcast(Event{
		Type: "review.completed", TS: time.Now(), JobID: 1,
		Repo: "fixture", RepoName: "fixture", SHA: "abc123",
	})
	var response *http.Response
	select {
	case err = <-requestErrCh:
		require.NoError(t, err)
	case response = <-responseCh:
	}
	require.NotNil(t, response)
	t.Cleanup(func() { _ = response.Body.Close() })
	require.Equal(t, http.StatusOK, response.StatusCode)

	stopped := make(chan error, 1)
	go func() { stopped <- server.Stop() }()
	t.Cleanup(func() {
		server.browserMu.Lock()
		defer server.browserMu.Unlock()
		if server.browserServer != nil {
			_ = server.browserServer.Close()
		}
	})
	require.Eventually(t, func() bool { return len(stopped) == 1 }, time.Second, 10*time.Millisecond)
	require.NoError(t, <-stopped)
	assert.NoError(t, <-errCh)
}

func TestServerBrowserBindFailureAbortsWithoutRuntime(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, occupied.Close()) })
	db, _ := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.ServerAddr = "127.0.0.1:0"
	cfg.Web.Listen = occupied.Addr().String()
	server := NewServer(db, cfg, "", withWebCompilationStub())
	t.Cleanup(func() { require.NoError(t, server.Close()) })

	err = server.Start(t.Context())
	require.Error(t, err)
	assert.NoFileExists(t, RuntimePath())
}

func TestServerDisabledBrowserOmitsRuntimeMetadata(t *testing.T) {
	db, _ := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.ServerAddr = "127.0.0.1:0"
	cfg.Web.Enabled = false
	server := NewServer(db, cfg, "")
	t.Cleanup(func() { require.NoError(t, server.Close()) })

	errCh, runtime := startServerAndWaitForRuntime(t, server)
	assert.Empty(t, runtime.WebAddress)
	assert.Empty(t, runtime.WebOrigin)
	assert.Empty(t, runtime.WebCapabilities)
	stopTestServer(t, server, errCh)
}

func TestServerRestartInvalidatesBrowserSession(t *testing.T) {
	db, _ := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.ServerAddr = "127.0.0.1:0"
	cfg.Web.Listen = "127.0.0.1:0"

	first := NewServer(db, cfg, "", withWebCompilationStub())
	firstErrCh, firstRuntime := startServerAndWaitForRuntime(t, first)
	firstCredentials, firstCookie := bootstrapLocalBrowserSession(t, firstRuntime.WebOrigin)
	stopTestServer(t, first, firstErrCh)

	second := NewServer(db, cfg, "", withWebCompilationStub())
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	secondErrCh, secondRuntime := startServerAndWaitForRuntime(t, second)
	secondCredentials, secondCookie := bootstrapLocalBrowserSession(t, secondRuntime.WebOrigin)
	assert.NotEqual(t, firstCookie.Name, secondCookie.Name)
	assert.NotEqual(t, firstCredentials.Session, secondCredentials.Session)

	request, err := http.NewRequest(http.MethodGet, secondRuntime.WebOrigin+"/api/status", nil)
	require.NoError(t, err)
	request.AddCookie(firstCookie)
	request.Header.Set(WebSessionHeader, firstCredentials.Session)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
	stopTestServer(t, second, secondErrCh)
}

func bootstrapLocalBrowserSession(t *testing.T, origin string) (WebSessionCredentials, *http.Cookie) {
	t.Helper()
	request, err := http.NewRequest(
		http.MethodPost, origin+"/api/ui/session/bootstrap", bytes.NewBufferString("{}"),
	)
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Sec-Fetch-Mode", "cors")
	request.Header.Set("Sec-Fetch-Dest", "empty")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	require.Equal(t, http.StatusOK, response.StatusCode)
	var credentials WebSessionCredentials
	require.NoError(t, json.NewDecoder(response.Body).Decode(&credentials))
	require.Len(t, response.Cookies(), 1)
	return credentials, response.Cookies()[0]
}
