package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
)

func TestCIPanelSkipRerunOrdering(t *testing.T) {
	for _, phase := range []string{"deferred", "done"} {
		for _, ordering := range []string{"skip first", "skip check fails", "skip status fails", "rerun queued", "rerun posted", "rerun no review", "expired skip claim"} {
			t.Run(phase+"/"+ordering, func(t *testing.T) {
				assert := assert.New(t)
				h := newCIPollerHarness(t, "https://github.com/acme/api.git")
				h.Cfg.CI.SkipLabels = []string{"skip-review"}
				comments := h.CaptureComments()
				statuses := h.CaptureCommitStatuses()
				pr := ghPR{Number: 7, HeadRefOid: "head-a", BaseRefName: "main", Labels: []string{"skip-review"}}
				panel, synth, _ := h.seedCIPanelRun(t, "acme/api", pr.Number, pr.HeadRefOid, "base..head-a",
					[]jobSpec{{Agent: "test", Status: "failed", Error: "model unavailable"}})
				h.markJobFailed(t, synth.ID, "no member output")
				if phase == "done" {
					for range 2 {
						require.NoError(t, h.DB.DeferReviewAttempt("acme/api", pr.Number, pr.HeadRefOid,
							"genuine", "model unavailable", &panel.PanelRunUUID, time.Now(), true))
					}
				}
				h.Poller.handleReviewFailed(ciEvent(synth.ID, "review.failed"))
				before, err := h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
				require.NoError(t, err)
				require.Equal(t, phase, before.State)
				*statuses = nil
				*comments = nil
				server := newServerWithLogs(h.DB, h.Cfg, "", newTestErrorLog(), newTestActivityLog())
				t.Cleanup(func() { require.NoError(t, server.Close()) })
				request := []byte(fmt.Sprintf(`{"job_id":%d,"request_id":%q}`, synth.ID, testUUID("skip-rerun")))
				var rerun RerunJobOutput
				skipCalls := 0
				skipErr := errors.New("GitHub unavailable")
				skipFirst := ordering == "skip first" || ordering == "skip check fails" || ordering == "skip status fails"
				h.Poller.setSkippedCheckFn = func(string, string, string) error {
					skipCalls++
					// The HTTP handler must return while the GitHub call is still
					// in flight, without acquiring ownership of the skipped target.
					if skipFirst {
						response := serveHuma(t, server, http.MethodPost, "/api/job/rerun", request)
						assert.Equal(http.StatusConflict, response.Code, response.Body.String())
					}
					if ordering == "skip check fails" {
						return skipErr
					}
					return nil
				}
				if ordering == "skip status fails" {
					h.Poller.setCommitStatusFn = func(string, string, string, string) error {
						response := serveHuma(t, server, http.MethodPost, "/api/job/rerun", request)
						assert.Equal(http.StatusConflict, response.Code, response.Body.String())
						return skipErr
					}
				}

				if skipFirst {
					err = h.Poller.processPR(context.Background(), "acme/api", pr, h.Cfg)
					if phase == "done" {
						require.NoError(t, err)
						require.Zero(t, skipCalls, "completed attempts use the health reconciliation skip path")
						err = h.Poller.reconcileRetryHealth("acme/api", []ghPR{pr}, h.Cfg)
					}
					if ordering == "skip first" {
						require.NoError(t, err)
					} else {
						require.ErrorIs(t, err, skipErr)
					}
					assert.Equal(1, skipCalls)
					attempt, err := h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
					require.NoError(t, err)
					if ordering == "skip first" {
						assert.Nil(attempt)
					} else {
						require.NotNil(t, attempt)
						assert.Equal(phase, attempt.State, "failed skip preserves the existing attempt")
					}
					// Both successful and failed skip requests release their claim.
					// The rejected request ID must remain available for a real rerun.
					response := serveHuma(t, server, http.MethodPost, "/api/job/rerun", request)
					require.Equal(t, http.StatusOK, response.Code, response.Body.String())
					require.NoError(t, json.Unmarshal(response.Body.Bytes(), &rerun.Body))
				} else {
					if ordering == "expired skip claim" {
						_, err := h.DB.Exec(`UPDATE ci_pr_panels SET posting_claimed_at = datetime('now', '-1 hour') WHERE id = ?`, panel.ID)
						require.NoError(t, err)
					}
					response := serveHuma(t, server, http.MethodPost, "/api/job/rerun", request)
					require.Equal(t, http.StatusOK, response.Code, response.Body.String())
					require.NoError(t, json.Unmarshal(response.Body.Bytes(), &rerun.Body))
					if ordering == "rerun posted" || ordering == "rerun no review" {
						members, err := h.DB.GetPanelMembers(*rerun.Body.RunUUID)
						require.NoError(t, err)
						require.Len(t, members, 1)
						if ordering == "rerun posted" {
							h.markJobDoneWithReview(t, members[0].ID, "test", "No issues found.")
							h.completeSynthesisWithReview(t, rerun.Body.JobID, "No issues found.")
							h.Poller.handleReviewCompleted(ciEvent(rerun.Body.JobID, "review.completed"))
						} else {
							h.markJobCanceled(t, members[0].ID, reviewpkg.TimeoutErrorPrefix+"deadline")
							h.markJobFailed(t, rerun.Body.JobID, "no member output")
							h.Poller.handleReviewFailed(ciEvent(rerun.Body.JobID, "review.failed"))
						}
						finished, err := h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
						require.NoError(t, err)
						require.Equal(t, "done", finished.State)
					}
					statusCount := len(*statuses)
					// The skip decision was made from the previous attempt before
					// the rerun transferred ownership, possibly finishing already.
					err = h.Poller.skipLabeledPR("acme/api", pr, "skip-review", before)
					require.ErrorIs(t, err, storage.ErrCIPanelActive)
					if ordering == "rerun queued" {
						err = h.Poller.skipLabeledPR("acme/api", pr, "skip-review", nil)
						require.ErrorIs(t, err, storage.ErrCIPanelActive, "a previously absent attempt cannot erase a new owner")
					}
					assert.Zero(skipCalls)
					assert.Len(*statuses, statusCount, "stale skip must not overwrite successor status")
				}
				attempt, err := h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
				require.NoError(t, err)
				require.NotNil(t, attempt, "skip must not remove the successor's attempt")
				expectedState := "pending"
				if ordering == "rerun posted" || ordering == "rerun no review" {
					expectedState = "done"
				}
				assert.Equal(expectedState, attempt.State)
				current, err := h.DB.GetActiveCIPanelByPRSHA("acme/api", pr.Number, pr.HeadRefOid)
				require.NoError(t, err)
				assert.Equal(*rerun.Body.RunUUID, current.PanelRunUUID)
			})
		}
	}
}
