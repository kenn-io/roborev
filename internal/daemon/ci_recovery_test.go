package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
)

func TestCIPanelRerunPublishesReview(t *testing.T) {
	for _, outcome := range []string{storage.PanelOutcomeNoReviewPosted, storage.PanelOutcomeReviewPosted} {
		t.Run(outcome, func(t *testing.T) {
			h := newCIPollerHarness(t, "https://github.com/acme/api.git")
			comments := h.CaptureComments()
			statuses := h.CaptureCommitStatuses()
			spec := jobSpec{Agent: "test", Status: "failed", Error: "model unavailable"}
			if outcome == storage.PanelOutcomeReviewPosted {
				spec = jobSpec{Agent: "test", Status: "done", Output: "No issues found."}
			}
			panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "reviewed-head", "base..reviewed-head",
				[]jobSpec{spec})
			if outcome == storage.PanelOutcomeReviewPosted {
				h.completeSynthesisWithReview(t, synth.ID, "No issues found.")
			} else {
				h.markJobFailed(t, synth.ID, "no member output")
			}
			require.NoError(t, h.DB.MarkPanelPosted(panel.ID, outcome))
			server := newServerWithLogs(h.DB, h.Cfg, "", newTestErrorLog(), newTestActivityLog())
			t.Cleanup(func() { require.NoError(t, server.Close()) })

			request := []byte(fmt.Sprintf(`{"job_id":%d,"request_id":%q}`, synth.ID, testUUID("ci-rerun")))
			response := serveHuma(t, server, http.MethodPost, "/api/job/rerun", request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var rerun RerunJobOutput
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &rerun.Body))
			attempt, err := h.DB.GetReviewAttempt("acme/api", 7, "reviewed-head")
			require.NoError(t, err)
			assert.Equal(t, "pending", attempt.State, "explicit rerun reopens the CI attempt")
			members, err := h.DB.GetPanelMembers(*rerun.Body.RunUUID)
			require.NoError(t, err)
			require.Len(t, members, 1)
			assert.Equal(t, "base..reviewed-head", members[0].GitRef)
			h.markJobDoneWithReview(t, members[0].ID, "test", "No issues found.")
			h.completeSynthesisWithReview(t, rerun.Body.JobID, "No issues found.")

			// A duplicate API request and completion event must not publish twice.
			replayed := serveHuma(t, server, http.MethodPost, "/api/job/rerun", request)
			require.Equal(t, http.StatusOK, replayed.Code)
			coalesced := serveHuma(t, server, http.MethodPost, "/api/job/rerun",
				[]byte(fmt.Sprintf(`{"job_id":%d}`, synth.ID)))
			require.Equal(t, http.StatusOK, coalesced.Code)
			var duplicate RerunJobOutput
			require.NoError(t, json.Unmarshal(coalesced.Body.Bytes(), &duplicate.Body))
			assert.Equal(t, rerun.Body.JobID, duplicate.Body.JobID, "unpublished successor still owns the request")
			h.Poller.handleReviewCompleted(ciEvent(rerun.Body.JobID, "review.completed"))
			h.Poller.handleReviewCompleted(ciEvent(rerun.Body.JobID, "review.completed"))
			require.Len(t, *comments, 1, "CI rerun must reach the GitHub publisher")
			assert.Equal(t, "acme/api", (*comments)[0].Repo)
			assert.Equal(t, 7, (*comments)[0].PR)
			require.Len(t, *statuses, 1)
			assert.Equal(t, "reviewed-head", (*statuses)[0].SHA)
			assert.Contains(t, (*comments)[0].Body, "No issues found.")
			original, err := h.DB.GetCIPanelByRunUUID(panel.PanelRunUUID)
			require.NoError(t, err)
			assert.Equal(t, outcome, *original.Outcome)
			assert.NotNil(t, original.RetiredAt)
			failures, err := h.DB.GetFailedReviewAttempts("acme/api")
			require.NoError(t, err)
			assert.Empty(t, failures, "historical failures must not obscure delivered recovery")
		})
	}
}

func TestCIPollerRestartRecoversExhaustedReview(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Cfg.CI.Agents = []string{"test"}
	h.stubProcessPRGit()
	comments := h.CaptureComments()
	pr := ghPR{Number: 7, HeadRefOid: "retry-head", BaseRefName: "main"}
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", pr.Number, pr.HeadRefOid, "base..retry-head",
		[]jobSpec{{Agent: "test", Status: "failed", Error: "model unavailable"}})
	h.markJobFailed(t, synth.ID, "no member output")
	for range 2 {
		require.NoError(t, h.DB.DeferReviewAttempt("acme/api", pr.Number, pr.HeadRefOid,
			"genuine", "model unavailable", &panel.PanelRunUUID, time.Now(), true))
	}
	h.Poller.handleReviewFailed(ciEvent(synth.ID, "review.failed"))
	attempt, err := h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
	require.NoError(t, err)
	require.Equal(t, "done", attempt.State)

	// Restart grants a fresh bounded budget after an operator repairs the agent.
	require.NoError(t, h.Poller.Start())
	h.Poller.Stop()
	attempt, err = h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
	require.NoError(t, err)
	require.Equal(t, "deferred", attempt.State)
	failures, err := h.DB.GetFailedReviewAttempts("acme/api")
	require.NoError(t, err)
	require.Len(t, failures, 1, "rearming must retain failure health until delivery")

	// A fetch failure after claiming the recovery must not strand it behind
	// the previous terminal panel's posted_at marker.
	h.Poller.gitFetchFn = func(context.Context, string, []string) error { return errors.New("fetch unavailable") }
	require.NoError(t, h.Poller.retryDueReviewAttempts(context.Background(), "acme/api", []ghPR{pr}, h.Cfg))
	require.NoError(t, h.Poller.reconcileStuckAttempts("acme/api"))
	attempt, err = h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
	require.NoError(t, err)
	require.Equal(t, "deferred", attempt.State, "failed recovery enqueue must remain retryable")
	h.stubProcessPRGit()

	// If repair did not help, the normal three-failure budget still stops work.
	for range 3 {
		_, err = h.DB.Exec(`UPDATE ci_pr_review_attempts SET next_attempt_at = datetime('now','-1 hour')`)
		require.NoError(t, err)
		require.NoError(t, h.Poller.retryDueReviewAttempts(context.Background(), "acme/api", []ghPR{pr}, h.Cfg))
		current, err := h.DB.GetActiveCIPanelByPRSHA("acme/api", pr.Number, pr.HeadRefOid)
		require.NoError(t, err)
		members, err := h.DB.GetPanelMembers(current.PanelRunUUID)
		require.NoError(t, err)
		for _, member := range members {
			h.markJobFailed(t, member.ID, "model unavailable")
		}
		h.markJobFailed(t, *current.SynthesisJobID, "no member output")
		h.Poller.handleReviewFailed(ciEvent(*current.SynthesisJobID, "review.failed"))
	}
	attempt, err = h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
	require.NoError(t, err)
	require.Equal(t, "done", attempt.State)
	require.NoError(t, h.Poller.processPR(context.Background(), "acme/api", pr, h.Cfg))
	require.NoError(t, h.Poller.retryDueReviewAttempts(context.Background(), "acme/api", []ghPR{pr}, h.Cfg))
	attempt, err = h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
	require.NoError(t, err)
	require.Equal(t, "done", attempt.State, "normal polling must not reset an exhausted budget")

	require.NoError(t, h.Poller.Start())
	h.Poller.Stop()
	require.NoError(t, h.Poller.retryDueReviewAttempts(context.Background(), "acme/api", []ghPR{pr}, h.Cfg))
	id := h.drivePanelOutcome(t, "acme/api", pr.Number, pr.HeadRefOid, "done")
	h.Poller.handleReviewCompleted(ciEvent(id, "review.completed"))
	require.Len(t, *comments, 1)
	assert.Equal(t, "acme/api", (*comments)[0].Repo)
	assert.Equal(t, pr.Number, (*comments)[0].PR)
	failures, err = h.DB.GetFailedReviewAttempts("acme/api")
	require.NoError(t, err)
	assert.Empty(t, failures)
}

func TestCIPollerHealthWaitsForInitialPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newCIPollerHarness(t, "https://github.com/acme/api.git")
		h.Cfg.CI.Repos = []string{"acme/api"}
		h.Poller.repoResolver.canonicalRepoFn = nil
		release := make(chan struct{})
		h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) {
			<-release
			return nil, nil
		}
		require.NoError(t, h.Poller.Start())
		defer h.Poller.Stop()
		synctest.Wait()
		healthy, _ := h.Poller.HealthCheck()
		assert.False(t, healthy, "starting a poller does not establish review health")
		ready, _ := h.Poller.ReadinessCheck()
		assert.False(t, ready)
		close(release)
		synctest.Wait()
		healthy, _ = h.Poller.HealthCheck()
		assert.True(t, healthy, "a completed initial poll establishes health")
		ready, _ = h.Poller.ReadinessCheck()
		assert.True(t, ready)
	})
}

func TestCIPanelRerunDoesNotPostToAdvancedHead(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "old-head", "base..old-head",
		[]jobSpec{{Agent: "test", Status: "failed", Error: "model unavailable"}})
	h.markJobFailed(t, synth.ID, "no member output")
	require.NoError(t, h.DB.MarkPanelPosted(panel.ID, storage.PanelOutcomeNoReviewPosted))
	server := newServerWithLogs(h.DB, h.Cfg, "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	rerun, err := server.humaRerunJob(context.Background(), &RerunJobInput{Body: RerunJobRequest{JobID: synth.ID}})
	require.NoError(t, err)
	members, err := h.DB.GetPanelMembers(*rerun.Body.RunUUID)
	require.NoError(t, err)
	for _, member := range members {
		h.markJobDoneWithReview(t, member.ID, "test", "No issues found.")
	}
	h.completeSynthesisWithReview(t, rerun.Body.JobID, "No issues found.")
	h.Poller.prPostTargetFn = func(context.Context, string, int) (panelPostTarget, error) {
		return panelPostTarget{Open: true, HeadSHA: "new-head"}, nil
	}
	h.Poller.handleReviewCompleted(ciEvent(rerun.Body.JobID, "review.completed"))
	assert.Empty(t, *comments)
	rerunPanel, err := h.DB.GetCIPanelByRunUUID(*rerun.Body.RunUUID)
	require.NoError(t, err)
	assert.Equal(t, "old-head", rerunPanel.HeadSHA)
	assert.NotNil(t, rerunPanel.RetiredAt)
}

func TestCIPollerRestartKeepsNonFailureOutcomesTerminal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		member  jobSpec
		outcome string
	}{
		{"empty", jobSpec{Agent: "test", Status: "done", Output: " "}, storage.PanelOutcomeNoReviewPosted},
		{"no verdict", jobSpec{Agent: "test", Status: "failed", Error: reviewpkg.NoVerdictErrorPrefix + "empty output"}, storage.PanelOutcomeNoReviewPosted},
		{"unreadable", jobSpec{Agent: "test", Status: "failed", Error: reviewpkg.NoVerdictErrorPrefix + "unable to read the diff"}, storage.PanelOutcomeNoReviewPosted},
		{"timeout", jobSpec{Agent: "test", Status: "canceled", Error: reviewpkg.TimeoutErrorPrefix + "deadline"}, storage.PanelOutcomeNoReviewPosted},
		{"posted", jobSpec{Agent: "test", Status: "done", Output: "No issues found."}, storage.PanelOutcomeReviewPosted},
		{"inaccessible", jobSpec{Agent: "test", Status: "failed", Error: "model unavailable"}, storage.PanelOutcomeAbandoned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCIPollerHarness(t, "https://github.com/acme/api.git")
			panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a", []jobSpec{tc.member})
			h.markJobFailed(t, synth.ID, "no member output")
			// Prior attempts may have failed even when the final outcome is empty.
			require.NoError(t, h.DB.DeferReviewAttempt("acme/api", 7, "head-a", "genuine", "old failure",
				&panel.PanelRunUUID, time.Now(), true))
			require.NoError(t, h.DB.MarkPanelPosted(panel.ID, tc.outcome))
			require.NoError(t, h.Poller.Start())
			h.Poller.Stop()
			attempt, err := h.DB.GetReviewAttempt("acme/api", 7, "head-a")
			require.NoError(t, err)
			assert.Equal(t, "done", attempt.State)
		})
	}
}

func TestCIPanelRerunIgnoresCanceledPredecessor(t *testing.T) {
	for _, successorState := range []string{"pending", "done"} {
		t.Run(successorState, func(t *testing.T) {
			h := newCIPollerHarness(t, "https://github.com/acme/api.git")
			_, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a",
				[]jobSpec{{Agent: "test", Status: "failed", Error: "model unavailable"}})
			h.markJobCanceled(t, synth.ID, "canceled by user")
			server := newServerWithLogs(h.DB, h.Cfg, "", newTestErrorLog(), newTestActivityLog())
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			rerun, err := server.humaRerunJob(context.Background(), &RerunJobInput{Body: RerunJobRequest{JobID: synth.ID}})
			require.NoError(t, err)
			if successorState == "done" {
				panel, err := h.DB.GetCIPanelByRunUUID(*rerun.Body.RunUUID)
				require.NoError(t, err)
				require.NoError(t, h.DB.MarkPanelPosted(panel.ID, storage.PanelOutcomeNoReviewPosted))
			}
			// API cancellation broadcasts asynchronously; its event can arrive
			// after a rerun has taken ownership of the same PR HEAD.
			h.Poller.handleReviewCanceled(ciEvent(synth.ID, "review.canceled"))
			server.retireCIPanelForCanceledSynthesis(synth)
			attempt, err := h.DB.GetReviewAttempt("acme/api", 7, "head-a")
			require.NoError(t, err)
			require.NotNil(t, attempt, "old cancellation must not remove the successor's attempt")
			assert.Equal(t, successorState, attempt.State)
			active, err := h.DB.GetActiveCIPanelByPRSHA("acme/api", 7, "head-a")
			require.NoError(t, err)
			assert.Equal(t, *rerun.Body.RunUUID, active.PanelRunUUID)
		})
	}
}

func TestCIPanelCanceledRerunKeepsPostedReview(t *testing.T) {
	for _, posted := range []bool{false, true} {
		t.Run(fmt.Sprintf("posted=%t", posted), func(t *testing.T) {
			h := newCIPollerHarness(t, "https://github.com/acme/api.git")
			panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a",
				[]jobSpec{{Agent: "test", Status: "done", Output: "No issues found."}})
			h.completeSynthesisWithReview(t, synth.ID, "No issues found.")
			if posted {
				require.NoError(t, h.DB.MarkPanelPosted(panel.ID, storage.PanelOutcomeReviewPosted))
			}
			server := newServerWithLogs(h.DB, h.Cfg, "", newTestErrorLog(), newTestActivityLog())
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			rerun, err := server.humaRerunJob(context.Background(), &RerunJobInput{Body: RerunJobRequest{JobID: synth.ID}})
			require.NoError(t, err)
			_, err = server.humaCancelJob(context.Background(), &CancelJobInput{Body: CancelJobRequest{JobID: rerun.Body.JobID}})
			require.NoError(t, err)

			reviewed, err := h.Poller.alreadyReviewedPR("acme/api", ghPR{Number: 7, HeadRefOid: "head-a"})
			require.NoError(t, err)
			assert.Equal(t, posted, reviewed, "only a delivered review suppresses a new automatic run after cancellation")
		})
	}
}

func TestCIPanelRerunRetainsFailureHealthUntilDelivery(t *testing.T) {
	for _, outcome := range []string{storage.PanelOutcomeNoReviewPosted, storage.PanelOutcomeAbandoned} {
		t.Run(outcome, func(t *testing.T) {
			h, server := newCIHealthHarness(t)
			comments := h.CaptureComments()
			panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a",
				[]jobSpec{{Agent: "test", Status: "canceled", Error: reviewpkg.TimeoutErrorPrefix + "deadline"}})
			h.markJobFailed(t, synth.ID, "no member output")
			if outcome == storage.PanelOutcomeAbandoned {
				h.Poller.abandonPanelPost(panel, "Review failed to post", "inaccessible GitHub repo/PR")
			} else {
				h.Poller.handleReviewFailed(ciEvent(synth.ID, "review.failed"))
			}
			require.NoError(t, h.Poller.reconcileRetryHealth("acme/api", nil, h.Cfg))
			require.False(t, decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
			rerun, err := server.humaRerunJob(context.Background(), &RerunJobInput{Body: RerunJobRequest{JobID: synth.ID}})
			require.NoError(t, err)
			require.NoError(t, h.Poller.reconcileRetryHealth("acme/api", nil, h.Cfg))
			assert.False(t, decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy,
				"queueing a rerun does not deliver a review")

			members, err := h.DB.GetPanelMembers(*rerun.Body.RunUUID)
			require.NoError(t, err)
			for _, member := range members {
				h.markJobDoneWithReview(t, member.ID, "test", "No issues found.")
			}
			h.completeSynthesisWithReview(t, rerun.Body.JobID, "No issues found.")
			h.Poller.handleReviewCompleted(ciEvent(rerun.Body.JobID, "review.completed"))
			require.Len(t, *comments, 1)
			require.NoError(t, h.Poller.reconcileRetryHealth("acme/api", nil, h.Cfg))
			assert.True(t, decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
		})
	}
}

func TestCIPanelRerunAfterClosedPRCleanup(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	_, synth, _ := h.seedBlockedPanelRun(t, "acme/api", 7, "head-a", "base..head-a",
		[]jobSpec{{Agent: "test"}})
	h.Poller.isPROpenFn = func(string, int) bool { return false }
	require.NoError(t, h.Poller.cleanupClosedPRPanels(context.Background(), "acme/api", nil))
	candidates, err := h.Poller.closedPRCleanupCandidates("acme/api")
	require.NoError(t, err)
	assert.Empty(t, candidates, "finished cleanup must not keep checking a closed PR")
	h.Poller.isPROpenFn = func(string, int) bool { return true }
	server := newServerWithLogs(h.DB, h.Cfg, "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	rerun, err := server.humaRerunJob(context.Background(), &RerunJobInput{Body: RerunJobRequest{JobID: synth.ID}})
	require.NoError(t, err)
	members, err := h.DB.GetPanelMembers(*rerun.Body.RunUUID)
	require.NoError(t, err)
	for _, member := range members {
		h.markJobDoneWithReview(t, member.ID, "test", "No issues found.")
	}
	h.completeSynthesisWithReview(t, rerun.Body.JobID, "No issues found.")
	h.Poller.handleReviewCompleted(ciEvent(rerun.Body.JobID, "review.completed"))
	require.Len(t, *comments, 1, "historical CI rerun must still publish after the PR reopens")
	assert.Equal(t, "acme/api", (*comments)[0].Repo)
	assert.Equal(t, 7, (*comments)[0].PR)
}

func TestCIPanelRerunRejectsMissingDeliveryTarget(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a",
		[]jobSpec{{Agent: "test", Status: "done", Output: "No issues found."}})
	h.completeSynthesisWithReview(t, synth.ID, "No issues found.")
	// Older closed-PR cleanup removed mappings but preserved CI job ownership.
	require.NoError(t, h.DB.DeleteCIPanel(panel.ID))
	server := newServerWithLogs(h.DB, h.Cfg, "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	response := serveHuma(t, server, http.MethodPost, "/api/job/rerun",
		[]byte(fmt.Sprintf(`{"job_id":%d}`, synth.ID)))
	assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	jobs, err := h.DB.ListJobs("", h.RepoPath, 0, 0)
	require.NoError(t, err)
	assert.Len(t, jobs, 2, "a rejected rerun must roll back its new jobs")
}
