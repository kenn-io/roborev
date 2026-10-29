package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestCIPollerDraftAPITransport(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token")
	for _, draft := range []bool{false, true} {
		t.Run(fmt.Sprint(draft), func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				pr := fmt.Sprintf(`{"number":9,"state":"open","draft":%t,"head":{"sha":"head-a","ref":"feature"},"base":{"ref":"main"}}`, draft)
				if r.URL.Path == "/api/v3/repos/acme/api/pulls" {
					pr = "[" + pr + "]"
				}
				fmt.Fprint(w, pr)
			}))
			t.Cleanup(api.Close)
			h := newCIPollerHarness(t, "https://github.com/acme/api.git")
			h.Poller.githubAPIURL = api.URL + "/api/v3"
			prs, err := h.Poller.listOpenPRs(t.Context(), "acme/api")
			require.NoError(t, err)
			require.Len(t, prs, 1)
			assert.Equal(t, draft, prs[0].Draft)
			target, err := h.Poller.panelPostTarget(t.Context(), "acme/api", 9)
			require.NoError(t, err)
			assert.Equal(t, draft, target.Draft)
		})
	}
}

// A commit status and check run belong to the SHA, even when the PRs differ.
func TestCIPollerDraftSharedHeadStatus(t *testing.T) {
	for _, tc := range []struct {
		name, state, owner                          string
		shared, siblingDraft, fresh, publish, label bool
	}{
		{name: "ready pending", state: "pending", shared: true},
		{name: "pending reservation without mapping", state: "pending", shared: true, siblingDraft: true, owner: "pending"},
		{name: "active draft sibling", state: "pending", shared: true, siblingDraft: true, owner: "active"},
		{name: "delivered draft sibling", state: "pending", shared: true, siblingDraft: true, owner: "delivered"},
		{name: "fresh shared drafts", shared: true, siblingDraft: true, fresh: true, publish: true},
		{name: "label-skipped ready sibling", shared: true, fresh: true, publish: true, label: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			h := newCIPollerHarness(t, "https://github.com/acme/api.git")
			h.Cfg.CI.SkipDrafts = true
			h.CaptureComments()
			pr := ghPR{Number: 9, HeadRefOid: "shared-head", BaseRefName: "main", Draft: true}
			ready := ghPR{Number: 10, HeadRefOid: pr.HeadRefOid, BaseRefName: "main", Draft: tc.siblingDraft}
			if tc.label {
				h.Cfg.CI.SkipLabels = []string{"skip-review"}
				ready.Labels = []string{"skip-review"}
			}
			if tc.owner == "pending" {
				_, err := h.DB.ReserveReviewAttempt("acme/api", ready.Number, ready.HeadRefOid, time.Now())
				require.NoError(t, err)
			} else if tc.owner != "" {
				_, synth, members := h.seedCIPanelRun(t, "acme/api", ready.Number, ready.HeadRefOid, "base..shared-head", []jobSpec{{Agent: "test"}})
				if tc.owner == "delivered" {
					h.markJobDoneWithReview(t, members[0].ID, "test", "Review finding.")
					h.completeSynthesisWithReview(t, synth.ID, "Review finding.")
					h.Poller.setCommitStatusFn = func(string, string, string, string) error { return errors.New("status unavailable") }
					h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))
				}
			}
			if !tc.fresh {
				_, err := h.DB.ReserveReviewAttempt("acme/api", pr.Number, pr.HeadRefOid, time.Now())
				require.NoError(t, err)
				require.NoError(t, h.DB.DeferReviewAttempt("acme/api", pr.Number, pr.HeadRefOid,
					"transient", "provider unavailable", nil, time.Now().Add(time.Hour), false))
			}
			openByNumber := map[int]ghPR{pr.Number: pr}
			if tc.shared {
				openByNumber[ready.Number] = ready
			}
			state := tc.state
			h.Poller.setCommitStatusFn = func(_, _, next, _ string) error { state = next; return nil }
			checks := h.CaptureSkippedChecks()
			for range 2 {
				require.NoError(t, h.Poller.processPR(t.Context(), "acme/api", pr, h.Cfg, openByNumber))
				if tc.fresh {
					require.NoError(t, h.Poller.processPR(t.Context(), "acme/api", ready, h.Cfg, openByNumber))
				}
			}
			attempt, err := h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
			require.NoError(t, err)
			assert.Nil(attempt)
			assert.False(h.hasPanel(t, "acme/api", pr.Number, pr.HeadRefOid))
			assert.Equal(tc.owner == "active" || tc.owner == "delivered", h.hasPanel(t, "acme/api", ready.Number, ready.HeadRefOid))
			if tc.owner == "delivered" {
				queued, err := h.DB.GetCIPanelFinalStatuses("acme/api")
				require.NoError(t, err)
				assert.Len(queued, 1, "suppression must not acknowledge another review's final intent")
			}
			if !tc.publish {
				assert.Equal(tc.state, state)
				assert.Empty(*checks)
			} else {
				assert.Equal("success", state)
				assert.NotEmpty(*checks)
			}
		})
	}
	for _, state := range []string{"deferred", "exhausted", "mixed"} {
		for _, failure := range []string{"none", "check", "status", "ack"} {
			t.Run(state+"/"+failure, func(t *testing.T) { testCIPollerSharedDraftOutage(t, state, failure, "") })
		}
	}
	for _, state := range []string{"deferred", "exhausted"} {
		for _, lookup := range []string{"error", "missing refs"} {
			t.Run("unknown owner/"+state+"/"+lookup, func(t *testing.T) { testCIPollerSharedDraftOutage(t, state, "none", lookup) })
		}
	}
	for _, owner := range []string{"advanced active", "advanced delivered", "closed delivered", "unrelated new head"} {
		t.Run(owner, func(t *testing.T) { testCIPollerDraftHistoricalOwner(t, owner) })
	}
}

func testCIPollerDraftHistoricalOwner(t *testing.T, owner string) {
	assert := assert.New(t)
	now := time.Now()
	h := newQuietHoursHarness(t, "0", nil, now)
	h.Cfg.CI.SkipDrafts = true
	h.Cfg.CI.Agents = []string{"test"}
	h.Poller.quietHours = quietWindow(t, now, "1h", true)
	h.CaptureComments()
	statuses := h.CaptureCommitStatuses()
	checks := h.CaptureSkippedChecks()
	draft := ghPR{Number: 9, HeadRefOid: "historical-head", BaseRefName: "release", Draft: true}
	sibling := ghPR{Number: 10, HeadRefOid: draft.HeadRefOid, BaseRefName: "main"}
	if owner == "unrelated new head" {
		sibling.HeadRefOid = "advanced-head"
	}
	h.Poller.prPostTargetFn = func(context.Context, string, int) (panelPostTarget, error) {
		return panelPostTarget{Open: true, HeadSHA: sibling.HeadRefOid, BaseRefName: sibling.BaseRefName}, nil
	}
	panel, synth, members := h.seedCIPanelRun(t, "acme/api", sibling.Number, sibling.HeadRefOid, "base.."+sibling.HeadRefOid,
		[]jobSpec{{Agent: "test"}, {Agent: "test"}})
	want := "pending"
	if owner == "advanced delivered" || owner == "closed delivered" {
		h.markJobDoneWithReview(t, members[0].ID, "test", "Review finding.")
		h.markJobFailed(t, members[1].ID, "model unavailable")
		h.completeSynthesisWithReview(t, synth.ID, "Review finding.")
		h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))
		require.NotEmpty(t, *statuses)
		require.Equal(t, "failure", (*statuses)[len(*statuses)-1].State)
		want = "failure"
	} else {
		require.NoError(t, h.Poller.setPendingReviewStatus("acme/api", sibling.Number, sibling.HeadRefOid, "Review in progress"))
	}
	sibling.HeadRefOid = "advanced-head"
	prs := []ghPR{sibling, draft}
	if owner == "closed delivered" {
		prs = []ghPR{draft}
		h.Poller.prPostTargetFn = func(_ context.Context, _ string, number int) (panelPostTarget, error) {
			return panelPostTarget{Open: number != sibling.Number, HeadSHA: draft.HeadRefOid, BaseRefName: "release"}, nil
		}
	}
	h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return prs, nil }
	require.NoError(t, h.Poller.pollRepo(t.Context(), "acme/api", h.Cfg))
	if owner == "advanced active" {
		retained, err := h.DB.GetCIPanelByRunUUID(panel.PanelRunUUID)
		require.NoError(t, err)
		assert.True(retained.AllowStalePost)
		assert.Equal(storage.JobStatusQueued, h.jobStatus(t, synth.ID))
	}
	if owner == "unrelated new head" {
		want = "success"
		assert.NotEmpty(*checks)
	} else {
		assert.Empty(*checks)
	}
	state := ""
	for _, status := range *statuses {
		if status.SHA == draft.HeadRefOid {
			state = status.State
		}
	}
	assert.Equal(want, state, "ownership follows the reviewed SHA rather than the owner's current head or open state")
	assert.False(h.hasPanel(t, "acme/api", draft.Number, draft.HeadRefOid))
}

func testCIPollerSharedDraftOutage(t *testing.T, state, failure, lookup string) {
	assert := assert.New(t)
	h, server := newCIHealthHarness(t)
	h.Cfg.CI.Repos = []string{"acme/api"}
	h.Cfg.CI.Agents = []string{"test"}
	h.Cfg.CI.SkipDrafts = true
	h.Cfg.CI.ThrottleInterval = "0"
	h.stubProcessPRGit()
	h.CaptureComments()
	statuses := h.CaptureCommitStatuses()
	prs := []ghPR{{Number: 9, HeadRefOid: "shared-outage", BaseRefName: "main"}, {Number: 10, HeadRefOid: "shared-outage", BaseRefName: "release"}}
	omitPeer, lookupFailed := false, lookup != ""
	h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) {
		if omitPeer {
			return prs[:1], nil
		}
		return prs, nil
	}
	h.Poller.poll(t.Context())
	for i, pr := range prs {
		if state == "exhausted" || state == "mixed" && i == 1 {
			_, err := h.DB.Exec(`UPDATE ci_pr_review_attempts SET first_attempt_at = datetime('now', '-4 days') WHERE pr_number = ?`, pr.Number)
			require.NoError(t, err)
		}
		synth := h.drivePanelOutcome(t, "acme/api", pr.Number, pr.HeadRefOid, "transient")
		h.Poller.handleReviewFailed(ciEvent(synth, "review.failed"))
		prs[i].Draft = lookup == "" || i == 0
	}
	if lookup != "" {
		omitPeer = true
		h.Poller.prPostTargetFn = func(_ context.Context, _ string, number int) (panelPostTarget, error) {
			require.Equal(t, prs[1].Number, number)
			if lookupFailed {
				if lookup == "error" {
					return panelPostTarget{}, errors.New("lookup unavailable")
				}
				return panelPostTarget{Open: true}, nil
			}
			return panelPostTarget{Open: true, HeadSHA: prs[1].HeadRefOid, BaseRefName: prs[1].BaseRefName, Draft: prs[1].Draft}, nil
		}
	}
	checks := h.CaptureSkippedChecks()
	switch failure {
	case "check":
		h.Poller.setSkippedCheckFn = func(string, string, string) error { return errors.New("check unavailable") }
	case "status":
		h.Poller.setCommitStatusFn = func(string, string, string, string) error { return errors.New("status unavailable") }
	case "ack":
		_, err := h.DB.Exec(`CREATE TRIGGER fail_skip_ack BEFORE UPDATE OF final_status_state ON ci_pr_panels
			WHEN NEW.final_status_state = '' BEGIN SELECT RAISE(FAIL, 'ack unavailable'); END`)
		require.NoError(t, err)
	}
	h.Poller.poll(t.Context())
	if lookup != "" {
		want := "pending"
		if state == "exhausted" {
			want = "error"
		}
		for range 2 {
			assert.Empty(*checks, "unknown or ready owners retain their review status")
			require.NotEmpty(t, *statuses)
			assert.Equal(want, (*statuses)[len(*statuses)-1].State)
			attempt, err := h.DB.GetReviewAttempt("acme/api", prs[1].Number, prs[1].HeadRefOid)
			require.NoError(t, err)
			assert.NotNil(attempt)
			assert.False(decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
			lookupFailed = false
			h.Poller.poll(t.Context())
		}
		prs[1].Draft = true
		_, err := h.DB.MakeTransientReviewAttemptsDue(time.Now())
		require.NoError(t, err)
		h.Poller.poll(t.Context())
	}
	for _, pr := range prs {
		attempt, err := h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
		require.NoError(t, err)
		if failure == "none" {
			assert.Nil(attempt)
		} else {
			assert.NotNil(attempt, "failed shared publication retains each retry")
		}
	}
	assert.Equal(failure == "none", decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
	if failure != "none" {
		_, err := h.DB.Exec(`DROP TRIGGER IF EXISTS fail_skip_ack`)
		require.NoError(t, err)
		checks = h.CaptureSkippedChecks()
		statuses = h.CaptureCommitStatuses()
		h.Poller.poll(t.Context())
	}
	require.NotEmpty(t, *statuses)
	assert.Equal("success", (*statuses)[len(*statuses)-1].State)
	assert.NotEmpty(*checks)
	queued, err := h.DB.GetCIPanelFinalStatuses("acme/api")
	require.NoError(t, err)
	assert.Empty(queued, "no leased peer may publish an obsolete error after release")
	for _, pr := range prs {
		attempt, err := h.DB.GetReviewAttempt("acme/api", pr.Number, pr.HeadRefOid)
		require.NoError(t, err)
		assert.Nil(attempt)
	}
	prs[0].Draft = false
	h.Poller.poll(t.Context())
	panel, err := h.DB.GetActiveCIPanelByPRSHA("acme/api", prs[0].Number, prs[0].HeadRefOid)
	require.NoError(t, err)
	assert.Equal(storage.JobStatusQueued, h.jobStatus(t, *panel.SynthesisJobID))
}
