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
	for _, scenario := range []string{"review", "no review", "stale claim", "superseded", "removed repository"} {
		t.Run(scenario, func(t *testing.T) {
			assert := assert.New(t)
			h := newCIPollerHarness(t, "https://github.com/acme/api.git")
			h.Cfg.CI.Repos = []string{"acme/api"}
			comments := h.CaptureComments()
			spec := jobSpec{Agent: "test", Status: "done", Output: "No issues found."}
			wantFinal := capturedStatus{Repo: "acme/api", SHA: "head-a", State: "success", Desc: "Review complete"}
			wantComments := 1
			if scenario == "no review" {
				spec = jobSpec{Agent: "test", Status: "canceled", Error: reviewpkg.TimeoutErrorPrefix + "deadline"}
				wantFinal.State, wantFinal.Desc = "error", "No review output"
				wantComments = 0
			}
			panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a", []jobSpec{spec})
			h.completeSynthesisWithReview(t, synth.ID, "No issues found.")
			attempts := 0
			h.Poller.setCommitStatusFn = func(repo, sha, state, desc string) error {
				attempts++
				assert.Equal(wantFinal, capturedStatus{repo, sha, state, desc})
				return errors.New("GitHub unavailable")
			}
			h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))
			require.Equal(t, 1, attempts)
			require.Len(t, *comments, wantComments)
			posted, err := h.DB.GetCIPanelByRunUUID(panel.PanelRunUUID)
			require.NoError(t, err)
			require.NotNil(t, posted.PostedAt, "comment finalization survives status failure")
			if scenario == "stale claim" {
				h.backdatePanelPostClaim(t, panel.ID)
			}
			if scenario == "superseded" {
				server := newServerWithLogs(h.DB, h.Cfg, "", newTestErrorLog(), newTestActivityLog())
				_, err := server.humaRerunJob(context.Background(), &RerunJobInput{Body: RerunJobRequest{JobID: synth.ID}})
				require.NoError(t, err, "a failed status write releases ownership for a successor")
				require.NoError(t, server.Close())
			}

			var dbPath string
			require.NoError(t, h.DB.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&dbPath))
			require.NoError(t, h.DB.Close())
			db, err := storage.Open(dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			if scenario == "removed repository" {
				h.Cfg.CI.Repos = nil
			}
			poller := NewCIPoller(db, NewStaticConfig(h.Cfg), nil)
			poller.repoResolver.canonicalRepoFn = nil
			poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) {
				return []ghPR{{Number: 7, HeadRefOid: "head-a"}}, nil
			}
			poller.postPRCommentFn = h.Poller.postPRCommentFn
			server := newServerWithLogs(db, h.Cfg, "", newTestErrorLog(), newTestActivityLog())
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			var statuses []capturedStatus
			poller.setCommitStatusFn = func(repo, sha, state, desc string) error {
				// An in-flight final write must finish before a successor owns this HEAD.
				response := serveHuma(t, server, http.MethodPost, "/api/job/rerun",
					[]byte(fmt.Sprintf(`{"job_id":%d}`, synth.ID)))
				assert.Equal(http.StatusConflict, response.Code, response.Body.String())
				statuses = append(statuses, capturedStatus{repo, sha, state, desc})
				return nil
			}
			poller.poll(context.Background())
			var want []capturedStatus
			if scenario != "superseded" && scenario != "removed repository" {
				want = []capturedStatus{wantFinal}
			}
			assert.Equal(want, statuses)
			assert.Len(*comments, wantComments, "status retry must not repost the comment")
			poller.poll(context.Background())
			assert.Equal(want, statuses, "acknowledged or superseded statuses must not retry")
			assert.Len(*comments, wantComments)
		})
	}
}

func TestCIPanelFinalStatusDoesNotOverwriteSkip(t *testing.T) {
	for _, state := range []string{"success", "error"} {
		t.Run(state, func(t *testing.T) {
			assert := assert.New(t)
			h := newCIPollerHarness(t, "https://github.com/acme/api.git")
			h.CaptureComments()
			spec := jobSpec{Agent: "test", Status: "done", Output: "No issues found."}
			if state == "error" {
				spec = jobSpec{Agent: "test", Status: "canceled", Error: reviewpkg.TimeoutErrorPrefix + "deadline"}
			}
			_, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a", []jobSpec{spec})
			h.completeSynthesisWithReview(t, synth.ID, "No issues found.")
			pr := ghPR{Number: 7, HeadRefOid: "head-a"}
			finalAttempts := 0
			failSkip := true
			var statuses []capturedStatus
			h.Poller.setCommitStatusFn = func(repo, sha, gotState, desc string) error {
				assert.Equal("acme/api", repo)
				assert.Equal("head-a", sha)
				if desc != "Review skipped: label skip-review" {
					assert.Equal(state, gotState)
					finalAttempts++
					attempt, err := h.DB.GetReviewAttempt(repo, pr.Number, sha)
					require.NoError(t, err)
					require.NotNil(t, attempt)
					err = h.Poller.skipLabeledPR(repo, pr, "skip-review", attempt)
					require.ErrorIs(t, err, storage.ErrCIPanelActive)
					return errors.New("GitHub unavailable")
				}
				if failSkip {
					return errors.New("skip status unavailable")
				}
				statuses = append(statuses, capturedStatus{repo, sha, gotState, desc})
				return nil
			}
			h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))
			attempt, err := h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
			require.NoError(t, err)
			require.NotNil(t, attempt)
			require.ErrorContains(t, h.Poller.skipLabeledPR("acme/api", pr, "skip-review", attempt), "skip status unavailable")
			failSkip = false
			require.NoError(t, h.Poller.skipLabeledPR("acme/api", pr, "skip-review", attempt))
			h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return nil, nil }
			require.NoError(t, h.Poller.pollRepo(context.Background(), "acme/api", h.Cfg))
			assert.Equal(1, finalAttempts)
			assert.Equal([]capturedStatus{{Repo: "acme/api", SHA: "head-a", State: "success", Desc: "Review skipped: label skip-review"}}, statuses)
		})
	}
}

func TestCIPanelFinalStatusWithoutTokenRemainsPending(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	installFakeGHAuthToken(t, "")
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a",
		[]jobSpec{{Agent: "test", Status: "done", Output: "No issues found."}})
	h.completeSynthesisWithReview(t, synth.ID, "No issues found.")
	h.Poller.setCommitStatusFn = nil
	h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))
	require.Len(t, *comments, 1)
	attempt, err := h.DB.GetReviewAttempt("acme/api", 7, "head-a")
	require.NoError(t, err)
	require.NotNil(t, attempt)
	err = h.Poller.skipLabeledPR("acme/api", ghPR{Number: 7, HeadRefOid: "head-a"}, "skip-review", attempt)
	require.ErrorContains(t, err, "GitHub token")
	attempt, err = h.DB.GetReviewAttempt("acme/api", 7, "head-a")
	require.NoError(t, err)
	require.NotNil(t, attempt, "an undelivered skip must retain the attempt")
	var state string
	require.NoError(t, h.DB.QueryRow(`SELECT final_status_state FROM ci_pr_panels WHERE id = ?`, panel.ID).Scan(&state))
	assert.Equal(t, "success", state, "an undelivered final must remain retryable")
	statuses := h.CaptureCommitStatuses()
	h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) {
		return []ghPR{{Number: 7, HeadRefOid: "head-a"}}, nil
	}
	require.NoError(t, h.Poller.pollRepo(context.Background(), "acme/api", h.Cfg))
	assert.Equal(t, []capturedStatus{{Repo: "acme/api", SHA: "head-a", State: "success", Desc: "Review complete"}}, *statuses)
	assert.Len(t, *comments, 1)
}
