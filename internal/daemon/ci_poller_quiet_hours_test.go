package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

// quietWindow resolves a quiet-hours window that either contains or
// excludes the instant at (in UTC), for driving throttlePR deterministically.
func quietWindow(t *testing.T, at time.Time, interval string, active bool) *config.QuietHoursWindow {
	t.Helper()
	u := at.UTC()
	start, end := u.Add(-time.Hour), u.Add(2*time.Hour)
	if !active {
		start, end = u.Add(2*time.Hour), u.Add(3*time.Hour)
	}
	w, err := (&config.QuietHoursConfig{
		Start:            start.Format("15:04"),
		End:              end.Format("15:04"),
		Timezone:         "UTC",
		ThrottleInterval: interval,
	}).Resolve()
	require.NoError(t, err)
	require.NotNil(t, w)
	require.Equal(t, active, w.Active(at), "window activity sanity check")
	return w
}

func TestResolveQuietHours(t *testing.T) {
	ci := &config.CIConfig{QuietHours: config.QuietHoursConfig{
		Start: "23:00", End: "05:00",
	}}
	assert.NotNil(t, resolveQuietHours(ci), "valid config resolves")

	ci.QuietHours.Start = "25:00"
	assert.Nil(t, resolveQuietHours(ci), "invalid config disables quiet hours")

	assert.Nil(t, resolveQuietHours(&config.CIConfig{}), "unset config disables quiet hours")
}

// newQuietHoursHarness builds a poller harness with the base throttle and
// bypass list configured, plus a fixed clock for throttle decisions.
func newQuietHoursHarness(t *testing.T, baseThrottle string, bypass []string, now time.Time) *ciPollerHarness {
	t.Helper()
	h := newCIPollerHarness(t, "git@github.com:acme/api.git")
	h.Cfg.CI.ReviewTypes = []string{"security"}
	h.Cfg.CI.Agents = []string{"codex"}
	h.Cfg.CI.ThrottleInterval = baseThrottle
	h.Cfg.CI.ThrottleBypassUsers = bypass
	h.Poller = NewCIPoller(h.DB, NewStaticConfig(h.Cfg), nil)
	h.Poller.nowFn = func() time.Time { return now }
	h.stubProcessPRGit()
	h.Poller.mergeBaseFn = func(_, _, _ string) (string, error) {
		return "base-sha", nil
	}
	return h
}

func TestCIPollerProcessPR_QuietHoursThrottlesBypassUser(t *testing.T) {
	assert := assert.New(t)
	now := time.Now()
	// Base throttle disabled and author on the bypass list: outside quiet
	// hours this PR would never be throttled.
	h := newQuietHoursHarness(t, "0", []string{"wesm"}, now)
	h.Poller.quietHours = quietWindow(t, now, "1h", true)

	pr := func(sha string) ghPR {
		return ghPR{
			Number: 90, HeadRefOid: sha, BaseRefName: "main",
			Author: ghPRAuthor{Login: "wesm"},
		}
	}

	// First push — no prior review, reviewed immediately even in the window.
	err := h.Poller.processPR(context.Background(), "acme/api", pr("first-sha"), h.Cfg)
	require.NoError(t, err, "first processPR")
	assert.True(h.hasPanel(t, "acme/api", 90, "first-sha"), "first review is never blocked")

	firstPanel, err := h.DB.GetCIPanelByPRSHA("acme/api", 90, "first-sha")
	require.NoError(t, err)
	firstSynth, err := h.DB.GetSynthesisJob(firstPanel.PanelRunUUID)
	require.NoError(t, err)
	firstMembers, err := h.DB.GetPanelMembers(firstPanel.PanelRunUUID)
	require.NoError(t, err)
	require.Len(t, firstMembers, 1)

	// Second push within the quiet interval — throttled despite bypass.
	captured := h.CaptureCommitStatuses()
	err = h.Poller.processPR(context.Background(), "acme/api", pr("second-sha"), h.Cfg)
	require.NoError(t, err, "second processPR")
	assert.False(h.hasPanel(t, "acme/api", 90, "second-sha"),
		"quiet hours must throttle bypass users")
	require.Len(t, *captured, 1, "expected one deferred status")
	assert.Equal("pending", (*captured)[0].State)
	assert.Contains((*captured)[0].Desc, "Review deferred")

	// A quiet-hours-only deferral must not supersede the in-flight panel:
	// with frequent overnight pushes, canceling on every push could kill
	// each panel before it completes and produce no reviews at all.
	active, err := h.DB.GetActivePanelsForPR("acme/api", 90)
	require.NoError(t, err)
	assert.Len(active, 1, "in-flight panel must survive a quiet-hours deferral")
	assert.NotEqual(storage.JobStatusCanceled, h.jobStatus(t, firstSynth.ID),
		"synthesis must not be canceled")
	assert.NotEqual(storage.JobStatusCanceled, h.jobStatus(t, firstMembers[0].ID),
		"member must not be canceled")
}

func TestCIPollerProcessPR_QuietHoursElapsedBaseKeepsPanel(t *testing.T) {
	assert := assert.New(t)
	// A non-bypass contributor whose base throttle (1h) has elapsed but whose
	// quiet-hours interval (2h) has not: the deferral is quiet-hours-only, so
	// the in-flight panel must keep running.
	now := time.Now().Add(75 * time.Minute)
	h := newQuietHoursHarness(t, "1h", nil, now)
	h.Poller.quietHours = quietWindow(t, now, "2h", true)

	pr := func(sha string) ghPR {
		return ghPR{
			Number: 93, HeadRefOid: sha, BaseRefName: "main",
			Author: ghPRAuthor{Login: "contributor"},
		}
	}

	err := h.Poller.processPR(context.Background(), "acme/api", pr("first-sha"), h.Cfg)
	require.NoError(t, err, "first processPR")
	require.True(t, h.hasPanel(t, "acme/api", 93, "first-sha"))

	err = h.Poller.processPR(context.Background(), "acme/api", pr("second-sha"), h.Cfg)
	require.NoError(t, err, "second processPR")
	assert.False(h.hasPanel(t, "acme/api", 93, "second-sha"),
		"quiet-hours interval must still throttle after the base interval elapses")

	active, err := h.DB.GetActivePanelsForPR("acme/api", 93)
	require.NoError(t, err)
	assert.Len(active, 1, "in-flight panel must survive a quiet-hours-only deferral")
}

func TestCIPollerProcessPR_QuietHoursInactiveKeepsBypass(t *testing.T) {
	now := time.Now()
	h := newQuietHoursHarness(t, "1h", []string{"wesm"}, now)
	h.Poller.quietHours = quietWindow(t, now, "1h", false)

	pr := func(sha string) ghPR {
		return ghPR{
			Number: 91, HeadRefOid: sha, BaseRefName: "main",
			Author: ghPRAuthor{Login: "wesm"},
		}
	}

	err := h.Poller.processPR(context.Background(), "acme/api", pr("first-sha"), h.Cfg)
	require.NoError(t, err, "first processPR")
	require.True(t, h.hasPanel(t, "acme/api", 91, "first-sha"))

	// Outside the window, bypass users keep their existing behavior.
	err = h.Poller.processPR(context.Background(), "acme/api", pr("second-sha"), h.Cfg)
	require.NoError(t, err, "second processPR")
	assert.True(t, h.hasPanel(t, "acme/api", 91, "second-sha"),
		"bypass user must not be throttled outside quiet hours")
}

func TestCIPollerProcessPR_QuietHoursBaseThrottleWins(t *testing.T) {
	assert := assert.New(t)
	// The clock reads 30 minutes into the base throttle window. The quiet
	// interval (1s) has long elapsed; if it replaced the base interval
	// instead of max-ing with it, the second push would be reviewed.
	now := time.Now().Add(30 * time.Minute)
	h := newQuietHoursHarness(t, "1h", nil, now)
	h.Poller.quietHours = quietWindow(t, now, "1s", true)

	pr := func(sha string) ghPR {
		return ghPR{
			Number: 92, HeadRefOid: sha, BaseRefName: "main",
			Author: ghPRAuthor{Login: "contributor"},
		}
	}

	err := h.Poller.processPR(context.Background(), "acme/api", pr("first-sha"), h.Cfg)
	require.NoError(t, err, "first processPR")
	assert.True(h.hasPanel(t, "acme/api", 92, "first-sha"))

	err = h.Poller.processPR(context.Background(), "acme/api", pr("second-sha"), h.Cfg)
	require.NoError(t, err, "second processPR")
	assert.False(h.hasPanel(t, "acme/api", 92, "second-sha"),
		"base throttle interval must still apply when longer than the quiet interval")

	// The base throttle drove this deferral, so ordinary supersede semantics
	// apply: the stale in-flight panel is canceled.
	active, err := h.DB.GetActivePanelsForPR("acme/api", 92)
	require.NoError(t, err)
	assert.Empty(active, "base-throttle deferral must supersede the stale panel")
}

func TestCIPollerProcessPR_QuietHoursSnapshotPostsAfterHeadAdvance(t *testing.T) {
	assert := assert.New(t)
	now := time.Now()
	h := newQuietHoursHarness(t, "0", []string{"wesm"}, now)
	h.Poller.quietHours = quietWindow(t, now, "1h", true)
	comments := h.CaptureComments()

	pr := func(sha string) ghPR {
		return ghPR{
			Number: 94, HeadRefOid: sha, BaseRefName: "main",
			Author: ghPRAuthor{Login: "wesm"},
		}
	}

	// First push starts a panel; the second is deferred by quiet hours only
	// and must flag the retained panel to post despite the HEAD advance.
	err := h.Poller.processPR(context.Background(), "acme/api", pr("first-sha"), h.Cfg)
	require.NoError(t, err, "first processPR")
	err = h.Poller.processPR(context.Background(), "acme/api", pr("second-sha"), h.Cfg)
	require.NoError(t, err, "second processPR")

	panel, err := h.DB.GetCIPanelByPRSHA("acme/api", 94, "first-sha")
	require.NoError(t, err)
	assert.True(panel.AllowStalePost, "quiet-only deferral flags the retained panel")

	// The PR is at second-sha by the time the retained panel completes.
	h.Poller.prPostTargetFn = func(context.Context, string, int) (panelPostTarget, error) {
		return panelPostTarget{Open: true, HeadSHA: "second-sha"}, nil
	}
	members, err := h.DB.GetPanelMembers(panel.PanelRunUUID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	h.markJobDoneWithReview(t, members[0].ID, "codex", "Snapshot finding")
	synth, err := h.DB.GetSynthesisJob(panel.PanelRunUUID)
	require.NoError(t, err)
	h.completeSynthesisWithReview(t, synth.ID, "## Combined\nsnapshot findings")

	h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))

	require.Len(t, *comments, 1, "retained quiet-hours snapshot must post its review")
	assert.True(h.panelPostedAt(t, panel.ID), "panel is marked posted")
	assert.False(h.panelRetiredAt(t, panel.ID), "panel is not retired as stale-head")
}

// TestCIPollerQuietHoursFlagSetAfterRowLoadStillPosts covers the cross-
// goroutine race: the event listener loads the panel row, then a concurrent
// quiet-hours poll flags allow_stale_post, then posting proceeds. The
// posting path must re-read the row under the claim and post the snapshot
// instead of retiring on the stale in-memory flag.
func TestCIPollerQuietHoursFlagSetAfterRowLoadStillPosts(t *testing.T) {
	assert := assert.New(t)
	now := time.Now()
	h := newQuietHoursHarness(t, "0", []string{"wesm"}, now)
	h.Poller.quietHours = quietWindow(t, now, "1h", true)
	comments := h.CaptureComments()

	err := h.Poller.processPR(context.Background(), "acme/api",
		ghPR{
			Number: 95, HeadRefOid: "first-sha", BaseRefName: "main",
			Author: ghPRAuthor{Login: "wesm"},
		}, h.Cfg)
	require.NoError(t, err, "first processPR")

	panel, err := h.DB.GetCIPanelByPRSHA("acme/api", 95, "first-sha")
	require.NoError(t, err)
	members, err := h.DB.GetPanelMembers(panel.PanelRunUUID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	h.markJobDoneWithReview(t, members[0].ID, "codex", "Snapshot finding")
	synth, err := h.DB.GetSynthesisJob(panel.PanelRunUUID)
	require.NoError(t, err)
	h.completeSynthesisWithReview(t, synth.ID, "## Combined\nsnapshot findings")

	// Load the row as the event listener would, BEFORE the marking poll.
	staleRow, err := h.DB.GetCIPanelBySynthesisJobID(synth.ID)
	require.NoError(t, err)
	require.False(t, staleRow.AllowStalePost, "row loaded before the flag is set")

	// Concurrent poll marks the panel and the PR advances.
	marked, err := h.DB.MarkPanelsAllowStalePost("acme/api", 95, "second-sha")
	require.NoError(t, err)
	require.Equal(t, int64(1), marked)
	h.Poller.prPostTargetFn = func(context.Context, string, int) (panelPostTarget, error) {
		return panelPostTarget{Open: true, HeadSHA: "second-sha"}, nil
	}

	h.Poller.postPanelRun(context.Background(), staleRow)

	require.Len(t, *comments, 1, "posting must honor the flag set after the row was loaded")
	assert.True(h.panelPostedAt(t, panel.ID), "panel is marked posted")
	assert.False(h.panelRetiredAt(t, panel.ID), "panel is not retired on the stale in-memory flag")
}

// TestCIPollerQuietHoursFlagSetDuringTargetLookupStillPosts covers the
// narrowest interleaving: the quiet-hours poll marks allow_stale_post while
// the posting goroutine is inside the GitHub target lookup, after any row
// (re)load. Stale-head retirement must be the atomic retire-unless-flagged
// statement, so the marking winning the race turns the outcome into a
// posted snapshot instead of a lost review.
func TestCIPollerQuietHoursFlagSetDuringTargetLookupStillPosts(t *testing.T) {
	assert := assert.New(t)
	now := time.Now()
	h := newQuietHoursHarness(t, "0", []string{"wesm"}, now)
	h.Poller.quietHours = quietWindow(t, now, "1h", true)
	comments := h.CaptureComments()

	err := h.Poller.processPR(context.Background(), "acme/api",
		ghPR{
			Number: 96, HeadRefOid: "first-sha", BaseRefName: "main",
			Author: ghPRAuthor{Login: "wesm"},
		}, h.Cfg)
	require.NoError(t, err, "first processPR")

	panel, err := h.DB.GetCIPanelByPRSHA("acme/api", 96, "first-sha")
	require.NoError(t, err)
	members, err := h.DB.GetPanelMembers(panel.PanelRunUUID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	h.markJobDoneWithReview(t, members[0].ID, "codex", "Snapshot finding")
	synth, err := h.DB.GetSynthesisJob(panel.PanelRunUUID)
	require.NoError(t, err)
	h.completeSynthesisWithReview(t, synth.ID, "## Combined\nsnapshot findings")

	row, err := h.DB.GetCIPanelBySynthesisJobID(synth.ID)
	require.NoError(t, err)
	require.False(t, row.AllowStalePost, "flag unset when posting begins")

	// The concurrent poll marks the panel while the posting goroutine is
	// inside the target lookup — after every load the posting path does.
	h.Poller.prPostTargetFn = func(context.Context, string, int) (panelPostTarget, error) {
		marked, err := h.DB.MarkPanelsAllowStalePost("acme/api", 96, "second-sha")
		require.NoError(t, err)
		require.Equal(t, int64(1), marked, "poll marks the panel mid-lookup")
		return panelPostTarget{Open: true, HeadSHA: "second-sha"}, nil
	}

	h.Poller.postPanelRun(context.Background(), row)

	require.Len(t, *comments, 1, "flag set during target lookup must still post the snapshot")
	assert.True(h.panelPostedAt(t, panel.ID), "panel is marked posted")
	assert.False(h.panelRetiredAt(t, panel.ID), "panel is not retired on the stale in-memory flag")
}
