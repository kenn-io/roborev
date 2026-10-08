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

type screenRecordingTransport struct{ screens []string }

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
	return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestSplitReviewReportsScreenWithListFocus(t *testing.T) {
	enableTelemetryEnv(t)
	m := splitModel()
	transport := &screenRecordingTransport{}
	m.client.Transport = transport
	result, cmd := m.Update(reviewMsg{review: splitTestReview(), jobID: m.selectedJobID, follow: true})
	got := result.(model)
	require.Equal(t, viewQueue, got.currentView)
	require.Equal(t, focusList, got.focus)
	require.True(t, got.selectedReviewLoaded())
	collectMsgs(cmd)
	assert.Equal(t, []string{"review"}, transport.screens)
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
		collectMsgs(cmd)
		assert.ElementsMatch(t, []string{"queue", "review"}, transport.screens)
		m = result.(model)
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
			collectMsgs(cmd)
		}
		update(reviewMsg{review: splitTestReview(), jobID: m.selectedJobID, follow: true})
		update(keyPressMsg('j'))
		require.Equal(t, []string{"queue"}, m.screensShown())
		update(keyPressMsg('k'))
		update(reviewMsg{review: splitTestReview(), jobID: m.selectedJobID, follow: true})
		require.Equal(t, []string{"queue", "review"}, m.screensShown())
		assert.Equal(t, []string{"review"}, transport.screens)
	})
}
