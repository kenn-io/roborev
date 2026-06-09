package tui

import (
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/version"
)

func TestFitStatusSegments(t *testing.T) {
	assert := assert.New(t)
	segs := []statusSeg{
		{rendered: "Daemon: v1", prio: 0},
		{rendered: "Workers: 0/4", prio: 30},
		{rendered: "Completed: 5", prio: 10},
		{rendered: "Closed: 4", prio: 20},
		{rendered: "Open: 1", prio: 90},
		{rendered: "~$1.00", prio: 80},
	}
	sep := " | "

	full := "Daemon: v1 | Workers: 0/4 | Completed: 5 | Closed: 4 | Open: 1 | ~$1.00"
	assert.Equal(full, fitStatusSegments(segs, sep, 200), "wide width keeps every segment")

	// Tight width drops lowest-prio first (Daemon, Completed, Closed) and keeps
	// Workers, Open, and cost.
	got := fitStatusSegments(segs, sep, 40)
	assert.LessOrEqual(xansi.StringWidth(got), 40)
	assert.NotContains(got, "Daemon", "redundant daemon version drops first")
	assert.NotContains(got, "Completed")
	assert.NotContains(got, "Closed")
	assert.Contains(got, "Workers: 0/4")
	assert.Contains(got, "Open: 1")
	assert.Contains(got, "~$1.00")

	// Below any single segment's width, the highest-prio segment survives;
	// the caller truncates as a final guard.
	assert.Equal("Open: 1", fitStatusSegments(segs, sep, 3), "highest-prio segment always kept")

	// Unknown width (0) keeps everything rather than dropping blindly.
	assert.Equal(full, fitStatusSegments(segs, sep, 0))
}

func TestRenderQueueStatusLineAdaptsToWidth(t *testing.T) {
	assert := assert.New(t)
	m := model{
		width:         200,
		daemonVersion: "v1.0.0",
		cost:          &storage.CostAggregate{TotalUSD: 12.50, JobsWithCost: 8, JobsTotal: 10},
	}
	m.status.ActiveWorkers = 1
	m.status.MaxWorkers = 4

	wide := m.renderQueueStatusLine(100, 90, 5)
	assert.NotContains(wide, "Daemon", "version info lives in the title, not the metric line")
	assert.Contains(wide, "Workers: 1/4")
	assert.Contains(wide, "Completed: 100")
	assert.Contains(wide, "Closed: 90")
	assert.Contains(wide, "Open: 5")
	assert.Contains(wide, "~$12.50 (8/10)")

	m.width = 40
	narrow := m.renderQueueStatusLine(100, 90, 5)
	assert.LessOrEqual(xansi.StringWidth(narrow), 40, "must fit the terminal width")
	assert.NotContains(narrow, "Completed")
	assert.NotContains(narrow, "Closed")
	assert.Contains(narrow, "Open: 5")
	assert.Contains(narrow, "~$12.50", "cost is kept longer than Completed/Closed")
}

func TestRenderQueueTitleShowsMismatch(t *testing.T) {
	assert := assert.New(t)

	// Matching daemon: client version right-aligned, no daemon callout anywhere.
	m := model{width: 200, daemonVersion: version.Version}
	title := m.renderQueueTitle()
	assert.Contains(title, "roborev")
	assert.Contains(title, version.Version, "client version is still shown (right-aligned)")
	assert.NotContains(title, "Daemon:")

	// Mismatch: the daemon version is shown inside the parens alongside the
	// client version (rendered red), and "queue"/"[MISMATCH]" are gone.
	m = model{width: 200, daemonVersion: "v0.0.1-old", versionMismatch: true}
	title = m.renderQueueTitle()
	assert.Contains(title, "Daemon: v0.0.1-old")
	assert.Contains(title, version.Version, "client version still shown")
	assert.NotContains(title, "[MISMATCH]")
	assert.NotContains(title, "queue")
}

func TestFitTitleLeft(t *testing.T) {
	assert := assert.New(t)
	app := "roborev"
	filters := " [f: roborev] [b: feat/aggregate-cost]"
	hideClosed := " [hiding closed]"
	full := app + filters + hideClosed

	// Wide: everything kept.
	assert.Equal(full, fitTitleLeft(app, filters, hideClosed, 200))

	// Medium: the hiding-closed flag drops as a unit; filters stay intact.
	assert.Equal(app+filters, fitTitleLeft(app, filters, hideClosed, len(app+filters)+2))

	// Narrow: truncates with an ellipsis; app + the leftmost filter survive.
	got := fitTitleLeft(app, filters, hideClosed, 25)
	assert.LessOrEqual(xansi.StringWidth(got), 25)
	assert.Contains(got, "roborev")
	assert.Contains(got, "[f: roborev")
	assert.Contains(got, "…")
}

func TestRenderQueueTitleRightAlignsVersion(t *testing.T) {
	assert := assert.New(t)

	// Wide: the version is pushed to the far right and the line fits the width.
	m := model{width: 200, daemonVersion: version.Version}
	wide := m.renderQueueTitle()
	assert.LessOrEqual(xansi.StringWidth(wide), 200)
	assert.Contains(wide, "roborev")
	assert.Contains(wide, version.Version)

	// Too narrow for the version: it drops out entirely; the app name stays.
	m.width = len("roborev") + 1
	narrow := m.renderQueueTitle()
	assert.LessOrEqual(xansi.StringWidth(narrow), len("roborev")+1)
	assert.Contains(narrow, "roborev")
}

func TestRenderQueueStatusLineOmitsStaleCost(t *testing.T) {
	assert := assert.New(t)
	m := model{
		width:    200,
		fetchSeq: 3,
		costSeq:  2, // stale: predates the active filter generation
		cost:     &storage.CostAggregate{TotalUSD: 9.99, JobsWithCost: 1, JobsTotal: 2},
	}
	got := m.renderQueueStatusLine(0, 0, 0)
	assert.NotContains(got, "~$", "stale cost segment omitted from the status line")
}
