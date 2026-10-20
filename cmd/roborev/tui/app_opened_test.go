package tui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	originalGate := telemetry.DefaultClientGate
	telemetry.DefaultClientGate = &telemetry.ClientGate{}
	t.Cleanup(func() { telemetry.DefaultClientGate = originalGate })
	t.Setenv(telemetry.EnabledEnv, "1")
	t.Setenv(telemetry.GenericEnabledEnv, "1")
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestReportSessionEndedBoundsDelivery(t *testing.T) {
	enableTelemetryEnv(t)
	synctest.Test(t, func(t *testing.T) {
		m := newModel(testEndpointFromURL("http://127.0.0.1:7373"), withExternalIODisabled())
		close(m.ready)
		m.client.Transport = &screenRecordingTransport{}
		startup := m.reportStartupTelemetry()
		require.NotNil(t, startup)
		require.IsType(t, screenDeliveryMsg{}, startup())
		require.True(t, m.awaitTelemetryProbe())

		release := make(chan struct{})
		transport := &stalledTransport{hit: make(chan struct{}, 1), release: release, events: make(chan string, 1)}
		m.client.Transport = transport
		started := time.Now()
		m.finishTelemetry(2 * time.Minute)
		synctest.Wait()
		assert.Equal(t, 50*time.Millisecond, time.Since(started))
		assert.Equal(t, started.Add(time.Second), transport.deadline)
		close(release)
		synctest.Wait()
		assert.Equal(t, int32(1), transport.requests.Load())
		assert.Equal(t, telemetry.EventSessionEnded, <-transport.events)
	})
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestReportStartupTelemetryReachesDaemonAllowlist(t *testing.T) {
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
		capture.ServeHTTP(rec, r)
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
	cmd := m.reportStartupTelemetry()
	require.NotNil(cmd)
	assert.IsType(screenDeliveryMsg{}, cmd())

	mu.Lock()
	defer mu.Unlock()
	require.Len(requests, 1)
	var event struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	}
	require.NoError(json.Unmarshal(requests[0].body, &event))
	assert.Equal(telemetry.EventAppOpened, event.Event)
	properties, err := rep.SanitizeProperties(event.Event, event.Properties)
	require.NoError(err)
	assert.Equal("tui", properties[telemetry.PropertySurface])
	assert.Equal(http.StatusAccepted, requests[0].code)
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestReportStartupTelemetryIgnoresDaemonFailures(t *testing.T) {
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
			enableTelemetryEnv(t)
			assert := assert.New(t)
			ts := httptest.NewServer(tt.handler)
			t.Cleanup(ts.Close)
			m := newModel(testEndpointFromURL(ts.URL), withExternalIODisabled())
			if tt.closed {
				ts.Close()
			}

			cmd := m.reportStartupTelemetry()
			require.NotNil(t, cmd)
			msg := cmd()
			require.IsType(t, screenDeliveryMsg{}, msg)
			updated, _ := m.Update(msg)
			updatedModel := updated.(model)
			require.NoError(t, updatedModel.err)
			assert.Zero(updatedModel.consecutiveErrors)
		})
	}
}

// stalledTransport holds requests until their context ends or the test releases them.
type stalledTransport struct {
	hit      chan struct{}
	release  <-chan struct{}
	events   chan string
	requests atomic.Int32
	deadline time.Time
}

func (s *stalledTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.requests.Add(1)
	s.deadline, _ = req.Context().Deadline()
	if s.events != nil {
		var event struct {
			Name string `json:"event"`
		}
		if err := json.NewDecoder(req.Body).Decode(&event); err == nil {
			select {
			case s.events <- event.Name:
			default:
			}
		}
	}
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
func TestReportStartupTelemetryReturnsWhenClientTimesOut(t *testing.T) {
	enableTelemetryEnv(t)

	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		m := newModel(testEndpointFromURL("http://127.0.0.1:7373"), withExternalIODisabled())
		transport := &stalledTransport{hit: make(chan struct{}, 1)}
		m.client.Transport = transport

		cmd := m.reportStartupTelemetry()
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

		time.Sleep(telemetry.NotificationTimeout)
		synctest.Wait()
		select {
		case <-done:
		default:
			assert.Fail("command still running after the client timeout")
		}
		assert.IsType(screenDeliveryMsg{}, result)
		assert.Equal(int32(1), transport.requests.Load())
	})
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestReportStartupTelemetrySkippedWhenTelemetryOff(t *testing.T) {
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

			assert.Nil(t, m.reportStartupTelemetry())
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
		telemetrySends.Wait(3 * telemetry.NotificationTimeout)
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
func TestTUIStartupUsesOneProbeWhenTelemetryIsDisabled(t *testing.T) {
	enableTelemetryEnv(t)
	var mu sync.Mutex
	var events []string
	d := &appOpenedTestDaemon{}
	ts := d.serve(t, func(w http.ResponseWriter, r *http.Request) {
		var event struct {
			Event string `json:"event"`
		}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&event))
		mu.Lock()
		events = append(events, event.Event)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		status := "queued"
		if event.Event == telemetry.EventAppOpened {
			status = "disabled"
		}
		_, _ = w.Write([]byte(`{"status":"` + status + `"}`))
	})
	p, runDone := startHeadlessProgram(t, newModel(testEndpointFromURL(ts.URL), withExternalIODisabled()))
	require.Eventually(t, func() bool { return d.telemetryPosts.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
	p.Quit()
	<-runDone
	telemetrySends.Wait(3 * telemetry.NotificationTimeout)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{telemetry.EventAppOpened}, events)
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestTUIProgramStartsAndExitsWhileAppOpenedHangs(t *testing.T) {
	enableTelemetryEnv(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	var enterOnce sync.Once
	d := &appOpenedTestDaemon{}
	ts := d.serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		enterOnce.Do(func() { close(entered) })
		<-release
		w.WriteHeader(http.StatusAccepted)
	})
	// Registered after the server's cleanup, so it runs first and the held handler can return.
	t.Cleanup(releaseOnce)

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
	releaseOnce()
	telemetrySends.Wait(2 * telemetry.NotificationTimeout)
}

//nolint:paralleltest // telemetry environment and shared send group
func TestTUIExitSharesTelemetryWaitBudget(t *testing.T) {
	enableTelemetryEnv(t)
	synctest.Test(t, func(t *testing.T) {
		m := newModel(testEndpointFromURL("http://127.0.0.1:7373"), withExternalIODisabled())
		close(m.ready)
		m.client.Transport = &screenRecordingTransport{}
		m.reportStartupTelemetry()()
		// The session report finishes at once, so only the stalled screen report can hold the exit.
		transport := &screenStallTransport{}
		m.client.Transport = transport
		done := make(chan struct{})
		go func() { m.postScreen("help")(); close(done) }()
		synctest.Wait()
		start := time.Now()
		m.finishTelemetry(2 * time.Minute)
		assert.Equal(t, 50*time.Millisecond, time.Since(start))
		assert.Equal(t, []string{telemetry.EventScreenViewed, telemetry.EventSessionEnded}, transport.received())
		<-done
	})
}

// screenStallTransport holds screen reports until their deadline and accepts every other report.
type screenStallTransport struct {
	mu     sync.Mutex
	events []string
}

func (s *screenStallTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var event struct {
		Name string `json:"event"`
	}
	if err := json.NewDecoder(req.Body).Decode(&event); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.events = append(s.events, event.Name)
	s.mu.Unlock()
	if event.Name == telemetry.EventScreenViewed {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"status":"queued"}`)), Request: req}, nil
}

func (s *screenStallTransport) received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events)
}

type delayedProbeTransport struct {
	status  string
	release chan struct{}
	events  chan string
}

func (s *delayedProbeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var event struct {
		Event      string            `json:"event"`
		Properties map[string]string `json:"properties"`
	}
	if err := json.NewDecoder(req.Body).Decode(&event); err != nil {
		return nil, err
	}
	name := event.Event
	if event.Event == telemetry.EventScreenViewed {
		name += ":" + event.Properties[telemetry.PropertyScreen]
	}
	s.events <- name
	if event.Event == telemetry.EventAppOpened {
		select {
		case <-s.release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(`{"status":"` + s.status + `"}`))}, nil
}

//nolint:paralleltest // telemetry environment and gate
func TestTUIStartupProbeHoldsNavigationActionsAndSession(t *testing.T) {
	for _, status := range []string{"queued", "disabled"} {
		t.Run(status, func(t *testing.T) {
			enableTelemetryEnv(t)
			synctest.Test(t, func(t *testing.T) {
				m := newModel(testEndpointFromURL("http://127.0.0.1:7373"), withExternalIODisabled())
				transport := &delayedProbeTransport{status: status, release: make(chan struct{}), events: make(chan string, 10)}
				m.client.Transport = transport
				var sends sync.WaitGroup
				run := func(cmd tea.Cmd) { sends.Go(func() { collectMsgs(cmd) }) }
				run(m.reportStartupTelemetry())
				synctest.Wait()
				require.Equal(t, "app_opened", <-transport.events)
				updated, nav := m.Update(keyPressMsg('?'))
				m = updated.(model)
				require.Equal(t, viewHelp, m.currentView)
				run(nav)
				run(m.reportAction(telemetry.EventReviewViewed))
				sends.Go(func() { m.reportSessionEnded(time.Minute) })
				synctest.Wait()
				require.Empty(t, transport.events)
				close(transport.release)
				sends.Wait()
				var events []string
				for len(transport.events) > 0 {
					events = append(events, <-transport.events)
				}
				if status == "disabled" {
					assert.Empty(t, events)
				} else {
					assert.ElementsMatch(t, []string{"screen_viewed:queue", "screen_viewed:help", "review_viewed", "session_ended"}, events)
				}
			})
		})
	}
}
