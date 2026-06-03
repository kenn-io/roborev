package daemon

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
)

// seedBlockedPanelRun creates a CI panel run whose synthesis is claim-blocked
// (production layout) and drives each member to its spec'd terminal state. It
// mirrors seedCIPanelRun but keeps the synthesis gated so timeout/cleanup tests
// can observe MaybeReleasePanelSynthesis flipping claim_blocked off. Returns the
// panel row, the synthesis job, and the member jobs.
func (h *ciPollerHarness) seedBlockedPanelRun(
	t *testing.T, ghRepo string, pr int, headSHA, gitRef string, specs []jobSpec,
) (*storage.CIPanel, *storage.ReviewJob, []*storage.ReviewJob) {
	t.Helper()
	members := make([]storage.EnqueueOpts, 0, len(specs))
	for i, s := range specs {
		members = append(members, storage.EnqueueOpts{
			RepoID: h.Repo.ID, GitRef: gitRef, Agent: s.Agent, ReviewType: s.ReviewType,
			JobType: storage.JobTypeRange, PanelName: "ci", PanelMemberName: s.Agent,
			PanelMemberIndex: i,
		})
	}
	synthesis := storage.EnqueueOpts{
		RepoID: h.Repo.ID, GitRef: gitRef, Agent: "test", PanelName: "ci",
		JobType: storage.JobTypeSynthesis, ClaimBlocked: true,
	}
	created, memberJobs, synthJob, err := h.DB.CreateCIPanelRun(ghRepo, pr, headSHA, members, synthesis)
	require.NoError(t, err)
	require.True(t, created, "panel run should be created")

	for i, s := range specs {
		switch s.Status {
		case "done":
			h.markJobDoneWithReview(t, memberJobs[i].ID, s.Agent, s.Output)
		case "failed":
			h.markJobFailed(t, memberJobs[i].ID, s.Error)
		case "canceled":
			h.markJobCanceled(t, memberJobs[i].ID, s.Error)
		case "running":
			h.markJobRunning(t, memberJobs[i].ID)
		}
	}

	panel, err := h.DB.GetCIPanelByPRSHA(ghRepo, pr, headSHA)
	require.NoError(t, err)
	return panel, synthJob, memberJobs
}

// jobStatus returns a job's current status.
func (h *ciPollerHarness) jobStatus(t *testing.T, jobID int64) storage.JobStatus {
	t.Helper()
	job, err := h.DB.GetJobByID(jobID)
	require.NoError(t, err)
	return job.Status
}

// synthBlocked reports whether the synthesis for a run is still claim-blocked.
func (h *ciPollerHarness) synthBlocked(t *testing.T, runUUID string) bool {
	t.Helper()
	synth, err := h.DB.GetSynthesisJob(runUUID)
	require.NoError(t, err)
	require.NotNil(t, synth)
	return synth.ClaimBlocked
}

// backdatePanelCreatedAt forces a panel row's created_at an hour into the past so
// the timeout sweep selects it.
func (h *ciPollerHarness) backdatePanelCreatedAt(t *testing.T, id int64) {
	t.Helper()
	_, err := h.DB.Exec("UPDATE ci_pr_panels SET created_at = datetime('now','-1 hour') WHERE id = ?", id)
	require.NoError(t, err)
}

// backdateJobStartedAt forces a running job to appear to have consumed runtime
// beyond the CI batch timeout.
func (h *ciPollerHarness) backdateJobStartedAt(t *testing.T, jobID int64) {
	t.Helper()
	_, err := h.DB.Exec("UPDATE review_jobs SET started_at = datetime('now','-1 hour') WHERE id = ?", jobID)
	require.NoError(t, err)
}

// TestSupersedePriorPanels covers the supersede sweep (F-supersede): a new HEAD
// for a PR cancels the prior run's synthesis AND members (parent-first), kills
// their workers, and deletes the stale mapping.
func TestSupersedePriorPanels(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")

	var canceled []int64
	var synthID int64
	var synthMappingActiveAtCancel bool
	h.Poller.jobCancelFn = func(jobID int64) {
		canceled = append(canceled, jobID)
		if jobID == synthID {
			_, err := h.DB.GetActiveCIPanelByPRSHA("acme/api", 7, "oldsha")
			synthMappingActiveAtCancel = err == nil
			require.ErrorIs(t, err, sql.ErrNoRows, "mapping must be retired before synthesis cancel event can fire")
		}
	}

	panel, synth, members := h.seedBlockedPanelRun(t, "acme/api", 7, "oldsha", "base..oldsha",
		[]jobSpec{{Agent: "test", ReviewType: "review"}}) // members left queued (cancelable)
	synthID = synth.ID
	require.Len(t, members, 1)
	attempt, err := h.DB.GetReviewAttempt("acme/api", 7, "oldsha")
	require.NoError(t, err)
	require.NotNil(t, attempt, "panel run reserves an attempt row")

	h.Poller.supersedePriorPanels("acme/api", 7, "newsha")

	assert.Equal(storage.JobStatusCanceled, h.jobStatus(t, synth.ID), "old synthesis canceled")
	assert.Equal(storage.JobStatusCanceled, h.jobStatus(t, members[0].ID), "old member canceled")
	assert.Contains(canceled, synth.ID, "synthesis worker killed")
	assert.Contains(canceled, members[0].ID, "member worker killed")

	rows, err := h.DB.GetActivePanelsForPR("acme/api", 7)
	require.NoError(t, err)
	assert.Empty(rows, "stale mapping deleted")
	assert.False(synthMappingActiveAtCancel, "synthesis cancellation must not observe an active stale mapping")
	attempt, err = h.DB.GetReviewAttempt("acme/api", 7, "oldsha")
	require.NoError(t, err)
	assert.Nil(attempt, "superseding an old HEAD deletes its retry attempt row")
	_ = panel
}

// TestExpireTimedOutPanelsDoesNotCancelQueuedMember verifies that panel age does
// not count as member runtime: a queued member has not started, so the timeout
// sweep must leave it queued and keep synthesis blocked.
func TestExpireTimedOutPanelsDoesNotCancelQueuedMember(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Cfg.CI.BatchTimeout = "15m"

	var canceled []int64
	h.Poller.jobCancelFn = func(jobID int64) { canceled = append(canceled, jobID) }

	panel, synth, members := h.seedBlockedPanelRun(t, "acme/api", 8, "headsha", "base..headsha",
		[]jobSpec{
			{Agent: "codex", ReviewType: "review", Status: "done", Output: "finding"},
			{Agent: "gemini", ReviewType: "review"}, // queued; no runtime yet
		})
	require.Len(t, members, 2)
	h.backdatePanelCreatedAt(t, panel.ID)
	require.True(t, h.synthBlocked(t, synth.PanelRunUUID), "synthesis blocked before sweep")

	h.Poller.expireTimedOutPanels("acme/api", h.Cfg)

	assert.Equal(storage.JobStatusQueued, h.jobStatus(t, members[1].ID), "queued member left queued")
	assert.Empty(canceled, "queued member worker not killed")
	assert.True(h.synthBlocked(t, synth.PanelRunUUID), "synthesis stays blocked while member is queued")
	assert.NotEqual(storage.JobStatusCanceled, h.jobStatus(t, synth.ID), "synthesis not canceled")
}

// TestExpireTimedOutPanelsDoesNotCancelRecentlyStartedMember covers the CI
// restart/backlog case: an old panel row may have a member that only just
// started. Timeout is measured from member started_at, not panel created_at.
func TestExpireTimedOutPanelsDoesNotCancelRecentlyStartedMember(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Cfg.CI.BatchTimeout = "15m"

	var canceled []int64
	h.Poller.jobCancelFn = func(jobID int64) { canceled = append(canceled, jobID) }

	panel, synth, members := h.seedBlockedPanelRun(t, "acme/api", 23, "headsha", "base..headsha",
		[]jobSpec{
			{Agent: "codex", ReviewType: "review", Status: "done", Output: "finding"},
			{Agent: "gemini", ReviewType: "review", Status: "running"},
		})
	require.Len(t, members, 2)
	h.backdatePanelCreatedAt(t, panel.ID)
	require.True(t, h.synthBlocked(t, synth.PanelRunUUID), "synthesis blocked before sweep")

	h.Poller.expireTimedOutPanels("acme/api", h.Cfg)

	assert.Equal(storage.JobStatusRunning, h.jobStatus(t, members[1].ID), "recently started member left running")
	assert.Empty(canceled, "recently started member worker not killed")
	assert.True(h.synthBlocked(t, synth.PanelRunUUID), "synthesis stays blocked while member is running")
	assert.NotEqual(storage.JobStatusCanceled, h.jobStatus(t, synth.ID), "synthesis not canceled")
}

// TestExpireTimedOutPanels covers F5: a timed-out running member is canceled
// WITH the timeout prefix, the synthesis is NOT canceled, the synthesis releases
// (claim_blocked off), and the resulting member breakdown maps to a success
// commit status (timeout = skip, never failure).
func TestExpireTimedOutPanels(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Cfg.CI.BatchTimeout = "15m"

	var canceled []int64
	h.Poller.jobCancelFn = func(jobID int64) { canceled = append(canceled, jobID) }

	panel, synth, members := h.seedBlockedPanelRun(t, "acme/api", 24, "headsha", "base..headsha",
		[]jobSpec{
			{Agent: "codex", ReviewType: "review", Status: "done", Output: "finding"},
			{Agent: "gemini", ReviewType: "review", Status: "running"},
		})
	require.Len(t, members, 2)
	h.backdatePanelCreatedAt(t, panel.ID)
	h.backdateJobStartedAt(t, members[1].ID)
	require.True(t, h.synthBlocked(t, synth.PanelRunUUID), "synthesis blocked before sweep")

	h.Poller.expireTimedOutPanels("acme/api", h.Cfg)

	hung, err := h.DB.GetJobByID(members[1].ID)
	require.NoError(t, err)
	assert.Equal(storage.JobStatusCanceled, hung.Status, "timed-out member canceled")
	assert.True(strings.HasPrefix(hung.Error, reviewpkg.TimeoutErrorPrefix),
		"member tagged with timeout prefix, got %q", hung.Error)
	assert.Contains(canceled, members[1].ID, "timed-out member worker killed")

	assert.NotEqual(storage.JobStatusCanceled, h.jobStatus(t, synth.ID), "synthesis not canceled")
	assert.NotContains(canceled, synth.ID, "synthesis worker not killed")
	assert.False(h.synthBlocked(t, synth.PanelRunUUID), "synthesis released after members terminal")

	// The member breakdown yields success (timeout = skip, not failure/error).
	reviews, err := h.DB.GetPanelMemberReviews(synth.PanelRunUUID)
	require.NoError(t, err)
	state, _ := panelCommitStatus(reviews)
	assert.Equal("success", state, "timeout skip keeps success, never failure")
}

// TestExpireTimedOutPanelsAllStuckNotExpired covers the meaningful-result guard:
// an OLD panel whose members are ALL running (none done/failed) must NOT be
// expired — canceling its members would fabricate an all-timeout-skip panel that
// synthesizes into a fake "success" with no real review output. The sweep must
// leave such a run alone (members untouched, synthesis still blocked).
func TestExpireTimedOutPanelsAllStuckNotExpired(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Cfg.CI.BatchTimeout = "1ms" // positive; rows are backdated an hour

	var canceled []int64
	h.Poller.jobCancelFn = func(jobID int64) { canceled = append(canceled, jobID) }

	panel, synth, members := h.seedBlockedPanelRun(t, "acme/api", 20, "headsha", "base..headsha",
		[]jobSpec{
			{Agent: "codex", ReviewType: "review", Status: "running"},
			{Agent: "gemini", ReviewType: "review", Status: "running"},
		})
	require.Len(t, members, 2)
	h.backdatePanelCreatedAt(t, panel.ID)
	require.True(t, h.synthBlocked(t, synth.PanelRunUUID), "synthesis blocked before sweep")

	h.Poller.expireTimedOutPanels("acme/api", h.Cfg)

	// Nothing is canceled: no meaningful result means the run keeps running.
	assert.Equal(storage.JobStatusRunning, h.jobStatus(t, members[0].ID), "member 0 left running")
	assert.Equal(storage.JobStatusRunning, h.jobStatus(t, members[1].ID), "member 1 left running")
	assert.Empty(canceled, "no worker killed for an all-stuck panel")

	// The synthesis stays gated, so no fake success can be posted.
	assert.True(h.synthBlocked(t, synth.PanelRunUUID), "synthesis stays blocked (no fake success)")
	assert.NotEqual(storage.JobStatusCanceled, h.jobStatus(t, synth.ID), "synthesis not canceled")
}

// TestExpireTimedOutPanelsMeaningfulDoneRunning covers the meaningful path with a
// running (not queued) sibling: an OLD panel with one done + one running member
// whose runtime exceeded the timeout IS expired. The running member is canceled
// with the timeout prefix, synthesis releases, and the breakdown maps to success
// (timeout = skip).
func TestExpireTimedOutPanelsMeaningfulDoneRunning(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Cfg.CI.BatchTimeout = "15m"

	var canceled []int64
	h.Poller.jobCancelFn = func(jobID int64) { canceled = append(canceled, jobID) }

	panel, synth, members := h.seedBlockedPanelRun(t, "acme/api", 21, "headsha", "base..headsha",
		[]jobSpec{
			{Agent: "codex", ReviewType: "review", Status: "done", Output: "finding"},
			{Agent: "gemini", ReviewType: "review", Status: "running"}, // in-flight → timed out
		})
	require.Len(t, members, 2)
	h.backdatePanelCreatedAt(t, panel.ID)
	h.backdateJobStartedAt(t, members[1].ID)
	require.True(t, h.synthBlocked(t, synth.PanelRunUUID), "synthesis blocked before sweep")

	h.Poller.expireTimedOutPanels("acme/api", h.Cfg)

	hung, err := h.DB.GetJobByID(members[1].ID)
	require.NoError(t, err)
	assert.Equal(storage.JobStatusCanceled, hung.Status, "running member canceled")
	assert.True(strings.HasPrefix(hung.Error, reviewpkg.TimeoutErrorPrefix),
		"member tagged with timeout prefix, got %q", hung.Error)
	assert.Contains(canceled, members[1].ID, "hung member worker killed")

	assert.NotEqual(storage.JobStatusCanceled, h.jobStatus(t, synth.ID), "synthesis not canceled")
	assert.False(h.synthBlocked(t, synth.PanelRunUUID), "synthesis released on partial results")

	reviews, err := h.DB.GetPanelMemberReviews(synth.PanelRunUUID)
	require.NoError(t, err)
	state, _ := panelCommitStatus(reviews)
	assert.Equal("success", state, "timeout skip keeps success, never failure")
}

// TestExpireTimedOutPanelsQuotaFailureNotMeaningful covers the refined
// meaningful-result guard: a member that "failed" with a QUOTA error is a SKIP
// downstream (panelCommitStatus subtracts it from real failures), not a result
// worth posting. So an OLD panel with one quota-failed member + one running
// member has NO meaningful real result and must NOT be expired — canceling the
// running member would fabricate an all-skip panel that synthesizes into a fake
// "success" with no actual review output. The sweep must leave it running and
// keep synthesis blocked.
func TestExpireTimedOutPanelsQuotaFailureNotMeaningful(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Cfg.CI.BatchTimeout = "1ms" // positive; rows are backdated an hour

	var canceled []int64
	h.Poller.jobCancelFn = func(jobID int64) { canceled = append(canceled, jobID) }

	panel, synth, members := h.seedBlockedPanelRun(t, "acme/api", 22, "headsha", "base..headsha",
		[]jobSpec{
			{Agent: "codex", ReviewType: "review", Status: "failed", Error: reviewpkg.QuotaErrorPrefix + "exhausted"},
			{Agent: "gemini", ReviewType: "review", Status: "running"}, // in-flight, cancelable
		})
	require.Len(t, members, 2)
	h.backdatePanelCreatedAt(t, panel.ID)
	require.True(t, h.synthBlocked(t, synth.PanelRunUUID), "synthesis blocked before sweep")

	h.Poller.expireTimedOutPanels("acme/api", h.Cfg)

	// The quota-failed member is a skip, not a postable result: nothing is canceled.
	assert.Equal(storage.JobStatusFailed, h.jobStatus(t, members[0].ID), "quota-failed member untouched")
	assert.Equal(storage.JobStatusRunning, h.jobStatus(t, members[1].ID), "running member left running")
	assert.Empty(canceled, "no worker killed when only skips are terminal")

	// Synthesis stays gated so no fake success can be posted.
	assert.True(h.synthBlocked(t, synth.PanelRunUUID), "synthesis stays blocked (no fake success)")
	assert.NotEqual(storage.JobStatusCanceled, h.jobStatus(t, synth.ID), "synthesis not canceled")
}

// TestExpireTimedOutPanelsDisabled verifies a zero ResolvedBatchTimeout disables
// the sweep entirely (no cancellation).
func TestExpireTimedOutPanelsDisabled(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Cfg.CI.BatchTimeout = "0" // disabled

	panel, _, members := h.seedBlockedPanelRun(t, "acme/api", 9, "headsha", "base..headsha",
		[]jobSpec{{Agent: "gemini", ReviewType: "review"}})
	h.backdatePanelCreatedAt(t, panel.ID)

	h.Poller.expireTimedOutPanels("acme/api", h.Cfg)

	assert.Equal(t, storage.JobStatusQueued, h.jobStatus(t, members[0].ID),
		"disabled timeout leaves members untouched")
}

// TestCleanupClosedPRPanels covers F13: a pending run whose PR is closed has its
// synthesis+members canceled (parent-first) and its mapping deleted.
func TestCleanupClosedPRPanels(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Poller.isPROpenFn = func(string, int) bool { return false } // PR is closed

	var canceled []int64
	h.Poller.jobCancelFn = func(jobID int64) { canceled = append(canceled, jobID) }

	_, synth, members := h.seedBlockedPanelRun(t, "acme/api", 10, "headsha", "base..headsha",
		[]jobSpec{{Agent: "test", ReviewType: "review"}})
	require.Len(t, members, 1)

	openPRs := map[int]bool{} // PR 10 absent → not open
	h.Poller.cleanupClosedPRPanels(context.Background(), "acme/api", openPRs)

	assert.Equal(storage.JobStatusCanceled, h.jobStatus(t, synth.ID), "synthesis canceled")
	assert.Equal(storage.JobStatusCanceled, h.jobStatus(t, members[0].ID), "member canceled")
	assert.Contains(canceled, synth.ID, "synthesis worker killed")

	rows, err := h.DB.GetActivePanelsForPR("acme/api", 10)
	require.NoError(t, err)
	assert.Empty(rows, "closed-PR mapping deleted")
}

// TestCleanupClosedPRPanelsKeepsOpenPR verifies a still-open PR's run is left
// intact: neither the open-PR map nor the callIsPROpen double-check flags it.
func TestCleanupClosedPRPanelsKeepsOpenPR(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Poller.isPROpenFn = func(string, int) bool { return true } // still open per double-check

	_, synth, members := h.seedBlockedPanelRun(t, "acme/api", 11, "headsha", "base..headsha",
		[]jobSpec{{Agent: "test", ReviewType: "review"}})

	openPRs := map[int]bool{} // absent from list, but callIsPROpen confirms open
	h.Poller.cleanupClosedPRPanels(context.Background(), "acme/api", openPRs)

	assert.NotEqual(t, storage.JobStatusCanceled, h.jobStatus(t, synth.ID), "open-PR synthesis kept")
	assert.Equal(t, storage.JobStatusQueued, h.jobStatus(t, members[0].ID), "open-PR member kept")
	rows, err := h.DB.GetActivePanelsForPR("acme/api", 11)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "open-PR mapping kept")
}
