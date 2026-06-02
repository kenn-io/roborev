package daemon

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	googlegithub "github.com/google/go-github/v84/github"
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

	// posted_at is set, so the CAS (posted_at IS NULL) bars any retry even though
	// the claim is intentionally left in place.
	won, err := h.DB.ClaimPanelForPosting(panel.ID, panelPostingStaleWindow)
	require.NoError(t, err)
	assert.False(won, "abandoned panel cannot be reclaimed for posting")
}

// TestPanelWrapperNoDoubleHeader covers F11: a synthesis output lacking the
// `## roborev:` header is wrapped exactly once; a raw/all-failed body that
// already starts with the header is not double-wrapped.
func TestPanelWrapperNoDoubleHeader(t *testing.T) {
	t.Run("plain output gets one header", func(t *testing.T) {
		h := newCIPollerHarness(t, "https://github.com/acme/api.git")
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
		h := newCIPollerHarness(t, "https://github.com/acme/api.git")
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

	t.Run("plain output footer includes panel members", func(t *testing.T) {
		h := newCIPollerHarness(t, "https://github.com/acme/api.git")
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
		assert.Contains(t, body, "Panel: ci")
		assert.Contains(t, body, "Members: codex (codex/default, done), codex (codex/security, done)")
		assert.Contains(t, body, "Synthesis: test")
		assert.NotContains(t, body, "Total: unknown")
		assert.Contains(t, body, "Job: ")
		assert.NotContains(t, body, "Head:", "reviewed head belongs in the title, not the footer")
		assert.NotContains(t, body, "base", "footer must show reviewed head, not merge base")
		assert.NotContains(t, body, "Review type:  | Agent:", "panel comments should not use the empty synthesis review_type footer")
	})

	t.Run("footer uses synthesis review agent", func(t *testing.T) {
		h := newCIPollerHarness(t, "https://github.com/acme/api.git")
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
		h := newCIPollerHarness(t, "https://github.com/acme/api.git")
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
		assert.Contains(t, body, "Panel: ci")
	})

	t.Run("plain output footer hides cost by default", func(t *testing.T) {
		h := newCIPollerHarness(t, "https://github.com/acme/api.git")
		comments := h.CaptureComments()
		_, synth, members := h.seedCIPanelRun(t, "acme/api", 18, "headsha3333", "base..headsha3333",
			[]jobSpec{
				{Agent: "codex", ReviewType: "default", Status: "done", Output: "x"},
				{Agent: "codex", ReviewType: "security", Status: "done", Output: "y"},
			})
		setJobTiming(t, h.DB, members[0].ID, "2026-06-01T18:00:00Z", "2026-06-01T18:04:32Z")
		setJobTiming(t, h.DB, members[1].ID, "2026-06-01T18:00:00Z", "2026-06-01T18:02:08Z")
		setJobTiming(t, h.DB, synth.ID, "2026-06-01T18:04:40Z", "2026-06-01T18:04:58Z")
		require.NoError(t, h.DB.SaveJobTokenUsage(members[0].ID, `{"cost_usd":0.11,"has_cost":true}`))
		require.NoError(t, h.DB.SaveJobTokenUsage(members[1].ID, `{"cost_usd":0.06,"has_cost":true}`))
		require.NoError(t, h.DB.SaveJobTokenUsage(synth.ID, `{"cost_usd":0.03,"has_cost":true}`))
		h.completeSynthesisWithReview(t, synth.ID, "Medium issue found.")

		h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

		require.Len(t, *comments, 1)
		body := (*comments)[0].Body
		assert.Contains(t, body, "Synthesis: test, 18s")
		assert.Contains(t, body, "codex (codex/default, done, 4m32s)")
		assert.Contains(t, body, "codex (codex/security, done, 2m8s)")
		assert.Contains(t, body, "Total: 6m58s")
		assert.NotContains(t, body, "~$")
		assert.NotContains(t, body, "cost partial")
	})

	t.Run("plain output footer includes runtime and cost when enabled", func(t *testing.T) {
		h := newCIPollerHarness(t, "https://github.com/acme/api.git")
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
		require.NoError(t, h.DB.SaveJobTokenUsage(members[0].ID, `{"cost_usd":0.11,"has_cost":true}`))
		require.NoError(t, h.DB.SaveJobTokenUsage(members[1].ID, `{"cost_usd":0.06,"has_cost":true}`))
		require.NoError(t, h.DB.SaveJobTokenUsage(synth.ID, `{"cost_usd":0.03,"has_cost":true}`))
		h.completeSynthesisWithReview(t, synth.ID, "Medium issue found.")

		h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

		require.Len(t, *comments, 1)
		body := (*comments)[0].Body
		assert.Contains(t, body, "Synthesis: test, 18s, ~$0.03")
		assert.Contains(t, body, "codex (codex/default, done, 4m32s, ~$0.11)")
		assert.Contains(t, body, "codex (codex/security, done, 2m8s, ~$0.06)")
		assert.Contains(t, body, "Total: 6m58s, ~$0.20")
	})

	t.Run("plain output footer shows partial cost when enabled", func(t *testing.T) {
		h := newCIPollerHarness(t, "https://github.com/acme/api.git")
		h.Cfg.CI.IncludeCosts = true
		comments := h.CaptureComments()
		_, synth, members := h.seedCIPanelRun(t, "acme/api", 17, "headsha2222", "base..headsha2222",
			[]jobSpec{
				{Agent: "codex", ReviewType: "default", Status: "done", Output: "x"},
				{Agent: "gemini", ReviewType: "security", Status: "canceled", Error: "timeout"},
			})
		setJobTiming(t, h.DB, members[0].ID, "2026-06-01T18:00:00Z", "2026-06-01T18:04:32Z")
		setJobTiming(t, h.DB, members[1].ID, "2026-06-01T18:00:00Z", "2026-06-01T18:01:14Z")
		require.NoError(t, h.DB.SaveJobTokenUsage(members[0].ID, `{"cost_usd":0.11,"has_cost":true}`))
		h.completeSynthesisWithReview(t, synth.ID, "Medium issue found.")

		h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

		require.Len(t, *comments, 1)
		body := (*comments)[0].Body
		assert.Contains(t, body, "codex (codex/default, done, 4m32s, ~$0.11)")
		assert.Contains(t, body, "gemini (gemini/security, canceled, 1m14s)")
		assert.Contains(t, body, "Total: 5m46s, cost partial ~$0.11")
	})

	t.Run("plain output footer includes failed and canceled members", func(t *testing.T) {
		h := newCIPollerHarness(t, "https://github.com/acme/api.git")
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
		assert.Contains(t, body, "codex (codex/default, done)")
		assert.Contains(t, body, "claude (claude/security, failed)")
		assert.Contains(t, body, "gemini (gemini/design, canceled)")
	})

	t.Run("prefixed output is not re-wrapped", func(t *testing.T) {
		h := newCIPollerHarness(t, "https://github.com/acme/api.git")
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
		assert.Contains(t, body, "Panel: ci")
		assert.Contains(t, body, "Members: test (test/review, done)")
		assert.NotContains(t, body, "Head:", "reviewed head belongs in the title, not the footer")
	})

	t.Run("prefixed output is bounded with footer", func(t *testing.T) {
		h := newCIPollerHarness(t, "https://github.com/acme/api.git")
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
		assert.Contains(t, body, "Panel: ci")
		assert.Contains(t, body, "Members: test (test/review, done)")
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
