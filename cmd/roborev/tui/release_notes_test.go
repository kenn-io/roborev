package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestReleaseNotesViewRendersAndCloses(t *testing.T) {
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
