package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

// orderingServerModel builds a split-layout model wired to a mock daemon
// that serves job 2's review plus whatever *responses currently holds, so a
// test can change the server's answer BETWEEN executing two already-issued
// fetch commands -- the only faithful way to reproduce an out-of-order
// landing (the older request genuinely carries the older content).
func orderingServerModel(t *testing.T, responses *[]storage.Response) model {
	t.Helper()
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/review":
			// No GitRef/commit on the embedded job, so loadResponses makes
			// no legacy SHA/commit lookup and the comment list below is the
			// whole answer.
			_ = json.NewEncoder(w).Encode(storage.Review{
				ID: 10, JobID: 2, Agent: "codex", Output: "## Findings\n\n1. first finding\n",
				Job: &storage.ReviewJob{ID: 2, Status: storage.JobStatusDone},
			})
		case "/api/comments":
			_ = json.NewEncoder(w).Encode(map[string]any{"responses": *responses})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	m.width, m.height = 150, 40
	m.layout = layoutSplit
	m.tasksEnabled = true // 'F' is one of the ordinary (non-follow) dispatchers
	m.currentView = viewQueue
	m.jobs = testQueueJobs()
	m.selectedIdx, m.selectedJobID = 1, 2 // job 2: done
	m.currentReview = &storage.Review{
		ID: 10, JobID: 2, Agent: "codex", Output: "## Findings\n\n1. first finding\n",
		Job: &storage.ReviewJob{ID: 2, Status: storage.JobStatusDone},
	}
	m.currentResponses = *responses
	return m
}

func applyReviewMsg(t *testing.T, m model, msg tea.Msg) model {
	t.Helper()
	rm, ok := msg.(reviewMsg)
	require.True(t, ok, "expected a reviewMsg, got %T", msg)
	res, _ := m.handleReviewMsg(rm)
	return res.(model)
}

// TestOlderOrdinaryFetchMustNotOverwriteNewerCommentRefresh: if ordering
// covered only the split-pane FOLLOW dispatchers, an ORDINARY
// fetchReview (here the queue 'F' fix-panel fetch; equally queue Enter,
// tasks Enter/'P', stepReviewNav or the pagination auto-nav) that was
// already in flight when a comment landed could resolve AFTER the comment
// refresh and overwrite currentResponses with pre-comment content -- the
// user's just-submitted comment vanishing indefinitely.
func TestOlderOrdinaryFetchMustNotOverwriteNewerCommentRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	responses := []storage.Response{{ID: 1, Responder: "user", Response: "old comment"}}
	m := orderingServerModel(t, &responses)

	// 'F' from the split list dispatches an ORDINARY review fetch. It
	// resolves against the server as it is NOW (pre-comment) but is
	// delivered later, below.
	res, fixCmd := m.handleFixKey()
	m = res.(model)
	require.NotNil(fixCmd, "'F' must dispatch a review fetch")
	olderMsg := fixCmd()

	// The user's comment is accepted by the daemon; the split comment
	// refresh dispatches a follow fetch, which sees the new comment.
	responses = append(responses, storage.Response{ID: 2, Responder: "user", Response: "the user's new comment"})
	res, refreshCmd := m.handleCommentResultMsg(commentResultMsg{jobID: 2})
	m = res.(model)
	require.NotNil(refreshCmd, "the comment refresh must dispatch")
	newerMsg := refreshCmd()

	// The comment refresh lands first and is accepted.
	m = applyReviewMsg(t, m, newerMsg)
	require.Len(m.currentResponses, 2, "the comment refresh must land the new comment")

	// The older ordinary fetch finally resolves.
	got := applyReviewMsg(t, m, olderMsg)
	assert.Len(got.currentResponses, 2,
		"an ordinary fetch dispatched BEFORE the comment refresh must not overwrite it -- the user's comment must not disappear")
}

// TestOlderFollowResponseMustNotOverwriteNewerOrdinaryFetch is the mirror
// direction: the newest dispatch must win regardless of WHICH dispatcher
// issued either response. Before one shared epoch, an ordinary fetch did
// not advance the ordering at all, so a follow response dispatched before
// it still compared equal to the current sequence and was accepted on top
// of the fresher ordinary content.
func TestOlderFollowResponseMustNotOverwriteNewerOrdinaryFetch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	responses := []storage.Response{{ID: 1, Responder: "user", Response: "old comment"}}
	m := orderingServerModel(t, &responses)

	// A FOLLOW fetch goes out first and resolves against the old content.
	res, refreshCmd := m.handleCommentResultMsg(commentResultMsg{jobID: 2})
	m = res.(model)
	require.NotNil(refreshCmd)
	olderFollowMsg := refreshCmd()

	// An ORDINARY fetch is dispatched afterward and sees fresher content.
	responses = append(responses, storage.Response{ID: 2, Responder: "user", Response: "newer comment"})
	res, fixCmd := m.handleFixKey()
	m = res.(model)
	require.NotNil(fixCmd)
	newerOrdinaryMsg := fixCmd()

	m = applyReviewMsg(t, m, newerOrdinaryMsg)
	require.Len(m.currentResponses, 2, "the newer ordinary fetch must be accepted")

	got := applyReviewMsg(t, m, olderFollowMsg)
	assert.Len(got.currentResponses, 2,
		"a follow response dispatched BEFORE the ordinary fetch must not overwrite it")
}

// TestQueueFixKeyFlowOpensPanelWhenReviewLands is the guard rail for the
// acceptance-path fix-panel close: the legitimate queue-'F' flow, where
// the panel is pending for exactly the job whose review is arriving, must
// still open the panel.
func TestQueueFixKeyFlowOpensPanelWhenReviewLands(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	responses := []storage.Response{}
	m := orderingServerModel(t, &responses)
	m.currentReview = nil // nothing loaded yet -- 'F' from the queue list

	res, fixCmd := m.handleFixKey()
	m = res.(model)
	require.NotNil(fixCmd)
	require.True(m.reviewFixPanelPending, "'F' from the queue arms the pending panel")
	require.Equal(int64(2), m.fixPromptJobID)

	got := applyReviewMsg(t, m, fixCmd())
	assert.True(got.reviewFixPanelOpen, "the review for the panel's own job must open it")
	assert.False(got.reviewFixPanelPending)
	assert.Equal(int64(2), got.fixPromptJobID)
}

// TestAcceptedReviewForDifferentJobClosesFixPanel covers the second half of
// the stale-action finding: the fix panel is bound to fixPromptJobID, and
// the split's two panes advance independently, so a review ACCEPTED for a
// different job leaves an open (or pending) panel floating over content it
// has nothing to do with -- submitting would start a fix for the job the
// user is no longer looking at.
func TestAcceptedReviewForDifferentJobClosesFixPanel(t *testing.T) {
	arriving := &storage.Review{
		ID: 40, JobID: 3, Agent: "codex", Output: "job 3 review",
		Job: &storage.ReviewJob{ID: 3},
	}
	cases := []struct {
		name    string
		arm     func(*model)
		follow  bool
		checkOn func(*assert.Assertions, model)
	}{
		{
			name: "open panel",
			arm: func(m *model) {
				m.reviewFixPanelOpen = true
				m.reviewFixPanelFocused = true
				m.fixPromptJobID = 2
				m.fixPromptText = "please fix"
			},
			follow: true,
			checkOn: func(a *assert.Assertions, got model) {
				a.False(got.reviewFixPanelOpen, "an open panel bound to job 2 must close when job 3's review is accepted")
				a.False(got.reviewFixPanelFocused)
				a.Zero(got.fixPromptJobID)
				a.Empty(got.fixPromptText)
			},
		},
		{
			name: "pending panel",
			arm: func(m *model) {
				m.reviewFixPanelPending = true
				m.fixPromptJobID = 2
			},
			follow: false,
			checkOn: func(a *assert.Assertions, got model) {
				a.False(got.reviewFixPanelPending, "a pending panel bound to job 2 must not survive job 3's review")
				a.False(got.reviewFixPanelOpen)
				a.Zero(got.fixPromptJobID)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			m := splitModel(withSelection(0, 3)) // job 3 selected
			tc.arm(&m)
			res, _ := m.handleReviewMsg(reviewMsg{
				review: arriving, jobID: 3, follow: tc.follow,
				gen: m.detailFollowGen, dispatchedFrom: viewQueue,
			})
			got := res.(model)
			require.NotNil(t, got.currentReview)
			require.Equal(t, int64(3), got.currentReview.JobID, "the review must be accepted")
			tc.checkOn(assert, got)
		})
	}
}

// TestSplitDetailReviewActionsBlockedWhileReviewStale covers the loading
// window opened by detail-pane navigation: stepReviewNav moves
// selectedJobID immediately and only replaces currentReview once the async
// fetch lands, so every review-scoped key in between would act on the
// PREVIOUS job -- closing, commenting on, fixing, prompting, fetching a
// commit message for, or copying the wrong review.
func TestSplitDetailReviewActionsBlockedWhileReviewStale(t *testing.T) {
	require := require.New(t)

	// Three done jobs so detail nav has somewhere to go that requires an
	// async fetch (a failed job is synthesized locally, with no window).
	verdictP := "P"
	jobs := []storage.ReviewJob{
		{
			ID: 3, GitRef: "cccc333", Branch: "main", RepoName: "repoA", Agent: "codex",
			Status: storage.JobStatusDone, Verdict: &verdictP,
		},
		{
			ID: 2, GitRef: "bbbb222", Branch: "feat/x", RepoName: "repoA", Agent: "codex",
			Status: storage.JobStatusDone, Verdict: &verdictP,
		},
		{
			ID: 1, GitRef: "aaaa111", Branch: "main", RepoName: "repoB", Agent: "codex",
			Status: storage.JobStatusDone, Verdict: &verdictP,
		},
	}
	closed := false
	loaded := &storage.Review{
		ID: 10, JobID: 2, Agent: "codex", Output: "job 2 review",
		Prompt: "job 2 prompt", Closed: closed,
		Job: &storage.ReviewJob{ID: 2, GitRef: "bbbb222", Status: storage.JobStatusDone},
	}

	base := splitModel(withTestJobs(jobs...), withSelection(1, 2))
	base.tasksEnabled = true // so 'F' would really open the panel if unguarded
	base.currentView = viewReview
	base.focus = focusDetail
	base.currentReview = loaded

	// Detail nav to the newer job dispatches its fetch and leaves job 2's
	// review displayed until it lands: the stale-action window.
	res, navCmd := base.stepReviewNav(-1)
	stale := res.(model)
	require.NotNil(navCmd, "detail nav must dispatch a fetch for the newly selected job")
	require.Equal(int64(3), stale.selectedJobID, "the selection moved ahead of the loaded review")
	require.NotNil(stale.currentReview)
	require.Equal(int64(2), stale.currentReview.JobID, "the previous job's review is still what's loaded")

	cases := []struct {
		key   rune
		check func(*assert.Assertions, model, tea.Cmd)
	}{
		{'a', func(a *assert.Assertions, got model, cmd tea.Cmd) {
			a.Nil(cmd, "'a' must not fire a close against the old job")
			a.Empty(got.pendingClosed)
			a.False(got.currentReview.Closed)
		}},
		{'c', func(a *assert.Assertions, got model, cmd tea.Cmd) {
			a.NotEqual(viewKindComment, got.currentView, "'c' must not open the comment editor for the old job")
			a.Zero(got.commentJobID)
		}},
		{'F', func(a *assert.Assertions, got model, cmd tea.Cmd) {
			a.False(got.reviewFixPanelOpen, "'F' must not open a fix panel bound to the old job")
			a.Zero(got.fixPromptJobID)
		}},
		{'p', func(a *assert.Assertions, got model, cmd tea.Cmd) {
			a.NotEqual(viewKindPrompt, got.currentView, "'p' must not open the old job's prompt")
		}},
		{'m', func(a *assert.Assertions, got model, cmd tea.Cmd) {
			a.Nil(cmd, "'m' must not fetch the old job's commit message")
			a.Zero(got.commitMsgJobID)
		}},
		{'y', func(a *assert.Assertions, got model, cmd tea.Cmd) {
			a.Nil(cmd, "'y' must not copy the old job's review")
		}},
	}
	for _, tc := range cases {
		t.Run(string(tc.key), func(t *testing.T) {
			assert := assert.New(t)
			m := stale
			reviewCopy := *loaded // fresh copy: 'a' mutates the review in place
			m.currentReview = &reviewCopy
			res, cmd := m.handleKeyMsg(keyPressMsg(tc.key))
			tc.check(assert, res.(model), cmd)
		})
	}
}

// TestCtrlCloseReviewHideClosedFollowsSelection covers the control-socket
// half of "selection changes that skip the detail follow": with hideClosed
// on, closing the selected job hides it and
// the handler advances the selection itself, bypassing the shared
// detail-follow transition -- leaving the detail pane, and a fix panel
// still bound to the just-closed job, pointed at a job the list no longer
// highlights.
func TestCtrlCloseReviewHideClosedFollowsSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	newModelForClose := func(layout layoutMode) model {
		jobs := testQueueJobs()
		closed := false
		jobs[1].Closed = &closed // job 2: done, selected
		m := splitModel(withTestJobs(jobs...), withSelection(1, 2))
		m.layout = layout
		m.hideClosed = true
		m.reviewFixPanelOpen = true
		m.reviewFixPanelFocused = true
		m.fixPromptJobID = 2
		return m
	}
	params, err := json.Marshal(map[string]any{"job_id": 2, "closed": true})
	require.NoError(err)

	m := newModelForClose(layoutSplit)
	beforeGen := m.detailFollowGen
	got, resp, cmd := m.handleCtrlCloseReview(params)
	require.True(resp.OK, "expected OK, got %s", resp.Error)
	require.NotEqual(int64(2), got.selectedJobID, "closing the selected job with hideClosed on moves the selection")
	assert.Greater(got.detailFollowGen, beforeGen, "the selection change must schedule a detail follow")
	assert.NotNil(cmd, "the follow cmd must be batched with the close cmd, not dropped")
	assert.False(got.reviewFixPanelOpen, "the fix panel bound to the closed job must not survive the selection change")

	stacked := newModelForClose(layoutStacked)
	gotStacked, respStacked, _ := stacked.handleCtrlCloseReview(params)
	require.True(respStacked.OK)
	// Abandonment is layout-independent: the selection leaving job 2
	// abandons its intents in stacked too (the gen bump invalidates any
	// in-flight dispatch for it) -- only the detail-follow scheduling is
	// split-specific. The OPEN panel is the exception outside split: it is
	// bound to the review displayed full-screen, not the queue selection,
	// so the selection moving under it must not dump an in-progress fix
	// prompt (displayed-review changes close it at their own sites).
	assert.Greater(gotStacked.detailFollowGen, stacked.detailFollowGen,
		"a genuine selection change is an abandonment in any layout")
	assert.True(gotStacked.reviewFixPanelOpen,
		"an open panel outside split is review-bound; the selection moving must not close it")
}

// TestClosedResultRollbackFollowsSelection covers the other half: when the
// server rejects an optimistic close, the rollback restores the selection
// via selectJobByID -- another selection mutation that skipped the shared
// transition.
func TestClosedResultRollbackFollowsSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	newModelForRollback := func(layout layoutMode) model {
		m := splitModel(withSelection(0, 3)) // the optimistic close moved the selection to job 3
		m.layout = layout
		m.pendingClosed = map[int64]pendingState{2: {newState: true, seq: 7}}
		return m
	}
	msg := closedResultMsg{
		jobID: 2, seq: 7, oldState: false, restoreSelection: true,
		err: errors.New("daemon rejected the close"),
	}

	m := newModelForRollback(layoutSplit)
	beforeGen := m.detailFollowGen
	res, cmd := m.handleClosedResultMsg(msg)
	got := res.(model)
	require.Equal(int64(2), got.selectedJobID, "the rollback restores the selection to job 2")
	assert.Greater(got.detailFollowGen, beforeGen, "the restored selection must schedule a detail follow")
	assert.NotNil(cmd, "the follow cmd must be returned, not dropped")

	stacked := newModelForRollback(layoutStacked)
	resStacked, cmdStacked := stacked.handleClosedResultMsg(msg)
	gotStacked := resStacked.(model)
	require.Equal(int64(2), gotStacked.selectedJobID)
	// The restored selection is a genuine selection change and therefore
	// an abandonment in any layout (gen bump, intents disarmed); only the
	// follow tick is split-specific, so no cmd is scheduled in stacked.
	assert.Greater(gotStacked.detailFollowGen, stacked.detailFollowGen,
		"a genuine selection change is an abandonment in any layout")
	assert.Nil(cmdStacked, "no follow tick is scheduled in stacked")
}

// TestCancelResultRollbackFollowsSelection is the cancel-side twin of the
// close rollback above.
func TestCancelResultRollbackFollowsSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := splitModel(withSelection(0, 3))
	beforeGen := m.detailFollowGen
	res, cmd := m.handleCancelResultMsg(cancelResultMsg{
		jobID: 2, oldState: storage.JobStatusRunning, restoreSelection: true,
		err: errors.New("cancel failed"),
	})
	got := res.(model)
	require.Equal(int64(2), got.selectedJobID)
	assert.Greater(got.detailFollowGen, beforeGen, "the restored selection must schedule a detail follow")
	assert.NotNil(cmd)
}

// TestEveryReviewFetchDispatcherStampsTheSharedEpoch is the completeness
// check behind the ordering guarantee: EVERY path that can emit a reviewMsg
// -- follow and ordinary alike -- must go through dispatchReviewFetch /
// dispatchReviewFollow, so it advances the one shared epoch and stamps the
// new value on its request. A dispatcher that forgot to would be
// permanently unorderable (and, worse, permanently unacceptable, since its
// stamp could never match). It doubles as the "every dispatcher can still
// dispatch" regression net for the suppression guard.
func TestEveryReviewFetchDispatcherStampsTheSharedEpoch(t *testing.T) {
	t1 := time.Now().Truncate(time.Second)
	doneJob := func(id int64) storage.ReviewJob {
		return storage.ReviewJob{
			ID: id, GitRef: "aaaa111", RepoName: "repoA", Agent: "codex",
			Status: storage.JobStatusDone, FinishedAt: &t1,
		}
	}

	cases := []struct {
		name     string
		dispatch func() (model, tea.Model, tea.Cmd)
	}{
		{"queue Enter (stacked)", func() (model, tea.Model, tea.Cmd) {
			m := initTestModel(withCurrentView(viewQueue), withDimensions(150, 40),
				withTestJobs(doneJob(2), doneJob(1)), withSelection(0, 2))
			m.layout = layoutStacked
			res, cmd := m.handleEnterKey()
			return m, res, cmd
		}},
		{"tasks Enter", func() (model, tea.Model, tea.Cmd) {
			m := initTestModel(withCurrentView(viewTasks), withDimensions(150, 40))
			m.fixJobs = []storage.ReviewJob{{ID: 101, Status: storage.JobStatusDone}}
			res, cmd := pressSpecial(m, tea.KeyEnter)
			return m, res, cmd
		}},
		{"tasks P (parent review)", func() (model, tea.Model, tea.Cmd) {
			parentID := int64(77)
			m := initTestModel(withCurrentView(viewTasks), withDimensions(150, 40))
			m.fixJobs = []storage.ReviewJob{{ID: 101, Status: storage.JobStatusDone, ParentJobID: &parentID}}
			res, cmd := pressKey(m, 'P')
			return m, res, cmd
		}},
		{"stepReviewNav", func() (model, tea.Model, tea.Cmd) {
			m := initTestModel(withCurrentView(viewReview), withDimensions(150, 40),
				withTestJobs(doneJob(3), doneJob(2)), withSelection(1, 2))
			m.currentReview = &storage.Review{ID: 10, JobID: 2, Job: &storage.ReviewJob{ID: 2}}
			res, cmd := m.stepReviewNav(-1)
			return m, res, cmd
		}},
		{"pagination auto-nav", func() (model, tea.Model, tea.Cmd) {
			m := initTestModel(withCurrentView(viewReview), withDimensions(150, 40),
				withTestJobs(doneJob(2), doneJob(1)), withSelection(0, 2))
			m.paginateNav = viewReview
			res, cmd := m.handleJobsMsg(jobsMsg{
				jobs: []storage.ReviewJob{doneJob(2), doneJob(1)}, append: true,
			})
			return m, res, cmd
		}},
		{"queue F (fix panel fetch)", func() (model, tea.Model, tea.Cmd) {
			m := splitModel(withSelection(1, 2))
			m.tasksEnabled = true
			res, cmd := m.handleFixKey()
			return m, res, cmd
		}},
		{"comment refresh (stacked)", func() (model, tea.Model, tea.Cmd) {
			m := initTestModel(withCurrentView(viewReview), withDimensions(150, 40),
				withTestJobs(doneJob(2)), withSelection(0, 2))
			m.layout = layoutStacked
			m.currentReview = &storage.Review{ID: 10, JobID: 2, Job: &storage.ReviewJob{ID: 2}}
			res, cmd := m.handleCommentResultMsg(commentResultMsg{jobID: 2})
			return m, res, cmd
		}},
		{"comment refresh (split follow)", func() (model, tea.Model, tea.Cmd) {
			m := splitModel(withSelection(1, 2), withReview(splitTestReview()))
			res, cmd := m.handleCommentResultMsg(commentResultMsg{jobID: 2})
			return m, res, cmd
		}},
		{"detail follow tick", func() (model, tea.Model, tea.Cmd) {
			m := splitModel(withSelection(1, 2))
			m.currentReview = nil
			res, cmd := m.handleDetailFollowTick(detailFollowTickMsg{gen: m.detailFollowGen})
			return m, res, cmd
		}},
		{"jobs-refresh reconcile", func() (model, tea.Model, tea.Cmd) {
			m := splitModel(withSelection(1, 2))
			m.currentReview = nil
			res, cmd := m.splitReconcileDetail()
			return m, res, cmd
		}},
		{"pane-log completion handoff", func() (model, tea.Model, tea.Cmd) {
			m := splitModel(withSelection(1, 2))
			m.paneLogJobID, m.paneLogSeq, m.paneLogStreaming = 2, 5, true
			res, cmd := m.handlePaneLogOutputMsg(paneLogOutputMsg{jobID: 2, seq: 5, hasMore: false})
			return m, res, cmd
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before, res, cmd := tc.dispatch()
			after := res.(model)
			require.NotNil(t, cmd, "this dispatcher must still be able to dispatch")
			assert.Equal(t, before.reviewFetchSeq+1, after.reviewFetchSeq,
				"every review-content fetch must advance the one shared ordering epoch")
		})
	}
}

// TestReconcileSuppressionReleasedByItsOwnResponse pins the suppression
// contract: splitReconcileDetail holds a slot
// released by the response to ITS OWN dispatch, identified by that
// dispatch's fetch epoch -- released before any staleness check, so a
// response rejected for display still frees it, and never released by a
// foreign response, which is what would break convergence.
func TestReconcileSuppressionReleasedByItsOwnResponse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t1 := time.Now().Truncate(time.Second)

	m := splitModel(withSelection(1, 2))
	m.currentReview = nil
	m, cmd := m.splitReconcileDetail()
	require.NotNil(cmd, "reconcile dispatches for the selected done job")
	require.Equal(int64(2), m.reconcileFetchJobID, "the dispatch is tracked for suppression")

	_, cmd2 := m.splitReconcileDetail()
	assert.Nil(cmd2, "a second reconcile pass must not pile another request on top of the outstanding one")

	// A FOREIGN response for the same job must NOT release the slot: the
	// dispatch it belongs to is
	// still in flight, and freeing the gate lets the next refresh
	// supersede it.
	dispatchSeq := m.reviewFetchSeq
	m.reviewFetchSeq++ // something newer went out in the meantime
	foreign := &storage.Review{ID: 12, JobID: 2, Agent: "codex", Output: "foreign", Job: &storage.ReviewJob{ID: 2, FinishedAt: &t1}}
	resForeign, _ := m.handleReviewMsg(reviewMsg{
		review: foreign, jobID: 2, follow: true, gen: m.detailFollowGen,
		fetchSeq: m.reviewFetchSeq,
	})
	assert.Equal(int64(2), resForeign.(model).reconcileFetchJobID,
		"a different dispatch's response must leave the outstanding slot held")

	// Its OWN response releases the slot even when REJECTED for display
	// (here: superseded by the newer dispatch above).
	stale := &storage.Review{ID: 11, JobID: 2, Agent: "codex", Output: "stale", Job: &storage.ReviewJob{ID: 2, FinishedAt: &t1}}
	res, _ := m.handleReviewMsg(reviewMsg{
		review: stale, jobID: 2, follow: true, gen: m.detailFollowGen,
		fetchSeq: dispatchSeq,
	})
	got := res.(model)
	assert.Zero(got.reconcileFetchJobID, "its own response releases the slot even when dropped as stale")

	// And a failure response releases it too.
	m2 := splitModel(withSelection(1, 2))
	m2.currentReview = nil
	m2, cmd3 := m2.splitReconcileDetail()
	require.NotNil(cmd3)
	resErr, _ := m2.handleReviewFollowErrMsg(reviewFollowErrMsg{
		jobID: 2, gen: m2.detailFollowGen, fetchSeq: m2.reviewFetchSeq, err: errors.New("boom"),
	})
	assert.Zero(resErr.(model).reconcileFetchJobID, "a failed fetch releases the slot so the next pass can retry")
}

// TestJobsRefreshSelectionReassignmentClosesStaleFixPanel is the last
// selection-mutating path from the audit: handleJobsMsg reassigns the
// selection when the selected job vanishes from the refreshed list.
// splitReconcileDetail (called on the same pass) is the authority for the
// pane's CONTENT there, but the fix panel is keyed to a job, so it needs
// the same close the shared selection transition applies.
func TestJobsRefreshSelectionReassignmentClosesStaleFixPanel(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := splitModel(withSelection(1, 2)) // job 2 selected, panel open for it
	m.reviewFixPanelOpen = true
	m.reviewFixPanelFocused = true
	m.fixPromptJobID = 2
	m.fixPromptText = "please fix"

	// Job 2 vanishes from the refreshed list.
	res, _ := m.handleJobsMsg(jobsMsg{jobs: []storage.ReviewJob{
		{ID: 3, GitRef: "cccc333", RepoName: "repoA", Agent: "codex", Status: storage.JobStatusRunning},
	}})
	got := res.(model)
	require.NotEqual(int64(2), got.selectedJobID, "the vanished selection must be reassigned")
	assert.False(got.reviewFixPanelOpen, "a fix panel bound to the vanished job must not survive")
	assert.Zero(got.fixPromptJobID)
}

// TestStackedCommentRefreshForUnselectedJobDoesNotDestroyConcurrentFetch
// covers a hazard universal epoch stamping introduced in the
// stacked/tasks-origin comment-refresh branch, which deliberately
// dispatches for the DISPLAYED review rather than the selected job.
// Once its dispatch advanced the shared epoch,
// dispatching for a job that is no longer selected -- a request that
// handleReviewMsg's own jobID gate would drop on arrival anyway -- began
// superseding the concurrent, legitimate fetch for the job that IS
// selected. Stacked mode has no splitReconcileDetail to heal it, so that
// review is silently skipped entirely.
func TestStackedCommentRefreshForUnselectedJobDoesNotDestroyConcurrentFetch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/review":
			id, _ := strconv.ParseInt(r.URL.Query().Get("job_id"), 10, 64)
			_ = json.NewEncoder(w).Encode(storage.Review{
				ID: id * 10, JobID: id, Agent: "codex",
				Output: fmt.Sprintf("job %d review", id),
				Job:    &storage.ReviewJob{ID: id, Status: storage.JobStatusDone},
			})
		case "/api/comments":
			_ = json.NewEncoder(w).Encode(map[string]any{"responses": []storage.Response{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	m.width, m.height = 150, 40
	m.layout = layoutStacked
	m.currentView = viewReview
	m.reviewFromView = viewQueue
	m.jobs = []storage.ReviewJob{
		{ID: 2, GitRef: "bbbb222", RepoName: "repoA", Agent: "codex", Status: storage.JobStatusDone},
		{ID: 6, GitRef: "ffff666", RepoName: "repoA", Agent: "codex", Status: storage.JobStatusDone},
	}
	m.selectedIdx, m.selectedJobID = 0, 2
	m.currentReview = &storage.Review{
		ID: 20, JobID: 2, Agent: "codex", Output: "job 2 review",
		Job: &storage.ReviewJob{ID: 2, Status: storage.JobStatusDone},
	}

	// The user comments on job X (2), then arrow-navigates to job Y (6)
	// before the comment POST resolves. Y's fetch is now in flight.
	res, navCmd := m.stepReviewNav(1)
	m = res.(model)
	require.NotNil(navCmd, "arrowing to job 6 dispatches its review fetch")
	require.Equal(int64(6), m.selectedJobID)
	yMsg := navCmd()

	// The comment result for X arrives afterwards.
	res2, refreshCmd := m.handleCommentResultMsg(commentResultMsg{jobID: 2})
	m = res2.(model)
	assert.Nil(refreshCmd,
		"a refresh for a job that is no longer selected could never be accepted -- dispatching it only supersedes the fetch that can be")

	// Y's response finally lands. It must still be accepted.
	got := applyReviewMsg(t, m, yMsg)
	require.NotNil(got.currentReview)
	assert.Equal(int64(6), got.currentReview.JobID,
		"the review the user navigated to must not be silently skipped")
}

// TestFollowResponseOpeningPendingFixPanelMakesItReachable covers the
// second regression from moving the pending-fix-panel consume onto the
// shared acceptance path: a FOLLOW response opened the panel (open AND
// focused) without the view/focus switch an ordinary response performs,
// leaving currentView == viewQueue -- so handleKeyMsg never routed to the
// panel and the user's fix prompt was interpreted as queue keys ('f'
// opening the filter modal, and so on) over a panel that looked focused.
func TestFollowResponseOpeningPendingFixPanelMakesItReachable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := splitModel(withSelection(1, 2)) // split, LIST focus
	m.tasksEnabled = true
	m.currentReview = nil // job 2's review isn't loaded yet

	// 'F' arms the pending panel and dispatches its own ordinary fetch.
	res, fixCmd := m.handleFixKey()
	m = res.(model)
	require.NotNil(fixCmd)
	require.True(m.reviewFixPanelPending)
	require.Equal(viewQueue, m.currentView)

	// A jobs refresh lands first: reconcile dispatches a FOLLOW fetch for
	// the same job, which overtakes the 'F' fetch.
	m, reconcileCmd := m.splitReconcileDetail()
	require.NotNil(reconcileCmd, "reconcile dispatches a follow fetch for the unloaded selected job")

	res2, _ := m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, follow: true, gen: m.detailFollowGen,
		dispatchedFrom: viewQueue, fetchSeq: m.reviewFetchSeq,
	})
	got := res2.(model)
	require.True(got.reviewFixPanelOpen, "the follow response consumes the pending panel")
	require.True(got.reviewFixPanelFocused)
	assert.Equal(viewReview, got.currentView, "an opened panel must be reachable by the keyboard")
	assert.Equal(focusDetail, got.focus)

	// The decisive check: a keystroke must reach the panel, not the queue.
	res3, _ := got.handleKeyMsg(keyPressMsg('f'))
	typed := res3.(model)
	assert.Equal("f", typed.fixPromptText, "typing must go into the fix prompt")
	assert.Equal(viewReview, typed.currentView, "'f' must not open the filter modal")
}

// TestPendingFixPanelStillWaitsWhenUserMovedToATransientView is the
// guard rail for the switch added above: the pending flag is user intent
// to open the panel, but not a licence to yank a user out of a view they
// deliberately opened while the fetch was in flight (the comment editor,
// mid-typing). openReviewView's guard is what keeps those two apart.
func TestPendingFixPanelStillWaitsWhenUserMovedToATransientView(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := splitModel(withSelection(1, 2))
	m.tasksEnabled = true
	res, fixCmd := m.handleFixKey()
	m = res.(model)
	require.NotNil(fixCmd)

	// The user opens the comment editor before the fetch resolves.
	res, _ = m.handleCommentOpenKey()
	m = res.(model)
	require.Equal(viewKindComment, m.currentView)
	m.commentText = "in progress comment"

	res2, _ := m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, gen: m.detailFollowGen,
		dispatchedFrom: viewQueue, fetchSeq: m.reviewFetchSeq,
	})
	got := res2.(model)
	assert.Equal(viewKindComment, got.currentView, "must not be yanked out of the comment editor")
	assert.Equal("in progress comment", got.commentText)
	assert.True(got.reviewFixPanelOpen, "the panel is still consumed, ready for the return to the review")
}

// TestReconcileConvergesWhenAnOlderResponseForTheSameJobLands: releasing
// the reconcile suppression slot on jobID alone would let an OLDER
// response for that job unlock the gate for a dispatch still in flight.
// The next jobs refresh would then dispatch again and supersede the
// outstanding request, and with fetches slower than the refresh interval
// every response would be superseded before it landed -- the detail pane
// never converging. Suppression's only remaining job is
// exactly this liveness property, so it must be released by the matching
// response and nothing else.
func TestReconcileConvergesWhenAnOlderResponseForTheSameJobLands(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := splitModel(withSelection(1, 2)) // job 2: done, selected
	m.currentReview = nil                // the pane is still loading

	// A selection-follow fetch for job 2 goes out first and stays in
	// flight (the "older" response below).
	res, tickCmd := m.handleDetailFollowTick(detailFollowTickMsg{gen: m.detailFollowGen})
	m = res.(model)
	require.NotNil(tickCmd, "the selection-follow fetch must dispatch")
	olderSeq := m.reviewFetchSeq

	// A jobs refresh comes in: reconcile dispatches its own fetch and
	// records it as outstanding.
	m, reconcileCmd := m.splitReconcileDetail()
	require.NotNil(reconcileCmd, "reconcile dispatches for the unloaded selected job")
	reconcileSeq := m.reviewFetchSeq
	require.NotEqual(olderSeq, reconcileSeq)

	// The OLDER selection-follow response lands. It is correctly dropped
	// for display (superseded), and must NOT release the slot held by the
	// still-outstanding reconcile fetch.
	res2, _ := m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, follow: true,
		gen: m.detailFollowGen, fetchSeq: olderSeq,
	})
	m = res2.(model)
	require.Nil(m.currentReview, "the older response is superseded, so it must not land")

	// The next jobs refresh must not dispatch on top of the outstanding
	// request -- that is what would supersede it and restart the cycle.
	m, cmd2 := m.splitReconcileDetail()
	assert.Nil(cmd2, "the outstanding reconcile fetch must still suppress a duplicate dispatch")

	// So the reconcile response is still the newest when it lands, and the
	// pane converges.
	res3, _ := m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, follow: true,
		gen: m.detailFollowGen, fetchSeq: reconcileSeq,
	})
	got := res3.(model)
	require.NotNil(got.currentReview, "the detail pane must converge -- this is the non-convergence repro")
	assert.Equal(int64(2), got.currentReview.JobID)
	assert.Zero(got.reconcileFetchJobID, "its own response releases the slot")
}

// TestPendingFixPanelUsesArmedOriginNotTheAcceptingResponsesOrigin: the
// pending-panel consume must not decide its view switch from the
// ACCEPTING response's dispatchedFrom. Any dispatcher's response can
// supersede the 'F' fetch and consume the flag -- a reconcile follow
// fetch dispatched while the comment editor is open carries
// dispatchedFrom == viewKindComment, matching the user's current view and
// interrupting the comment they are typing. The decision belongs to the
// origin of the request that ARMED the flag.
func TestPendingFixPanelUsesArmedOriginNotTheAcceptingResponsesOrigin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := splitModel(withSelection(1, 2)) // split, LIST focus (viewQueue)
	m.tasksEnabled = true
	m.currentReview = nil

	// 'F' arms the pending panel from the queue.
	res, fixCmd := m.handleFixKey()
	m = res.(model)
	require.NotNil(fixCmd)
	require.True(m.reviewFixPanelPending)

	// The user opens the comment editor while that fetch is in flight.
	res, _ = m.handleCommentOpenKey()
	m = res.(model)
	require.Equal(viewKindComment, m.currentView)
	m.commentText = "half-written comment"

	// A reconcile follow fetch dispatched from THIS view supersedes the
	// 'F' fetch and is the response that consumes the pending flag.
	m, reconcileCmd := m.splitReconcileDetail()
	require.NotNil(reconcileCmd)
	res2, _ := m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, follow: true,
		gen: m.detailFollowGen, dispatchedFrom: viewKindComment,
		fetchSeq: m.reviewFetchSeq,
	})
	got := res2.(model)

	require.True(got.reviewFixPanelOpen, "the pending panel is still consumed")
	assert.Equal(viewKindComment, got.currentView,
		"the user's in-progress comment must not be interrupted by a response that merely happens to have been dispatched from this view")
	assert.Equal("half-written comment", got.commentText)

	// End state, once the user leaves the editor: the panel waits, open
	// but not yet focused-and-reachable, and the ordinary way back into
	// the detail pane (tab, which requires the review to be loaded --
	// it is, this response loaded it) hands the keyboard to it.
	got.currentView = viewQueue // Esc out of the comment editor
	res3, _ := got.handleTabKey()
	tabbed := res3.(model)
	require.Equal(viewReview, tabbed.currentView)
	require.Equal(focusDetail, tabbed.focus)
	res4, _ := tabbed.handleKeyMsg(keyPressMsg('f'))
	typed := res4.(model)
	assert.Equal("f", typed.fixPromptText, "the waiting panel takes the keyboard once the user enters the detail pane")
}

// TestTasksParentReviewOpensDespiteReconcileFollowRace: pressing 'P' on a fix task's
// parent review, with layout split, dispatches an ORDINARY fetch
// (dispatchReviewFetch) whose eventual response is what's supposed to switch
// into the review view. splitReconcileDetail gates only on
// m.layout == layoutSplit -- not on whether the split pane is actually the
// thing being rendered -- so it keeps running while the user sits on
// viewTasks. On the very next jobs refresh it observes the SAME job
// (selectedJobID already points at the parent, set by 'P' itself) with no
// matching currentReview loaded, and dispatches its OWN follow fetch for it,
// superseding the ordinary fetch's epoch. A follow response never switches
// views by itself (that's the definition of "follow"). If the now-superseded
// ordinary response -- dispatched first, so the realistic landing order --
// arrives BEFORE the follow's response lands the content, the explicit 'P'
// request must still result in the review opening once that follow response
// does land, or the user's action silently does nothing.
func TestTasksParentReviewOpensDespiteReconcileFollowRace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	parentID := int64(2) // Done job in testQueueJobs()
	m := initTestModel(withCurrentView(viewTasks), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2))
	m.layout = layoutSplit
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusDone, ParentJobID: &parentID},
	}
	m.fixSelectedIdx = 0

	// 'P': dispatches the ordinary fetch (origin viewTasks) and points
	// selectedJobID at the parent job. Doesn't switch views itself -- the
	// response is what's supposed to do that.
	got, cmd := pressKey(m, 'P')
	require.NotNil(cmd)
	require.Equal(viewTasks, got.currentView)
	require.Equal(parentID, got.selectedJobID)
	ordinarySeq := got.reviewFetchSeq
	ordinaryGen := got.detailFollowGen

	// A jobs refresh (SSE push or the periodic poll) lands next, BEFORE the
	// ordinary fetch's response arrives. splitReconcileDetail runs
	// regardless of currentView, sees the parent job (Done) selected with no
	// matching currentReview loaded, and dispatches its own follow fetch.
	res2, reconcileCmd := got.handleJobsMsg(jobsMsg{jobs: testQueueJobs(), stats: storage.JobStats{}})
	got2 := res2.(model)
	require.NotNil(reconcileCmd, "reconcile must dispatch a follow fetch for the parent job")
	require.Greater(got2.reviewFetchSeq, ordinarySeq, "the follow dispatch must have advanced the shared epoch")
	followSeq := got2.reviewFetchSeq

	// The now-superseded ordinary response arrives first (it was dispatched
	// first).
	res3, _ := got2.handleReviewMsg(reviewMsg{
		review:         &storage.Review{ID: 50, JobID: parentID, Agent: "codex", Job: &storage.ReviewJob{ID: parentID, Status: storage.JobStatusDone}},
		jobID:          parentID,
		fetchSeq:       ordinarySeq,
		gen:            ordinaryGen,
		follow:         false,
		dispatchedFrom: viewTasks,
	})
	got3 := res3.(model)
	require.Equal(viewTasks, got3.currentView,
		"content for the parent job isn't loaded yet, so this stale response can't open the review on its own")

	// The reconcile follow's response lands next, carrying the content.
	res4, _ := got3.handleReviewMsg(reviewMsg{
		review:         &storage.Review{ID: 51, JobID: parentID, Agent: "codex", Job: &storage.ReviewJob{ID: parentID, Status: storage.JobStatusDone}},
		jobID:          parentID,
		fetchSeq:       followSeq,
		gen:            got2.detailFollowGen,
		follow:         true,
		dispatchedFrom: viewQueue, // irrelevant: a follow response never switches views using its own origin
	})
	got4 := res4.(model)

	assert.Equal(viewReview, got4.currentView, "the user's explicit 'P' request must still open the parent review")
	require.NotNil(got4.currentReview)
	assert.Equal(parentID, got4.currentReview.JobID)
}

// TestTasksParentReviewOpensInStackedDespiteJobsRefresh confirms the race
// TestTasksParentReviewOpensDespiteReconcileFollowRace exercises cannot arise
// in stacked layout: splitReconcileDetail's own gate (m.layout != layoutSplit)
// makes it a no-op there, so a jobs refresh landing between 'P' and its
// response never dispatches a competing follow, and the ordinary response
// opens the review exactly as before.
func TestTasksParentReviewOpensInStackedDespiteJobsRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	parentID := int64(2)
	m := initTestModel(withCurrentView(viewTasks), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2))
	m.layout = layoutStacked
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusDone, ParentJobID: &parentID},
	}
	m.fixSelectedIdx = 0

	got, cmd := pressKey(m, 'P')
	require.NotNil(cmd)
	ordinarySeq := got.reviewFetchSeq
	ordinaryGen := got.detailFollowGen

	res2, reconcileCmd := got.handleJobsMsg(jobsMsg{jobs: testQueueJobs(), stats: storage.JobStats{}})
	got2 := res2.(model)
	assert.Nil(reconcileCmd, "splitReconcileDetail must no-op outside split layout")
	assert.Equal(ordinarySeq, got2.reviewFetchSeq, "no competing dispatch in stacked layout")

	res3, _ := got2.handleReviewMsg(reviewMsg{
		review:         &storage.Review{ID: 50, JobID: parentID, Agent: "codex", Job: &storage.ReviewJob{ID: parentID, Status: storage.JobStatusDone}},
		jobID:          parentID,
		fetchSeq:       ordinarySeq,
		gen:            ordinaryGen,
		follow:         false,
		dispatchedFrom: viewTasks,
	})
	got3 := res3.(model)
	assert.Equal(viewReview, got3.currentView, "the ordinary response opens the review directly, unaffected by reconcile")
}

// TestFollowFailureResolvesSupersededPendingReviewOpen: the reconcile
// follow that supersedes tasks 'P”s ordinary fetch
// (TestTasksParentReviewOpensDespiteReconcileFollowRace's setup) can also
// simply FAIL. Nothing else would ever resolve pendingReviewOpenJobID on
// that outcome -- "reconcile will save us" does not hold, since reconcile
// dispatches follows, which never re-arm the intent. This pins the
// terminal-outcome treatment: one bounded retry first (the intent's own
// originating ordinary dispatch may still be in flight and even its
// SUCCESS arrives fetchSeq-superseded, unable to serve the open -- see
// pendingReviewOpenRetried's doc comment), then the second failure
// resolves (clears) the intent, the same shape reviewFixPanelPending gets.
func TestFollowFailureResolvesSupersededPendingReviewOpen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	parentID := int64(2)
	m := initTestModel(withCurrentView(viewTasks), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2))
	m.layout = layoutSplit
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusDone, ParentJobID: &parentID},
	}
	m.fixSelectedIdx = 0

	got, cmd := pressKey(m, 'P')
	require.NotNil(cmd)
	require.Equal(parentID, got.pendingReviewOpenJobID, "sanity: 'P' arms the pending-open intent")

	res, reconcileCmd := got.handleJobsMsg(jobsMsg{jobs: testQueueJobs(), stats: storage.JobStats{}})
	got2 := res.(model)
	require.NotNil(reconcileCmd, "reconcile must dispatch a competing follow")
	followSeq := got2.reviewFetchSeq

	// The reconcile follow FAILS: retried once, intent survives.
	res2, retryCmd := got2.handleReviewFollowErrMsg(reviewFollowErrMsg{
		jobID: parentID, err: errors.New("boom"),
		gen: got2.detailFollowGen, fetchSeq: followSeq,
	})
	got3 := res2.(model)
	require.NotNil(retryCmd, "first failure with an armed open intent must retry")
	require.Equal(parentID, got3.pendingReviewOpenJobID)

	// The retry FAILS too: terminal, the intent is resolved.
	res3, _ := got3.handleReviewFollowErrMsg(reviewFollowErrMsg{
		jobID: parentID, err: errors.New("boom"),
		gen: got3.detailFollowGen, fetchSeq: got3.reviewFetchSeq,
	})
	got4 := res3.(model)

	assert.Equal(int64(0), got4.pendingReviewOpenJobID,
		"the retry's failure must resolve (clear) the pending-open intent, not leave it stranded with nothing in flight")
}

// TestOrdinaryFetchStillOpensReviewAfterLayoutToggleBeforeResponseLands:
// stacked queue, Enter on a done job, immediately L before the response
// lands. The response then lands gen-stale (L's bootstrap bumped
// detailFollowGen with the selection unchanged) while its fetchSeq is
// still the single freshest dispatch (nothing has re-armed) -- so
// reviewIntentRescuable rescues it: rather than being silently discarded
// (the bootstrap's own follow can be dropped with nothing else ever
// serving the intent -- see that function's doc comment), the response is
// treated as current and opens the review directly. A later, harmless
// follow response for the same job (as the bootstrap's own debounced tick
// would eventually produce, had it not been rescued already) is asserted
// not to do anything further -- the intent was already consumed.
func TestOrdinaryFetchStillOpensReviewAfterLayoutToggleBeforeResponseLands(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := initTestModel(withCurrentView(viewQueue), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2))
	m.layout = layoutStacked

	// Enter on job 2 (done): dispatches the ordinary fetch (A), arms the
	// pending-open intent.
	res, cmd := m.handleEnterKey()
	got := res.(model)
	require.NotNil(cmd)
	require.Equal(int64(2), got.pendingReviewOpenJobID)
	seqA := got.reviewFetchSeq
	genA := got.detailFollowGen

	// L, immediately, before A's response lands: toggles into split.
	// maybeBootstrapDetail's own scheduleDetailFollow call bumps
	// detailFollowGen (selection unchanged); nothing has dispatched a NEWER
	// review fetch yet, so A's fetchSeq is still current.
	res2, _ := got.handleToggleLayoutKey()
	toggled := res2.(model)
	require.Equal(layoutSplit, toggled.layout)
	require.Greater(toggled.detailFollowGen, genA, "sanity: the bootstrap must have bumped gen")
	require.Equal(int64(2), toggled.pendingReviewOpenJobID,
		"the layout toggle alone must not clear a still-valid intent for the unchanged selection")

	// A's response lands: gen-stale, but still fetchSeq-current with the
	// intent still armed -- rescued.
	res3, _ := toggled.handleReviewMsg(reviewMsg{
		review: &storage.Review{ID: 80, JobID: 2, Agent: "codex", Job: &storage.ReviewJob{ID: 2, Status: storage.JobStatusDone}},
		jobID:  2, fetchSeq: seqA, gen: genA, follow: false, dispatchedFrom: viewQueue,
	})
	afterA := res3.(model)
	assert.Equal(viewReview, afterA.currentView,
		"the user's explicit Enter must open the review even though its own response landed gen-stale")
	assert.Equal(int64(0), afterA.pendingReviewOpenJobID, "the rescued response must consume the intent")

	// A later, unrelated follow response for the same job (e.g. the
	// bootstrap's own debounced tick, had it not been overtaken by the
	// rescue above) must land harmlessly -- nothing left to consume, no
	// further view-switch surprise.
	res4, _ := afterA.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, fetchSeq: afterA.reviewFetchSeq,
		gen: afterA.detailFollowGen, follow: true, dispatchedFrom: viewQueue,
	})
	final := res4.(model)
	assert.Equal(viewReview, final.currentView, "still open, unaffected by the later follow")
}

// TestGenuineSelectionChangeDisarmsPendingReviewOpen confirms that
// disarmPendingReviewOpen, called from followSelectionChange only after
// its own prevSelected/selectedJobID comparison proves the selection
// GENUINELY changed, still fires for a real move -- distinguishing it from maybeBootstrapDetail's same-job
// bootstrap call, which deliberately does not.
func TestGenuineSelectionChangeDisarmsPendingReviewOpen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := splitModel(withSelection(1, 2)) // split layout, job 2 selected
	cmd := m.dispatchReviewFetch(2)
	require.NotNil(cmd)
	require.Equal(int64(2), m.pendingReviewOpenJobID)

	prevSelected := m.selectedJobID
	m = m.moveSelectionToJobID(3) // a genuine move to a different job
	require.NotEqual(prevSelected, m.selectedJobID)

	got, followCmd := m.followSelectionChange(prevSelected)
	require.NotNil(followCmd, "a real selection change must still schedule the debounced follow")

	assert.Equal(int64(0), got.pendingReviewOpenJobID,
		"a genuine selection change must disarm the pending-open intent for the job left behind")
}

// TestStaleOrdinaryResponseDoesNotClobberFreshlyArmedPendingOpen: keying
// pendingReviewOpenJobID's stale-rejection clear off jobID alone is
// unsound. An ordinary fetch (A) arms the intent for job 2; the
// user navigates away and back to job 2 (gen bumps, without anything
// touching pendingReviewOpenJobID in between); a FRESH ordinary fetch (B)
// for the SAME job re-arms the intent at a NEWER identity
// (pendingReviewOpenSeq). A's now-doubly-stale response finally arrives,
// carrying the OLD gen and fetchSeq -- it must not clear B's still-live
// intent, and B's own eventual response must still be able to open the
// review.
func TestStaleOrdinaryResponseDoesNotClobberFreshlyArmedPendingOpen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := initTestModel(withCurrentView(viewTasks), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2))
	m.layout = layoutSplit
	m.selectedJobID = 2 // job 2: done

	// Dispatch A: arms the pending-open intent for job 2.
	cmdA := m.dispatchReviewFetch(2)
	require.NotNil(cmdA)
	seqA := m.reviewFetchSeq
	genA := m.detailFollowGen

	// The user navigates away and back to job 2 -- the precise mechanism
	// (a real selection change, or a same-job rerun confirmation) doesn't
	// matter to this guard, only that gen has moved on since A.
	m.detailFollowGen++

	// A fresh dispatch for the SAME job re-arms the intent.
	cmdB := m.dispatchReviewFetch(2)
	require.NotNil(cmdB)
	seqB := m.reviewFetchSeq
	require.Greater(seqB, seqA)
	require.Equal(int64(2), m.pendingReviewOpenJobID, "sanity: the fresh dispatch is armed")

	// A's now-doubly-stale response finally arrives: old gen AND old
	// fetchSeq, same job (still selected).
	res, _ := m.handleReviewMsg(reviewMsg{
		review:         &storage.Review{ID: 50, JobID: 2, Agent: "codex", Job: &storage.ReviewJob{ID: 2, Status: storage.JobStatusDone}},
		jobID:          2,
		fetchSeq:       seqA,
		gen:            genA,
		follow:         false,
		dispatchedFrom: viewTasks,
	})
	got := res.(model)

	assert.Equal(int64(2), got.pendingReviewOpenJobID,
		"A's stale response must not clear B's freshly armed intent")
	assert.Equal(seqB, got.pendingReviewOpenSeq)

	// B's own eventual response (here, a follow that overtook it) lands and
	// must still be able to open the review.
	res2, _ := got.handleReviewMsg(reviewMsg{
		review:         &storage.Review{ID: 51, JobID: 2, Agent: "codex", Job: &storage.ReviewJob{ID: 2, Status: storage.JobStatusDone}},
		jobID:          2,
		fetchSeq:       seqB,
		gen:            got.detailFollowGen,
		follow:         true,
		dispatchedFrom: viewQueue,
	})
	got2 := res2.(model)
	assert.Equal(viewReview, got2.currentView, "B's intent must still open the review")
}

// TestOldFollowFailureDoesNotClobberFreshlyArmedFixPanel: the same
// jobID-only-keying hazard as the test above, but for the fix panel's
// early-rejection clear. An old follow fetch is in
// flight for job 2; the user navigates away and back (gen bumps); the user
// presses 'F' on job 2, arming a FRESH pending fix-panel request (a new
// ordinary dispatch, re-arming fixPromptSeq); the OLD follow's failure
// finally arrives, carrying the OLD gen -- it must not clear the fresh 'F'
// request, and that fresh request must still open the panel once its own
// review lands.
func TestOldFollowFailureDoesNotClobberFreshlyArmedFixPanel(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := splitModel(withSelection(1, 2)) // job 2: done, viewQueue
	m.tasksEnabled = true

	// An old follow fetch is dispatched for job 2 (e.g. by reconcile).
	oldFollowCmd := m.dispatchReviewFollow(2)
	require.NotNil(oldFollowCmd)
	oldFollowSeq := m.reviewFetchSeq
	oldGen := m.detailFollowGen

	// The user navigates away and back to job 2 -- gen bumps.
	m.detailFollowGen++

	// The user presses 'F': arms a fresh pending fix-panel request.
	res, fixCmd := m.handleFixKey()
	m = res.(model)
	require.NotNil(fixCmd)
	require.True(m.reviewFixPanelPending)
	require.Equal(int64(2), m.fixPromptJobID)
	freshFixSeq := m.fixPromptSeq
	require.Greater(freshFixSeq, oldFollowSeq)

	// The OLD follow finally fails, carrying the OLD gen.
	res2, _ := m.handleReviewFollowErrMsg(reviewFollowErrMsg{
		jobID: 2, err: errors.New("boom"), gen: oldGen, fetchSeq: oldFollowSeq,
	})
	got := res2.(model)

	require.True(got.reviewFixPanelPending,
		"the fresh 'F' request must survive the old follow's failure")
	assert.Equal(int64(2), got.fixPromptJobID)
	assert.Equal(freshFixSeq, got.fixPromptSeq)

	// The fresh request's own review response lands and still opens the
	// panel.
	res3, _ := got.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, fetchSeq: got.reviewFetchSeq,
		gen: got.detailFollowGen, follow: false, dispatchedFrom: viewQueue,
	})
	got2 := res3.(model)
	assert.True(got2.reviewFixPanelOpen, "the fresh request must still open the panel")
}

// TestOriginalDispatchGenStaleResponseDoesNotCancelInFlightFixPanelRetry:
// the retry inside handleReviewFollowErrMsg dispatches a NEW follow
// request (C) and must re-stamp fixPromptSeq to C's own identity --
// left pointing at the ORIGINAL (now-dead) dispatch A, A's late response
// would cancel C. Reproduced with ordinary
// keystrokes: stacked, 'F' on a done job (dispatch A, panel pending,
// currentReview nil) -> 'L' toggles into split, bumping detailFollowGen
// with the selection unchanged (closeFixPanelIfJobChanged no-ops, the panel
// stays pending) -> the resulting follow tick's fetch (B) for the same job
// FAILS -> the retry dispatches C -> A's long-in-flight response
// finally lands, gen-stale. It must not cancel C's still-in-flight retry,
// and C's own success must still open the panel.
func TestOriginalDispatchGenStaleResponseDoesNotCancelInFlightFixPanelRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := initTestModel(withCurrentView(viewQueue), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2)) // job 2: done
	m.layout = layoutStacked
	m.tasksEnabled = true

	// 'F': dispatch A, arms the pending fix-panel request.
	res, cmdA := m.handleFixKey()
	m = res.(model)
	require.NotNil(cmdA)
	require.True(m.reviewFixPanelPending)
	seqA := m.fixPromptSeq
	genA := m.detailFollowGen

	// 'L': toggles into split layout. currentReview is still nil (A hasn't
	// landed), so maybeBootstrapDetail schedules a follow -- bumping
	// detailFollowGen with the selection unchanged, so
	// closeFixPanelIfJobChanged no-ops and the panel stays pending.
	res2, _ := m.handleToggleLayoutKey()
	m = res2.(model)
	require.Equal(layoutSplit, m.layout)
	require.Greater(m.detailFollowGen, genA)
	require.True(m.reviewFixPanelPending, "sanity: L must not itself close the pending panel")

	// The follow tick fires, dispatching B for the same job.
	res3, followTickCmd := m.handleDetailFollowTick(detailFollowTickMsg{gen: m.detailFollowGen})
	m = res3.(model)
	require.NotNil(followTickCmd, "B must be dispatched")
	seqB := m.reviewFetchSeq

	// B fails: the bounded retry dispatches C.
	res4, retryCmd := m.handleReviewFollowErrMsg(reviewFollowErrMsg{
		jobID: 2, err: errors.New("boom"), gen: m.detailFollowGen, fetchSeq: seqB,
	})
	m = res4.(model)
	require.NotNil(retryCmd, "the #478 retry (C) must be dispatched")
	require.True(m.fixPromptFollowRetried)
	seqC := m.fixPromptSeq
	require.Greater(seqC, seqA, "the retry must re-stamp the identity to its own dispatch")

	// A's long-in-flight response finally lands, carrying the OLD gen.
	res5, _ := m.handleReviewMsg(reviewMsg{
		review: &storage.Review{ID: 60, JobID: 2, Agent: "codex", Job: &storage.ReviewJob{ID: 2, Status: storage.JobStatusDone}},
		jobID:  2, fetchSeq: seqA, gen: genA, follow: false, dispatchedFrom: viewQueue,
	})
	m = res5.(model)

	assert.True(m.reviewFixPanelPending, "A's gen-stale response must not cancel the in-flight retry (C)")
	assert.Equal(int64(2), m.fixPromptJobID)
	assert.Equal(seqC, m.fixPromptSeq)

	// C's own success lands and opens the panel.
	res6, _ := m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, fetchSeq: m.reviewFetchSeq, gen: m.detailFollowGen,
		follow: true, dispatchedFrom: viewQueue,
	})
	m = res6.(model)
	assert.True(m.reviewFixPanelOpen, "the retry's success must open the panel")
}

// TestFollowFailureAfterTasksParentReviewShowsVisibleFlashNotSilence
// reproduces the reviewer's blocker 2: resolving pendingReviewOpenJobID on
// follow failure with a bare clear made the ORIGINALLY-reported tasks-'P'
// bug silent again -- worse, permanently, since (unlike the fix panel)
// dispatchReviewFollow never re-arms this intent, so reconcile can never
// recover it. The exact 'P' repro through a follow failure must leave the
// user with VISIBLE feedback in the view they made the request from
// (viewTasks), never silence.
func TestFollowFailureAfterTasksParentReviewShowsVisibleFlashNotSilence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	parentID := int64(2)
	m := initTestModel(withCurrentView(viewTasks), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2))
	m.layout = layoutSplit
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusDone, ParentJobID: &parentID},
	}
	m.fixSelectedIdx = 0

	got, cmd := pressKey(m, 'P')
	require.NotNil(cmd)
	require.Equal(parentID, got.pendingReviewOpenJobID)

	res, reconcileCmd := got.handleJobsMsg(jobsMsg{jobs: testQueueJobs(), stats: storage.JobStats{}})
	got2 := res.(model)
	require.NotNil(reconcileCmd)
	followSeq := got2.reviewFetchSeq

	// The reconcile follow FAILS, and its bounded retry fails too.
	res2, retryCmd := got2.handleReviewFollowErrMsg(reviewFollowErrMsg{
		jobID: parentID, err: errors.New("boom"), gen: got2.detailFollowGen, fetchSeq: followSeq,
	})
	require.NotNil(retryCmd)
	res2, _ = res2.(model).handleReviewFollowErrMsg(reviewFollowErrMsg{
		jobID: parentID, err: errors.New("boom"),
		gen: res2.(model).detailFollowGen, fetchSeq: res2.(model).reviewFetchSeq,
	})
	got3 := res2.(model)

	assert.Equal(int64(0), got3.pendingReviewOpenJobID,
		"the intent must be resolved (cleared), not left silently stranded forever")
	assert.Equal(viewTasks, got3.currentView, "still on Tasks -- the review never opened")
	flash := got3.renderFlash(viewTasks)
	assert.NotEmpty(flash,
		"the user must get VISIBLE feedback in the view they made the request from, not silence")
	assert.Contains(flash, "boom")
}

// TestReviewErrMsgResolvesPendingOpenWithVisibleFlashNotSilence: an
// ordinary fetch's own outright failure is a first-party abandonment
// signal, distinct from a success that merely arrived gen-stale, and
// needs its own typed message (reviewErrMsg, mirroring
// reviewFollowErrMsg). handleReviewErrMsg resolves pendingReviewOpenJobID on
// this outcome -- clears it AND shows a visible flash on the request's
// origin view -- so a later, unrelated bootstrap follow for the
// still-selected job does not switch views (there is nothing left for it
// to serve).
func TestReviewErrMsgResolvesPendingOpenWithVisibleFlashNotSilence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := initTestModel(withCurrentView(viewQueue), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2))
	m.layout = layoutStacked

	// Queue Enter on job 2 (done): dispatches the ordinary fetch, arms the
	// pending-open intent.
	res, cmd := m.handleEnterKey()
	got := res.(model)
	require.NotNil(cmd)
	require.Equal(int64(2), got.pendingReviewOpenJobID)
	seq := got.reviewFetchSeq
	gen := got.detailFollowGen

	// The fetch fails outright: the typed reviewErrMsg lands.
	res2, _ := got.handleReviewErrMsg(reviewErrMsg{
		jobID: 2, err: errors.New("boom"), gen: gen, fetchSeq: seq,
	})
	failed := res2.(model)

	assert.Equal(int64(0), failed.pendingReviewOpenJobID,
		"the failure must resolve (clear) the pending-open intent, not leave it silently armed")
	flash := failed.renderFlash(viewQueue)
	assert.NotEmpty(flash, "the user must get visible feedback on the origin view")
	assert.Contains(flash, "boom")

	// Much later, unrelated to job 2, the user toggles into split layout.
	res3, _ := failed.handleToggleLayoutKey()
	toggled := res3.(model)
	require.Equal(layoutSplit, toggled.layout)

	// The bootstrap follow this triggers lands, carrying job 2's content.
	res4, _ := toggled.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, fetchSeq: toggled.reviewFetchSeq,
		gen: toggled.detailFollowGen, follow: true, dispatchedFrom: viewQueue,
	})
	final := res4.(model)

	assert.Equal(viewQueue, final.currentView,
		"the intent was already resolved by the failure -- the bootstrap follow must not switch views")
}

// TestFixPanelDoesNotSpringOpenAfterRerunConfirmsWhilePaneLoading:
// split, 'F' on job 2 with the pane still loading (currentReview nil), a
// rerun of job 2 confirms, then 'F”s own ordinary fetch response finally
// lands. handleRerunResultMsg's disarm must not be gated on
// currentReview != nil (false in exactly this case), and the gen-based
// early-reject deliberately does not clear the intent reactively, since
// the job never moved. The panel must not spring open, switch view, or
// steal focus for content that belongs to the pre-rerun attempt.
//
// MECHANISM NOTE: the stale response is rejected at the per-job attempt
// stamp (a confirmed rerun does not bump detailFollowGen), so the rescue
// path is never consulted here. The rescue path itself remains covered by
// TestThreeKeystrokeRescueRepro and
// TestFixPanelPendingRescuedOnGenMismatchWhenStillFreshest, which reach
// it through a bootstrap bump rather than a rerun.
func TestFixPanelDoesNotSpringOpenAfterRerunConfirmsWhilePaneLoading(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := splitModel(withSelection(1, 2)) // split, job 2 selected, viewQueue
	m.tasksEnabled = true

	// 'F' on job 2: arms the pending fix panel, dispatches ordinary fetch A.
	res, cmdA := m.handleFixKey()
	m = res.(model)
	require.NotNil(cmdA)
	require.True(m.reviewFixPanelPending)
	require.Nil(m.currentReview, "sanity: the pane is still loading")
	seqA := m.reviewFetchSeq
	genA := m.detailFollowGen

	// A rerun of job 2 confirms while the pane is still loading.
	res2, _ := m.handleRerunResultMsg(rerunResultMsg{jobID: 2})
	m = res2.(model)
	require.Positive(m.jobAttemptGen[2],
		"sanity: the rerun's per-job attempt bump must have happened (roborev #521 replaced the old detailFollowGen bump)")
	require.False(m.reviewFixPanelPending, "the rerun must resolve the pending fix panel itself")
	require.Equal(int64(0), m.fixPromptJobID)

	// A's response finally lands, now gen-stale.
	res3, _ := m.handleReviewMsg(reviewMsg{
		review: splitTestReview(), jobID: 2, fetchSeq: seqA, gen: genA,
		follow: false, dispatchedFrom: viewQueue,
	})
	got := res3.(model)

	assert.False(got.reviewFixPanelOpen, "the panel must not spring open for the pre-rerun attempt")
	assert.False(got.reviewFixPanelFocused)
	assert.Equal(viewQueue, got.currentView, "must not switch view")
	assert.Equal(focusList, got.focus, "must not steal focus")
}

// TestTasksNotYankedOutAfterRerunConfirmsParentWhilePending: tasks 'P'
// on a fix job's parent, a rerun of the parent confirms while the pane is
// still loading, then 'P”s own response finally lands stale. The user
// must not be yanked out of the Tasks list.
//
// Same mechanism note as the test above: the response is rejected by the
// per-job attempt stamp rather than as gen-stale, so this does not
// traverse reviewIntentRescuable; the property is unchanged.
func TestTasksNotYankedOutAfterRerunConfirmsParentWhilePending(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	parentID := int64(2)
	m := initTestModel(withCurrentView(viewTasks), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2))
	m.layout = layoutSplit
	m.fixJobs = []storage.ReviewJob{
		{ID: 101, Status: storage.JobStatusDone, ParentJobID: &parentID},
	}
	m.fixSelectedIdx = 0

	got, cmd := pressKey(m, 'P')
	require.NotNil(cmd)
	require.Equal(parentID, got.pendingReviewOpenJobID)
	seq := got.reviewFetchSeq
	gen := got.detailFollowGen

	// A rerun of the parent job confirms while the pane is still loading.
	res2, _ := got.handleRerunResultMsg(rerunResultMsg{jobID: parentID})
	got2 := res2.(model)
	require.Positive(got2.jobAttemptGen[parentID],
		"sanity: the rerun's per-job attempt bump must have happened")
	require.Equal(int64(0), got2.pendingReviewOpenJobID,
		"the rerun must disarm the pending-open intent itself")

	// 'P''s response finally lands, now gen-stale.
	res3, _ := got2.handleReviewMsg(reviewMsg{
		review: &storage.Review{ID: 90, JobID: parentID, Agent: "codex", Job: &storage.ReviewJob{ID: parentID, Status: storage.JobStatusDone}},
		jobID:  parentID, fetchSeq: seq, gen: gen, follow: false, dispatchedFrom: viewTasks,
	})
	final := res3.(model)

	assert.Equal(viewTasks, final.currentView,
		"the user must not be yanked out of Tasks by a stale response for a job that just reran")
}

// TestThreeKeystrokeRescueRepro is the end-to-end rescue repro: stacked,
// job 2 Done, currentReview nil -> Enter -> L within the debounce
// (bootstrap bumps gen, schedules a tick) -> L again before the tick
// fires (maybeBootstrapDetail early-returns since layout flipped back to
// stacked, so nothing NEW is scheduled) -> the outstanding tick fires and
// is DROPPED (its gen still matches, but m.layout is no longer split) ->
// A's SUCCESSFUL response finally lands, gen-stale, with nothing else
// ever having been dispatched for this job. Without the rescue, that
// response would be silently discarded and both the intent AND its
// content lost, permanently unrecoverable since nothing is left in
// flight. With it, the review opens directly
// from A's own response, and no intent survives to later "spring open"
// via an unrelated action -- verified by toggling layout again afterward
// and confirming nothing unexpected happens.
func TestThreeKeystrokeRescueRepro(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := initTestModel(withCurrentView(viewQueue), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2))
	m.layout = layoutStacked

	// Enter on job 2 (done): dispatches the ordinary fetch (A), arms the
	// pending-open intent.
	res, cmd := m.handleEnterKey()
	got := res.(model)
	require.NotNil(cmd)
	require.Equal(int64(2), got.pendingReviewOpenJobID)
	seqA := got.reviewFetchSeq
	genA := got.detailFollowGen

	// L, within the debounce: toggles into split, bumps gen, schedules a
	// tick.
	res2, _ := got.handleToggleLayoutKey()
	toggled1 := res2.(model)
	require.Equal(layoutSplit, toggled1.layout)
	require.Greater(toggled1.detailFollowGen, genA)

	// L again, before the tick fires: toggles back to stacked.
	// maybeBootstrapDetail early-returns (layout != layoutSplit), so no
	// NEW tick is scheduled -- but the FIRST tick is still outstanding.
	res3, _ := toggled1.handleToggleLayoutKey()
	toggled2 := res3.(model)
	require.Equal(layoutStacked, toggled2.layout)
	require.Equal(toggled1.detailFollowGen, toggled2.detailFollowGen,
		"sanity: the second L must not schedule another bump")

	// The outstanding tick (from the first L) fires now: its gen still
	// matches, but layout is no longer split, so it is dropped -- nothing
	// gets dispatched.
	res4, tickCmd := toggled2.handleDetailFollowTick(detailFollowTickMsg{gen: toggled2.detailFollowGen})
	dropped := res4.(model)
	require.Nil(tickCmd, "sanity: the dropped tick must not dispatch anything")
	require.Equal(int64(2), dropped.pendingReviewOpenJobID, "sanity: still armed, nothing has resolved it yet")
	require.Equal(seqA, dropped.reviewFetchSeq, "sanity: nothing new has been dispatched")

	// A's successful response finally lands, gen-stale, with nothing else
	// ever having been dispatched for this job.
	res5, _ := dropped.handleReviewMsg(reviewMsg{
		review: &storage.Review{ID: 81, JobID: 2, Agent: "codex", Job: &storage.ReviewJob{ID: 2, Status: storage.JobStatusDone}},
		jobID:  2, fetchSeq: seqA, gen: genA, follow: false, dispatchedFrom: viewQueue,
	})
	final := res5.(model)

	assert.Equal(viewReview, final.currentView, "the explicit Enter must still open the review")
	assert.Equal(int64(0), final.pendingReviewOpenJobID, "no intent must be left armed")

	// The delayed-L half: pressing L again later must not produce a
	// surprise view switch, because nothing is stranded to spring open.
	res6, _ := final.handleToggleLayoutKey()
	afterLater := res6.(model)
	assert.Equal(viewReview, afterLater.currentView, "still the same review, no surprise switch")
	assert.Equal(int64(2), afterLater.currentReview.JobID)
}

// TestRerunConfirmedWhileNotSelectedDoesNotWronglyServePendingOpen
// reproduces roborev ITEM 1: the previous round's fix hoisted
// handleRerunResultMsg's two disarm blocks so they key on msg.jobID alone
// -- but they used to be NESTED inside the SAME `m.selectedJobID ==
// msg.jobID` gate as the gen bump, so a rerun confirmed for a job that is
// NOT currently selected bumped nothing (correctly, per the gate's own
// cross-job-cost reasoning) AND disarmed nothing (a real bug: the two
// fields are already keyed on jobID, so gating their disarm on the
// SELECTION was never justified the way gating the gen bump is).
//
// Exact probe: Enter on job 2 (arms the pending-open intent) -> R
// (rerun dispatched, not yet confirmed) -> arrow to job 3 -- a REAL
// selection change, but in STACKED layout, where followSelectionChange's
// own layout gate means NOTHING proactively disarms (the same structural
// fact several Table 2 rows lean on) -- so the intent for job 2 survives,
// still armed, with the selection now on job 3 -> the rerun of job 2
// confirms while job 2 is NOT selected -> arrow back to job 2 -> L, L
// within the debounce (bumps gen once, then drops the resulting tick,
// exactly like TestThreeKeystrokeRescueRepro) -> the PRE-RERUN response
// for job 2 finally lands, gen-stale but still fetchSeq-fresh. Before this
// fix, reviewIntentRescuable found the (wrongly still-armed) intent and
// served it: the PREVIOUS attempt's review opened for a job that is
// CURRENTLY RE-RUNNING. The two existing rerun regression tests
// (TestFixPanelDoesNotSpringOpenAfterRerunConfirmsWhilePaneLoading,
// TestTasksNotYankedOutAfterRerunConfirmsParentWhilePending) both have the
// reran job SELECTED throughout, which is exactly why neither one catches
// this.
// NOTE: since the abandonment chokepoint became layout-independent
// (followSelectionChange disarms in stacked too), the arrow-away step below
// disarms the intent BEFORE the rerun ever confirms -- the scenario is now
// protected twice over. The first half of this test therefore constructs
// the armed-for-a-non-selected-job state directly (no navigation path can
// produce it anymore) to keep the rerun disarm's contract property pinned
// as defense-in-depth; the second half keeps the original end-to-end
// keystroke sequence to prove the final outcome.
func TestRerunConfirmedWhileNotSelectedDoesNotWronglyServePendingOpen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	// Contract property (jobAttemptGen clause 2b): a confirmed rerun
	// disarms the intents for ITS job, keyed on the rerun's jobID, never
	// on the selection. Constructed directly: every navigation path now
	// disarms on the selection change itself, so this state cannot arise
	// from real input -- the unconditional rerun disarm is the backstop
	// for any future path that slips past the chokepoint.
	direct := initTestModel(withCurrentView(viewQueue), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(0, 3))
	direct.layout = layoutStacked
	direct.pendingReviewOpenJobID = 2
	direct.pendingReviewOpenOrigin = viewQueue
	direct.pendingReviewOpenSeq = 1
	res, _ := direct.handleRerunResultMsg(rerunResultMsg{jobID: 2})
	reran := res.(model)
	assert.Equal(int64(0), reran.pendingReviewOpenJobID,
		"a confirmed rerun must disarm the intent for ITS job regardless of what is currently selected")

	// End to end, from real keystrokes: Enter on job 2 arms; arrowing away
	// now abandons immediately (the Finding-1 fix); the rerun, the return
	// to job 2 and the L,L toggle change nothing about that; the pre-rerun
	// response lands multiply stale (gen from the abandonment, attempt
	// from the rerun) with no intent to rescue, and is dropped.
	m := initTestModel(withCurrentView(viewQueue), withDimensions(150, 40),
		withTestJobs(testQueueJobs()...), withSelection(1, 2))
	m.layout = layoutStacked

	res2, cmd := m.handleEnterKey()
	got := res2.(model)
	require.NotNil(cmd)
	require.Equal(int64(2), got.pendingReviewOpenJobID)
	seqA := got.reviewFetchSeq
	genA := got.detailFollowGen

	moved, _ := pressSpecial(got, tea.KeyUp)
	require.Equal(int64(3), moved.selectedJobID, "sanity: selection actually moved")
	assert.Equal(int64(0), moved.pendingReviewOpenJobID,
		"the selection change abandons the intent immediately, in stacked too")

	res3, _ := moved.handleRerunResultMsg(rerunResultMsg{jobID: 2})
	reran2 := res3.(model)

	back, _ := pressSpecial(reran2, tea.KeyDown)
	require.Equal(int64(2), back.selectedJobID)

	res5, _ := back.handleToggleLayoutKey()
	toggled1 := res5.(model)
	res6, _ := toggled1.handleToggleLayoutKey()
	toggled2 := res6.(model)
	res7, tickCmd := toggled2.handleDetailFollowTick(detailFollowTickMsg{gen: toggled2.detailFollowGen})
	dropped := res7.(model)
	require.Nil(tickCmd, "sanity: the dropped tick must not dispatch anything")

	res8, _ := dropped.handleReviewMsg(reviewMsg{
		review: &storage.Review{ID: 82, JobID: 2, Agent: "codex", Job: &storage.ReviewJob{ID: 2, Status: storage.JobStatusDone}},
		jobID:  2, fetchSeq: seqA, gen: genA, follow: false, dispatchedFrom: viewQueue,
	})
	final := res8.(model)

	assert.Equal(viewQueue, final.currentView,
		"the pre-rerun response must not be wrongly served for a job that is currently re-running")
}
