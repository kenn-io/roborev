package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

// cmdHeaderLine returns the rendered "Command:" header line from a full view,
// or "" if none is present. Used to assert on the command header in isolation
// from the rest of the rendered screen.
func cmdHeaderLine(view string) string {
	for ln := range strings.SplitSeq(view, "\n") {
		if strings.Contains(ln, "Command:") {
			return ln
		}
	}
	return ""
}

func TestCommandHeaderLinesCollapsedTruncates(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 12 // narrower than "Command: test" (13)
	m.cmdExpanded = false

	job := makeJob(1, withAgent("test"))
	lines := m.commandHeaderLines(&job)

	assert.Len(t, lines, 1)
	assert.Contains(t, lines[0], "…")
}

func TestCommandHeaderLinesExpandedWraps(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 12
	m.cmdExpanded = true

	job := makeJob(1, withAgent("test"))
	lines := m.commandHeaderLines(&job)

	assert.Greater(t, len(lines), 1, "expanded command should wrap to multiple lines")
	joined := strings.Join(lines, " ")
	assert.Contains(t, joined, "Command:")
	assert.Contains(t, joined, "test")
	for _, ln := range lines {
		assert.NotContains(t, ln, "…", "wrapped lines must not be truncated")
	}
}

func TestCommandHeaderLinesEmptyForNoCommand(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 80

	job := makeJob(1, withAgent("")) // no agent -> no command line
	assert.Empty(t, m.commandHeaderLines(&job))
}

func TestCommandHeaderLinesFitsWithoutTruncation(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 80
	m.cmdExpanded = false

	job := makeJob(1, withAgent("test"))
	lines := m.commandHeaderLines(&job)

	assert.Len(t, lines, 1)
	assert.NotContains(t, lines[0], "…")
	assert.Contains(t, lines[0], "Command: test")
}

func TestLogVisibleLinesShrinksWhenCommandExpanded(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.height = 30
	m.width = 12 // forces the command to wrap when expanded
	m.logJobID = 1
	m.jobs = []storage.ReviewJob{makeJob(1, withAgent("test"))}

	collapsed := m.logVisibleLines()
	m.cmdExpanded = true
	expanded := m.logVisibleLines()

	assert.Greater(t, collapsed, expanded,
		"expanding a wrapped command must reserve more header lines")
}

func TestLogViewTogglesCommandExpand(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewLog
	m.logJobID = 1
	m.logFromView = viewQueue
	m.height = 30
	m.width = 80
	m.jobs = []storage.ReviewJob{makeJob(1, withAgent("test"))}

	assert.False(t, m.cmdExpanded)

	m2, _ := pressKey(m, 'i')
	assert.True(t, m2.cmdExpanded, "i should expand the command in the log view")

	m3, _ := pressKey(m2, 'i')
	assert.False(t, m3.cmdExpanded, "i should collapse the command again")
}

func TestPromptViewTogglesCommandExpand(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewKindPrompt
	m.height = 30
	m.width = 80
	job := makeJob(1, withAgent("test"))
	m.currentReview = &storage.Review{
		ID:     1,
		JobID:  1,
		Agent:  "test",
		Prompt: "hello",
		Job:    &job,
	}

	m2, _ := pressKey(m, 'i')
	assert.True(t, m2.cmdExpanded, "i should expand the command in the prompt view")
}

func TestQueueViewIgnoresCommandExpandKey(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.height = 30
	m.width = 80
	m.jobs = []storage.ReviewJob{makeJob(1, withAgent("test"))}
	m.selectedIdx = 0

	m2, _ := pressKey(m, 'i')
	assert.False(t, m2.cmdExpanded, "i should not toggle command expand outside log/prompt views")
}

func TestLogViewRendersFullCommandWhenExpanded(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewLog
	m.logJobID = 1
	m.logFromView = viewQueue
	m.height = 30
	m.width = 12 // forces truncation when collapsed
	m.jobs = []storage.ReviewJob{makeJob(1, withAgent("test"))}
	m.logLines = []logLine{{text: "out"}}

	collapsed := cmdHeaderLine(m.View().Content)
	assert.Contains(t, collapsed, "…", "collapsed command header should be truncated")

	m.cmdExpanded = true
	expanded := cmdHeaderLine(m.View().Content)
	assert.NotContains(t, expanded, "…", "expanded command header line should not be truncated")
}
