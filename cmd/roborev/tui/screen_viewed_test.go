package tui

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/telemetry/posthog"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/telemetry"
)

type screenRecordingTransport struct {
	events  []string
	screens []string
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
		if event.Event == telemetry.EventScreenViewed {
			s.screens = append(s.screens, event.Properties[telemetry.PropertyScreen])
		} else {
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
		switch msg.(type) {
		case screenDeliveryMsg:
			result, _ = m.Update(msg)
			*m = result.(model)
		}
	}
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestInitialScreenFailureRetriesOnInput(t *testing.T) {
	enableTelemetryEnv(t)
	synctest.Test(t, func(t *testing.T) {
		m := newModel(testEndpointFromURL("http://127.0.0.1:7373"), withExternalIODisabled())
		transport := &screenRecordingTransport{}
		m.client.Transport = transport
		require.True(t, m.awaitTelemetryProbe())
		transport.fail = true
		updateScreenTestModel(&m, m.reportScreenViewed()())
		for range 100 {
			updateScreenTestModel(&m, keyPressMsg('x'))
		}
		assert.Equal(t, []string{"queue"}, transport.screens)
		time.Sleep(time.Second)
		transport.fail = false
		updateScreenTestModel(&m, keyPressMsg('x'))
		assert.Equal(t, []string{"queue", "queue"}, transport.screens)
	})
}

//nolint:paralleltest // telemetry environment and gate
func TestSkippedScreenDeliveryCanRetryAfterBackoff(t *testing.T) {
	enableTelemetryEnv(t)
	synctest.Test(t, func(t *testing.T) {
		m := newModel(testEndpointFromURL("http://127.0.0.1:7373"), withExternalIODisabled())
		transport := &screenRecordingTransport{}
		m.client.Transport = transport
		require.True(t, m.awaitTelemetryProbe())
		transport.fail = true
		first, overlapping := m.reportScreenViewed(), m.reportScreenViewed()
		require.NotNil(t, first)
		require.NotNil(t, overlapping)
		first()
		msg := overlapping().(screenDeliveryMsg)
		require.Equal(t, posthog.StatusSkipped, msg.status)
		updateScreenTestModel(&m, msg)
		time.Sleep(time.Second)
		transport.fail = false
		updateScreenTestModel(&m, keyPressMsg('x'))
		assert.Equal(t, []string{"queue", "queue"}, transport.screens)
	})
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestSkippedStartupScreenCanRetryAfterBackoff(t *testing.T) {
	enableTelemetryEnv(t)
	synctest.Test(t, func(t *testing.T) {
		m := newModel(testEndpointFromURL("http://127.0.0.1:7373"), withExternalIODisabled())
		transport := &screenRecordingTransport{}
		m.client.Transport = transport
		day := time.Now().UTC().Format(time.DateOnly)
		next := time.Now().UTC().Add(500 * time.Millisecond).Format(time.RFC3339Nano)
		receipt := `{"version":1,"day":"` + day + `","endpoints":{"` + m.endpoint.TelemetryScope() + `":{"events":{"screen_viewed:tui:queue":{"attempts":1,"next":"` + next + `"}}}}}`
		require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("ROBOREV_DATA_DIR"), "telemetry-client.json"), []byte(receipt), 0o600))
		telemetry.DefaultClientGate.Load()

		startup := m.reportStartupTelemetry()
		require.NotNil(t, startup)
		msg := startup().(screenDeliveryMsg)
		require.Equal(t, posthog.StatusSkipped, msg.status)
		updateScreenTestModel(&m, msg)
		assert.NotContains(t, m.screensSent, "queue")

		time.Sleep(time.Second)
		updateScreenTestModel(&m, keyPressMsg('x'))
		assert.Equal(t, []string{"queue"}, transport.screens)
	})
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
