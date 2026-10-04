package tui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/telemetry"
)

func enableTelemetryEnv(t *testing.T) {
	t.Helper()
	t.Setenv(telemetry.EnabledEnv, "1")
	t.Setenv(telemetry.GenericEnabledEnv, "1")
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestReportAppOpenedReachesDaemonAllowlist(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Setenv(telemetry.EnabledEnv, "0")
	rep, err := telemetry.NewReporter(telemetry.Options{})
	require.NoError(err)
	enableTelemetryEnv(t)

	type recorded struct {
		method, path, contentType string
		body                      []byte
		code                      int
		response                  string
	}
	var (
		mu       sync.Mutex
		requests []recorded
	)
	capture := telemetry.NewCaptureHandler(rep)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/telemetry/events", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := httptest.NewRecorder()
		capture.ServeHTTP(rec, r)
		mu.Lock()
		requests = append(requests, recorded{
			method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type"),
			body: body, code: rec.Code, response: rec.Body.String(),
		})
		mu.Unlock()
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	m := newModel(testEndpointFromURL(ts.URL), withExternalIODisabled())
	cmd := m.reportAppOpened()
	require.NotNil(cmd)
	assert.Nil(cmd())

	mu.Lock()
	defer mu.Unlock()
	require.Len(requests, 1)
	got := requests[0]
	assert.Equal(http.MethodPost, got.method)
	assert.Equal(daemon.TelemetryEventsPath, got.path)
	assert.Equal("application/json", got.contentType)
	assert.JSONEq(`{"event":"app_opened","properties":{"surface":"tui"}}`, string(got.body))
	assert.Equal(http.StatusAccepted, got.code)
	assert.JSONEq(`{"status":"disabled"}`, got.response)

	var decoded struct {
		Properties map[string]any `json:"properties"`
	}
	require.NoError(json.Unmarshal(got.body, &decoded))
	props, err := rep.SanitizeProperties(telemetry.EventAppOpened, decoded.Properties)
	require.NoError(err)
	assert.Equal("tui", props["surface"])
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestReportAppOpenedIgnoresDaemonFailures(t *testing.T) {
	enableTelemetryEnv(t)

	answer := func(status int, body string, header map[string]string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			for k, v := range header {
				w.Header().Set(k, v)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	tests := []struct {
		name    string
		handler http.HandlerFunc
		closed  bool
	}{
		{name: "daemon without the route", handler: answer(http.StatusNotFound, "404 page not found", nil)},
		{name: "allowlist without app_opened", handler: answer(http.StatusBadRequest, "unsupported telemetry event", nil)},
		{name: "method not allowed", handler: answer(http.StatusMethodNotAllowed, "Method Not Allowed", map[string]string{"Allow": "POST"})},
		{name: "body too large", handler: answer(http.StatusRequestEntityTooLarge, "telemetry request too large", nil)},
		{name: "server error", handler: answer(http.StatusInternalServerError, "internal error", nil)},
		{name: "connection refused", handler: answer(http.StatusAccepted, `{"status":"queued"}`, nil), closed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			ts := httptest.NewServer(tt.handler)
			t.Cleanup(ts.Close)
			m := newModel(testEndpointFromURL(ts.URL), withExternalIODisabled())
			if tt.closed {
				ts.Close()
			}

			cmd := m.reportAppOpened()
			require.NotNil(t, cmd)
			assert.Nil(cmd())
			require.NoError(t, m.err)
			assert.Zero(m.consecutiveErrors)
		})
	}
}

// stalledTransport never answers; it returns only when the request context ends.
type stalledTransport struct {
	hit      chan struct{}
	requests atomic.Int32
}

func (s *stalledTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.requests.Add(1)
	select {
	case s.hit <- struct{}{}:
	default:
	}
	<-req.Context().Done()
	return nil, req.Context().Err()
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestReportAppOpenedReturnsWhenClientTimesOut(t *testing.T) {
	enableTelemetryEnv(t)

	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		m := newModel(testEndpointFromURL("http://127.0.0.1:7373"), withExternalIODisabled())
		require.Equal(t, 10*time.Second, m.client.Timeout)
		t.Logf("client timeout %s", m.client.Timeout)
		transport := &stalledTransport{hit: make(chan struct{}, 1)}
		m.client.Transport = transport

		cmd := m.reportAppOpened()
		require.NotNil(t, cmd)
		var result tea.Msg
		done := make(chan struct{})
		go func() {
			result = cmd()
			close(done)
		}()

		<-transport.hit
		synctest.Wait()
		select {
		case <-done:
			assert.Fail("command returned before the client timeout")
		default:
		}

		time.Sleep(10 * time.Second)
		synctest.Wait()
		select {
		case <-done:
		default:
			assert.Fail("command still running after the client timeout")
		}
		assert.Nil(result)
		assert.Equal(int32(1), transport.requests.Load())
	})
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestReportAppOpenedSkippedWhenTelemetryOff(t *testing.T) {
	tests := []struct {
		name    string
		roborev string
		generic string
	}{
		{name: "roborev opt-out", roborev: "0", generic: ""},
		{name: "generic opt-out", roborev: "1", generic: "0"},
		{name: "padded roborev opt-out", roborev: "  0 ", generic: "1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(telemetry.EnabledEnv, tt.roborev)
			t.Setenv(telemetry.GenericEnabledEnv, tt.generic)
			var requests atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusAccepted)
			}))
			t.Cleanup(ts.Close)
			m := newModel(testEndpointFromURL(ts.URL), withExternalIODisabled())

			assert.Nil(t, m.reportAppOpened())
			assert.Zero(t, requests.Load())
		})
	}
}

// appOpenedTestDaemon answers the TUI's startup fetches and counts telemetry posts and job fetches.
type appOpenedTestDaemon struct {
	telemetryPosts atomic.Int32
	jobFetches     atomic.Int32
}

func (d *appOpenedTestDaemon) serve(t *testing.T, telemetryHandler http.HandlerFunc) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/telemetry/events":
			d.telemetryPosts.Add(1)
			telemetryHandler(w, r)
		case r.URL.Path == "/api/jobs":
			d.jobFetches.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs":     []any{},
				"has_more": false,
				"stats":    storage.JobStats{},
			})
		case r.URL.Path == "/api/status":
			_ = json.NewEncoder(w).Encode(storage.DaemonStatus{Version: "test"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func startHeadlessProgram(t *testing.T, m model) (*tea.Program, chan struct{}) {
	t.Helper()
	p := tea.NewProgram(m, tea.WithoutRenderer(), tea.WithInput(nil))
	runDone := make(chan struct{})
	go func() { _, _ = p.Run(); close(runDone) }()
	t.Cleanup(func() {
		p.Kill()
		<-runDone
	})
	return p, runDone
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestTUIProgramReportsAppOpenedOncePerLaunch(t *testing.T) {
	enableTelemetryEnv(t)

	d := &appOpenedTestDaemon{}
	ts := d.serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"queued"}`))
	})
	ep := testEndpointFromURL(ts.URL)
	p, runDone := startHeadlessProgram(t, newModel(ep, withExternalIODisabled()))

	// Waits on socket work in the fake daemon, so wall-clock polling is required.
	require.Eventually(t, func() bool { return d.telemetryPosts.Load() == 1 }, 5*time.Second, 10*time.Millisecond)

	jobsBefore := d.jobFetches.Load()
	p.Send(reconnectMsg{endpoint: ep, version: "test"})
	p.Send(tickMsg(time.Now()))
	// Waits on socket work in the fake daemon, so wall-clock polling is required.
	require.Eventually(t, func() bool { return d.jobFetches.Load() > jobsBefore }, 5*time.Second, 10*time.Millisecond)

	p.Quit()
	<-runDone
	assert.Equal(t, int32(1), d.telemetryPosts.Load())
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestTUIProgramStartsAndExitsWhileAppOpenedHangs(t *testing.T) {
	enableTelemetryEnv(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	d := &appOpenedTestDaemon{}
	ts := d.serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		enterOnce.Do(func() { close(entered) })
		<-release
		w.WriteHeader(http.StatusAccepted)
	})
	// Registered after the server's cleanup, so it runs first and the held handler can return.
	t.Cleanup(func() { close(release) })

	isClosed := func(ch chan struct{}) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}

	p, runDone := startHeadlessProgram(t, newModel(testEndpointFromURL(ts.URL), withExternalIODisabled()))

	// Waits on socket work in the fake daemon, so wall-clock polling is required.
	require.Eventually(t, func() bool { return isClosed(entered) }, 5*time.Second, 10*time.Millisecond)

	jobsBefore := d.jobFetches.Load()
	// Waits on socket work in the fake daemon, so wall-clock polling is required.
	require.Eventually(t, func() bool {
		// Startup may still be loading jobs and intentionally skip a tick.
		p.Send(tickMsg(time.Now()))
		return d.jobFetches.Load() > jobsBefore && !isClosed(release)
	}, 5*time.Second, 10*time.Millisecond)

	p.Quit()
	// Waits on socket work in the fake daemon, so wall-clock polling is required.
	require.Eventually(t, func() bool { return isClosed(runDone) && !isClosed(release) }, 5*time.Second, 10*time.Millisecond)
}
