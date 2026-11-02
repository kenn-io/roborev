package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestReleaseNotesViewRendersAndCloses(t *testing.T) {
	t.Parallel()
	m := initTestModel(withCurrentView(viewReleaseNotes), withDimensions(100, 28))
	m.releaseNotesFromView = viewQueue
	m.releaseNotes = []storage.ReleaseNote{{
		TagName: "v1.2.3", Name: "Roborev 1.2.3", Body: "## Changes\n\nFixed the queue.",
		PublishedAt: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
	}}

	output := m.renderReleaseNotesView()
	assert.Contains(t, output, "Roborev 1.2.3")
	assert.Contains(t, output, "Fixed the queue")

	updated, _ := m.handleReleaseNotesKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	closed, ok := updated.(model)
	require.True(t, ok)
	assert.Equal(t, viewQueue, closed.currentView)
}

func TestReleaseNotesLinesCached(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	m := initTestModel(withCurrentView(viewReleaseNotes), withDimensions(100, 28))
	require.NotNil(t, m.mdCache)
	m.releaseNotes = []storage.ReleaseNote{{
		TagName: "v1.2.3", Name: "Roborev 1.2.3", Body: "Fixed the queue.",
		PublishedAt: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
	}}

	first := m.releaseNotesLines()
	second := m.releaseNotesLines()
	assert.Same(&first[0], &second[0], "unchanged input should reuse cached lines")

	m.width = 60
	resized := m.releaseNotesLines()
	assert.NotSame(&first[0], &resized[0], "width change should re-render")

	m.releaseNotes[0].Body = "Fixed the worker."
	changed := m.releaseNotesLines()
	assert.NotSame(&resized[0], &changed[0], "content change should re-render")
	assert.Contains(strings.Join(changed, "\n"), "Fixed the worker")
}
