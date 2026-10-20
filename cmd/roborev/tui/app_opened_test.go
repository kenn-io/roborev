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

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/telemetry"
)

func enableTelemetryEnv(t *testing.T) {
	t.Helper()
	t.Setenv(telemetry.EnabledEnv, "1")
	t.Setenv(telemetry.GenericEnabledEnv, "1")
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	telemetry.DefaultClientGate = &telemetry.ClientGate{}
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestReportSessionEndedBoundsDelivery(t *testing.T) {
	enableTelemetryEnv(t)
	synctest.Test(t, func(t *testing.T) {
		m := newModel(testEndpointFromURL("http://127.0.0.1:7373"), withExternalIODisabled())
		close(m.ready)
		release := make(chan struct{})
		transport := &stalledTransport{hit: make(chan struct{}, 1), release: release}
		m.client.Transport = transport
		started := time.Now()
		m.reportSessionEnded(2 * time.Minute)
		synctest.Wait()
		assert.Equal(t, 50*time.Millisecond, time.Since(started))
		assert.Equal(t, started.Add(time.Second), transport.deadline)
		close(release)
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		assert.Equal(t, int32(2), transport.requests.Load(), "the app_opened check, then the session")
	})
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
		body []byte
		code int
	}
	var (
		mu       sync.Mutex
		requests []recorded
	)
	capture := (&telemetry.AppOpenedLimiter{}).Handler(rep)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/telemetry/events", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := httptest.NewRecorder()
		if bytes.Contains(body, []byte(telemetry.EventAppOpened)) {
			// The opted-out reporter would answer disabled and end the day's reports; the check settles here instead.
			rec.WriteHeader(http.StatusAccepted)
			_, _ = rec.WriteString(`{"status":"queued"}`)
		} else {
			capture.ServeHTTP(rec, r)
		}
		mu.Lock()
		requests = append(requests, recorded{
			body: body, code: rec.Code,
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

	var names []string
	for view := viewQueue; view <= viewRerunAgent; view++ {
		// This opted-out reporter answers disabled, which would silence the rest; each screen starts fresh.
		t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
		telemetry.DefaultClientGate = &telemetry.ClientGate{}
		names = append(names, view.String())
		assert.NotEqual("unknown", view.String())
		m.currentView = view
		cmd = m.reportScreenViewed()
		require.NotNil(cmd)
		cmd()
	}
	mu.Lock()
	defer mu.Unlock()
	var screens []recorded
	for _, request := range requests {
		if bytes.Contains(request.body, []byte(telemetry.EventScreenViewed)) {
			screens = append(screens, request)
		}
	}
	require.Len(screens, len(names))
	for i, name := range names {
		var screenEvent struct {
			Event      string         `json:"event"`
			Properties map[string]any `json:"properties"`
		}
		require.NoError(json.Unmarshal(screens[i].body, &screenEvent))
		assert.Equal(telemetry.EventScreenViewed, screenEvent.Event)
		properties, err := rep.SanitizeProperties(screenEvent.Event, screenEvent.Properties)
		require.NoError(err)
		assert.Equal(name, properties[telemetry.PropertyScreen])
		assert.Equal("tui", properties[telemetry.PropertySurface])
		assert.Equal(http.StatusAccepted, screens[i].code)
	}
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

// stalledTransport holds requests until their context ends or the test releases them.
type stalledTransport struct {
	hit      chan struct{}
	release  <-chan struct{}
	requests atomic.Int32
	deadline time.Time
}

func (s *stalledTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.requests.Add(1)
	s.deadline, _ = req.Context().Deadline()
	select {
	case s.hit <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
		return &http.Response{StatusCode: http.StatusAccepted, Body: http.NoBody}, nil
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
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
			assert.Nil(t, m.reportScreenViewed())
			_, cmd := m.Update(keyPressMsg('?'))
			collectMsgs(cmd)
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
			var event struct {
				Event string `json:"event"`
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &event)
			r.Body = io.NopCloser(bytes.NewReader(body))
			if event.Event == telemetry.EventAppOpened {
				d.telemetryPosts.Add(1)
			}
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

	screens := make(chan string, 16)
	d := &appOpenedTestDaemon{}
	ts := d.serve(t, func(w http.ResponseWriter, r *http.Request) {
		var event struct {
			Event      string            `json:"event"`
			Properties map[string]string `json:"properties"`
		}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&event))
		if event.Event == telemetry.EventScreenViewed {
			assert.Equal(t, "tui", event.Properties[telemetry.PropertySurface])
			screens <- event.Properties[telemetry.PropertyScreen]
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"queued"}`))
	})
	ep := testEndpointFromURL(ts.URL)
	p, runDone := startHeadlessProgram(t, newModel(ep, withExternalIODisabled()))

	// Waits on socket work in the fake daemon, so wall-clock polling is required.
	require.Eventually(t, func() bool { return d.telemetryPosts.Load() == 1 }, 5*time.Second, 10*time.Millisecond)

	// Requests cross a test-server socket.
	require.Eventually(t, func() bool { return len(screens) == 1 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, "queue", <-screens)
	p.Send(keyPressMsg('?'))
	// Requests cross a test-server socket.
	require.Eventually(t, func() bool { return len(screens) == 1 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, "help", <-screens)
	// The queue was already reported today, so returning to it posts nothing; the final Empty checks that.
	p.Send(keyPressMsg('?'))

	jobsBefore := d.jobFetches.Load()
	p.Send(reconnectMsg{endpoint: ep, version: "test"})
	p.Send(tickMsg(time.Now()))
	// Waits on socket work in the fake daemon, so wall-clock polling is required.
	require.Eventually(t, func() bool { return d.jobFetches.Load() > jobsBefore }, 5*time.Second, 10*time.Millisecond)

	p.Quit()
	<-runDone
	assert.Equal(t, int32(1), d.telemetryPosts.Load())
	assert.Empty(t, screens)
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

	// The startup fetch must run while telemetry is held. A refresh tick can be
	// skipped until that fetch completes, so it cannot signal startup here.
	// Waits on socket work in the fake daemon, so wall-clock polling is required.
	require.Eventually(t, func() bool {
		return d.jobFetches.Load() > 0 && !isClosed(release)
	}, 5*time.Second, 10*time.Millisecond)

	p.Quit()
	// Waits on socket work in the fake daemon, so wall-clock polling is required.
	require.Eventually(t, func() bool { return isClosed(runDone) && !isClosed(release) }, 5*time.Second, 10*time.Millisecond)
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestReportSessionEndedJoinsThePendingProbe(t *testing.T) {
	enableTelemetryEnv(t)
	synctest.Test(t, func(t *testing.T) {
		m := newModel(testEndpointFromURL("http://127.0.0.1:7373"), withExternalIODisabled())
		close(m.ready)
		transport := &holdingTransport{release: make(chan struct{})}
		m.client.Transport = transport
		go func() { m.probeTelemetry() }()
		synctest.Wait()

		started := time.Now()
		m.reportSessionEnded(2 * time.Minute)
		assert.Equal(t, 50*time.Millisecond, time.Since(started), "quit waits only the exit budget")
		transport.mu.Lock()
		assert.Empty(t, transport.order, "the session waits for the probe")
		transport.mu.Unlock()

		close(transport.release)
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		assert.Equal(t, []string{telemetry.EventAppOpened, telemetry.EventSessionEnded}, transport.order)
	})
}
