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

	job := makeJob(1, withAgent("test"))
	lines := m.commandHeaderLines(&job, false)

	assert.Len(t, lines, 1)
	assert.Contains(t, lines[0], "…")
}

func TestCommandHeaderLinesExpandedWraps(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 12

	job := makeJob(1, withAgent("test"))
	lines := m.commandHeaderLines(&job, true)

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
	assert.Empty(t, m.commandHeaderLines(&job, false))
}

func TestCommandHeaderLinesFitsWithoutTruncation(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 80

	job := makeJob(1, withAgent("test"))
	lines := m.commandHeaderLines(&job, false)

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
	m.logCmdExpanded = true
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

	assert.False(t, m.logCmdExpanded)

	m2, _ := pressKey(m, 'i')
	assert.True(t, m2.logCmdExpanded, "i should expand the command in the log view")

	m3, _ := pressKey(m2, 'i')
	assert.False(t, m3.logCmdExpanded, "i should collapse the command again")
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

	assert.True(t, m.promptCmdExpanded)

	m2, _ := pressKey(m, 'i')
	assert.False(t, m2.promptCmdExpanded, "i should collapse the command in the prompt view")

	m3, _ := pressKey(m2, 'i')
	assert.True(t, m3.promptCmdExpanded, "i should expand the command again")
}

func TestQueueViewIgnoresCommandExpandKey(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.height = 30
	m.width = 80
	m.jobs = []storage.ReviewJob{makeJob(1, withAgent("test"))}
	m.selectedIdx = 0

	m2, _ := pressKey(m, 'i')
	assert.Equal(t, m.promptCmdExpanded, m2.promptCmdExpanded,
		"i should not toggle prompt command expand outside the prompt view")
	assert.Equal(t, m.logCmdExpanded, m2.logCmdExpanded,
		"i should not toggle log command expand outside the log view")
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

	m.logCmdExpanded = true
	expanded := cmdHeaderLine(m.View().Content)
	assert.NotContains(t, expanded, "…", "expanded command header line should not be truncated")
}

func TestPromptCommandWrapsByDefault(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 12
	job := makeJob(1, withAgent("test"))

	lines := m.commandHeaderLines(&job, m.promptCmdExpanded)

	assert.Greater(t, len(lines), 1)
	assert.NotContains(t, strings.Join(lines, "\n"), "…")
}

func TestCommandExpansionStateIsIndependentByView(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.height = 30
	m.width = 12
	job := makeJob(1, withAgent("test"))
	m.jobs = []storage.ReviewJob{job}
	m.currentReview = &storage.Review{
		ID:     1,
		JobID:  1,
		Agent:  "test",
		Prompt: "hello",
		Job:    &job,
	}

	assert.True(t, m.promptCmdExpanded)
	assert.False(t, m.logCmdExpanded)

	m.currentView = viewKindPrompt
	promptCollapsed, _ := pressKey(m, 'i')
	assert.False(t, promptCollapsed.promptCmdExpanded)
	assert.False(t, promptCollapsed.logCmdExpanded)

	promptCollapsed.currentView = viewLog
	promptCollapsed.logJobID = 1
	logExpanded, _ := pressKey(promptCollapsed, 'i')
	assert.False(t, logExpanded.promptCmdExpanded)
	assert.True(t, logExpanded.logCmdExpanded)
}

func TestPromptViewLabelsCommandToggle(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewKindPrompt
	m.height = 30
	m.width = 160
	job := makeJob(1, withAgent("test"))
	m.currentReview = &storage.Review{
		ID:     1,
		JobID:  1,
		Agent:  "test",
		Prompt: "hello",
		Job:    &job,
	}

	view := m.View().Content
	assert.Contains(t, view, "toggle cmd")
	assert.NotContains(t, view, "expand cmd")
}
