package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// backdatePanelPostClaim forces a panel row's posting_claimed_at older than the
// stale window so the reconcile (and ClaimPanelForPosting's CAS) treats the
// lease as a crashed poster's and reclaims it.
func (h *ciPollerHarness) backdatePanelPostClaim(t *testing.T, id int64) {
	t.Helper()
	_, err := h.DB.Exec(
		"UPDATE ci_pr_panels SET posting_claimed_at = datetime('now','-10 minutes') WHERE id = ?", id)
	require.NoError(t, err)
}

// TestReconcilePanelPostingDroppedEvent covers the spec §10 dropped-event
// recovery: a run whose synthesis is done with a persisted review but whose
// posting event was never delivered still posts. Running the reconcile a second
// time posts nothing more, proving the posting CAS makes it idempotent with the
// event-driven path.
func TestReconcilePanelPostingDroppedEvent(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	statuses := h.CaptureCommitStatuses()

	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 20, "headrec111", "base..headrec111",
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "Finding R"}})
	// Synthesis goes terminal but NO review.completed event is delivered.
	h.completeSynthesisWithReview(t, synth.ID, "## Combined\nVerified finding R.")
	require.False(t, h.panelPostedAt(t, panel.ID), "precondition: not yet posted")

	h.Poller.reconcilePanelPosting(context.Background(), "acme/api")

	assert.Len(*comments, 1, "dropped-event recovery posts exactly one comment")
	assert.True(h.panelPostedAt(t, panel.ID), "panel finalized (posted_at set)")
	assert.NotEmpty(*statuses, "commit status set")

	// Second pass: the posting CAS bars a re-post (idempotent with the event path).
	h.Poller.reconcilePanelPosting(context.Background(), "acme/api")
	assert.Len(*comments, 1, "still exactly one comment after a second reconcile")
}

func TestReconcilePanelPostingBackfillsMissingAttempt(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	statuses := h.CaptureCommitStatuses()

	const headSHA = "headrec-missing-attempt"
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 23, headSHA, "base.."+headSHA,
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "Finding U"}})
	h.completeSynthesisWithReview(t, synth.ID, "## Combined\nVerified finding U.")
	require.NoError(t, h.DB.DeleteReviewAttempt("acme/api", 23, headSHA))

	h.Poller.reconcilePanelPosting(context.Background(), "acme/api")

	assert.Len(*comments, 1, "reconcile posts terminal panel after attempt backfill")
	assert.True(h.panelPostedAt(t, panel.ID), "panel finalized after reconcile")
	assert.NotEmpty(*statuses, "commit status set")
	attempt, err := h.DB.GetReviewAttempt("acme/api", 23, headSHA)
	require.NoError(t, err)
	require.NotNil(t, attempt)
	assert.Equal("done", attempt.State, "backfilled attempt is marked done")
}

// TestReconcilePanelPostingStaleClaim covers reclaiming a claim whose holder
// crashed mid-post: a terminal-unposted run with a posting_claimed_at older than
// panelPostingStaleWindow is reclaimed and posted by the reconcile.
func TestReconcilePanelPostingStaleClaim(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	h.CaptureCommitStatuses() // keep hermetic: don't shell out to the real status API

	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 21, "headrec222", "base..headrec222",
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "Finding S"}})
	h.completeSynthesisWithReview(t, synth.ID, "## Combined\nVerified finding S.")
	// A crashed poster left a stale claim; posted_at is still NULL.
	h.backdatePanelPostClaim(t, panel.ID)

	h.Poller.reconcilePanelPosting(context.Background(), "acme/api")

	assert.Len(*comments, 1, "stale-claim recovery reclaims and posts one comment")
	assert.True(h.panelPostedAt(t, panel.ID), "panel finalized after reclaim")
}

// TestReconcilePanelPostingNoop verifies the reconcile posts nothing when the
// only run is already posted (posted_at set bars it from the recovery set).
func TestReconcilePanelPostingNoop(t *testing.T) {
	assert := assert.New(t)
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()

	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 22, "headrec333", "base..headrec333",
		[]jobSpec{{Agent: "test", ReviewType: "review", Status: "done", Output: "Finding T"}})
	h.completeSynthesisWithReview(t, synth.ID, "## Combined\nVerified finding T.")
	require.NoError(t, h.DB.MarkPanelPosted(panel.ID))

	h.Poller.reconcilePanelPosting(context.Background(), "acme/api")

	assert.Empty(*comments, "already-posted run is not re-posted")
}
