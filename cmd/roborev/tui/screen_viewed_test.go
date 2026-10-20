package tui

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/telemetry"
)

type screenRecordingTransport struct {
	events  []string
	screens []string
	opened  int // app_opened checks; events holds every other report
	fail    bool
	status  string
}

func (s *screenRecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/api/telemetry/events" {
		var event struct {
			Event      string            `json:"event"`
			Properties map[string]string `json:"properties"`
		}
		if err := json.NewDecoder(req.Body).Decode(&event); err != nil {
			return nil, err
		}
		switch event.Event {
		case telemetry.EventScreenViewed:
			s.screens = append(s.screens, event.Properties[telemetry.PropertyScreen])
		case telemetry.EventAppOpened:
			s.opened++
		default:
			s.events = append(s.events, event.Event)
		}
	}
	status := http.StatusAccepted
	if req.URL.Path == "/api/review/close" {
		status = http.StatusOK
	}
	if s.fail {
		status = http.StatusServiceUnavailable
	}
	captureStatus := s.status
	if captureStatus == "" {
		captureStatus = "queued"
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"status":"` + captureStatus + `"}`)), Header: make(http.Header)}, nil
}

func updateScreenTestModel(m *model, msg tea.Msg) {
	result, cmd := m.Update(msg)
	*m = result.(model)
	for _, msg := range collectMsgs(cmd) {
		if failed, ok := msg.(reportFailedMsg); ok {
			result, _ = m.Update(failed)
			*m = result.(model)
		}
	}
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestScreenFailureIsNotRetriedByHeldKey(t *testing.T) {
	enableTelemetryEnv(t)
	for _, tt := range []struct {
		name, screen string
		initial      bool
	}{
		{"initial queue", "queue", true}, {"help after toggle", "help", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			enableTelemetryEnv(t)
			m := newModel(testEndpointFromURL("http://127.0.0.1:7373"), withExternalIODisabled())
			transport := &screenRecordingTransport{fail: true}
			m.client.Transport = transport
			if tt.initial {
				updateScreenTestModel(&m, m.reportScreenViewed()())
			} else {
				updateScreenTestModel(&m, keyPressMsg('?'))
			}
			for range 50 {
				updateScreenTestModel(&m, keyPressMsg('x'))
			}
			assert.LessOrEqual(t, len(transport.screens), 1)
		})
	}
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestFailedScreenReportIsRetriedOnLaterInputAfterBackoff(t *testing.T) {
	enableTelemetryEnv(t)
	synctest.Test(t, func(t *testing.T) {
		m := newModel(testEndpointFromURL("http://127.0.0.1:7373"), withExternalIODisabled())
		transport := &screenRecordingTransport{fail: true}
		m.client.Transport = transport
		updateScreenTestModel(&m, keyPressMsg('?'))
		for range 50 {
			updateScreenTestModel(&m, keyPressMsg('x'))
		}
		require.LessOrEqual(t, len(transport.screens), 1, "a held key stays at one request")

		transport.fail = false
		time.Sleep(time.Minute)
		updateScreenTestModel(&m, keyPressMsg('x'))
		assert.Contains(t, transport.screens, "help", "later input resends the screen the daemon never took")
	})
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestScreenWaitsForTheProbeBeforeSending(t *testing.T) {
	enableTelemetryEnv(t)
	m := newModel(testEndpointFromURL("http://127.0.0.1:7373"), withExternalIODisabled())
	transport := &screenRecordingTransport{}
	m.client.Transport = transport
	cmd := m.postScreen("help")
	require.NotNil(t, cmd, "input returns a command without waiting on the daemon")
	assert.Empty(t, transport.screens)
	cmd()
	assert.Equal(t, 1, transport.opened)
	assert.Equal(t, []string{"help"}, transport.screens)
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestScreenViewedNextDayWaitsForInput(t *testing.T) {
	enableTelemetryEnv(t)
	synctest.Test(t, func(t *testing.T) {
		m := splitModel()
		m.currentReview = splitTestReview()
		m.currentView = viewReview
		m.focus = focusDetail
		transport := &screenRecordingTransport{}
		m.client.Transport = transport
		time.Sleep(24 * time.Hour)
		result, cmd := m.Update(displayTickMsg{})
		collectMsgs(cmd)
		assert.Empty(t, transport.screens)
		m = result.(model)
		updateScreenTestModel(&m, keyPressMsg('x'))
		assert.ElementsMatch(t, []string{"queue", "review"}, transport.screens)
		require.Equal(t, focusDetail, m.focus)
		_, cmd = m.Update(keyPressMsg('x'))
		collectMsgs(cmd)
		assert.Len(t, transport.screens, 2)
	})
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestSplitRunningJobShowsOnlyQueue(t *testing.T) {
	m := splitModel()
	m.currentReview = splitTestReview()
	m.selectedIdx = 0
	m.selectedJobID = m.jobs[0].ID
	m.jobs[0].Status = storage.JobStatusRunning
	require.NotEqual(t, m.selectedJobID, m.currentReview.JobID)
	assert.Equal(t, []string{"queue"}, m.screensShown())
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestSplitNavigationReportsReviewOncePerDay(t *testing.T) {
	enableTelemetryEnv(t)
	synctest.Test(t, func(t *testing.T) {
		m := splitModel()
		transport := &screenRecordingTransport{}
		m.client.Transport = transport
		updateScreenTestModel(&m, reviewMsg{review: splitTestReview(), jobID: m.selectedJobID, follow: true})
		require.Equal(t, viewQueue, m.currentView)
		require.Equal(t, focusList, m.focus)
		require.True(t, m.selectedReviewLoaded())
		assert.ElementsMatch(t, []string{"queue", "review"}, m.screensSent)
		updateScreenTestModel(&m, keyPressMsg('j'))
		require.Equal(t, []string{"queue"}, m.screensShown())
		updateScreenTestModel(&m, keyPressMsg('k'))
		updateScreenTestModel(&m, reviewMsg{review: splitTestReview(), jobID: m.selectedJobID, follow: true})
		require.Equal(t, []string{"queue", "review"}, m.screensShown())
		assert.Equal(t, []string{"review"}, transport.screens)
	})
}

// holdingTransport answers app_opened only after release and records the order of reports.
type holdingTransport struct {
	release chan struct{}
	mu      sync.Mutex
	order   []string
}

func (h *holdingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var event struct {
		Event string `json:"event"`
	}
	_ = json.NewDecoder(req.Body).Decode(&event)
	if event.Event == telemetry.EventAppOpened {
		<-h.release
	}
	h.mu.Lock()
	h.order = append(h.order, event.Event)
	h.mu.Unlock()
	return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader(`{"status":"queued"}`)), Header: make(http.Header)}, nil
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestScreenVisitedWhileProbePendingIsSentOnceItSettles(t *testing.T) {
	enableTelemetryEnv(t)
	m := newModel(testEndpointFromURL("http://127.0.0.1:7373"), withExternalIODisabled())
	transport := &holdingTransport{release: make(chan struct{})}
	m.client.Transport = transport

	var wg sync.WaitGroup
	for _, cmd := range []tea.Cmd{m.reportAppOpened(), m.postScreen("help")} {
		wg.Go(func() { cmd() })
	}
	time.Sleep(50 * time.Millisecond)
	transport.mu.Lock()
	assert.Empty(t, transport.order, "the screen waits for the probe")
	transport.mu.Unlock()

	close(transport.release)
	wg.Wait()
	assert.Equal(t, []string{telemetry.EventAppOpened, telemetry.EventScreenViewed}, transport.order)
}
