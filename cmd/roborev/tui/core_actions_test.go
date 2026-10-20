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
func TestCoreActionReviewDeliveryRetriesOnInput(t *testing.T) {
	enableTelemetryEnv(t)
	f := newCoreActionReviewVisitFixture(t)
	f.update(f.review())
	f.transport.fail = true
	for range 2 {
		f.update(tea.KeyPressMsg{Code: '0'})
	}
	require.Len(t, f.transport.events, 2)
	f.transport.fail = false
	f.update(displayTickMsg{})
	require.Len(t, f.transport.events, 2)
	f.update(tea.KeyPressMsg{Code: '0'})
	assert.Equal(t, []string{"review_viewed", "review_viewed", "review_viewed"}, f.transport.events)
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
func TestCoreActionReviewReconnectResumesReports(t *testing.T) {
	enableTelemetryEnv(t)
	f := newCoreActionReviewVisitFixture(t)
	f.update(f.review())
	f.transport.status = "disabled"
	f.update(tea.KeyPressMsg{Code: '0'})
	require.Equal(t, []string{"review_viewed"}, f.transport.events)
	f.update(reconnectMsg{err: errors.New("no daemon found")})
	f.update(tea.KeyPressMsg{Code: '0'})
	require.Len(t, f.transport.events, 1)
	f.update(reconnectMsg{endpoint: f.m.endpoint})
	f.transport.status = "queued"
	require.Len(t, f.transport.events, 1)
	f.update(tea.KeyPressMsg{Code: '0'})
	require.Equal(t, []string{"review_viewed", "review_viewed"}, f.transport.events)
	f.update(tea.KeyPressMsg{Code: '0'})
	assert.Len(t, f.transport.events, 2)
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
		{name: "failure", result: closedResultMsg{newState: true, fromKey: true, err: errors.New("failed")}, count: 1},
		{name: "reopen failure", result: closedResultMsg{fromKey: true, err: errors.New("failed")}, count: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoreActionReviewVisitFixture(t)
			if tc.result.err != nil {
				tc.result.jobID, tc.result.seq = 42, 1
				f.m.pendingClosed[42] = pendingState{seq: 1}
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
				if tc.result.err != nil {
					assert.Equal(t, "error_shown", f.transport.events[0])
					assert.Equal(t, "review_closed", f.transport.kinds[0])
				} else {
					assert.Equal(t, "review_closed", f.transport.events[0])
				}
			}
		})
	}
}

//nolint:paralleltest // telemetry environment variables
func TestCoreActionFixOutcomes(t *testing.T) {
	enableTelemetryEnv(t)
	for _, tc := range []struct {
		name   string
		result tea.Msg
		events []string
		kinds  []string
	}{
		{name: "enqueue failure", result: fixTriggerResultMsg{err: errors.New("enqueue failed")}, events: []string{"error_shown"}, kinds: []string{"finding_fixed"}},
		{name: "committed", result: applyPatchResultMsg{success: true}, events: []string{"finding_fixed"}, kinds: []string{""}},
		{name: "commit failure", result: applyPatchResultMsg{success: true, commitFailed: true, err: errors.New("commit failed")}, events: []string{"error_shown"}, kinds: []string{"finding_fixed"}},
		{name: "apply failure", result: applyPatchResultMsg{err: errors.New("apply failed")}, events: []string{"error_shown"}, kinds: []string{"finding_fixed"}},
		{name: "bookkeeping failure", result: applyPatchResultMsg{success: true, err: errors.New("mark failed")}, events: []string{"finding_fixed", "error_shown"}, kinds: []string{"", "finding_fixed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoreActionReviewVisitFixture(t)
			var cmd tea.Cmd
			switch result := tc.result.(type) {
			case applyPatchResultMsg:
				_, cmd = f.m.handleApplyPatchResultMsg(result)
			case fixTriggerResultMsg:
				var updated tea.Model
				updated, cmd = f.m.handleFixTriggerResultMsg(result)
				assert.Contains(t, updated.(model).renderTasksView(), "Fix failed: enqueue failed")
			}
			collectMsgs(cmd)
			assert.Equal(t, tc.events, f.transport.events)
			assert.Equal(t, tc.kinds, f.transport.kinds)
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

//nolint:paralleltest // telemetry environment variables
func TestCoreActionReviewOpenFailure(t *testing.T) {
	enableTelemetryEnv(t)
	for _, mode := range []string{"ordinary", "follow", "fix panel"} {
		t.Run(mode, func(t *testing.T) {
			f := newCoreActionReviewVisitFixture(t)
			response := f.review()
			f.m.pendingReviewOpenJobID = response.jobID
			msg := reviewErrMsg{
				jobID: response.jobID, gen: response.gen, fetchSeq: response.fetchSeq,
				attempt: response.attempt, err: errors.New("load failed"),
			}
			var cmd tea.Cmd
			if mode != "ordinary" {
				f.m.pendingReviewOpenRetried = true
				if mode == "fix panel" {
					f.m.reviewFixPanelPending = true
					f.m.fixPromptJobID = response.jobID
					f.m.fixPromptFollowRetried = true
				}
				_, cmd = f.m.handleReviewFollowErrMsg(reviewFollowErrMsg(msg))
			} else {
				_, cmd = f.m.handleReviewErrMsg(msg)
			}
			collectMsgs(cmd)
			assert.Equal(t, []string{"error_shown"}, f.transport.events)
			assert.Equal(t, []string{"review_viewed"}, f.transport.kinds)
		})
	}
}

//nolint:paralleltest // telemetry environment variables
func TestCoreActionSplitPaneLoadFailureReportsOncePerSelection(t *testing.T) {
	enableTelemetryEnv(t)
	f := newCoreActionReviewVisitFixture(t)
	fail := func() {
		response := f.review()
		updated, cmd := f.m.handleReviewFollowErrMsg(reviewFollowErrMsg{
			jobID: response.jobID, gen: response.gen, fetchSeq: response.fetchSeq,
			attempt: response.attempt, err: errors.New("load failed"),
		})
		f.m = updated.(model)
		collectMsgs(cmd)
	}
	fail()
	fail() // A background retry shows the same failure.
	assert.Equal(t, []string{"error_shown"}, f.transport.events)
	f.m, _ = f.m.scheduleDetailFollow()
	fail()
	assert.Equal(t, []string{"error_shown", "error_shown"}, f.transport.events)
	assert.Equal(t, []string{"review_viewed", "review_viewed"}, f.transport.kinds)
}
