package daemon

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

func newCIHealthHarness(t *testing.T) (*ciPollerHarness, *Server) {
	t.Helper()
	t.Setenv("GH_TOKEN", "test-token")
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Poller.repoResolver.canonicalRepoFn = nil
	server := newServerWithLogs(h.DB, h.Cfg, "", newTestErrorLog(), newTestActivityLog())
	server.SetCIPoller(h.Poller)
	// Drive polls synchronously so each health request observes a complete poll.
	h.Poller.running = true
	t.Cleanup(func() {
		h.Poller.running = false
		require.NoError(t, server.Close())
	})
	return h, server
}

func TestHealthCIPollerRepositoryFailureAndRecovery(t *testing.T) {
	h, server := newCIHealthHarness(t)
	h.Cfg.CI.Repos = []string{"acme/api", "acme/web"}
	listErr := errors.New("provider unavailable")
	var visited []string
	h.Poller.listOpenPRsFn = func(_ context.Context, repo string) ([]ghPR, error) {
		visited = append(visited, repo)
		if repo == "acme/api" {
			return nil, listErr
		}
		return nil, nil
	}

	for range 2 {
		visited = nil
		h.Poller.poll(context.Background())
		health := decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
		assert := assert.New(t)
		assert.Equal([]string{"acme/api", "acme/web"}, visited)
		assert.False(health.Healthy)
		assert.Contains(health.Components, storage.ComponentHealth{
			Name: "ci", Healthy: false, Message: "polling failed for acme/api",
		})
		require.NotEmpty(t, health.RecentErrors)
		assert.Equal("ci", health.RecentErrors[0].Component)
		assert.Equal("polling failed for acme/api; see daemon log for details", health.RecentErrors[0].Message)
	}

	listErr = nil
	h.Poller.poll(context.Background())
	health := decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
	assert.True(t, health.Healthy)
	assert.Contains(t, health.Components, storage.ComponentHealth{
		Name: "ci", Healthy: true, Message: "running",
	})
	assert.NotEmpty(t, health.RecentErrors, "recovery preserves error history")
}

func TestHealthCIPollerPRFailureContinuesPolling(t *testing.T) {
	h, server := newCIHealthHarness(t)
	h.stubProcessPRGit()
	h.Cfg.CI.Repos = []string{"acme/api", "acme/web"}
	h.Cfg.CI.SkipLabels = []string{"skip-review"}
	fetchErr := errors.New("fetch failed with diagnostic output")
	h.Poller.gitFetchFn = func(context.Context, string, []string) error { return fetchErr }
	var visited []string
	h.Poller.listOpenPRsFn = func(_ context.Context, repo string) ([]ghPR, error) {
		visited = append(visited, repo)
		if repo == "acme/web" {
			return nil, nil
		}
		return []ghPR{
			{Number: 1, HeadRefOid: "head-a", BaseRefName: "main"},
			{Number: 2, HeadRefOid: "head-b", Labels: []string{"skip-review"}},
		}, nil
	}
	skipped := h.CaptureSkippedChecks()

	h.Poller.poll(context.Background())
	health := decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
	assert := assert.New(t)
	assert.False(health.Healthy)
	assert.Contains(health.Components, storage.ComponentHealth{
		Name: "ci", Healthy: false, Message: "polling failed for acme/api",
	})
	assert.Equal([]string{"acme/api", "acme/web"}, visited)
	require.Len(t, *skipped, 1)
	assert.Equal("head-b", (*skipped)[0].SHA)
	require.NotEmpty(t, health.RecentErrors)
	assert.Equal("polling failed for acme/api; see daemon log for details", health.RecentErrors[0].Message)

	// The repository poll must propagate processing errors to its caller.
	err := h.Poller.pollRepo(context.Background(), "acme/api", h.Cfg)
	assert.ErrorIs(err, fetchErr)
}

func TestHealthCIPollerRetryFailure(t *testing.T) {
	for _, recovery := range []string{"retry", "new head", "reviewed head", "active", "done", "older done", "removed", "closed", "skipped", "disabled"} {
		t.Run(recovery, func(t *testing.T) {
			h, server := newCIHealthHarness(t)
			h.stubProcessPRGit()
			h.Cfg.CI.Repos = []string{"acme/api"}
			h.Cfg.CI.Agents = []string{"test"}
			h.Cfg.CI.ReviewTypes = []string{"security"}
			pr := ghPR{Number: 1, HeadRefOid: "head-a", BaseRefName: "main"}
			now := time.Now()
			created, err := h.DB.ReserveReviewAttempt("acme/api", pr.Number, pr.HeadRefOid, now.Add(-time.Hour))
			require.NoError(t, err)
			require.True(t, created)
			require.NoError(t, h.DB.DeferReviewAttempt("acme/api", pr.Number, pr.HeadRefOid,
				"transient", "provider unavailable", nil, now.Add(-time.Minute), false))
			h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) {
				return []ghPR{pr}, nil
			}
			fetches := 0
			h.Poller.gitFetchFn = func(context.Context, string, []string) error {
				fetches++
				return errors.New("fetch failed")
			}

			// A deferred head skips the ordinary enqueue path before the retry sweep.
			require.NoError(t, h.Poller.processPR(context.Background(), "acme/api", pr, h.Cfg))
			require.Zero(t, fetches)
			h.Poller.poll(context.Background())
			health := decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
			assert.Equal(t, 1, fetches)
			assert.False(t, health.Healthy)
			assert.Contains(t, health.Components, storage.ComponentHealth{
				Name: "ci", Healthy: false, Message: "retry failed for acme/api#1",
			})

			// Reconciliation defers the stranded attempt; skipping it during backoff
			// is not evidence that enqueueing has recovered.
			h.Poller.poll(context.Background())
			assert.Equal(t, 1, fetches)
			assert.False(t, decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)

			h.Poller.gitFetchFn = func(context.Context, string, []string) error { return nil }
			skipped := h.CaptureSkippedChecks()
			switch recovery {
			case "new head":
				pr.HeadRefOid = "head-b"
			case "reviewed head":
				pr.HeadRefOid = "head-b"
				_, err := h.DB.ReserveReviewAttempt("acme/api", pr.Number, pr.HeadRefOid, now)
				require.NoError(t, err)
				require.NoError(t, h.DB.MarkReviewAttemptDone("acme/api", pr.Number, pr.HeadRefOid))
			case "active", "done":
				h.Cfg.CI.SkipLabels = []string{"skip-review"}
				pr.Labels = []string{"skip-review"}
				// The retry was committed, but its caller did not observe success.
				opts := storage.EnqueueOpts{RepoID: h.Repo.ID, GitRef: "base..head-a", Agent: "test"}
				created, _, _, err := h.DB.CreateCIPanelRun("acme/api", pr.Number, pr.HeadRefOid,
					[]storage.EnqueueOpts{opts}, opts)
				require.NoError(t, err)
				require.True(t, created)
				_, err = h.DB.Exec(`UPDATE ci_pr_review_attempts SET state = 'pending', next_attempt_at = NULL`)
				require.NoError(t, err)
				if recovery == "done" {
					require.NoError(t, h.DB.MarkReviewAttemptDone("acme/api", pr.Number, pr.HeadRefOid))
				}
			case "closed":
				h.Poller.isPROpenFn = func(string, int) bool { return false }
				h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return nil, nil }
			case "skipped":
				h.Cfg.CI.SkipLabels = []string{"skip-review"}
				pr.Labels = []string{"skip-review"}
			case "disabled":
				h.Cfg.CI.Reviews = map[string][]string{"test": {}}
			case "older done", "removed":
				// A worker can finish or remove the failed attempt before the next
				// poll, after which the PR may already point at a newer commit.
				if recovery == "older done" {
					require.NoError(t, h.DB.MarkReviewAttemptDone("acme/api", pr.Number, pr.HeadRefOid))
				} else {
					require.NoError(t, h.DB.DeleteReviewAttempt("acme/api", pr.Number, pr.HeadRefOid))
				}
				pr.HeadRefOid = "head-b"
			}
			if recovery == "new head" || recovery == "reviewed head" {
				h.Poller.poll(context.Background())
				assert.False(t, decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy,
					"a newer review does not resolve the older attempt until it is removed")
			}
			if recovery == "retry" || recovery == "new head" || recovery == "reviewed head" || recovery == "disabled" {
				_, err = h.DB.MakeTransientReviewAttemptsDue(time.Now())
				require.NoError(t, err)
			}
			h.Poller.poll(context.Background())
			assert.True(t, decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
			if recovery == "skipped" || recovery == "disabled" {
				h.Poller.poll(context.Background())
				attempt, err := h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
				require.NoError(t, err)
				assert.Nil(t, attempt, "canceled attempts must not be re-armed")
			}
			if recovery == "active" || recovery == "done" {
				assert.True(t, h.hasPanel(t, "acme/api", pr.Number, pr.HeadRefOid))
				assert.Empty(t, *skipped, "skip labels leave active and completed reviews unchanged")
			}
			if recovery == "retry" || recovery == "new head" {
				assert.True(t, h.hasPanel(t, "acme/api", pr.Number, pr.HeadRefOid))
			}
		})
	}
}

func TestHealthCIPollerRetryErrorsBelongToHead(t *testing.T) {
	h, server := newCIHealthHarness(t)
	h.stubProcessPRGit()
	h.Cfg.CI.Repos = []string{"acme/api"}
	pr := ghPR{Number: 1, HeadRefOid: "head-new", BaseRefName: "main"}
	// The current retry fails before the sweep removes the older attempt.
	for _, head := range []string{"head-new", "head-old"} {
		_, err := h.DB.ReserveReviewAttempt("acme/api", pr.Number, head, time.Now())
		require.NoError(t, err)
		require.NoError(t, h.DB.DeferReviewAttempt("acme/api", pr.Number, head,
			"transient", "provider unavailable", nil, time.Now().Add(-time.Minute), false))
	}
	h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return []ghPR{pr}, nil }
	h.Poller.gitFetchFn = func(context.Context, string, []string) error { return errors.New("fetch unavailable") }
	h.Poller.poll(context.Background())
	assert.False(t, decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
	old, err := h.DB.GetReviewAttempt("acme/api", pr.Number, "head-old")
	require.NoError(t, err)
	assert.Nil(t, old)
	current, err := h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
	require.NoError(t, err)
	require.NotNil(t, current)
	assert.Equal(t, "pending", current.State)
}

func TestHealthCIPollerRetrySweepFailures(t *testing.T) {
	for _, failure := range []string{"list", "lookup", "missing refs", "claim", "delete stale", "delete skipped", "delete closed"} {
		t.Run(failure, func(t *testing.T) {
			h, server := newCIHealthHarness(t)
			h.stubProcessPRGit()
			h.Cfg.CI.Repos = []string{"acme/api"}
			h.Cfg.CI.Agents = []string{"test"}
			h.Cfg.CI.ReviewTypes = []string{"security"}
			pr := ghPR{Number: 1, HeadRefOid: "head-a", BaseRefName: "main"}
			_, err := h.DB.ReserveReviewAttempt("acme/api", pr.Number, pr.HeadRefOid, time.Now())
			require.NoError(t, err)
			require.NoError(t, h.DB.DeferReviewAttempt("acme/api", pr.Number, pr.HeadRefOid,
				"transient", "provider unavailable", nil, time.Now().Add(-time.Minute), false))
			// The ordinary list can omit a PR; the retry sweep checks it directly.
			h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return nil, nil }
			target := panelPostTarget{Open: true, HeadSHA: pr.HeadRefOid, BaseRefName: pr.BaseRefName}
			var lookupErr error
			h.Poller.prPostTargetFn = func(_ context.Context, repo string, number int) (panelPostTarget, error) {
				require.Equal(t, "acme/api", repo)
				require.Equal(t, pr.Number, number)
				return target, lookupErr
			}
			switch failure {
			case "list":
				_, err = h.DB.Exec(`UPDATE ci_pr_review_attempts SET last_panel_run_uuid = 'invalid'`)
			case "lookup":
				lookupErr = errors.New("provider unavailable")
			case "missing refs":
				target.HeadSHA = ""
			case "claim":
				_, err = h.DB.Exec(`CREATE TRIGGER fail_retry BEFORE UPDATE ON ci_pr_review_attempts
					BEGIN SELECT RAISE(FAIL, 'claim unavailable'); END`)
			case "delete stale", "delete skipped", "delete closed":
				_, err = h.DB.Exec(`CREATE TRIGGER fail_retry BEFORE DELETE ON ci_pr_review_attempts
					BEGIN SELECT RAISE(FAIL, 'delete unavailable'); END`)
				switch failure {
				case "delete stale":
					target.HeadSHA = "head-b"
				case "delete skipped":
					h.Cfg.CI.SkipLabels = []string{"skip-review"}
					target.Labels = []string{"skip-review"}
				case "delete closed":
					target.Open = false
				}
			}
			require.NoError(t, err)
			h.Poller.poll(context.Background())
			health := decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
			assert.False(t, health.Healthy)
			require.NotEmpty(t, health.RecentErrors)
			assert.Equal(t, "ci", health.RecentErrors[0].Component)

			// Repair the dependency, then let the real sweep enqueue or clean up.
			_, err = h.DB.Exec(`DROP TRIGGER IF EXISTS fail_retry`)
			require.NoError(t, err)
			_, err = h.DB.Exec(`UPDATE ci_pr_review_attempts SET last_panel_run_uuid = ''`)
			require.NoError(t, err)
			lookupErr = nil
			if failure == "missing refs" {
				target.HeadSHA = pr.HeadRefOid
			}
			h.Poller.poll(context.Background())
			assert.True(t, decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
		})
	}
}

func TestHealthCIPollerClosedPRCleanupFailure(t *testing.T) {
	for _, failure := range []string{"lookup", "parent cancellation", "member cancellation", "mapping deletion", "attempt deletion"} {
		t.Run(failure, func(t *testing.T) {
			assert := assert.New(t)
			h, server := newCIHealthHarness(t)
			h.Cfg.CI.Repos = []string{"acme/api"}
			_, synth, members := h.seedBlockedPanelRun(t, "acme/api", 1, "head-a", "base..head-a",
				[]jobSpec{{Agent: "test", ReviewType: "review"}})
			h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return nil, nil }
			h.Poller.isPROpenFn = func(string, int) bool { return false }
			var lookupErr error
			h.Poller.prPostTargetFn = func(context.Context, string, int) (panelPostTarget, error) {
				return panelPostTarget{Open: false}, lookupErr
			}
			// Workers can emit cancellation before the rest of the sweep finishes.
			// The synthesis event removes its attempt, so cleanup must remain
			// discoverable through the panel mapping if a later operation fails.
			h.Poller.jobCancelFn = func(id int64) {
				h.Poller.handleReviewCanceled(Event{Type: "review.canceled", JobID: id})
			}
			var trigger string
			switch failure {
			case "lookup":
				lookupErr = errors.New("provider unavailable")
			case "parent cancellation":
				trigger = `CREATE TRIGGER fail_cleanup BEFORE UPDATE OF status ON review_jobs
					WHEN OLD.panel_role = 'synthesis' AND NEW.status = 'canceled'
					BEGIN SELECT RAISE(FAIL, 'cancel unavailable'); END`
			case "member cancellation":
				trigger = `CREATE TRIGGER fail_cleanup BEFORE UPDATE OF status ON review_jobs
					WHEN OLD.panel_role = 'member' AND NEW.status = 'canceled'
					BEGIN SELECT RAISE(FAIL, 'cancel unavailable'); END`
			case "mapping deletion":
				trigger = `CREATE TRIGGER fail_cleanup BEFORE DELETE ON ci_pr_panels
					BEGIN SELECT RAISE(FAIL, 'delete unavailable'); END`
			case "attempt deletion":
				trigger = `CREATE TRIGGER fail_cleanup BEFORE DELETE ON ci_pr_review_attempts
					BEGIN SELECT RAISE(FAIL, 'delete unavailable'); END`
			}
			if trigger != "" {
				_, err := h.DB.Exec(trigger)
				require.NoError(t, err)
			}
			for range 2 {
				h.Poller.poll(context.Background())
				health := decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
				assert.False(health.Healthy, "an unrelated successful sweep must not clear the failure")
				assert.Contains(health.Components, storage.ComponentHealth{
					Name: "ci", Healthy: false, Message: "polling failed for acme/api",
				})
			}
			if failure == "lookup" || failure == "parent cancellation" {
				assert.Equal(storage.JobStatusQueued, h.jobStatus(t, synth.ID))
			}
			if failure == "lookup" || failure == "parent cancellation" || failure == "member cancellation" {
				assert.Equal(storage.JobStatusQueued, h.jobStatus(t, members[0].ID))
			}
			if failure == "member cancellation" || failure == "mapping deletion" {
				attempt, err := h.DB.GetReviewAttempt("acme/api", 1, "head-a")
				require.NoError(t, err)
				assert.Nil(attempt, "the cancellation event has removed the attempt")
				panel, err := h.DB.GetCIPanelByPRSHA("acme/api", 1, "head-a")
				require.NoError(t, err, "unfinished cleanup must retain its mapping")
				assert.NotNil(panel.RetiredAt, "the canceled run must not post")
			}
			_, err := h.DB.Exec(`DROP TRIGGER IF EXISTS fail_cleanup`)
			require.NoError(t, err)
			lookupErr = nil
			h.Poller.poll(context.Background())
			assert.True(decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
			assert.Equal(storage.JobStatusCanceled, h.jobStatus(t, synth.ID))
			assert.Equal(storage.JobStatusCanceled, h.jobStatus(t, members[0].ID))
			_, err = h.DB.GetCIPanelByPRSHA("acme/api", 1, "head-a")
			require.ErrorIs(t, err, sql.ErrNoRows)
			attempt, err := h.DB.GetReviewAttempt("acme/api", 1, "head-a")
			require.NoError(t, err)
			assert.Nil(attempt)
		})
	}
}

func TestHealthCIPollerStuckAttemptFailure(t *testing.T) {
	for _, failure := range []string{"list", "rearm"} {
		t.Run(failure, func(t *testing.T) {
			assert := assert.New(t)
			h, server := newCIHealthHarness(t)
			h.Cfg.CI.Repos = []string{"acme/api"}
			pr := ghPR{Number: 1, HeadRefOid: "head-a", BaseRefName: "main"}
			_, err := h.DB.ReserveReviewAttempt("acme/api", pr.Number, pr.HeadRefOid, time.Now())
			require.NoError(t, err)
			h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return nil, nil }
			switch failure {
			case "list":
				_, err = h.DB.Exec(`UPDATE ci_pr_review_attempts SET last_panel_run_uuid = 'invalid'`)
			case "rearm":
				_, err = h.DB.Exec(`CREATE TRIGGER fail_rearm BEFORE UPDATE ON ci_pr_review_attempts
					BEGIN SELECT RAISE(FAIL, 'rearm unavailable'); END`)
			}
			require.NoError(t, err)
			for range 2 {
				h.Poller.poll(context.Background())
				assert.False(decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
				var state string
				require.NoError(t, h.DB.QueryRow(`SELECT state FROM ci_pr_review_attempts`).Scan(&state))
				assert.Equal("pending", state, "failed reconciliation must not change an indeterminate attempt")
			}
			_, err = h.DB.Exec(`DROP TRIGGER IF EXISTS fail_rearm`)
			require.NoError(t, err)
			_, err = h.DB.Exec(`UPDATE ci_pr_review_attempts SET last_panel_run_uuid = ''`)
			require.NoError(t, err)
			h.Poller.poll(context.Background())
			assert.True(decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
			attempt, err := h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
			require.NoError(t, err)
			require.NotNil(t, attempt)
			assert.Equal("deferred", attempt.State)
			assert.NotNil(attempt.NextAttemptAt)
		})
	}
}

func TestHealthCIPollerDiscoveryFailureAndRecovery(t *testing.T) {
	for _, discovery := range []string{"wildcard", "canonical name"} {
		t.Run(discovery, func(t *testing.T) {
			h, server := newCIHealthHarness(t)
			discoveryErr := errors.New("provider unavailable")
			h.Cfg.CI.Repos = []string{"acme/api"}
			if discovery == "wildcard" {
				h.Cfg.CI.Repos = append(h.Cfg.CI.Repos, "acme/*")
				h.Poller.repoResolver.listReposFn = func(context.Context, string, string) ([]string, error) {
					return []string{"acme/api"}, discoveryErr
				}
			} else {
				h.Poller.repoResolver.canonicalRepoFn = func(context.Context, string, string) (string, error) {
					return "acme/api", discoveryErr
				}
			}
			var visited []string
			h.Poller.listOpenPRsFn = func(_ context.Context, repo string) ([]ghPR, error) {
				visited = append(visited, repo)
				return nil, nil
			}

			h.Poller.poll(context.Background())
			health := decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
			assert.False(t, health.Healthy)
			assert.Contains(t, health.Components, storage.ComponentHealth{
				Name: "ci", Healthy: false, Message: "repository discovery failed",
			})
			assert.Equal(t, []string{"acme/api"}, visited, "fallback repositories still poll")

			discoveryErr = nil
			h.Poller.poll(context.Background())
			health = decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
			assert.True(t, health.Healthy)
		})
	}
}

func TestHealthCIPollerRemovedRepository(t *testing.T) {
	h, server := newCIHealthHarness(t)
	h.Cfg.CI.Repos = []string{"acme/api"}
	h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) {
		return nil, errors.New("provider unavailable")
	}
	h.Poller.poll(context.Background())
	assert.False(t, decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)

	h.Cfg.CI.Repos = nil
	h.Poller.poll(context.Background())
	assert.True(t, decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
}

func TestHealthCIPollerStopped(t *testing.T) {
	server := setupTestServer(t)
	cfg := config.DefaultConfig()
	cfg.CI.Enabled = true
	p := NewCIPoller(server.db, NewStaticConfig(cfg), nil)
	server.SetCIPoller(p)
	require.NoError(t, p.Start())
	p.Stop()

	health := decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
	assert.False(t, health.Healthy)
	assert.Contains(t, health.Components, storage.ComponentHealth{
		Name: "ci", Healthy: false, Message: "not running",
	})
}
