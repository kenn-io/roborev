package tui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
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
	kinds   []string
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
			s.kinds = append(s.kinds, event.Properties["kind"])
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
func TestDisabledReceiptQuietsModel(t *testing.T) {
	enableTelemetryEnv(t)
	m := newModel(testEndpointFromURL("http://127.0.0.1:7373"), withExternalIODisabled())
	transport := &screenRecordingTransport{status: "disabled"}
	m.client.Transport = transport
	close(m.ready)
	collectMsgs(m.reportScreenViewed())
	collectMsgs(m.reportAppOpened())
	collectMsgs(m.postScreen("help"))
	collectMsgs(m.reportAction(telemetry.EventReviewViewed, nil))
	m.reportSessionEnded(time.Minute)
	assert.Equal(t, []string{"queue"}, transport.screens)
	assert.Empty(t, transport.events)
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestInitialScreenFailureRetriesOnInput(t *testing.T) {
	enableTelemetryEnv(t)
	for _, tt := range []struct {
		name, screen string
		initial      bool
	}{
		{"initial queue", "queue", true}, {"help after toggle", "help", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel(testEndpointFromURL("http://127.0.0.1:7373"), withExternalIODisabled())
			transport := &screenRecordingTransport{fail: true}
			m.client.Transport = transport
			key := 'x'
			if tt.initial {
				updateScreenTestModel(&m, m.reportScreenViewed()())
			} else {
				key = '?'
				updateScreenTestModel(&m, keyPressMsg(key))
			}
			transport.fail = false
			updateScreenTestModel(&m, keyPressMsg(key))
			if !tt.initial {
				updateScreenTestModel(&m, keyPressMsg(key))
			}
			assert.Equal(t, []string{tt.screen, tt.screen}, transport.screens)
			if tt.initial {
				result, _ := m.Update(screenDeliveryMsg{screen: tt.screen, day: "2000-01-01", err: errors.New("delivery failed")})
				m = result.(model)
				assert.Contains(t, m.screensSent, tt.screen)
			}
		})
	}
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
