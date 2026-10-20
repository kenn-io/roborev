package tui

import (
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/splitlayout"

	"go.kenn.io/roborev/internal/storage"
)

type coreActionReviewVisitFixture struct {
	m         model
	transport *screenRecordingTransport
}

func newCoreActionReviewVisitFixture(t *testing.T) *coreActionReviewVisitFixture {
	t.Helper()
	f := &coreActionReviewVisitFixture{m: splitModel(), transport: &screenRecordingTransport{}}
	f.m.endpoint = testEndpointFromURL("http://127.0.0.1:7373")
	f.m.client = &http.Client{Transport: f.transport}
	f.m.api = newDaemonAPI(f.m.endpoint, f.m.client)
	return f
}

func (f *coreActionReviewVisitFixture) update(msg tea.Msg) {
	updateScreenTestModel(&f.m, msg)
}

func (f *coreActionReviewVisitFixture) review() reviewMsg {
	review := splitTestReview()
	review.JobID, review.Job.ID, review.Job.JobType = f.m.selectedJobID, f.m.selectedJobID, storage.JobTypeReview
	return reviewMsg{review: review, jobID: f.m.selectedJobID, follow: true, gen: f.m.detailFollowGen, fetchSeq: f.m.reviewFetchSeq, attempt: f.m.jobAttemptGen[f.m.selectedJobID]}
}

//nolint:paralleltest // telemetry environment variables
func TestCoreActionReviewVisits(t *testing.T) {
	enableTelemetryEnv(t)
	f := newCoreActionReviewVisitFixture(t)
	f.update(f.review())
	f.update(displayTickMsg{})
	assert.Empty(t, f.transport.events)
	f.m.currentReview = nil
	f.update(tea.KeyPressMsg{Code: '0'})
	f.m.selectedJobID = 43
	f.update(f.review())
	assert.Empty(t, f.transport.events)
	f.m.layout, f.m.currentView = splitlayout.Stacked, viewReview
	f.update(tea.KeyPressMsg{Code: 'q'})
	require.Len(t, f.transport.events, 1)
}

//nolint:paralleltest // telemetry environment variables
func TestCoreActionReviewFailureIsNotRetriedByHeldKey(t *testing.T) {
	enableTelemetryEnv(t)
	f := newCoreActionReviewVisitFixture(t)
	f.update(f.review())
	f.transport.fail = true
	for range 50 {
		f.update(tea.KeyPressMsg{Code: '0'})
	}
	assert.Equal(t, []string{"review_viewed"}, f.transport.events)
}

//nolint:paralleltest // telemetry environment variables
func TestCoreActionReviewInFlightAndNextUTCDay(t *testing.T) {
	enableTelemetryEnv(t)
	synctest.Test(t, func(t *testing.T) {
		f := newCoreActionReviewVisitFixture(t)
		f.update(f.review())
		f.transport.status = "queued"
		// Hold delivery while later input exercises the in-flight gate.
		result, cmd := f.m.Update(tea.KeyPressMsg{Code: '0'})
		f.m = result.(model)
		f.update(tea.KeyPressMsg{Code: '0'})
		for _, msg := range collectMsgs(cmd) {
			f.update(msg)
		}
		f.update(tea.KeyPressMsg{Code: '0'})
		require.Equal(t, []string{"review_viewed"}, f.transport.events)
		time.Sleep(24 * time.Hour)
		f.update(displayTickMsg{})
		require.Len(t, f.transport.events, 1)
		f.update(tea.KeyPressMsg{Code: '0'})
		assert.Equal(t, []string{"review_viewed", "review_viewed"}, f.transport.events)
	})
}

//nolint:paralleltest // telemetry environment variables
func TestCoreActionDisabledReceiptSilencesTheDayThroughReconnect(t *testing.T) {
	enableTelemetryEnv(t)
	f := newCoreActionReviewVisitFixture(t)
	f.update(f.review())
	f.transport.status = "disabled"
	f.update(tea.KeyPressMsg{Code: '0'})
	require.Equal(t, []string{"review_viewed"}, f.transport.events)
	f.update(reconnectMsg{endpoint: f.m.endpoint})
	f.transport.status = "queued"
	f.update(tea.KeyPressMsg{Code: '0'})
	f.update(tea.KeyPressMsg{Code: '0'})
	assert.Equal(t, []string{"review_viewed"}, f.transport.events)
}

//nolint:paralleltest // telemetry environment variables
func TestCoreActionOpenedReviewCountsOnNextInput(t *testing.T) {
	enableTelemetryEnv(t)
	for _, tc := range []struct {
		name  string
		count int
	}{
		{name: "queue", count: 1},
		{name: "parent from tasks", count: 1},
		{name: "fix output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoreActionReviewVisitFixture(t)
			f.m.layout = splitlayout.Stacked
			f.m.currentView = viewQueue
			f.m.tasksEnabled = true
			origin := viewQueue
			key := tea.KeyPressMsg{Code: tea.KeyEnter}
			if tc.name != "queue" {
				origin = viewTasks
				f.m.currentView = viewTasks
				f.m.fixJobs = []storage.ReviewJob{{ID: 9, Status: storage.JobStatusDone, JobType: storage.JobTypeFix, ParentJobID: new(int64(42))}}
				f.m.fixSelectedIdx = 0
				if tc.name == "parent from tasks" {
					key = tea.KeyPressMsg{Code: 'P'}
				}
			}
			f.update(key)
			response := f.review()
			response.follow, response.dispatchedFrom = false, origin
			if tc.name == "fix output" {
				response.review.Job.JobType = storage.JobTypeFix
			}
			f.update(response)
			require.Equal(t, viewReview, f.m.currentView)
			assert.Empty(t, f.transport.events, "opening and loading are not a read")
			f.update(tea.KeyPressMsg{Code: 'j'})
			assert.Len(t, f.transport.events, tc.count)
		})
	}
}

//nolint:paralleltest // telemetry environment variables
func TestCoreActionExplicitClose(t *testing.T) {
	enableTelemetryEnv(t)
	for _, tc := range []struct {
		name   string
		result closedResultMsg
		count  int
	}{
		{name: "queue key", result: closedResultMsg{newState: true, fromKey: true}, count: 1},
		{name: "key", result: closedResultMsg{newState: true, fromKey: true}, count: 1},
		{name: "control socket", result: closedResultMsg{newState: true}},
		{name: "reopen", result: closedResultMsg{fromKey: true}},
		{name: "failure", result: closedResultMsg{newState: true, fromKey: true, err: errors.New("failed")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoreActionReviewVisitFixture(t)
			if tc.name == "failure" {
				_, cmd := f.m.handleClosedResultMsg(tc.result)
				collectMsgs(cmd)
			} else {
				var result tea.Model
				var cmd tea.Cmd
				if tc.name == "control socket" {
					f.m.jobs = []storage.ReviewJob{makeJob(42, withClosed(new(false)))}
					updated, response, next := f.m.handleCtrlCloseReview([]byte(`{"job_id":42,"closed":true}`))
					require.True(t, response.OK)
					result, cmd = updated, next
				} else {
					f.m.currentReview = f.review().review
					f.m.currentReview.Closed = !tc.result.newState
					f.m.currentView = viewReview
					if tc.name == "queue key" {
						f.m.jobs = []storage.ReviewJob{makeJob(42, withClosed(new(false)))}
						f.m.selectedIdx, f.m.selectedJobID = 0, 42
						f.m.currentView = viewQueue
					}
					result, cmd = f.m.handleCloseKey()
				}
				f.m = result.(model)
				for _, msg := range collectMsgs(cmd) {
					f.update(msg)
				}
			}
			require.Len(t, f.transport.events, tc.count)
			if tc.count > 0 {
				assert.Equal(t, "review_closed", f.transport.events[0])
			}
		})
	}
}

//nolint:paralleltest // telemetry environment variables
func TestCoreActionCommittedPatch(t *testing.T) {
	enableTelemetryEnv(t)
	for _, tc := range []struct {
		name   string
		result applyPatchResultMsg
		count  int
	}{
		{name: "committed", result: applyPatchResultMsg{success: true}, count: 1},
		{name: "commit failure", result: applyPatchResultMsg{success: true, commitFailed: true, err: errors.New("commit failed")}},
		{name: "bookkeeping failure", result: applyPatchResultMsg{success: true, err: errors.New("mark failed")}, count: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoreActionReviewVisitFixture(t)
			_, cmd := f.m.handleApplyPatchResultMsg(tc.result)
			collectMsgs(cmd)
			require.Len(t, f.transport.events, tc.count)
			if tc.count > 0 {
				assert.Equal(t, "finding_fixed", f.transport.events[0])
			}
		})
	}
}

//nolint:paralleltest // telemetry environment variables
func TestCoreActionUnreadReviews(t *testing.T) {
	enableTelemetryEnv(t)
	for _, outcome := range []string{"load failed", "quit: ctrl+c", "quit: ctrl+d"} {
		t.Run(outcome, func(t *testing.T) {
			f := newCoreActionReviewVisitFixture(t)
			response := f.review()
			switch outcome {
			case "load failed":
				f.update(tea.KeyPressMsg{Code: '0'})
				f.update(reviewErrMsg{jobID: response.jobID, gen: response.gen, fetchSeq: response.fetchSeq, attempt: response.attempt, err: errors.New("load failed")})
				f.update(tea.KeyPressMsg{Code: '0'})
			case "quit: ctrl+c":
				f.update(response)
				f.update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
			case "quit: ctrl+d":
				f.update(response)
				f.update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
			}
			assert.Empty(t, f.transport.events)
		})
	}
}
