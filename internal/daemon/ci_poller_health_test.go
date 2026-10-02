package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	googlegithub "github.com/google/go-github/v91/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/review"
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

func TestHealthCIPollerReviewRecovery(t *testing.T) {
	assert := assert.New(t)
	h, server := newCIHealthHarness(t)
	h.stubProcessPRGit()
	h.Cfg.CI.Repos = []string{"acme/api"}
	h.Cfg.CI.Agents = []string{"test"}
	h.Cfg.CI.ReviewTypes = []string{"security"}
	pr := ghPR{Number: 1, HeadRefOid: "head-a", BaseRefName: "main"}
	h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return []ghPR{pr}, nil }
	comments := h.CaptureComments()
	h.Poller.poll(context.Background())
	assert.True(decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy,
		"ordinary queued reviews are healthy")

	for range 2 {
		synthID := h.drivePanelOutcome(t, "acme/api", pr.Number, pr.HeadRefOid, "transient")
		h.Poller.handleReviewFailed(ciEvent(synthID, "review.failed"))
		h.Poller.poll(context.Background())
		assert.False(decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)

		_, err := h.DB.MakeTransientReviewAttemptsDue(time.Now())
		require.NoError(t, err)
		// Observe health inside enqueue, before end-of-poll reconciliation can
		// hide a premature recovery from a concurrent health request.
		h.Poller.setCommitStatusFn = func(string, string, string, string) error {
			assert.False(decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy,
				"enqueueing a retry does not prove recovery")
			return nil
		}
		h.Poller.poll(context.Background())
		assert.False(decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy,
			"a queued retry must retain its previous failure")
		for _, member := range h.panelMembers(t, "acme/api", pr.Number, pr.HeadRefOid) {
			h.markJobRunning(t, member.ID)
		}
		h.Poller.poll(context.Background())
		health := decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
		assert.False(health.Healthy,
			"a running retry must retain its previous failure")
		assert.Len(health.RecentErrors, 1, "retries continue the same failure without recording a fresh alert")
	}
	assert.Empty(*comments, "failed runs produce no review")
	synthID := h.drivePanelOutcome(t, "acme/api", pr.Number, pr.HeadRefOid, "done")
	h.Poller.handleReviewCompleted(ciEvent(synthID, "review.completed"))
	require.Len(t, *comments, 1, "recovery requires delivery of usable review output")
	h.Poller.poll(context.Background())
	assert.True(decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
}

func TestHealthCIPollerFirstAttemptWithoutOutput(t *testing.T) {
	assert := assert.New(t)
	h, server := newCIHealthHarness(t)
	h.stubProcessPRGit()
	h.Cfg.CI.Repos = []string{"acme/api"}
	h.Cfg.CI.Agents = []string{"test"}
	h.Cfg.CI.ReviewTypes = []string{"security"}
	pr := ghPR{Number: 1, HeadRefOid: "head-a", BaseRefName: "main"}
	h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return []ghPR{pr}, nil }
	h.Poller.poll(context.Background())
	assert.True(decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)

	panel, err := h.DB.GetActiveCIPanelByPRSHA("acme/api", pr.Number, pr.HeadRefOid)
	require.NoError(t, err)
	for _, member := range h.panelMembers(t, "acme/api", pr.Number, pr.HeadRefOid) {
		h.markJobCanceled(t, member.ID, review.TimeoutErrorPrefix+"review deadline reached")
	}
	require.NotNil(t, panel.SynthesisJobID)
	h.markJobFailed(t, *panel.SynthesisJobID, "synthesis released after all members timed out")
	h.Poller.handleReviewFailed(ciEvent(*panel.SynthesisJobID, "review.failed"))
	h.Poller.poll(context.Background())

	attempt, err := h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
	require.NoError(t, err)
	require.NotNil(t, attempt)
	assert.Equal(1, attempt.Attempt)
	assert.Equal("done", attempt.State)
	assert.Empty(attempt.LastErrorClass)
	assert.Empty(attempt.LastErrorExcerpt)
	health := decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
	assert.False(health.Healthy)
	assert.Contains(health.Components, storage.ComponentHealth{
		Name: "ci", Healthy: false, Message: "review failed for acme/api#1",
	})
}

func TestHealthCIPollerSkipStatusIsNotRepeated(t *testing.T) {
	for _, tc := range []struct {
		name        string
		state       string
		description string
		readError   bool
		wantWrites  int
	}{
		{name: "no status", wantWrites: 1},
		{name: "failed review", state: "error", description: "Review unavailable", wantWrites: 1},
		{name: "pending review", state: "pending", description: "Review in progress", wantWrites: 1},
		{name: "already skipped", state: "success", description: "Review skipped: label skip-review"},
		{name: "changed label", state: "success", description: "Review skipped: label old-label", wantWrites: 1},
		{name: "lookup failed", readError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			h, healthServer := newCIHealthHarness(t)
			h.Cfg.CI.Repos = []string{"acme/api"}
			h.Cfg.CI.SkipLabels = []string{"skip-review"}
			pr := ghPR{Number: 1, HeadRefOid: "head-a", Labels: []string{"skip-review"}}
			h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return []ghPR{pr}, nil }
			current := googlegithub.RepoStatus{
				Context: new("roborev"), State: &tc.state, Description: &tc.description,
			}
			var writes []googlegithub.RepoStatus
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v3/repos/acme/api/commits/head-a/status":
					if tc.readError {
						http.Error(w, `{"message":"status lookup unavailable"}`, http.StatusBadGateway)
						return
					}
					if r.URL.Query().Get("page") == "" {
						w.Header().Set("Link", fmt.Sprintf("<http://%s%s?page=2>; rel=\"next\"", r.Host, r.URL.Path))
						fmt.Fprint(w, `{"statuses":[{"context":"build","state":"success","description":"Review skipped: label skip-review"}]}`)
						return
					}
					statuses := []googlegithub.RepoStatus{}
					if current.GetState() != "" {
						statuses = append(statuses, current)
					}
					assert.NoError(json.NewEncoder(w).Encode(map[string]any{"statuses": statuses}))
				case r.Method == http.MethodPost && r.URL.Path == "/api/v3/repos/acme/api/statuses/head-a":
					assert.NoError(json.NewDecoder(r.Body).Decode(&current))
					writes = append(writes, current)
					w.WriteHeader(http.StatusCreated)
					assert.NoError(json.NewEncoder(w).Encode(current))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(api.Close)
			h.Poller.githubAPIURL = api.URL + "/api/v3"
			h.Poller.setCommitStatusFn = nil
			for range 2 {
				h.Poller.poll(context.Background())
				assert.Equal(!tc.readError, decodeHealthStatus(t, executeHealthCheck(healthServer, http.MethodGet)).Healthy)
			}
			require.Len(t, writes, tc.wantWrites)
			for _, status := range writes {
				assert.Equal("roborev", status.GetContext())
				assert.Equal("success", status.GetState())
				assert.Equal("Review skipped: label skip-review", status.GetDescription())
			}
		})
	}
}

func TestHealthCIPollerExhaustedRetry(t *testing.T) {
	for _, recovery := range []string{"closed", "new head", "skipped"} {
		t.Run(recovery, func(t *testing.T) {
			assert := assert.New(t)
			h, server := newCIHealthHarness(t)
			h.stubProcessPRGit()
			h.Cfg.CI.Repos = []string{"acme/api"}
			h.Cfg.CI.Agents = []string{"test"}
			h.Cfg.CI.ReviewTypes = []string{"security"}
			h.Cfg.CI.ThrottleInterval = "0"
			pr := ghPR{Number: 1, HeadRefOid: "head-a", BaseRefName: "main"}
			h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return []ghPR{pr}, nil }
			comments := h.CaptureComments()
			h.Poller.poll(context.Background())
			// Exhaust the existing retry window through the normal finalizer.
			_, err := h.DB.Exec(`UPDATE ci_pr_review_attempts SET first_attempt_at = datetime('now', '-4 days')`)
			require.NoError(t, err)
			synthID := h.drivePanelOutcome(t, "acme/api", pr.Number, pr.HeadRefOid, "transient")
			h.Poller.handleReviewFailed(ciEvent(synthID, "review.failed"))
			h.Poller.poll(context.Background())
			failedPanel, err := h.DB.GetActiveCIPanelByPRSHA("acme/api", pr.Number, pr.HeadRefOid)
			require.NoError(t, err)
			assert.Empty(*comments)
			assert.False(decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy,
				"giving up without a review is not recovery")
			switch recovery {
			case "closed":
				h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return nil, nil }
				h.Poller.isPROpenFn = func(string, int) bool { return false }
				_, err := h.DB.Exec(`CREATE TRIGGER fail_cleanup BEFORE DELETE ON ci_pr_review_attempts
					BEGIN SELECT RAISE(FAIL, 'cleanup unavailable'); END`)
				require.NoError(t, err)
				h.Poller.poll(context.Background())
				assert.False(decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
				panel, err := h.DB.GetCIPanelByRunUUID(failedPanel.PanelRunUUID)
				require.NoError(t, err)
				assert.Nil(panel.RetiredAt, "failed cleanup must roll back panel retirement")
				_, err = h.DB.Exec(`DROP TRIGGER fail_cleanup`)
				require.NoError(t, err)
			case "new head":
				pr.HeadRefOid = "head-b"
			case "skipped":
				h.Cfg.CI.SkipLabels = []string{"skip-review"}
				pr.Labels = []string{"skip-review"}
				h.Poller.setSkippedCheckFn = func(string, string, string) error {
					return errors.New("check publishing unavailable")
				}
				h.Poller.poll(context.Background())
				assert.False(decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy,
					"failed check publishing must retain the failure for the next poll")
				h.CaptureSkippedChecks()
				h.Poller.setCommitStatusFn = func(string, string, string, string) error {
					return errors.New("status publishing unavailable")
				}
				h.Poller.poll(context.Background())
				assert.False(decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy,
					"failed status publishing must retain the failure for the next poll")
			}
			skipped := h.CaptureSkippedChecks()
			statuses := h.CaptureCommitStatuses()
			h.Poller.poll(context.Background())
			assert.True(decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy,
				"a failed review no longer needed by the PR must not hold health unhealthy")
			if recovery == "skipped" {
				assert.Contains(*skipped, capturedSkippedCheck{
					Repo: "acme/api", SHA: "head-a", Summary: "Review skipped: label skip-review",
				})
				assert.Contains(*statuses, capturedStatus{
					Repo: "acme/api", SHA: "head-a", State: "success", Desc: "Review skipped: label skip-review",
				})
			}

			// Reopen, unskip, or return to the same commit. The failed panel
			// must not suppress a fresh review of that commit.
			pr.HeadRefOid = "head-a"
			pr.Labels = nil
			h.Poller.isPROpenFn = func(string, int) bool { return true }
			h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return []ghPR{pr}, nil }
			h.Poller.poll(context.Background())
			panel, err := h.DB.GetActiveCIPanelByPRSHA("acme/api", pr.Number, pr.HeadRefOid)
			require.NoError(t, err)
			require.NotEqual(t, failedPanel.PanelRunUUID, panel.PanelRunUUID)
			synthID = h.drivePanelOutcome(t, "acme/api", pr.Number, pr.HeadRefOid, "done")
			h.Poller.handleReviewCompleted(ciEvent(synthID, "review.completed"))
			assert.Len(*comments, 1, "the reopened review delivers output")
		})
	}
}

func TestHealthCIPollerEnqueueFailure(t *testing.T) {
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
			// A stranded enqueue has no provider failure to recover from.
			_, err = h.DB.RearmStuckReviewAttempt("acme/api", pr.Number, pr.HeadRefOid, now.Add(-time.Minute))
			require.NoError(t, err)
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
				Name: "ci", Healthy: false, Message: "review failed for acme/api#1",
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
				_, err = h.DB.Exec(`UPDATE ci_pr_review_attempts SET next_attempt_at = datetime('now', '-1 minute')
					WHERE state = 'deferred'`)
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

func TestHealthCIPollerStartRestoresRetryHealth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		h, server := newCIHealthHarness(t)
		h.Cfg.CI.Repos = []string{"acme/api"}
		h.Cfg.CI.PollInterval = "30s"
		pr := ghPR{Number: 1, HeadRefOid: "head-a", BaseRefName: "main"}
		nextAt := time.Now().Add(time.Hour).Truncate(time.Second)
		for _, repo := range []string{"acme/api", "acme/unconfigured"} {
			_, err := h.DB.ReserveReviewAttempt(repo, pr.Number, pr.HeadRefOid, time.Now())
			require.NoError(t, err)
			require.NoError(t, h.DB.DeferReviewAttempt(repo, pr.Number, pr.HeadRefOid,
				"genuine", "review failed", nil, nextAt, true))
		}
		h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return []ghPR{pr}, nil }
		// Start a fresh poller over persisted failures. Unlike transient failures,
		// genuine failures retain their backoff across startup.
		h.Poller.running = false
		require.NoError(t, h.Poller.Start())
		defer h.Poller.Stop()
		synctest.Wait()

		health := decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
		assert.False(health.Healthy)
		assert.Contains(health.Components, storage.ComponentHealth{
			Name: "ci", Healthy: false, Message: "review failed for acme/api#1",
		})
		require.Len(t, health.RecentErrors, 1, "only configured repositories contribute retry health")
		attempt, err := h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
		require.NoError(t, err)
		require.NotNil(t, attempt)
		assert.Equal("deferred", attempt.State)
		require.NotNil(t, attempt.NextAttemptAt)
		assert.WithinDuration(nextAt, *attempt.NextAttemptAt, 0, "hydration must not accelerate retries")

		time.Sleep(30 * time.Second)
		synctest.Wait()
		health = decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
		assert.False(health.Healthy)
		assert.Len(health.RecentErrors, 1, "restoring the same failure must not duplicate error history")

		require.NoError(t, h.DB.MarkReviewAttemptDone("acme/api", pr.Number, pr.HeadRefOid))
		time.Sleep(30 * time.Second)
		synctest.Wait()
		health = decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
		assert.True(health.Healthy, "the unconfigured repository must not keep CI unhealthy")
		assert.Len(health.RecentErrors, 1, "recovery preserves error history")
	})
}

func TestHealthCIPollerRestartRecognizesRetryOutcome(t *testing.T) {
	for _, recovery := range []string{"active", "posted", "exhausted", "abandoned", "removed"} {
		t.Run(recovery, func(t *testing.T) {
			h, server := newCIHealthHarness(t)
			h.Cfg.CI.Repos = []string{"acme/api"}
			h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return nil, nil }
			_, err := h.DB.ReserveReviewAttempt("acme/api", 1, "head-a", time.Now())
			require.NoError(t, err)
			require.NoError(t, h.DB.DeferReviewAttempt("acme/api", 1, "head-a",
				"genuine", "review failed", nil, time.Now().Add(-time.Minute), true))
			switch recovery {
			case "active", "posted", "exhausted", "abandoned":
				claimed, _, _, err := h.DB.ClaimDueReviewAttempt("acme/api", 1, "head-a", time.Now())
				require.NoError(t, err)
				require.True(t, claimed)
				// Successful re-enqueue retains the previous attempt's error fields.
				opts := storage.EnqueueOpts{RepoID: h.Repo.ID, GitRef: "base..head-a", Agent: "test"}
				created, _, _, err := h.DB.CreateCIPanelRun("acme/api", 1, "head-a", []storage.EnqueueOpts{opts}, opts)
				require.NoError(t, err)
				require.True(t, created)
				if recovery != "active" {
					panel, err := h.DB.GetActiveCIPanelByPRSHA("acme/api", 1, "head-a")
					require.NoError(t, err)
					outcome := storage.PanelOutcomeReviewPosted
					switch recovery {
					case "exhausted":
						outcome = storage.PanelOutcomeNoReviewPosted
					case "abandoned":
						outcome = storage.PanelOutcomeAbandoned
					}
					require.NoError(t, h.DB.MarkPanelPosted(panel.ID, outcome))
				}
			case "removed":
				require.NoError(t, h.DB.DeleteReviewAttempt("acme/api", 1, "head-a"))
			}
			h.Poller.poll(context.Background())
			health := decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
			if recovery == "posted" || recovery == "removed" {
				assert.True(t, health.Healthy)
				assert.Empty(t, health.RecentErrors, "recovered retries must not create fresh alerts")
			} else {
				assert.False(t, health.Healthy, "retry health must survive a restart until a review is delivered")
				assert.Len(t, health.RecentErrors, 1)
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
			if failure == "delete stale" || failure == "delete skipped" || failure == "delete closed" {
				assert.True(t, decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
			} else {
				assert.False(t, decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy,
					"a repaired sweep must still deliver the failed review")
				comments := h.CaptureComments()
				synthID := h.drivePanelOutcome(t, "acme/api", pr.Number, pr.HeadRefOid, "done")
				h.Poller.handleReviewCompleted(ciEvent(synthID, "review.completed"))
				require.Len(t, *comments, 1)
				h.Poller.poll(context.Background())
				assert.True(t, decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
			}
		})
	}
}

func TestHealthCIPollerCleanupNonPullRequestTarget(t *testing.T) {
	for _, tc := range []struct {
		name        string
		issueStatus int
		issueBody   string
		wantCleanup bool
	}{
		{"ordinary issue", http.StatusOK, `{"number":1,"state":"open"}`, true},
		{"actual pull request", http.StatusOK, `{"number":1,"state":"open","pull_request":{"url":"https://api.github.com/repos/acme/api/pulls/1"}}`, false},
		{"missing issue", http.StatusNotFound, `{"message":"Not Found"}`, false},
		{"inaccessible issue", http.StatusForbidden, `{"message":"Resource not accessible by integration"}`, false},
		{"issue lookup unavailable", http.StatusBadGateway, `{"message":"Unavailable"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			h, server := newCIHealthHarness(t)
			h.Cfg.CI.Repos = []string{"acme/api"}
			panel, synth, members := h.seedBlockedPanelRun(t, "acme/api", 1, "head-a", "base..head-a",
				[]jobSpec{{Agent: "test", ReviewType: "review"}})
			require.NoError(t, h.DB.MarkPanelRetired(panel.ID))
			require.NoError(t, h.DB.DeferReviewAttempt("acme/api", 1, "head-a",
				"genuine", "review failed", &panel.PanelRunUUID, time.Now().Add(time.Hour), true))
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(http.MethodGet, r.Method, "cleanup must not publish to an issue")
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v3/repos/acme/api/pulls/1":
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `{"message":"Not Found"}`)
				case "/api/v3/repos/acme/api/issues/1":
					w.WriteHeader(tc.issueStatus)
					fmt.Fprint(w, tc.issueBody)
				default:
					http.NotFound(w, r)
				}
			}))
			defer api.Close()
			h.Poller.githubAPIURL = api.URL + "/api/v3"
			h.Poller.prPostTargetFn = nil
			h.Poller.isPROpenFn = nil
			h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return nil, nil }
			for range 2 {
				h.Poller.poll(context.Background())
				assert.Equal(tc.wantCleanup, decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
			}
			attempt, err := h.DB.GetReviewAttempt("acme/api", 1, "head-a")
			require.NoError(t, err)
			_, panelErr := h.DB.GetCIPanelByPRSHA("acme/api", 1, "head-a")
			if tc.wantCleanup {
				assert.Nil(attempt, "confirmed issue must no longer reserve a review attempt")
				require.ErrorIs(t, panelErr, sql.ErrNoRows)
				assert.Equal(storage.JobStatusCanceled, h.jobStatus(t, synth.ID))
				assert.Equal(storage.JobStatusCanceled, h.jobStatus(t, members[0].ID))
			} else {
				assert.NotNil(attempt, "ambiguous access failure must retain the attempt")
				require.NoError(t, panelErr)
				assert.Equal(storage.JobStatusQueued, h.jobStatus(t, synth.ID))
				assert.Equal(storage.JobStatusQueued, h.jobStatus(t, members[0].ID))
			}
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

func TestHealthCIPollerSupersededCleanupFailure(t *testing.T) {
	for _, failure := range []string{"parent cancellation", "member cancellation", "attempt deletion"} {
		t.Run(failure, func(t *testing.T) {
			assert := assert.New(t)
			h, server := newCIHealthHarness(t)
			h.stubProcessPRGit()
			h.Cfg.CI.Repos = []string{"acme/api"}
			oldPanel, oldSynth, oldMembers := h.seedBlockedPanelRun(t, "acme/api", 1, "head-old", "base..head-old",
				[]jobSpec{{Agent: "test", ReviewType: "review"}})
			pr := ghPR{Number: 1, HeadRefOid: "head-new", BaseRefName: "main"}
			h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return []ghPR{pr}, nil }
			h.Poller.jobCancelFn = func(id int64) {
				h.Poller.handleReviewCanceled(Event{Type: "review.canceled", JobID: id})
			}
			var trigger string
			switch failure {
			case "parent cancellation":
				trigger = `CREATE TRIGGER fail_supersede BEFORE UPDATE OF status ON review_jobs
					WHEN OLD.panel_role = 'synthesis' AND NEW.status = 'canceled'
					BEGIN SELECT RAISE(FAIL, 'cancel unavailable'); END`
			case "member cancellation":
				trigger = `CREATE TRIGGER fail_supersede BEFORE UPDATE OF status ON review_jobs
					WHEN OLD.panel_role = 'member' AND NEW.status = 'canceled'
					BEGIN SELECT RAISE(FAIL, 'cancel unavailable'); END`
			case "attempt deletion":
				trigger = `CREATE TRIGGER fail_supersede BEFORE DELETE ON ci_pr_review_attempts
					BEGIN SELECT RAISE(FAIL, 'delete unavailable'); END`
			}
			_, err := h.DB.Exec(trigger)
			require.NoError(t, err)
			require.Error(t, h.Poller.processPR(context.Background(), "acme/api", pr, h.Cfg))

			// A replacement may already have been queued by another poller. Its
			// deduplication gate must not hide unfinished cleanup of the old run.
			_, newSynth, newMembers := h.seedBlockedPanelRun(t, "acme/api", 1, pr.HeadRefOid, "base..head-new",
				[]jobSpec{{Agent: "test", ReviewType: "review"}})
			for range 2 {
				h.Poller.poll(context.Background())
				assert.False(decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
			}
			if failure == "parent cancellation" {
				assert.Equal(storage.JobStatusQueued, h.jobStatus(t, oldSynth.ID))
				assert.True(h.synthBlocked(t, oldSynth.PanelRunUUID), "failed parent cancellation must not release synthesis")
			}
			if failure != "attempt deletion" {
				assert.Equal(storage.JobStatusQueued, h.jobStatus(t, oldMembers[0].ID))
			}
			assert.True(h.panelRetiredAt(t, oldPanel.ID), "unfinished cleanup must remain non-postable")

			_, err = h.DB.Exec(`DROP TRIGGER fail_supersede`)
			require.NoError(t, err)
			h.Poller.poll(context.Background())
			assert.True(decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
			assert.Equal(storage.JobStatusCanceled, h.jobStatus(t, oldSynth.ID))
			assert.Equal(storage.JobStatusCanceled, h.jobStatus(t, oldMembers[0].ID))
			assert.Equal(storage.JobStatusQueued, h.jobStatus(t, newSynth.ID))
			assert.Equal(storage.JobStatusQueued, h.jobStatus(t, newMembers[0].ID))
			attempt, err := h.DB.GetReviewAttempt("acme/api", 1, "head-old")
			require.NoError(t, err)
			assert.Nil(attempt)
			panel, err := h.DB.GetCIPanelByPRSHA("acme/api", 1, "head-old")
			require.NoError(t, err)
			assert.Equal(oldPanel.CreatedAt, panel.CreatedAt, "cleanup retains the throttle clock")
		})
	}
}

func TestHealthCIPollerRetiredCurrentRetryKeepsBackoff(t *testing.T) {
	h, server := newCIHealthHarness(t)
	h.Cfg.CI.Repos = []string{"acme/api"}
	pr := ghPR{Number: 1, HeadRefOid: "head-a", BaseRefName: "main"}
	panel, synth, _ := h.seedBlockedPanelRun(t, "acme/api", pr.Number, pr.HeadRefOid, "base..head-a",
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "failed", Error: "provider unavailable"}})
	h.markJobFailed(t, synth.ID, "provider unavailable")
	require.NoError(t, h.DB.MarkPanelRetired(panel.ID))
	nextAt := time.Now().Add(time.Hour).Truncate(time.Second)
	require.NoError(t, h.DB.DeferReviewAttempt("acme/api", pr.Number, pr.HeadRefOid,
		"transient", "provider unavailable", &panel.PanelRunUUID, nextAt, false))
	h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return []ghPR{pr}, nil }
	h.Poller.poll(context.Background())
	assert.False(t, decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
	attempt, err := h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
	require.NoError(t, err)
	require.NotNil(t, attempt)
	assert.Equal(t, "deferred", attempt.State)
	require.NotNil(t, attempt.NextAttemptAt)
	assert.WithinDuration(t, nextAt, *attempt.NextAttemptAt, 0, "current-head backoff must survive retired-panel cleanup")
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
