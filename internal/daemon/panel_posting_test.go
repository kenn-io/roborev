package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	googlegithub "github.com/google/go-github/v88/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/git"
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
)

// ciEvent builds a review.completed/failed Event for a synthesis or member job.
func ciEvent(jobID int64, eventType string) Event {
	return Event{Type: eventType, JobID: jobID}
}

// seedCIPanelRun creates a ci_pr_panels mapping with a member + synthesis run
// (via CreateCIPanelRun) and drives each member to its spec'd terminal state.
// The synthesis job is left blocked/queued; callers complete or fail it to pick
// the posting body source. Returns the panel row, synthesis job, and members.
func (h *ciPollerHarness) seedCIPanelRun(
	t *testing.T, ghRepo string, pr int, headSHA, gitRef string, specs []jobSpec,
) (*storage.CIPanel, *storage.ReviewJob, []*storage.ReviewJob) {
	t.Helper()
	members := make([]storage.EnqueueOpts, 0, len(specs))
	for i, s := range specs {
		members = append(members, storage.EnqueueOpts{
			RepoID: h.Repo.ID, GitRef: gitRef, Agent: s.Agent, ReviewType: s.ReviewType,
			JobType: storage.JobTypeReview, PanelName: "ci", PanelMemberName: s.Agent,
			PanelMemberIndex: i,
		})
	}
	synthesis := storage.EnqueueOpts{
		RepoID: h.Repo.ID, GitRef: gitRef, Agent: "test", PanelName: "ci",
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
		}
	}

	panel, err := h.DB.GetCIPanelByPRSHA(ghRepo, pr, headSHA)
	require.NoError(t, err)
	return panel, synthJob, memberJobs
}

// completeSynthesisWithReview drives the synthesis job to done with a stored
// review output (the verify-dedupe / passthrough success path).
func (h *ciPollerHarness) completeSynthesisWithReview(t *testing.T, jobID int64, output string) {
	t.Helper()
	h.markJobDoneWithReview(t, jobID, "test", output)
}

func setJobTiming(t *testing.T, db *storage.DB, jobID int64, startedAt, finishedAt string) {
	t.Helper()
	_, err := db.Exec(`UPDATE review_jobs SET started_at = ?, finished_at = ? WHERE id = ?`, startedAt, finishedAt, jobID)
	require.NoError(t, err)
}

// seedJobCost prices a job the way the worker does — token usage scoped to the
// job's captured session — by assigning a session id and storing the usage
// blob against it.
func seedJobCost(t *testing.T, db *storage.DB, jobID int64, tokenUsageJSON string) {
	t.Helper()
	sessionID := fmt.Sprintf("sess-%d", jobID)
	_, err := db.Exec(`UPDATE review_jobs SET session_id = ? WHERE id = ?`, sessionID, jobID)
	require.NoError(t, err)
	require.NoError(t, db.SaveJobTokenUsage(jobID, sessionID, tokenUsageJSON))
}

// panelPostedAt reports whether the panel row's posted_at is set.
func (h *ciPollerHarness) panelPostedAt(t *testing.T, id int64) bool {
	t.Helper()
	var postedAt *string
	err := h.DB.QueryRow(`SELECT posted_at FROM ci_pr_panels WHERE id = ?`, id).Scan(&postedAt)
	require.NoError(t, err)
	return postedAt != nil
}

// member builds a panel member BatchReviewResult row for status tests.
func member(agent, reviewType, status, errText string) storage.BatchReviewResult {
	return storage.BatchReviewResult{
		Agent:      agent,
		ReviewType: reviewType,
		Status:     status,
		Error:      errText,
	}
}

// TestPanelCommitStatus exercises the §9 four-arm switch over member outcomes.
// Status reflects whether the review process ran, never the synthesis verdict:
// a Fail verdict still posts success; quota/timeout skips are success-with-note.
func TestPanelCommitStatus(t *testing.T) {
	quotaErr := reviewpkg.QuotaErrorPrefix + "agent quota exhausted"
	timeoutErr := reviewpkg.TimeoutErrorPrefix + "posted early"

	cases := []struct {
		name      string
		members   []storage.BatchReviewResult
		wantState string
		wantDesc  string
	}{
		{
			name: "clean all done",
			members: []storage.BatchReviewResult{
				member("codex", "review", "done", ""),
				member("gemini", "security", "done", ""),
			},
			wantState: "success",
			wantDesc:  "Review complete",
		},
		{
			name: "all failed real",
			members: []storage.BatchReviewResult{
				member("codex", "review", "failed", "boom"),
				member("gemini", "security", "failed", "kaboom"),
			},
			wantState: "error",
			wantDesc:  "All reviews failed",
		},
		{
			name: "only skips no real failures is success not failure",
			members: []storage.BatchReviewResult{
				member("codex", "review", "failed", quotaErr),
				member("gemini", "security", "canceled", timeoutErr),
			},
			wantState: "success",
			wantDesc:  "Review complete (2 agent(s) skipped)",
		},
		{
			name: "mixed real failures",
			members: []storage.BatchReviewResult{
				member("codex", "review", "done", ""),
				member("gemini", "security", "failed", "boom"),
				member("droid", "review", "failed", quotaErr),
			},
			wantState: "failure",
			wantDesc:  "Review complete (1/3 jobs failed)",
		},
		{
			name: "allowed failure with successful sibling is success",
			members: []storage.BatchReviewResult{
				member("codex", "review", "done", ""),
				{
					Agent:                 "pi",
					ReviewType:            "security",
					Status:                "failed",
					Error:                 "pi host disappeared",
					PanelMemberConfigJSON: `{"allow_failure":true}`,
				},
			},
			wantState: "success",
			wantDesc:  "Review complete",
		},
		{
			name: "done plus skip is success with note",
			members: []storage.BatchReviewResult{
				member("codex", "review", "done", ""),
				member("gemini", "security", "failed", quotaErr),
			},
			wantState: "success",
			wantDesc:  "Review complete (1 agent(s) skipped)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			state, desc := panelCommitStatus(tc.members)
			assert.Equal(tc.wantState, state, "state")
			assert.Equal(tc.wantDesc, desc, "desc")
		})
	}
}

// TestSynthesisCompletedPostsOnce verifies a synthesis review.completed posts
// the persisted review exactly once even under duplicate event delivery (the
// posting CAS is idempotent) and finalizes the panel row (posted_at set).
func TestSynthesisCompletedPostsOnce(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	statuses := h.CaptureCommitStatuses()

	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 5, "headsha111", "base..headsha111",
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "Finding A"}})
	h.completeSynthesisWithReview(t, synth.ID, "## Combined findings\nVerified finding A.")

	h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))
	h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed")) // duplicate delivery

	assert.Len(*comments, 1, "exactly one PR comment despite duplicate delivery")
	assert.True(h.panelPostedAt(t, panel.ID), "panel finalized (posted_at set)")
	assert.NotEmpty(*statuses, "commit status set")
}

// TestSynthesisFailedPostsRawFallback covers F4: when the synthesis agent fails
// (no persisted review), the member findings still reach the PR via
// FormatRawBatchComment, status is set, and the row is finalized.
func TestSynthesisFailedPostsRawFallback(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	statuses := h.CaptureCommitStatuses()

	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 6, "headsha222", "base..headsha222",
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "Member finding X"}})
	h.markJobFailed(t, synth.ID, "synthesis agent crashed")

	h.Poller.handleReviewFailed(ciEvent(synth.ID, "review.failed"))

	require.Len(t, *comments, 1, "raw fallback posts one comment")
	body := (*comments)[0].Body
	assertContainsAll(t, body, "raw fallback",
		"## roborev: Combined Review", "Member finding X")
	assert.True(h.panelPostedAt(t, panel.ID), "panel finalized")
	assert.NotEmpty(*statuses, "commit status set")
}

// TestSynthesisQuotaFailureDefersInsteadOfRawFallback covers the quota-exhausted
// synthesis case: the members produced real review output but the consolidation
// step failed on quota exhaustion. Rather than posting the degraded "Synthesis
// unavailable" raw fallback, the run defers for a later retry (when quota
// resets) — no comment, a pending status, a deferred attempt, and a retired
// panel — so the PR eventually gets a properly synthesized comment.
func TestSynthesisQuotaFailureDefersInsteadOfRawFallback(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	statuses := h.CaptureCommitStatuses()

	const headSHA = "synthquota123456"
	created, err := h.DB.ReserveReviewAttempt("acme/api", 90, headSHA, time.Now())
	require.NoError(t, err)
	require.True(t, created, "attempt row reserved")

	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 90, headSHA, "base.."+headSHA,
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "Member finding X"}})
	h.markJobFailed(t, synth.ID, reviewpkg.QuotaErrorPrefix+"agent test quota exhausted")

	h.Poller.handleReviewFailed(ciEvent(synth.ID, "review.failed"))

	assert.Empty(*comments, "synthesis quota failure must not post the degraded raw fallback")
	require.Len(t, *statuses, 1, "synthesis quota defer sets exactly one status")
	assert.Equal("pending", (*statuses)[0].State, "synthesis quota defer status is pending, never failure")
	assert.False(h.panelPostedAt(t, panel.ID), "deferred panel is not marked posted")
	assert.True(h.panelRetiredAt(t, panel.ID), "deferred panel is retired (removed from active set)")

	attempt, err := h.DB.GetReviewAttempt("acme/api", 90, headSHA)
	require.NoError(t, err)
	require.NotNil(t, attempt)
	assert.Equal("deferred", attempt.State, "attempt deferred for retry")
	assert.Equal("transient", attempt.LastErrorClass, "quota defer records the retryable class")
	assert.NotNil(attempt.NextAttemptAt, "quota defer schedules a next attempt")
}

func TestSynthesisCanceledDoesNotPostRawFallback(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	statuses := h.CaptureCommitStatuses()

	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 16, "headsha-canceled", "base..headsha-canceled",
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "Stale member finding"}})
	h.markJobCanceled(t, synth.ID, "superseded by newer PR head")

	eventCh := make(chan Event, 1)
	eventCh <- ciEvent(synth.ID, "review.canceled")
	close(eventCh)
	h.Poller.listenForEvents(make(chan struct{}), eventCh)

	assert.Empty(*comments, "canceled synthesis must not post stale raw fallback")
	assert.Empty(*statuses, "canceled synthesis must not set commit status")
	assert.False(h.panelPostedAt(t, panel.ID), "canceled panel is not marked posted")
	_, err := h.DB.GetActiveCIPanelByPRSHA("acme/api", 16, "headsha-canceled")
	require.ErrorIs(t, err, sql.ErrNoRows, "canceled synthesis retires active panel mapping")
	row, err := h.DB.GetCIPanelByPRSHA("acme/api", 16, "headsha-canceled")
	require.NoError(t, err)
	assert.NotNil(row.RetiredAt, "canceled synthesis records retirement for throttle memory")
	attempt, err := h.DB.GetReviewAttempt("acme/api", 16, "headsha-canceled")
	require.NoError(t, err)
	assert.Nil(attempt, "canceled synthesis deletes the reserved retry attempt")
}

// TestPanelClosedPRPostsNothing covers F13: a closed/merged PR is abandoned
// without a comment and without suppressing a same-HEAD reopen.
func TestPanelClosedPRPostsNothing(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Poller.isPROpenFn = func(string, int) bool { return false }
	comments := h.CaptureComments()

	_, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "headsha333", "base..headsha333",
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "F"}})
	h.completeSynthesisWithReview(t, synth.ID, "## Combined\nfindings")

	h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

	assert.Empty(*comments, "no comment on a closed PR")
	_, err := h.DB.GetCIPanelByPRSHA("acme/api", 7, "headsha333")
	require.ErrorIs(t, err, sql.ErrNoRows, "closed PR deletes the mapping")
	reviewed, err := h.Poller.alreadyReviewedPR("acme/api", ghPR{Number: 7, HeadRefOid: "headsha333"})
	require.NoError(t, err)
	assert.False(reviewed, "same-HEAD reopen must be reviewable")
}

func TestPanelStalePRHeadPostsNothing(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	statuses := h.CaptureCommitStatuses()

	const reviewedHead = "headsha-stale"
	h.Poller.prPostTargetFn = func(context.Context, string, int) (panelPostTarget, error) {
		return panelPostTarget{Open: true, HeadSHA: "new-headsha"}, nil
	}
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 18, reviewedHead, "base.."+reviewedHead,
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "Stale finding"}})
	h.completeSynthesisWithReview(t, synth.ID, "## Combined\nstale finding")

	h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

	assert.Empty(*comments, "stale-head panel must not post a PR comment")
	assert.Empty(*statuses, "stale-head panel must not set commit status")
	assert.False(h.panelPostedAt(t, panel.ID), "stale-head panel is not marked posted")
	assert.True(h.panelRetiredAt(t, panel.ID), "stale-head panel is retired")
	attempt, err := h.DB.GetReviewAttempt("acme/api", 18, reviewedHead)
	require.NoError(t, err)
	assert.Nil(attempt, "stale-head panel deletes its attempt row")
}

func TestPanelRepoIdentityMismatchPostsNothing(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	statuses := h.CaptureCommitStatuses()

	const headSHA = "headsha-mismatch"
	h.Poller.prPostTargetFn = func(context.Context, string, int) (panelPostTarget, error) {
		return panelPostTarget{Open: true, HeadSHA: headSHA}, nil
	}
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 19, headSHA, "base.."+headSHA,
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "Private finding"}})
	require.NoError(t, h.DB.SetRepoIdentity(h.Repo.ID, "https://github.com/other/private.git"))
	h.completeSynthesisWithReview(t, synth.ID, "## Combined\nprivate finding")

	h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

	assert.Empty(*comments, "repo mismatch must not post a PR comment")
	assert.Empty(*statuses, "repo mismatch must not set commit status")
	assert.False(h.panelPostedAt(t, panel.ID), "repo mismatch panel is not marked posted")
	assert.True(h.panelRetiredAt(t, panel.ID), "repo mismatch panel is retired")
	attempt, err := h.DB.GetReviewAttempt("acme/api", 19, headSHA)
	require.NoError(t, err)
	assert.Nil(attempt, "repo mismatch deletes its attempt row")
}

// TestPanelPermanentPostErrorAbandons covers the permanent-GitHub-error path: an
// inaccessible repo/PR sets an error status and finalizes the row (never retry).
func TestPanelPermanentPostErrorAbandons(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	statuses := h.CaptureCommitStatuses()
	h.Poller.postPRCommentFn = func(string, int, string) error {
		return &googlegithub.ErrorResponse{
			Response: &http.Response{StatusCode: http.StatusNotFound},
			Message:  "Not Found",
		}
	}

	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 8, "headsha444", "base..headsha444",
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "F"}})
	h.completeSynthesisWithReview(t, synth.ID, "## Combined\nfindings")

	h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

	require.NotEmpty(t, *statuses, "error status set on permanent failure")
	assert.Equal("error", (*statuses)[len(*statuses)-1].State, "permanent error status")
	assert.True(h.panelPostedAt(t, panel.ID), "permanent error abandons (posted_at set)")
	attempt, err := h.DB.GetReviewAttempt("acme/api", 8, "headsha444")
	require.NoError(t, err)
	require.NotNil(t, attempt)
	assert.Equal("done", attempt.State, "permanent error marks attempt terminal")

	// posted_at is set, so the CAS (posted_at IS NULL) bars any retry even though
	// the claim is intentionally left in place.
	won, err := h.DB.ClaimPanelForPosting(panel.ID, panelPostingStaleWindow)
	require.NoError(t, err)
	assert.False(won, "abandoned panel cannot be reclaimed for posting")
}

// TestPanelPermanentPreflightErrorAbandons covers the permanent-GitHub-error
// path before posting: an inaccessible repo/PR during target validation must
// abandon the panel instead of releasing the claim for endless retry.
func TestPanelPermanentPreflightErrorAbandons(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	statuses := h.CaptureCommitStatuses()
	h.Poller.prPostTargetFn = func(context.Context, string, int) (panelPostTarget, error) {
		return panelPostTarget{}, &googlegithub.ErrorResponse{
			Response: &http.Response{StatusCode: http.StatusForbidden},
			Message:  "Resource not accessible by integration",
		}
	}

	const headSHA = "headsha-preflight"
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 20, headSHA, "base.."+headSHA,
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "private finding"}})
	h.completeSynthesisWithReview(t, synth.ID, "## Combined\nprivate finding")

	h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

	assert.Empty(*comments, "permanent preflight failure must not post a PR comment")
	require.NotEmpty(t, *statuses, "error status set on permanent preflight failure")
	assert.Equal("error", (*statuses)[len(*statuses)-1].State, "permanent preflight status")
	assert.True(h.panelPostedAt(t, panel.ID), "permanent preflight error abandons (posted_at set)")

	attempt, err := h.DB.GetReviewAttempt("acme/api", 20, headSHA)
	require.NoError(t, err)
	require.NotNil(t, attempt)
	assert.Equal("done", attempt.State, "permanent preflight error marks attempt terminal")

	won, err := h.DB.ClaimPanelForPosting(panel.ID, panelPostingStaleWindow)
	require.NoError(t, err)
	assert.False(won, "abandoned panel cannot be reclaimed for posting")
}

// TestPanelWrapperNoDoubleHeader covers F11: a synthesis output lacking the
// `## roborev:` header is wrapped exactly once; a raw/all-failed body that
// already starts with the header is not double-wrapped.
func TestPanelWrapperNoDoubleHeader(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")

	t.Run("plain output gets one header", func(t *testing.T) {
		h.Cfg.CI.IncludeCosts = false
		comments := h.CaptureComments()
		_, synth, _ := h.seedCIPanelRun(t, "acme/api", 9, "headsha555", "base..headsha555",
			[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "x"}})
		h.completeSynthesisWithReview(t, synth.ID, "Consolidated review body without a header.")

		h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

		require.Len(t, *comments, 1)
		body := (*comments)[0].Body
		assert.Equal(t, 1, strings.Count(body, "## roborev:"), "exactly one roborev header")
	})

	t.Run("plain output is not collapsed", func(t *testing.T) {
		h.Cfg.CI.IncludeCosts = false
		comments := h.CaptureComments()
		_, synth, _ := h.seedCIPanelRun(t, "acme/api", 13, "headsha888", "base..headsha888",
			[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "x"}})
		h.completeSynthesisWithReview(t, synth.ID, "Panel review fanout needs fixes before merge.\n\n## Review Findings\n\n- Medium finding")

		h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

		require.Len(t, *comments, 1)
		body := (*comments)[0].Body
		assert.NotContains(t, body, "<details>", "panel synthesis findings should be visible by default")
		assert.NotContains(t, body, "<summary>Review findings</summary>", "panel synthesis findings should not be hidden behind a disclosure")
		assert.Contains(t, body, "Panel review fanout needs fixes before merge.")
		assert.Contains(t, body, "- Medium finding")
	})

	t.Run("plain output footer includes reviewer summary", func(t *testing.T) {
		h.Cfg.CI.IncludeCosts = false
		comments := h.CaptureComments()
		const headSHA = "9999999cccccc"
		_, synth, _ := h.seedCIPanelRun(t, "acme/api", 14, headSHA, "base.."+headSHA,
			[]jobSpec{
				{Agent: "codex", ReviewType: "default", Status: "done", Output: "x"},
				{Agent: "codex", ReviewType: "security", Status: "done", Output: "y"},
			})
		h.completeSynthesisWithReview(t, synth.ID, "Medium issue found.")

		h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

		require.Len(t, *comments, 1)
		body := (*comments)[0].Body
		assert.Contains(t, body, "## roborev: Combined Review (`"+git.ShortSHA(headSHA)+"`)")
		assert.Contains(t, body, "Reviewers: 2 done")
		assert.Contains(t, body, "Synthesis: test")
		assert.NotContains(t, body, "Total: unknown")
		assert.NotContains(t, body, "Panel:")
		assert.NotContains(t, body, "Members:")
		assert.NotContains(t, body, "Job:", "synthesis footer must not leak a job ID that confuses local fixing agents")
		assert.NotContains(t, body, "Head:", "reviewed head belongs in the title, not the footer")
		assert.NotContains(t, body, "base", "footer must show reviewed head, not merge base")
		assert.NotContains(t, body, "Review type:  | Agent:", "panel comments should not use the empty synthesis review_type footer")
	})

	t.Run("clean synthesis body gets one panel footer", func(t *testing.T) {
		h.Cfg.CI.IncludeCosts = false
		comments := h.CaptureComments()
		const headSHA = "b00cdbf1234567"
		_, synth, _ := h.seedCIPanelRun(t, "acme/api", 22, headSHA, "base.."+headSHA,
			[]jobSpec{
				{Agent: "codex", ReviewType: "default", Status: "done", Output: "No issues found."},
				{Agent: "codex", ReviewType: "security", Status: "done", Output: "No issues found."},
			})
		h.completeSynthesisWithReview(t, synth.ID, "No issues found.")

		h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

		require.Len(t, *comments, 1)
		body := (*comments)[0].Body
		assert.Contains(t, body, "## roborev: Combined Review (`"+git.ShortSHA(headSHA)+"`)")
		assert.Contains(t, body, "No issues found.")
		assert.NotContains(t, body, "Synthesized from", "persisted synthesis output is body-only")
		assert.Contains(t, body, "Reviewers: 2 done")
		assert.NotContains(t, body, "Panel:")
		assert.NotContains(t, body, "Members:")
		assert.Equal(t, 1, strings.Count(body, "\n\n---\n*"), "only the panel footer should remain")
	})

	t.Run("public footer omits reviewer role names", func(t *testing.T) {
		h.Cfg.CI.IncludeCosts = false
		comments := h.CaptureComments()
		const headSHA = "5ec0fed1234567"
		_, synth, members := h.seedCIPanelRun(t, "acme/api", 23, headSHA, "base.."+headSHA,
			[]jobSpec{
				{Agent: "codex", ReviewType: "default", Status: "done", Output: "No issues found."},
				{Agent: "codex", ReviewType: "security", Status: "done", Output: "No issues found."},
			})
		_, err := h.DB.Exec(`UPDATE review_jobs SET panel_name = ? WHERE id IN (?, ?, ?)`,
			"ci_default_security", synth.ID, members[0].ID, members[1].ID)
		require.NoError(t, err)
		_, err = h.DB.Exec(`UPDATE review_jobs SET panel_member_name = ? WHERE id = ?`,
			"codex_security", members[1].ID)
		require.NoError(t, err)
		h.completeSynthesisWithReview(t, synth.ID, "No issues found.")

		h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

		require.Len(t, *comments, 1)
		body := (*comments)[0].Body
		assert.Contains(t, body, "No issues found.")
		assert.Contains(t, body, "Reviewers: 2 done")
		assert.NotContains(t, body, "ci_default_security")
		assert.NotContains(t, body, "codex_security")
		assert.NotContains(t, body, "codex/security")
		assert.NotContains(t, body, "Panel:")
		assert.NotContains(t, body, "Members:")
	})

	t.Run("footer uses synthesis review agent", func(t *testing.T) {
		h.Cfg.CI.IncludeCosts = false
		comments := h.CaptureComments()
		const headSHA = "facefeed1234567"
		_, synth, _ := h.seedCIPanelRun(t, "acme/api", 20, headSHA, "base.."+headSHA,
			[]jobSpec{{Agent: "codex", ReviewType: "default", Status: "done", Output: "x"}})
		_, err := h.DB.Exec(`UPDATE review_jobs SET agent = ? WHERE id = ?`, "codex", synth.ID)
		require.NoError(t, err)
		h.markJobDoneWithReview(t, synth.ID, "claude-code", "Medium issue found.")

		h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

		require.Len(t, *comments, 1)
		body := (*comments)[0].Body
		assert.Contains(t, body, "Synthesis: claude-code")
		assert.NotContains(t, body, "Synthesis: codex")
	})

	t.Run("headed pass output keeps result text", func(t *testing.T) {
		h.Cfg.CI.IncludeCosts = false
		comments := h.CaptureComments()
		const headSHA = "1234567feedface"
		_, synth, _ := h.seedCIPanelRun(t, "acme/api", 19, headSHA, "base.."+headSHA,
			[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "No issues found."}})
		h.completeSynthesisWithReview(t, synth.ID, "No issues found.")

		h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

		require.Len(t, *comments, 1)
		body := (*comments)[0].Body
		assert.Contains(t, body, "## roborev: Combined Review (`"+git.ShortSHA(headSHA)+"`)")
		assert.Contains(t, body, "No issues found.")
		assert.Contains(t, body, "Reviewers: 1 done")
	})

	t.Run("plain output footer hides cost by default", func(t *testing.T) {
		h.Cfg.CI.IncludeCosts = false
		comments := h.CaptureComments()
		_, synth, members := h.seedCIPanelRun(t, "acme/api", 18, "headsha3333", "base..headsha3333",
			[]jobSpec{
				{Agent: "codex", ReviewType: "default", Status: "done", Output: "x"},
				{Agent: "codex", ReviewType: "security", Status: "done", Output: "y"},
			})
		setJobTiming(t, h.DB, members[0].ID, "2026-06-01T18:00:00Z", "2026-06-01T18:04:32Z")
		setJobTiming(t, h.DB, members[1].ID, "2026-06-01T18:00:00Z", "2026-06-01T18:02:08Z")
		setJobTiming(t, h.DB, synth.ID, "2026-06-01T18:04:40Z", "2026-06-01T18:04:58Z")
		seedJobCost(t, h.DB, members[0].ID, `{"cost_usd":0.11,"has_cost":true}`)
		seedJobCost(t, h.DB, members[1].ID, `{"cost_usd":0.06,"has_cost":true}`)
		seedJobCost(t, h.DB, synth.ID, `{"cost_usd":0.03,"has_cost":true}`)
		h.completeSynthesisWithReview(t, synth.ID, "Medium issue found.")

		h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

		require.Len(t, *comments, 1)
		body := (*comments)[0].Body
		assert.Contains(t, body, "Reviewers: 2 done")
		assert.Contains(t, body, "Synthesis: test, 18s")
		assert.Contains(t, body, "Total: 6m58s")
		assert.NotContains(t, body, "~$")
		assert.NotContains(t, body, "cost partial")
		assert.NotContains(t, body, "codex/default")
		assert.NotContains(t, body, "codex/security")
	})

	t.Run("plain output footer includes runtime and cost when enabled", func(t *testing.T) {
		h.Cfg.CI.IncludeCosts = true
		comments := h.CaptureComments()
		_, synth, members := h.seedCIPanelRun(t, "acme/api", 16, "headsha1111", "base..headsha1111",
			[]jobSpec{
				{Agent: "codex", ReviewType: "default", Status: "done", Output: "x"},
				{Agent: "codex", ReviewType: "security", Status: "done", Output: "y"},
			})
		setJobTiming(t, h.DB, members[0].ID, "2026-06-01T18:00:00Z", "2026-06-01T18:04:32Z")
		setJobTiming(t, h.DB, members[1].ID, "2026-06-01T18:00:00Z", "2026-06-01T18:02:08Z")
		setJobTiming(t, h.DB, synth.ID, "2026-06-01T18:04:40Z", "2026-06-01T18:04:58Z")
		seedJobCost(t, h.DB, members[0].ID, `{"cost_usd":0.11,"has_cost":true}`)
		seedJobCost(t, h.DB, members[1].ID, `{"cost_usd":0.06,"has_cost":true}`)
		seedJobCost(t, h.DB, synth.ID, `{"cost_usd":0.03,"has_cost":true}`)
		h.completeSynthesisWithReview(t, synth.ID, "Medium issue found.")

		h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

		require.Len(t, *comments, 1)
		body := (*comments)[0].Body
		assert.Contains(t, body, "Reviewers: 2 done")
		assert.Contains(t, body, "Synthesis: test, 18s, ~$0.03")
		assert.Contains(t, body, "Total: 6m58s, ~$0.20")
		assert.NotContains(t, body, "codex/default")
		assert.NotContains(t, body, "codex/security")
	})

	t.Run("plain output footer shows partial cost when enabled", func(t *testing.T) {
		h.Cfg.CI.IncludeCosts = true
		comments := h.CaptureComments()
		_, synth, members := h.seedCIPanelRun(t, "acme/api", 17, "headsha2222", "base..headsha2222",
			[]jobSpec{
				{Agent: "codex", ReviewType: "default", Status: "done", Output: "x"},
				{Agent: "gemini", ReviewType: "security", Status: "canceled", Error: "timeout"},
			})
		setJobTiming(t, h.DB, members[0].ID, "2026-06-01T18:00:00Z", "2026-06-01T18:04:32Z")
		setJobTiming(t, h.DB, members[1].ID, "2026-06-01T18:00:00Z", "2026-06-01T18:01:14Z")
		seedJobCost(t, h.DB, members[0].ID, `{"cost_usd":0.11,"has_cost":true}`)
		h.completeSynthesisWithReview(t, synth.ID, "Medium issue found.")

		h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

		require.Len(t, *comments, 1)
		body := (*comments)[0].Body
		assert.Contains(t, body, "Reviewers: 2 total (1 done, 1 canceled)")
		assert.Contains(t, body, "Total: 5m46s, cost partial ~$0.11")
		assert.NotContains(t, body, "codex/default")
		assert.NotContains(t, body, "gemini/security")
	})

	t.Run("plain output footer includes failed and canceled members", func(t *testing.T) {
		h.Cfg.CI.IncludeCosts = false
		comments := h.CaptureComments()
		_, synth, _ := h.seedCIPanelRun(t, "acme/api", 15, "headsha000", "base..headsha000",
			[]jobSpec{
				{Agent: "codex", ReviewType: "default", Status: "done", Output: "x"},
				{Agent: "claude", ReviewType: "security", Status: "failed", Error: "boom"},
				{Agent: "gemini", ReviewType: "design", Status: "canceled", Error: "timeout"},
			})
		h.completeSynthesisWithReview(t, synth.ID, "Medium issue found.")

		h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

		require.Len(t, *comments, 1)
		body := (*comments)[0].Body
		assert.Contains(t, body, "Reviewers: 3 total (1 done, 1 failed, 1 canceled)")
		assert.NotContains(t, body, "codex/default")
		assert.NotContains(t, body, "claude/security")
		assert.NotContains(t, body, "gemini/design")
	})

	t.Run("plain output footer counts quota timeout and outage members as skipped", func(t *testing.T) {
		h.Cfg.CI.IncludeCosts = false
		comments := h.CaptureComments()
		_, synth, _ := h.seedCIPanelRun(t, "acme/api", 24, "headsha4444", "base..headsha4444",
			[]jobSpec{
				{Agent: "codex", ReviewType: "default", Status: "done", Output: "x"},
				{Agent: "gemini", ReviewType: "security", Status: "failed", Error: reviewpkg.QuotaErrorPrefix + "quota exhausted"},
				{Agent: "claude", ReviewType: "default", Status: "canceled", Error: reviewpkg.TimeoutErrorPrefix + "batch expired"},
				{Agent: "droid", ReviewType: "default", Status: "failed", Error: reviewpkg.OutageErrorPrefix + "503"},
			})
		h.completeSynthesisWithReview(t, synth.ID, "Medium issue found.")

		h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

		require.Len(t, *comments, 1)
		body := (*comments)[0].Body
		assert.Contains(t, body, "Reviewers: 4 total (1 done, 3 skipped)")
		assert.NotContains(t, body, "1 failed")
		assert.NotContains(t, body, "1 canceled")
		assert.NotContains(t, body, "gemini/security")
	})

	t.Run("prefixed output is not re-wrapped", func(t *testing.T) {
		h.Cfg.CI.IncludeCosts = false
		comments := h.CaptureComments()
		const headSHA = "abc1234feedface"
		_, synth, _ := h.seedCIPanelRun(t, "acme/api", 10, headSHA, "base.."+headSHA,
			[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "x"}})
		h.completeSynthesisWithReview(t, synth.ID, "## roborev: Combined Review (`abc1234`)\n\nAlready headed.")

		h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

		require.Len(t, *comments, 1)
		body := (*comments)[0].Body
		assert.Equal(t, 1, strings.Count(body, "## roborev:"), "no double header")
		assert.Contains(t, body, "## roborev: Combined Review (`"+git.ShortSHA(headSHA)+"`)")
		assert.Contains(t, body, "Already headed.")
		assert.Contains(t, body, "Reviewers: 1 done")
		assert.NotContains(t, body, "Panel:")
		assert.NotContains(t, body, "Members:")
		assert.NotContains(t, body, "Head:", "reviewed head belongs in the title, not the footer")
	})

	t.Run("prefixed output is bounded with footer", func(t *testing.T) {
		h.Cfg.CI.IncludeCosts = false
		comments := h.CaptureComments()
		const headSHA = "7654321feedface"
		_, synth, _ := h.seedCIPanelRun(t, "acme/api", 21, headSHA, "base.."+headSHA,
			[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "x"}})
		output := "## roborev: Combined Review (`7654321`)\n\n" +
			strings.Repeat("ü", reviewpkg.MaxCommentLen)
		h.completeSynthesisWithReview(t, synth.ID, output)

		h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

		require.Len(t, *comments, 1)
		body := (*comments)[0].Body
		assert.LessOrEqual(t, len(body), reviewpkg.MaxCommentLen)
		assert.True(t, utf8.ValidString(body), "truncated comment must be valid UTF-8")
		assert.Equal(t, 1, strings.Count(body, "## roborev:"), "no double header")
		assert.Contains(t, body, "...(truncated)")
		assert.Contains(t, body, "Reviewers: 1 done")
		assert.NotContains(t, body, "Panel:")
		assert.NotContains(t, body, "Members:")
	})
}

// TestPanelRawFallbackRendersHeadSHA covers F11's SHA rule: the comment renders
// row.HeadSHA (short), never the synthesis job's merge-base range.
func TestPanelRawFallbackRendersHeadSHA(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()

	const headSHA = "2222222bbbbbb"
	_, synth, _ := h.seedCIPanelRun(t, "acme/api", 11, headSHA, "1111111aaaaaa.."+headSHA,
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "finding"}})
	h.markJobFailed(t, synth.ID, "synthesis crashed") // raw fallback path renders the SHA

	h.Poller.handleReviewFailed(ciEvent(synth.ID, "review.failed"))

	require.Len(t, *comments, 1)
	body := (*comments)[0].Body
	assert.Contains(body, git.ShortSHA(headSHA), "renders the head short SHA")
	assert.NotContains(body, "1111111", "must not render the merge-base SHA")
}

// panelRetiredAt reports whether the panel row's retired_at is set.
func (h *ciPollerHarness) panelRetiredAt(t *testing.T, id int64) bool {
	t.Helper()
	var retiredAt *string
	err := h.DB.QueryRow(`SELECT retired_at FROM ci_pr_panels WHERE id = ?`, id).Scan(&retiredAt)
	require.NoError(t, err)
	return retiredAt != nil
}

// TestPostPanelRunDefersTransient covers the non-blocking transient policy: an
// all-transient panel (no member succeeded; every failure is a provider outage)
// posts NO comment, sets a pending status, defers the attempt, and retires the
// panel run so a later sweep re-enqueues — never a terminal "Review Failed".
func TestPostPanelRunDefersTransient(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	statuses := h.CaptureCommitStatuses()

	const headSHA = "transient1234567"
	created, err := h.DB.ReserveReviewAttempt("acme/api", 80, headSHA, time.Now())
	require.NoError(t, err)
	require.True(t, created, "attempt row reserved")

	outage := reviewpkg.OutageErrorPrefix + "429 Too Many Requests"
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 80, headSHA, "base.."+headSHA,
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "failed", Error: outage}})
	h.markJobFailed(t, synth.ID, "synthesis released after all members failed")

	h.Poller.handleReviewFailed(ciEvent(synth.ID, "review.failed"))

	assert.Empty(*comments, "transient defer must not post a comment")
	require.Len(t, *statuses, 1, "transient defer sets exactly one status")
	assert.Equal("pending", (*statuses)[0].State, "transient defer status is pending, never failure")
	assert.False(h.panelPostedAt(t, panel.ID), "deferred panel is not marked posted")
	assert.True(h.panelRetiredAt(t, panel.ID), "deferred panel is retired (removed from active set)")

	attempt, err := h.DB.GetReviewAttempt("acme/api", 80, headSHA)
	require.NoError(t, err)
	require.NotNil(t, attempt)
	assert.Equal("deferred", attempt.State, "attempt deferred for retry")
	assert.Equal("transient", attempt.LastErrorClass, "deferral records the transient class")
	assert.Equal(0, attempt.ConsecutiveGenuineAttempts, "transient defer does not bump the genuine streak")
	assert.NotNil(attempt.NextAttemptAt, "transient defer schedules a next attempt")
}

// TestPostPanelRunDefersQuotaOnly covers the agent-unavailable policy: when
// every member is skipped because the configured agent is quota/session limited,
// the panel is deferred for the retry sweep instead of posting an all-skipped
// result and marking the HEAD terminal.
func TestPostPanelRunDefersQuotaOnly(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	statuses := h.CaptureCommitStatuses()

	const headSHA = "quotaonly123456"
	created, err := h.DB.ReserveReviewAttempt("acme/api", 86, headSHA, time.Now())
	require.NoError(t, err)
	require.True(t, created, "attempt row reserved")

	quota := reviewpkg.QuotaErrorPrefix + "agent codex quota cooldown active"
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 86, headSHA, "base.."+headSHA,
		[]jobSpec{{Agent: "codex", ReviewType: "review", Status: "failed", Error: quota}})
	h.markJobFailed(t, synth.ID, "synthesis released after all members failed")

	h.Poller.handleReviewFailed(ciEvent(synth.ID, "review.failed"))

	assert.Empty(*comments, "quota-only defer must not post a comment")
	require.Len(t, *statuses, 1, "quota-only defer sets exactly one status")
	assert.Equal("pending", (*statuses)[0].State, "quota-only defer status is pending")
	assert.False(h.panelPostedAt(t, panel.ID), "deferred panel is not marked posted")
	assert.True(h.panelRetiredAt(t, panel.ID), "deferred panel is retired for retry")

	attempt, err := h.DB.GetReviewAttempt("acme/api", 86, headSHA)
	require.NoError(t, err)
	require.NotNil(t, attempt)
	assert.Equal("deferred", attempt.State)
	assert.Equal("transient", attempt.LastErrorClass)
	assert.Equal(0, attempt.ConsecutiveGenuineAttempts)
	assert.NotNil(attempt.NextAttemptAt, "quota-only defer schedules a next attempt")
}

// TestPostPanelRunDefersGenuine covers the genuine-failure defer: a genuine
// failure below the give-up cap posts nothing, sets pending, bumps the
// consecutive-genuine streak, and retires the run.
func TestPostPanelRunDefersGenuine(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	statuses := h.CaptureCommitStatuses()

	const headSHA = "genuine12345678"
	_, err := h.DB.ReserveReviewAttempt("acme/api", 81, headSHA, time.Now())
	require.NoError(t, err)

	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 81, headSHA, "base.."+headSHA,
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "failed", Error: "bad model config"}})
	h.markJobFailed(t, synth.ID, "synthesis released after all members failed")

	h.Poller.handleReviewFailed(ciEvent(synth.ID, "review.failed"))

	assert.Empty(*comments, "genuine defer must not post a comment")
	require.Len(t, *statuses, 1)
	assert.Equal("pending", (*statuses)[0].State, "genuine defer status is pending")
	assert.True(h.panelRetiredAt(t, panel.ID), "deferred panel is retired")

	attempt, err := h.DB.GetReviewAttempt("acme/api", 81, headSHA)
	require.NoError(t, err)
	require.NotNil(t, attempt)
	assert.Equal("deferred", attempt.State)
	assert.Equal("genuine", attempt.LastErrorClass)
	assert.Equal(1, attempt.ConsecutiveGenuineAttempts, "genuine defer bumps the streak")
}

// TestPostPanelRunGenuineGiveUp covers the genuine give-up: once the
// consecutive-genuine streak reaches the cap, the run posts a soft note, sets a
// blocking status, and finalizes the attempt as done.
func TestPostPanelRunGenuineGiveUp(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	statuses := h.CaptureCommitStatuses()

	const headSHA = "giveup123456789"
	_, err := h.DB.ReserveReviewAttempt("acme/api", 82, headSHA, time.Now())
	require.NoError(t, err)
	// Drive the streak to one below the cap so this attempt (streak+1) hits it.
	_, err = h.DB.Exec(`UPDATE ci_pr_review_attempts SET consecutive_genuine_attempts = ?
		WHERE github_repo = ? AND pr_number = ? AND head_sha = ?`,
		reviewpkg.DefaultRetrySchedule.GenuineMax-1, "acme/api", 82, headSHA)
	require.NoError(t, err)

	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 82, headSHA, "base.."+headSHA,
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "failed", Error: "still broken"}})
	h.markJobFailed(t, synth.ID, "synthesis released after all members failed")

	h.Poller.handleReviewFailed(ciEvent(synth.ID, "review.failed"))

	require.Len(t, *comments, 1, "give-up posts a soft note")
	assert.Contains((*comments)[0].Body, "## roborev: Review Unavailable", "give-up note header")
	require.Len(t, *statuses, 1)
	assert.Equal("error", (*statuses)[0].State, "genuine give-up status blocks required checks")
	assert.Equal("All reviews failed", (*statuses)[0].Desc)
	assert.True(h.panelPostedAt(t, panel.ID), "give-up finalizes the panel")

	attempt, err := h.DB.GetReviewAttempt("acme/api", 82, headSHA)
	require.NoError(t, err)
	require.NotNil(t, attempt)
	assert.Equal("done", attempt.State, "give-up marks the attempt done")
}

// TestPostPanelRunTransientGiveUp covers the transient give-up: an all-transient
// panel whose first attempt is older than the 3-day retry wall stops deferring
// and posts a non-blocking transient note instead, sets a success (non-failing)
// status, finalizes the attempt as done, and marks the panel posted (not retired).
func TestPostPanelRunTransientGiveUp(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	statuses := h.CaptureCommitStatuses()

	const headSHA = "transientgiveup1"
	created, err := h.DB.ReserveReviewAttempt("acme/api", 85, headSHA, time.Now())
	require.NoError(t, err)
	require.True(t, created, "attempt row reserved")
	// Backdate first_attempt_at past the transient wall so TransientExhausted is
	// true (the wall is a strict threshold to exceed).
	oldFirst := time.Now().Add(-(reviewpkg.DefaultRetrySchedule.TransientWall + time.Hour)).Format(time.RFC3339)
	_, err = h.DB.Exec(`UPDATE ci_pr_review_attempts SET first_attempt_at = ?
		WHERE github_repo = ? AND pr_number = ? AND head_sha = ?`,
		oldFirst, "acme/api", 85, headSHA)
	require.NoError(t, err)

	outage := reviewpkg.OutageErrorPrefix + "429 too many requests"
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 85, headSHA, "base.."+headSHA,
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "failed", Error: outage}})
	h.markJobFailed(t, synth.ID, "synthesis released after all members failed")

	h.Poller.handleReviewFailed(ciEvent(synth.ID, "review.failed"))

	require.Len(t, *comments, 1, "transient give-up posts one note after the wall")
	body := (*comments)[0].Body
	assert.Contains(body, "## roborev: Review Unavailable", "transient give-up note header")
	assert.Contains(body, "3 days", "transient note explains the 3-day wall")
	assert.Contains(body, "provider", "transient note blames the AI provider")
	assert.NotContains(body, "next commit", "must be the transient note, not the genuine soft note")
	assert.NotContains(body, "Review Failed", "give-up note is not a terminal Review Failed comment")
	assert.NotContains(body, "Check CI logs", "give-up note is not a terminal failure comment")

	require.Len(t, *statuses, 1, "transient give-up sets exactly one status")
	assert.Equal("success", (*statuses)[0].State, "give-up status is non-failing")
	assert.NotEqual("failure", (*statuses)[0].State, "give-up status is never failure")
	assert.NotEqual("error", (*statuses)[0].State, "give-up status is never error")

	assert.True(h.panelPostedAt(t, panel.ID), "give-up finalizes the panel (posted_at set)")
	assert.False(h.panelRetiredAt(t, panel.ID), "give-up posts the panel, it is not retired")

	attempt, err := h.DB.GetReviewAttempt("acme/api", 85, headSHA)
	require.NoError(t, err)
	require.NotNil(t, attempt)
	assert.Equal("done", attempt.State, "transient give-up marks the attempt done")
}

// TestFinalizePanelRunBackfillsMissingAttemptRow covers upgrade-boundary panel
// rows that predate reserve-on-enqueue. A terminal panel with no attempt row
// must self-heal and still post instead of remaining active/unposted forever.
func TestFinalizePanelRunBackfillsMissingAttemptRow(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	statuses := h.CaptureCommitStatuses()

	const headSHA = "missingattempt01"
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 86, headSHA, "base.."+headSHA,
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "Finding A"}})
	h.completeSynthesisWithReview(t, synth.ID, "## Combined\nVerified.")

	// Force the invariant violation: delete the reserve-on-enqueue attempt row.
	require.NoError(t, h.DB.DeleteReviewAttempt("acme/api", 86, headSHA))
	gone, err := h.DB.GetReviewAttempt("acme/api", 86, headSHA)
	require.NoError(t, err)
	require.Nil(t, gone, "attempt row deleted to force the missing-row invariant")

	// Drive finalize through the real event path.
	h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

	assert.Len(*comments, 1, "missing attempt row is backfilled and comment still posts")
	assert.Len(*statuses, 1, "commit status is set after backfill")
	assert.True(h.panelPostedAt(t, panel.ID), "panel finalized after backfill")
	assert.False(h.panelRetiredAt(t, panel.ID), "posted panel is not retired")

	attempt, err := h.DB.GetReviewAttempt("acme/api", 86, headSHA)
	require.NoError(t, err)
	require.NotNil(t, attempt)
	assert.Equal("done", attempt.State, "backfilled attempt is marked done")
}

// TestPanelMemberEventIgnored verifies a member job's event posts nothing: only
// the synthesis job routes to posting.
func TestPanelMemberEventIgnored(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()

	_, _, members := h.seedCIPanelRun(t, "acme/api", 12, "headsha777", "base..headsha777",
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "finding"}})

	h.Poller.handleReviewCompleted(ciEvent(members[0].ID, "review.completed"))

	assert.Empty(*comments, "a member event must not post a PR comment")
}
