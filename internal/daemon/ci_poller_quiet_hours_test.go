package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
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

	// Second push within the quiet interval — throttled despite bypass.
	captured := h.CaptureCommitStatuses()
	err = h.Poller.processPR(context.Background(), "acme/api", pr("second-sha"), h.Cfg)
	require.NoError(t, err, "second processPR")
	assert.False(h.hasPanel(t, "acme/api", 90, "second-sha"),
		"quiet hours must throttle bypass users")
	require.Len(t, *captured, 1, "expected one deferred status")
	assert.Equal("pending", (*captured)[0].State)
	assert.Contains((*captured)[0].Desc, "Review deferred")
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
}
