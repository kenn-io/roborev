package tui

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/splitlayout"

	"go.kenn.io/roborev/internal/storage"
)

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestCoreActionReviewVisits(t *testing.T) {
	enableTelemetryEnv(t)
	expectViews := func(t *testing.T, transport *recordingTransport, count int) {
		t.Helper()
		var views []string
		for _, body := range transport.events {
			if strings.Contains(body, `"event":"review_viewed"`) {
				views = append(views, body)
			}
		}
		require.Len(t, views, count)
		for _, body := range views {
			assert.JSONEq(t, `{"event":"review_viewed","properties":{"surface":"tui"}}`, body)
		}
	}
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "interaction while loading and job switches", run: func(t *testing.T) {
			f := newCoreActionReviewVisitFixture()
			f.update(f.review())
			expectViews(t, f.transport, 0)
			// A key while loading remains eligible when the review response arrives.
			f.m.currentReview = nil
			f.update(tea.KeyPressMsg{Code: '0'})
			f.update(f.review())
			expectViews(t, f.transport, 1)
			f.update(tea.KeyPressMsg{Code: '?'})
			require.Equal(t, viewHelp, f.m.currentView)
			f.update(tea.KeyPressMsg{Code: 'q'})
			expectViews(t, f.transport, 1)
			f.m.selectedJobID = 43
			f.update(f.review())
			f.m.selectedJobID = 42
			f.update(f.review())
			expectViews(t, f.transport, 1)
			f.update(tea.KeyPressMsg{Code: '0'})
			expectViews(t, f.transport, 2)
		}},
		{name: "pending interaction belongs to its review", run: func(t *testing.T) {
			f := newCoreActionReviewVisitFixture()
			f.update(tea.KeyPressMsg{Code: '0'})
			f.m.selectedJobID = 43
			f.update(f.review())
			expectViews(t, f.transport, 0)
			f.m.selectedJobID = 42
			f.update(f.review())
			expectViews(t, f.transport, 0)
			f.update(tea.KeyPressMsg{Code: '0'})
			expectViews(t, f.transport, 1)
		}},
		{name: "queue interaction cannot authorize a later review", run: func(t *testing.T) {
			f := newCoreActionReviewVisitFixture()
			f.m.currentView = viewQueue
			f.m.layout = splitlayout.Stacked
			f.update(tea.KeyPressMsg{Code: '0'})
			f.m.currentView = viewReview
			f.update(f.review())
			expectViews(t, f.transport, 0)
		}},
		{name: "same-day reload after rerun", run: func(t *testing.T) {
			f := newCoreActionReviewVisitFixture()
			f.update(f.review())
			f.update(tea.KeyPressMsg{Code: '0'})
			expectViews(t, f.transport, 1)
			f.update(rerunResultMsg{jobID: f.m.selectedJobID})
			assert.Nil(t, f.m.currentReview)
			require.Equal(t, viewQueue, f.m.currentView)
			f.update(f.review())
			require.NotNil(t, f.m.currentReview)
			expectViews(t, f.transport, 1)
		}},
		{name: "one view per UTC day", run: func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newCoreActionReviewVisitFixture()
				f.update(tea.KeyPressMsg{Code: '0'})
				expectViews(t, f.transport, 0)
				time.Sleep(24 * time.Hour)
				f.update(f.review())
				expectViews(t, f.transport, 0)
				f.update(tea.KeyPressMsg{Code: '0'})
				expectViews(t, f.transport, 1)
				time.Sleep(24 * time.Hour)
				f.update(displayTickMsg{})
				expectViews(t, f.transport, 1)
				f.update(tea.KeyPressMsg{Code: '0'})
				expectViews(t, f.transport, 2)
			})
		}},
		{name: "quit before first interaction", run: func(t *testing.T) {
			f := newCoreActionReviewVisitFixture()
			f.m.currentReview = splitTestReview()
			assert.Contains(t, f.update(tea.KeyPressMsg{Code: 'q'}), tea.QuitMsg{})
			expectViews(t, f.transport, 0)
		}},
		{name: "failed job has no persisted review", run: func(t *testing.T) {
			f := newCoreActionReviewVisitFixture()
			f.update(tea.KeyPressMsg{Code: 'j'})
			require.Equal(t, int64(1), f.m.selectedJobID)
			f.update(detailFollowTickMsg{gen: f.m.detailFollowGen})
			require.NotNil(t, f.m.currentReview)
			assert.Contains(t, f.m.currentReview.Output, "Job failed")
			assert.Zero(t, f.m.currentReview.ID)
			expectViews(t, f.transport, 0)
			f.update(tea.KeyPressMsg{Code: 'k'})
			f.update(f.review())
			expectViews(t, f.transport, 1)
		}},
	} {
		t.Run(tc.name, tc.run)
	}
}

type coreActionReviewVisitFixture struct {
	m         model
	transport *recordingTransport
}

func newCoreActionReviewVisitFixture() *coreActionReviewVisitFixture {
	f := &coreActionReviewVisitFixture{m: splitModel(), transport: &recordingTransport{}}
	f.m.endpoint = testEndpointFromURL("http://127.0.0.1:7373")
	f.m.client = &http.Client{Transport: f.transport}
	f.m.api = newDaemonAPI(f.m.endpoint, f.m.client)
	return f
}

func (f *coreActionReviewVisitFixture) update(msg tea.Msg) []tea.Msg {
	result, cmd := f.m.Update(msg)
	f.m = result.(model)
	return collectMsgs(cmd)
}

func (f *coreActionReviewVisitFixture) review() reviewMsg {
	review := splitTestReview()
	review.JobID, review.Job.ID = f.m.selectedJobID, f.m.selectedJobID
	return reviewMsg{review: review, jobID: f.m.selectedJobID, follow: true, gen: f.m.detailFollowGen, fetchSeq: f.m.reviewFetchSeq, attempt: f.m.jobAttemptGen[f.m.selectedJobID]}
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestCoreActionExplicitClose(t *testing.T) {
	enableTelemetryEnv(t)
	for _, tc := range []struct {
		name         string
		closed       bool
		err          error
		count        int
		programmatic bool
	}{
		{name: "close", closed: true, count: 1},
		{name: "reopen", closed: false},
		{name: "failure", closed: true, err: errors.New("failed"), count: 1},
		{name: "reopen failure", err: errors.New("failed")},
		{name: "programmatic", programmatic: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var bodies []string
			closed := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/review/close" {
					closed = true
					w.WriteHeader(http.StatusOK)
					return
				}
				if r.URL.Path == "/api/telemetry/events" {
					body, _ := io.ReadAll(r.Body)
					bodies = append(bodies, string(body))
				}
				w.WriteHeader(http.StatusAccepted)
			}))
			defer srv.Close()
			m := newModel(testEndpointFromURL(srv.URL), withExternalIODisabled())
			if tc.programmatic {
				m.jobs = []storage.ReviewJob{makeJob(42, withClosed(new(false)))}
				updated, response, cmd := m.handleCtrlCloseReview([]byte(`{"job_id":42,"closed":true}`))
				require.True(t, response.OK)
				for _, msg := range collectMsgs(cmd) {
					result, next := updated.Update(msg)
					updated = result.(model)
					collectMsgs(next)
				}
				assert.True(t, closed)
				assert.Empty(t, bodies)
				return
			}
			m.pendingClosed[42] = pendingState{seq: 1}
			_, cmd := m.handleClosedResultMsg(closedResultMsg{jobID: 42, seq: 1, newState: tc.closed, err: tc.err})
			collectMsgs(cmd)
			require.Len(t, bodies, tc.count)
			if tc.err != nil && tc.closed {
				assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"tui","kind":"review_closed"}}`, bodies[0])
			} else if tc.count == 1 {
				assert.JSONEq(t, `{"event":"review_closed","properties":{"surface":"tui"}}`, bodies[0])
			}
		})
	}
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestCoreActionCommittedPatch(t *testing.T) {
	enableTelemetryEnv(t)
	for _, tc := range []struct {
		name       string
		result     applyPatchResultMsg
		wantEvents int
	}{
		{name: "committed", result: applyPatchResultMsg{success: true}, wantEvents: 1},
		{name: "commit failure", result: applyPatchResultMsg{commitFailed: true, err: errors.New("commit failed")}},
		{name: "mark failure", result: applyPatchResultMsg{success: true, err: errors.New("mark failed")}, wantEvents: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var bodies []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/telemetry/events" {
					body, err := io.ReadAll(r.Body)
					assert.NoError(t, err)
					if strings.Contains(string(body), `"event":"finding_fixed"`) {
						bodies = append(bodies, string(body))
					}
					w.WriteHeader(http.StatusAccepted)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"jobs":[]}`)
			}))
			defer srv.Close()
			m := newModel(testEndpointFromURL(srv.URL), withExternalIODisabled())
			_, reportCmd := m.handleApplyPatchResultMsg(tc.result)
			collectMsgs(reportCmd)
			require.Len(t, bodies, tc.wantEvents)
			for _, body := range bodies {
				assert.JSONEq(t, `{"event":"finding_fixed","properties":{"surface":"tui"}}`, body)
			}
		})
	}
}

type recordingTransport struct {
	events      []string
	reviewFails bool
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
	}
	if strings.Contains(string(body), `"review_viewed"`) || strings.Contains(string(body), `"event":"error_shown"`) {
		r.events = append(r.events, string(body))
	}
	if r.reviewFails && req.URL.Path == "/api/review" {
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader("unavailable")), Header: make(http.Header)}, nil
	}
	return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader(`{"status":"queued"}`)), Header: make(http.Header)}, nil
}

//nolint:paralleltest // telemetry opt-out variables are process-wide
func TestOutcomeTUIActions(t *testing.T) {
	enableTelemetryEnv(t)
	for _, key := range []rune{tea.KeyEnter, 'F'} {
		t.Run(map[rune]string{tea.KeyEnter: "failed open", 'F': "failed fix panel"}[key], func(t *testing.T) {
			f := newCoreActionReviewVisitFixture()
			f.m.tasksEnabled = true
			f.m.layout = splitlayout.Stacked
			f.m.currentReview = nil
			f.transport.reviewFails = true
			messages := f.update(tea.KeyPressMsg{Code: key})
			require.NotEmpty(t, messages)
			for _, msg := range messages {
				if key == 'F' {
					if failure, ok := msg.(reviewErrMsg); ok {
						msg = reviewFollowErrMsg(failure)
					}
				}
				for _, retry := range f.update(msg) {
					f.update(retry)
				}
			}
			require.Len(t, f.transport.events, 1)
			kind := "review_viewed"
			if key == 'F' {
				kind = "finding_fixed"
			}
			assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"tui","kind":"`+kind+`"}}`, f.transport.events[0])
			f.update(displayTickMsg{})
			assert.Len(t, f.transport.events, 1)
		})
	}
	t.Run("patch actions", func(t *testing.T) {
		for _, failure := range []tea.Msg{fixTriggerResultMsg{err: errors.New("unavailable")}, patchMsg{err: errors.New("unavailable")}, applyPatchResultMsg{err: errors.New("unavailable")}} {
			f := newCoreActionReviewVisitFixture()
			f.update(failure)
			require.Len(t, f.transport.events, 1)
			assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"tui","kind":"finding_fixed"}}`, f.transport.events[0])
		}
	})
}
