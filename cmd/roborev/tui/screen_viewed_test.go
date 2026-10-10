package tui

import (
	"encoding/json"
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
	screens []string
	fail    bool
	status  string
}

func (s *screenRecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/api/telemetry/events" {
		var event struct {
			Properties map[string]string `json:"properties"`
		}
		if err := json.NewDecoder(req.Body).Decode(&event); err != nil {
			return nil, err
		}
		s.screens = append(s.screens, event.Properties[telemetry.PropertyScreen])
	}
	status := http.StatusAccepted
	if s.fail {
		status = http.StatusServiceUnavailable
	}
	deliveryStatus := s.status
	if deliveryStatus == "" {
		deliveryStatus = "queued"
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"status":"` + deliveryStatus + `"}`)), Header: make(http.Header)}, nil
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
			update := func(msg tea.Msg) {
				result, cmd := m.Update(msg)
				m = result.(model)
				for _, msg := range collectMsgs(cmd) {
					result, _ = m.Update(msg)
					m = result.(model)
				}
			}
			key := 'x'
			if tt.initial {
				update(m.reportScreenViewed()())
			} else {
				key = '?'
				update(keyPressMsg(key))
			}
			transport.fail = false
			update(keyPressMsg(key))
			if !tt.initial {
				update(keyPressMsg(key))
			}
			assert.Equal(t, []string{tt.screen, tt.screen}, transport.screens)
			update(keyPressMsg(key))
			if !tt.initial {
				update(keyPressMsg(key))
			}
			assert.Len(t, transport.screens, 2)
		})
	}
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestScreenDeliveryWaitsForAcceptanceOnMatchingDay(t *testing.T) {
	enableTelemetryEnv(t)
	m := newModel(testEndpointFromURL("http://127.0.0.1:7373"), withExternalIODisabled())
	transport := &screenRecordingTransport{}
	m.client.Transport = transport
	_, cmd := m.Update(keyPressMsg('x'))
	collectMsgs(cmd)
	assert.Empty(t, transport.screens)
	assert.Empty(t, m.screensSent)
	result, _ := m.Update(screenDeliveryMsg{screen: "queue", day: m.screenDay})
	m = result.(model)
	assert.Equal(t, []string{"queue"}, m.screensSent)
	assert.Empty(t, m.screensPending)
	m.screensSent = nil
	m.screensPending = []string{"queue"}
	result, _ = m.Update(screenDeliveryMsg{screen: "queue", day: "2000-01-01"})
	m = result.(model)
	assert.Empty(t, m.screensSent)
	assert.Equal(t, []string{"queue"}, m.screensPending)
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestScreenDeliveryCompletesOnSuccess(t *testing.T) {
	enableTelemetryEnv(t)
	for _, status := range []string{"queued", "disabled", "skipped", "unknown"} {
		t.Run(status, func(t *testing.T) {
			m := newModel(testEndpointFromURL("http://127.0.0.1:7373"), withExternalIODisabled())
			transport := &screenRecordingTransport{status: status}
			m.client.Transport = transport
			result, _ := m.Update(m.reportScreenViewed()())
			m = result.(model)
			if status != "unknown" {
				assert.Equal(t, []string{"queue"}, m.screensSent)
			} else {
				assert.Empty(t, m.screensSent)
			}
			assert.Empty(t, m.screensPending)
			_, cmd := m.Update(keyPressMsg('x'))
			collectMsgs(cmd)
			wantPosts := 1
			if status == "unknown" {
				wantPosts = 2
			}
			assert.Len(t, transport.screens, wantPosts)
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
		result, cmd = m.Update(keyPressMsg('x'))
		m = result.(model)
		for _, msg := range collectMsgs(cmd) {
			if delivery, ok := msg.(screenDeliveryMsg); ok {
				result, _ = m.Update(delivery)
				m = result.(model)
			}
		}
		assert.ElementsMatch(t, []string{"queue", "review"}, transport.screens)
		assert.ElementsMatch(t, []string{"queue", "review"}, m.screensSent)
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
		update := func(msg tea.Msg) {
			result, cmd := m.Update(msg)
			m = result.(model)
			for _, msg := range collectMsgs(cmd) {
				if delivery, ok := msg.(screenDeliveryMsg); ok {
					result, _ = m.Update(delivery)
					m = result.(model)
				}
			}
		}
		update(reviewMsg{review: splitTestReview(), jobID: m.selectedJobID, follow: true})
		require.Equal(t, viewQueue, m.currentView)
		require.Equal(t, focusList, m.focus)
		require.True(t, m.selectedReviewLoaded())
		assert.Equal(t, []string{"review"}, m.screensSent)
		update(keyPressMsg('j'))
		require.Equal(t, []string{"queue"}, m.screensShown())
		update(keyPressMsg('k'))
		update(reviewMsg{review: splitTestReview(), jobID: m.selectedJobID, follow: true})
		require.Equal(t, []string{"queue", "review"}, m.screensShown())
		assert.Equal(t, []string{"review"}, transport.screens)
		time.Sleep(24 * time.Hour)
		transport.fail = true
		update(keyPressMsg('x'))
		assert.Empty(t, m.screensSent)
		assert.Empty(t, m.screensPending)
		transport.fail = false
		update(keyPressMsg('x'))
		assert.ElementsMatch(t, []string{"queue", "review"}, m.screensSent)
		assert.Equal(t, []string{"review", "queue", "review", "queue", "review"}, transport.screens)
		update(keyPressMsg('x'))
		assert.Len(t, transport.screens, 5)
	})
}
