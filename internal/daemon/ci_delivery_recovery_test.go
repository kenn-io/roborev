package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
)

func TestCIPanelFinalStatusRecoveryAfterRestart(t *testing.T) {
	for _, scenario := range []string{"review", "no review", "stale claim", "superseded"} {
		t.Run(scenario, func(t *testing.T) {
			assert := assert.New(t)
			h := newCIPollerHarness(t, "https://github.com/acme/api.git")
			comments := h.CaptureComments()
			panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a",
				[]jobSpec{{Agent: "test", Status: "done", Output: "No issues found."}})
			h.completeSynthesisWithReview(t, synth.ID, "No issues found.")
			require.NoError(t, h.DB.MarkPanelPosted(panel.ID, storage.PanelOutcomeReviewPosted, "", ""))
			server := newServerWithLogs(h.DB, h.Cfg, "", newTestErrorLog(), newTestActivityLog())
			server.SetCIPoller(h.Poller)
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			var statuses []capturedStatus
			failFinal := true
			var rerunJobID int64
			activeServer := server
			setStatus := func(repo, sha, state, desc string) error {
				assert.Equal("acme/api", repo)
				assert.Equal("head-a", sha)
				if state != "pending" {
					if failFinal {
						return errors.New("GitHub unavailable")
					}
					// A successor cannot publish pending while this final write is in flight.
					response := serveHuma(t, activeServer, http.MethodPost, "/api/job/rerun",
						[]byte(fmt.Sprintf(`{"job_id":%d}`, rerunJobID)))
					assert.Equal(http.StatusConflict, response.Code, response.Body.String())
				}
				statuses = append(statuses, capturedStatus{repo, sha, state, desc})
				return nil
			}
			h.Poller.setCommitStatusFn = setStatus
			rerun, err := server.humaRerunJob(context.Background(), &RerunJobInput{Body: RerunJobRequest{JobID: synth.ID}})
			require.NoError(t, err)
			rerunJobID = rerun.Body.JobID
			members, err := h.DB.GetPanelMembers(*rerun.Body.RunUUID)
			require.NoError(t, err)
			wantFinal := capturedStatus{Repo: "acme/api", SHA: "head-a", State: "success", Desc: "Review complete"}
			wantComments := 1
			for _, member := range members {
				if scenario == "no review" {
					h.markJobCanceled(t, member.ID, reviewpkg.TimeoutErrorPrefix+"deadline")
					wantFinal.State, wantFinal.Desc = "error", "No review output"
					wantComments = 0
				} else {
					h.markJobDoneWithReview(t, member.ID, "test", "No issues found.")
				}
			}
			h.completeSynthesisWithReview(t, rerunJobID, "No issues found.")
			h.Poller.handleReviewCompleted(ciEvent(rerunJobID, "review.completed"))
			pending := capturedStatus{Repo: "acme/api", SHA: "head-a", State: "pending", Desc: "Review in progress"}
			require.Equal(t, []capturedStatus{pending}, statuses, "final write failed after pending succeeded")
			require.Len(t, *comments, wantComments)
			posted, err := h.DB.GetCIPanelBySynthesisJobID(rerunJobID)
			require.NoError(t, err)
			require.NotNil(t, posted.PostedAt, "comment delivery is complete despite the status failure")
			want := []capturedStatus{pending, wantFinal}
			switch scenario {
			case "stale claim":
				// A terminated status publisher left its persisted intent and lease.
				h.backdatePanelPostClaim(t, posted.ID)
			case "superseded":
				_, err := server.humaRerunJob(context.Background(), &RerunJobInput{Body: RerunJobRequest{JobID: rerunJobID}})
				require.NoError(t, err, "failed status delivery releases ownership for a new rerun")
				want = []capturedStatus{pending, pending}
			}

			var dbPath string
			require.NoError(t, h.DB.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&dbPath))
			require.NoError(t, server.Close())
			require.NoError(t, h.DB.Close())
			db, err := storage.Open(dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			h.Cfg.CI.Repos = nil
			poller := NewCIPoller(db, NewStaticConfig(h.Cfg), nil)
			poller.setCommitStatusFn = setStatus
			poller.postPRCommentFn = h.Poller.postPRCommentFn
			restartedServer := newServerWithLogs(db, h.Cfg, "", newTestErrorLog(), newTestActivityLog())
			restartedServer.SetCIPoller(poller)
			t.Cleanup(func() { require.NoError(t, restartedServer.Close()) })
			activeServer = restartedServer
			failFinal = false
			poller.poll(context.Background())
			assert.Equal(want, statuses)
			assert.Len(*comments, wantComments, "status recovery must not repost comments")
			poller.poll(context.Background())
			assert.Equal(want, statuses, "delivered or superseded statuses must not retry")
			assert.Len(*comments, wantComments)
		})
	}
}

func TestCIPanelFinalStatusDoesNotOverwriteSkip(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	_, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a",
		[]jobSpec{{Agent: "test", Status: "canceled", Error: reviewpkg.TimeoutErrorPrefix + "deadline"}})
	h.markJobFailed(t, synth.ID, "no member output")
	pr := ghPR{Number: 7, HeadRefOid: "head-a"}
	finalAttempts := 0
	failSkip := true
	var statuses []capturedStatus
	h.Poller.setCommitStatusFn = func(repo, sha, state, desc string) error {
		assert.Equal("acme/api", repo)
		assert.Equal("head-a", sha)
		if state == "error" {
			finalAttempts++
			err := h.Poller.skipLabeledPR(repo, pr, "skip-review")
			require.ErrorContains(t, err, "status delivery in progress", "skip must wait for the in-flight final write")
			return errors.New("GitHub unavailable")
		}
		if failSkip {
			return errors.New("skip status unavailable")
		}
		statuses = append(statuses, capturedStatus{repo, sha, state, desc})
		return nil
	}
	h.Poller.handleReviewFailed(ciEvent(synth.ID, "review.failed"))
	require.ErrorContains(t, h.Poller.skipLabeledPR("acme/api", pr, "skip-review"), "skip status unavailable")
	failSkip = false
	require.NoError(t, h.Poller.skipLabeledPR("acme/api", pr, "skip-review"))
	h.Poller.poll(context.Background())
	assert.Equal(1, finalAttempts, "a successful skip supersedes the queued error")
	assert.Equal([]capturedStatus{{Repo: "acme/api", SHA: "head-a", State: "success", Desc: "Review skipped: label skip-review"}}, statuses)
}

func TestCIPanelCancellationRecoveryAfterRestart(t *testing.T) {
	for _, scenario := range []string{"failed write", "interrupted handoff", "superseded"} {
		t.Run(scenario, func(t *testing.T) {
			assert := assert.New(t)
			h := newCIPollerHarness(t, "https://github.com/acme/api.git")
			panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a",
				[]jobSpec{{Agent: "test", Status: "done", Output: "No issues found."}})
			h.completeSynthesisWithReview(t, synth.ID, "No issues found.")
			require.NoError(t, h.DB.MarkPanelPosted(panel.ID, storage.PanelOutcomeReviewPosted, "", ""))
			var statuses []capturedStatus
			failCancellation := true
			setStatus := func(repo, sha, state, desc string) error {
				if desc == "Review canceled" && failCancellation {
					return errors.New("GitHub unavailable")
				}
				statuses = append(statuses, capturedStatus{repo, sha, state, desc})
				return nil
			}
			h.Poller.setCommitStatusFn = setStatus
			server := newServerWithLogs(h.DB, h.Cfg, "", newTestErrorLog(), newTestActivityLog())
			server.SetCIPoller(h.Poller)
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			rerun, err := server.humaRerunJob(context.Background(), &RerunJobInput{Body: RerunJobRequest{JobID: synth.ID}})
			require.NoError(t, err)
			rerunPanel, err := h.DB.GetCIPanelBySynthesisJobID(rerun.Body.JobID)
			require.NoError(t, err)
			if scenario == "interrupted handoff" {
				// Pending publication was acknowledged, but its owner exited before
				// releasing the claim and observing the concurrent cancellation.
				won, err := h.DB.ClaimPanelForPosting(rerunPanel.ID, panelPostingStaleWindow)
				require.NoError(t, err)
				require.True(t, won)
			}
			_, err = server.humaCancelJob(context.Background(), &CancelJobInput{Body: CancelJobRequest{JobID: rerun.Body.JobID}})
			require.NoError(t, err)
			if scenario == "interrupted handoff" {
				h.backdatePanelPostClaim(t, rerunPanel.ID)
			}
			want := []capturedStatus{{Repo: "acme/api", SHA: "head-a", State: "pending", Desc: "Review in progress"}}
			require.Equal(t, want, statuses)
			if scenario == "superseded" {
				_, err := server.humaRerunJob(context.Background(), &RerunJobInput{Body: RerunJobRequest{JobID: rerun.Body.JobID}})
				require.NoError(t, err)
				want = append(want, capturedStatus{Repo: "acme/api", SHA: "head-a", State: "pending", Desc: "Review in progress"})
			} else {
				want = append(want, capturedStatus{Repo: "acme/api", SHA: "head-a", State: "error", Desc: "Review canceled"})
			}

			var dbPath string
			require.NoError(t, h.DB.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&dbPath))
			require.NoError(t, server.Close())
			require.NoError(t, h.DB.Close())
			db, err := storage.Open(dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			h.Cfg.CI.Repos = nil
			poller := NewCIPoller(db, NewStaticConfig(h.Cfg), nil)
			poller.setCommitStatusFn = setStatus
			failCancellation = false
			poller.poll(context.Background())
			assert.Equal(want, statuses)
			retired, err := db.GetCIPanelBySynthesisJobID(rerun.Body.JobID)
			require.NoError(t, err)
			assert.NotNil(retired.RetiredAt)
			attempt, err := db.GetReviewAttempt("acme/api", 7, "head-a")
			require.NoError(t, err)
			if scenario == "superseded" {
				require.NotNil(t, attempt)
				assert.Equal("pending", attempt.State, "cancellation must preserve the successor's attempt")
			} else {
				assert.Nil(attempt, "delivered cancellation clears the canceled attempt")
			}
			poller.poll(context.Background())
			assert.Equal(want, statuses, "recovery must not repeat a delivered cancellation")
		})
	}
}

func TestCIPanelRerunDeliveryRecoveryWithoutPolledRepo(t *testing.T) {
	for _, scenario := range []string{"lost completion", "failed comment"} {
		t.Run(scenario, func(t *testing.T) {
			assert := assert.New(t)
			h := newCIPollerHarness(t, "https://github.com/acme/api.git")
			statuses := h.CaptureCommitStatuses()
			panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a",
				[]jobSpec{{Agent: "test", Status: "done", Output: "No issues found."}})
			h.completeSynthesisWithReview(t, synth.ID, "No issues found.")
			require.NoError(t, h.DB.MarkPanelPosted(panel.ID, storage.PanelOutcomeReviewPosted, "", ""))
			server := newServerWithLogs(h.DB, h.Cfg, "", newTestErrorLog(), newTestActivityLog())
			server.SetCIPoller(h.Poller)
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			rerun, err := server.humaRerunJob(context.Background(), &RerunJobInput{Body: RerunJobRequest{JobID: synth.ID}})
			require.NoError(t, err)
			members, err := h.DB.GetPanelMembers(*rerun.Body.RunUUID)
			require.NoError(t, err)
			for _, member := range members {
				h.markJobDoneWithReview(t, member.ID, "test", "No issues found.")
			}
			h.completeSynthesisWithReview(t, rerun.Body.JobID, "No issues found.")
			if scenario == "failed comment" {
				h.Poller.postPRCommentFn = func(repo string, pr int, body string) error {
					assert.Equal("acme/api", repo)
					assert.Equal(7, pr)
					assert.Contains(body, "No issues found.")
					return errors.New("GitHub unavailable")
				}
				h.Poller.handleReviewCompleted(ciEvent(rerun.Body.JobID, "review.completed"))
			}
			comments := h.CaptureComments()
			*statuses = nil

			var dbPath string
			require.NoError(t, h.DB.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&dbPath))
			require.NoError(t, server.Close())
			require.NoError(t, h.DB.Close())
			db, err := storage.Open(dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			h.Cfg.CI.Repos = nil
			poller := NewCIPoller(db, NewStaticConfig(h.Cfg), nil)
			poller.setCommitStatusFn = h.Poller.setCommitStatusFn
			poller.postPRCommentFn = h.Poller.postPRCommentFn
			poller.prPostTargetFn = func(_ context.Context, repo string, pr int) (panelPostTarget, error) {
				assert.Equal("acme/api", repo)
				assert.Equal(7, pr)
				return panelPostTarget{Open: true, HeadSHA: "head-a"}, nil
			}
			poller.poll(context.Background())
			require.Len(t, *comments, 1, "recover final delivery even after repository polling stops")
			assert.Equal("acme/api", (*comments)[0].Repo)
			assert.Equal(7, (*comments)[0].PR)
			assert.Contains((*comments)[0].Body, "No issues found.")
			want := []capturedStatus{{Repo: "acme/api", SHA: "head-a", State: "success", Desc: "Review complete"}}
			assert.Equal(want, *statuses)
			poller.poll(context.Background())
			assert.Len(*comments, 1, "a delivered review must not be reposted")
			assert.Equal(want, *statuses)
		})
	}
}
