package daemon

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func recoveringCIHealth(t *testing.T) (*ciPollerHarness, *Server, ghPR) {
	t.Helper()
	h, server := newCIHealthHarness(t)
	h.stubProcessPRGit()
	h.Cfg.CI.Repos = []string{"acme/api"}
	h.Cfg.CI.Agents = []string{"test"}
	h.Cfg.CI.ReviewTypes = []string{"security"}
	pr := ghPR{Number: 1, HeadRefOid: "head-a", BaseRefName: "main"}
	h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return []ghPR{pr}, nil }
	h.Poller.poll(t.Context())
	synthID := h.drivePanelOutcome(t, "acme/api", pr.Number, pr.HeadRefOid, "transient")
	h.Poller.handleReviewFailed(ciEvent(synthID, "review.failed"))
	h.Poller.poll(t.Context())
	return h, server, pr
}

func readCIHealth(t *testing.T, server *Server) (storage.HealthStatus, storage.ComponentHealth) {
	t.Helper()
	health := decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
	i := slices.IndexFunc(health.Components, func(component storage.ComponentHealth) bool { return component.Name == "ci" })
	require.NotEqual(t, -1, i, "CI health must be present")
	return health, health.Components[i]
}

func TestCIHealthReportsRetryRecoveryUntilDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, server, pr := recoveringCIHealth(t)
		comments := h.CaptureComments()
		health, ci := readCIHealth(t, server)
		assert.False(t, health.Healthy)
		assert.True(t, health.Ready)
		require.NotNil(t, ci.Recovery, "a scheduled transient retry explains the retained CI failure")
		assert.Equal(t, time.Now().UTC(), ci.Recovery.ObservedAt)
		assert.Equal(t, time.Now().UTC().Add(72*time.Hour), ci.Recovery.Deadline)
		deadline := ci.Recovery.Deadline

		_, err := h.DB.MakeTransientReviewAttemptsDue(time.Now())
		require.NoError(t, err)
		h.Poller.poll(t.Context())
		health, ci = readCIHealth(t, server)
		assert.False(t, health.Healthy, "queueing a retry does not restore health")
		require.NotNil(t, ci.Recovery)
		assert.Equal(t, deadline, ci.Recovery.Deadline, "retrying cannot extend the recovery budget")
		for _, member := range h.panelMembers(t, "acme/api", pr.Number, pr.HeadRefOid) {
			h.markJobRunning(t, member.ID)
		}
		health, ci = readCIHealth(t, server)
		assert.False(t, health.Healthy)
		require.NotNil(t, ci.Recovery, "a live running retry retains recovery evidence")
		assert.Len(t, health.RecentErrors, 1, "recovery observations preserve the original failure history")

		synthID := h.drivePanelOutcome(t, "acme/api", pr.Number, pr.HeadRefOid, "done")
		h.Poller.handleReviewCompleted(ciEvent(synthID, "review.completed"))
		require.Len(t, *comments, 1)
		health, ci = readCIHealth(t, server)
		assert.True(t, health.Healthy)
		assert.Nil(t, ci.Recovery, "delivered reviews no longer need a recovery observation")
		assert.Len(t, health.RecentErrors, 1)
	})
}

func TestCIHealthRestoresRecoveryAfterRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, server, pr := recoveringCIHealth(t)
		_, before := readCIHealth(t, server)
		require.NotNil(t, before.Recovery)
		h.Poller.running = false
		h.Poller = NewCIPoller(h.DB, NewStaticConfig(h.Cfg), nil)
		h.Poller.repoResolver.canonicalRepoFn = nil
		h.stubProcessPRGit()
		h.Poller.isPROpenFn = func(string, int) bool { return true }
		h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return []ghPR{pr}, nil }
		server.SetCIPoller(h.Poller)
		require.NoError(t, h.Poller.Start())
		defer h.Poller.Stop()
		synctest.Wait()

		health, ci := readCIHealth(t, server)
		assert.False(t, health.Healthy)
		assert.True(t, health.Ready)
		require.NotNil(t, ci.Recovery, "startup restores retry evidence from SQLite")
		assert.Equal(t, before.Recovery.Deadline, ci.Recovery.Deadline)
	})
}

func TestCIHealthRecoveryExpiresWithoutAnotherPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, server, _ := recoveringCIHealth(t)
		_, err := h.DB.MakeTransientReviewAttemptsDue(time.Now())
		require.NoError(t, err)
		h.Poller.poll(t.Context())
		_, err = h.DB.Exec(`UPDATE ci_pr_review_attempts SET first_attempt_at = ?`, time.Now().Add(-72*time.Hour+time.Minute).Format(time.RFC3339))
		require.NoError(t, err)
		_, ci := readCIHealth(t, server)
		require.NotNil(t, ci.Recovery)

		time.Sleep(time.Minute)
		health, ci := readCIHealth(t, server)
		assert.False(t, health.Healthy)
		assert.Nil(t, ci.Recovery, "the existing retry deadline expires even if the scheduler stops")
	})
}

func TestCIHealthDueRetryWaitsForNormalPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, server, _ := recoveringCIHealth(t)
		_, ci := readCIHealth(t, server)
		require.NotNil(t, ci.Recovery)
		time.Sleep(3 * time.Minute)
		health, ci := readCIHealth(t, server)
		assert.False(t, health.Healthy)
		require.NotNil(t, ci.Recovery, "the two-minute retry is due before the normal five-minute poll")
		time.Sleep(2 * time.Minute)
		h.Poller.poll(t.Context())
		health, ci = readCIHealth(t, server)
		assert.False(t, health.Healthy)
		require.NotNil(t, ci.Recovery, "dispatching the due retry preserves recovery evidence")
	})
}

func TestCIHealthRejectsUnverifiedRetryRecovery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change string
	}{
		{"genuine failure", `UPDATE ci_pr_review_attempts SET last_error_class = 'genuine'`},
		{"missing schedule", `UPDATE ci_pr_review_attempts SET next_attempt_at = NULL`},
		{"missing live panel", `UPDATE ci_pr_review_attempts SET state = 'pending'`},
		{"unreadable attempt", `UPDATE ci_pr_review_attempts SET last_panel_run_uuid = 'invalid'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, server, _ := recoveringCIHealth(t)
			_, ci := readCIHealth(t, server)
			require.NotNil(t, ci.Recovery)
			_, err := h.DB.Exec(tc.change)
			require.NoError(t, err)
			health, ci := readCIHealth(t, server)
			assert.False(t, health.Healthy)
			assert.Nil(t, ci.Recovery)
		})
	}
}

func TestCIHealthOperationalFailureDoesNotInheritRetryRecovery(t *testing.T) {
	h, server, _ := recoveringCIHealth(t)
	h.Poller.gitFetchFn = func(context.Context, string, []string) error { return errors.New("fetch unavailable") }
	_, err := h.DB.MakeTransientReviewAttemptsDue(time.Now())
	require.NoError(t, err)
	h.Poller.poll(t.Context())
	// The next poll re-arms the stranded attempt, but its newer enqueue failure
	// must not become a recoverable provider failure just because backoff exists.
	h.Poller.poll(t.Context())
	attempt, err := h.DB.GetReviewAttempt("acme/api", 1, "head-a")
	require.NoError(t, err)
	require.Equal(t, "deferred", attempt.State)
	health, ci := readCIHealth(t, server)
	assert.False(t, health.Healthy)
	assert.Nil(t, ci.Recovery)

	h.stubProcessPRGit()
	_, err = h.DB.MakeTransientReviewAttemptsDue(time.Now())
	require.NoError(t, err)
	h.Poller.poll(t.Context())
	_, err = h.DB.GetActiveCIPanelByPRSHA("acme/api", 1, "head-a")
	require.NoError(t, err, "the operational error recovered only after a real panel was enqueued")
	health, ci = readCIHealth(t, server)
	assert.False(t, health.Healthy)
	require.NotNil(t, ci.Recovery, "a recovered enqueue error must not hide current retry progress")
}

func TestCIHealthRecoveryRequiresEveryFailureToBeRetryable(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "genuine", true: "terminal"}[terminal], func(t *testing.T) {
			h, server, _ := recoveringCIHealth(t)
			secondRepo, err := h.DB.GetOrCreateRepo(t.TempDir(), "https://github.com/acme/web.git")
			require.NoError(t, err)
			second := *h
			second.Repo, second.RepoPath = secondRepo, secondRepo.RootPath
			h.Cfg.CI.Repos = []string{"acme/api", "acme/web"}
			h.Poller.listOpenPRsFn = func(_ context.Context, repo string) ([]ghPR, error) {
				if repo == "acme/api" {
					return []ghPR{{Number: 1, HeadRefOid: "head-a", BaseRefName: "main"}}, nil
				}
				return nil, nil
			}
			h.Poller.poll(t.Context())
			panel, synth, _ := second.seedCIPanelRun(t, "acme/web", 2, "head-b", "base..head-b",
				[]jobSpec{{Agent: "test", Status: "failed", Error: "model unavailable"}})
			h.markJobFailed(t, synth.ID, "no member output")
			if terminal {
				for range 2 {
					require.NoError(t, h.DB.DeferReviewAttempt("acme/web", 2, "head-b", "genuine", "model unavailable", &panel.PanelRunUUID, time.Now(), true))
				}
			}
			h.Poller.handleReviewFailed(ciEvent(synth.ID, "review.failed"))
			// A worker can fail another repository between polls. Its durable
			// failure must affect recovery evidence before health reconciliation.
			health, ci := readCIHealth(t, server)
			assert.False(t, health.Healthy)
			assert.Nil(t, ci.Recovery, "a recovering PR cannot cover another failed PR")

			h.Cfg.CI.Repos = []string{"acme/api"}
			h.Poller.poll(t.Context())
			failed, err := h.DB.GetFailedReviewAttempts("acme/web")
			require.NoError(t, err)
			require.Len(t, failed, 1, "removed repositories retain their historical attempts")
			health, ci = readCIHealth(t, server)
			assert.False(t, health.Healthy)
			require.NotNil(t, ci.Recovery, "a removed repository no longer blocks monitored retries")
		})
	}
}

func TestCIHealthStalledWorkersPreventRecoveryEvidence(t *testing.T) {
	h, server, pr := recoveringCIHealth(t)
	_, err := h.DB.MakeTransientReviewAttemptsDue(time.Now())
	require.NoError(t, err)
	h.Poller.poll(t.Context())
	for _, member := range h.panelMembers(t, "acme/api", pr.Number, pr.HeadRefOid) {
		h.markJobRunning(t, member.ID)
		backdateJobStartedAt(t, h.DB, member.ID)
	}
	health, ci := readCIHealth(t, server)
	assert.False(t, health.Healthy)
	assert.False(t, health.Ready)
	assert.Nil(t, ci.Recovery, "stalled work must remain actionable")
}

func TestCIHealthHealthyCIHasNoRecoveryForWorkerFailure(t *testing.T) {
	h, server := newCIHealthHarness(t)
	_, _, members := h.seedBlockedPanelRun(t, "acme/api", 1, "head-a", "base..head-a", []jobSpec{{Agent: "test"}})
	h.markJobRunning(t, members[0].ID)
	backdateJobStartedAt(t, h.DB, members[0].ID)
	health, ci := readCIHealth(t, server)
	assert.False(t, health.Healthy)
	assert.False(t, health.Ready)
	assert.True(t, ci.Healthy)
	assert.Nil(t, ci.Recovery, "ordinary queued work cannot explain an independent worker failure")
}

func TestCIHealthRecoveryUsesEarliestRetryDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, server, pr := recoveringCIHealth(t)
		_, err := h.DB.ReserveReviewAttempt("acme/api", 2, "head-b", time.Now().Add(-24*time.Hour))
		require.NoError(t, err)
		require.NoError(t, h.DB.DeferReviewAttempt("acme/api", 2, "head-b", "transient", "provider unavailable", nil, time.Now().Add(time.Minute), false))
		require.NoError(t, h.Poller.reconcileRetryHealth("acme/api", []ghPR{pr, {Number: 2, HeadRefOid: "head-b"}}, h.Cfg, nil))
		health, ci := readCIHealth(t, server)
		assert.False(t, health.Healthy)
		require.NotNil(t, ci.Recovery)
		assert.Equal(t, time.Now().UTC().Add(48*time.Hour), ci.Recovery.Deadline)
	})
}

func TestCIHealthTerminalPanelDoesNotProveRecovery(t *testing.T) {
	for _, synthStatus := range []string{"failed", "queued"} {
		t.Run(synthStatus, func(t *testing.T) {
			h, server, _ := recoveringCIHealth(t)
			_, err := h.DB.MakeTransientReviewAttemptsDue(time.Now())
			require.NoError(t, err)
			h.Poller.poll(t.Context())
			_, ci := readCIHealth(t, server)
			require.NotNil(t, ci.Recovery)
			_, err = h.DB.Exec(`UPDATE review_jobs SET status = 'failed'`)
			require.NoError(t, err)
			_, err = h.DB.Exec(`UPDATE review_jobs SET status = ?, claim_blocked = 1 WHERE job_type = ?`, synthStatus, storage.JobTypeSynthesis)
			require.NoError(t, err)
			health, ci := readCIHealth(t, server)
			assert.False(t, health.Healthy)
			assert.Nil(t, ci.Recovery, "a completed panel or blocked synthesis without live members is not progressing")
		})
	}
}

func TestCIHealthKeepsRecoveryDuringRetryDispatch(t *testing.T) {
	h, server, _ := recoveringCIHealth(t)
	_, err := h.DB.MakeTransientReviewAttemptsDue(time.Now())
	require.NoError(t, err)
	observed := false
	h.Poller.gitFetchFn = func(context.Context, string, []string) error {
		observed = true
		health, ci := readCIHealth(t, server)
		assert.False(t, health.Healthy)
		require.NotNil(t, ci.Recovery, "a claimed retry remains owned while its panel is being prepared")
		return nil
	}
	h.Poller.poll(t.Context())
	require.True(t, observed)
}

func TestCIHealthKeepsRecoveryThroughPublication(t *testing.T) {
	h, server, pr := recoveringCIHealth(t)
	_, err := h.DB.MakeTransientReviewAttemptsDue(time.Now())
	require.NoError(t, err)
	h.Poller.poll(t.Context())
	synthID := h.drivePanelOutcome(t, "acme/api", pr.Number, pr.HeadRefOid, "done")
	_, ci := readCIHealth(t, server)
	require.NotNil(t, ci.Recovery, "completed output still waits for automatic publication")
	posted := false
	h.Poller.postPRCommentFn = func(string, int, string) error {
		posted = true
		health, ci := readCIHealth(t, server)
		assert.False(t, health.Healthy)
		require.NotNil(t, ci.Recovery, "publication is part of the active retry")
		return nil
	}
	h.Poller.handleReviewCompleted(ciEvent(synthID, "review.completed"))
	require.True(t, posted)
	health, ci := readCIHealth(t, server)
	assert.True(t, health.Healthy, "delivery establishes health before the next poll")
	assert.Nil(t, ci.Recovery)
}

func TestCIHealthObservesFirstFailureBeforePoll(t *testing.T) {
	h, server := newCIHealthHarness(t)
	h.Cfg.CI.Repos = []string{"acme/api"}
	h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return nil, nil }
	h.Poller.poll(t.Context())
	_, synth, _ := h.seedCIPanelRun(t, "acme/api", 1, "head-a", "base..head-a",
		[]jobSpec{{Agent: "test", Status: "failed", Error: "outage: 429 Too Many Requests"}})
	h.markJobFailed(t, synth.ID, "outage: 429 Too Many Requests")
	h.Poller.handleReviewFailed(ciEvent(synth.ID, "review.failed"))
	health, ci := readCIHealth(t, server)
	assert.False(t, health.Healthy, "worker failures affect health before the next poll")
	require.NotNil(t, ci.Recovery, "a newly scheduled transient retry carries its own evidence")
}

func TestCIHealthKeepsRecoveryBeforeTransientFinalization(t *testing.T) {
	h, server, pr := recoveringCIHealth(t)
	_, err := h.DB.MakeTransientReviewAttemptsDue(time.Now())
	require.NoError(t, err)
	h.Poller.poll(t.Context())
	synthID := h.drivePanelOutcome(t, "acme/api", pr.Number, pr.HeadRefOid, "transient")
	health, ci := readCIHealth(t, server)
	assert.False(t, health.Healthy)
	require.NotNil(t, ci.Recovery, "transient finalization belongs to the automatic retry workflow")
	h.Poller.handleReviewFailed(ciEvent(synthID, "review.failed"))
	health, ci = readCIHealth(t, server)
	assert.False(t, health.Healthy)
	require.NotNil(t, ci.Recovery)
}
