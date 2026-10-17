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
		require.Len(t, transport.events, count)
		for _, body := range transport.events {
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
			require.Len(t, f.transport.events, 1)
			assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"tui","kind":"review_viewed"}}`, f.transport.events[0])
			f.transport.events = nil
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
		{name: "failure", closed: true, err: errors.New("failed")},
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
			_, cmd := m.handleClosedResultMsg(closedResultMsg{jobID: 42, newState: tc.closed, err: tc.err})
			if cmd != nil {
				cmd()
			}
			require.Len(t, bodies, tc.count)
			if tc.count == 1 {
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
					bodies = append(bodies, string(body))
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

type recordingTransport struct{ events []string }

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
	return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader(`{"status":"queued"}`)), Header: make(http.Header)}, nil
}

//nolint:paralleltest // telemetry opt-out variables are process-wide
func TestOutcomeVisibleTUIErrors(t *testing.T) {
	enableTelemetryEnv(t)
	f := newCoreActionReviewVisitFixture()
	f.m.currentReview = nil
	failure := reviewFollowErrMsg{jobID: f.m.selectedJobID, gen: f.m.detailFollowGen, fetchSeq: f.m.reviewFetchSeq, attempt: f.m.jobAttemptGen[f.m.selectedJobID], err: errors.New("unavailable")}
	f.update(failure)
	require.Len(t, f.transport.events, 1)
	assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"tui","kind":"review_viewed"}}`, f.transport.events[0])
	f.update(displayTickMsg{})
	f.update(failure)
	assert.Len(t, f.transport.events, 1)
	f.m.splitDetailErr = nil
	f.update(displayTickMsg{})
	f.update(failure)
	assert.Len(t, f.transport.events, 2)
	// A superseded request cannot replace the visible error or report it again.
	failure.fetchSeq++
	f.update(failure)
	assert.Len(t, f.transport.events, 2)

	synctest.Test(t, func(t *testing.T) {
		delayed := newCoreActionReviewVisitFixture()
		delayed.m.currentView = viewQueue
		delayed.update(patchMsg{err: errors.New("patch unavailable")})
		assert.Empty(t, delayed.transport.events)
		time.Sleep(time.Second)
		delayed.m.currentView = viewTasks
		delayed.update(displayTickMsg{})
		require.Len(t, delayed.transport.events, 1)
		assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"tui","kind":"finding_fixed"}}`, delayed.transport.events[0])
		delayed.update(displayTickMsg{})
		assert.Len(t, delayed.transport.events, 1)
	})
	f.m.currentView = viewTasks
	f.update(applyPatchResultMsg{err: errors.New("apply failed")})
	require.Len(t, f.transport.events, 3)
	assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"tui","kind":"finding_fixed"}}`, f.transport.events[2])
	f.update(displayTickMsg{})
	assert.Len(t, f.transport.events, 3)
	f.update(applyPatchResultMsg{err: errors.New("apply failed")})
	assert.Len(t, f.transport.events, 4)
}

//nolint:paralleltest // telemetry opt-out variables are process-wide
func TestOutcomeFailedReviewLayouts(t *testing.T) {
	enableTelemetryEnv(t)
	for _, fullScreen := range []bool{false, true} {
		t.Run(map[bool]string{false: "split", true: "full screen"}[fullScreen], func(t *testing.T) {
			f := newCoreActionReviewVisitFixture()
			f.m.selectedJobID = 1
			f.m.currentReview = nil
			if fullScreen {
				f.m.currentView = viewReview
				f.m.layout = splitlayout.Stacked
			}
			job, ok := f.m.selectedJob()
			require.True(t, ok)
			require.Equal(t, storage.JobStatusFailed, job.Status)
			collectMsgs(f.m.acceptSynthesizedFailure(job.ID, synthesizeFailedReview(job, nil)))
			f.update(displayTickMsg{})
			require.Len(t, f.transport.events, 1)
			assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"tui","kind":"review_viewed"}}`, f.transport.events[0])
			f.update(displayTickMsg{})
			assert.Len(t, f.transport.events, 1)
			f.m.currentView = viewReview
			f.m.layout = splitlayout.Stacked
			f.update(displayTickMsg{})
			assert.Len(t, f.transport.events, 1)
			f.m.currentView = viewHelp
			f.update(displayTickMsg{})
			f.m.jobAttemptGen[job.ID]++
			f.update(displayTickMsg{})
			assert.Len(t, f.transport.events, 1)
			if fullScreen {
				f.m.currentView = viewReview
			} else {
				f.m.currentView = viewQueue
				f.m.layout = splitlayout.Split
			}
			collectMsgs(f.m.acceptSynthesizedFailure(job.ID, synthesizeFailedReview(job, nil)))
			f.update(displayTickMsg{})
			assert.Len(t, f.transport.events, 2)
		})
	}
}
